package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"llmproxy/config"
	"llmproxy/stats"
)

// Proxy runs the forwarding, retry and failover logic.
type Proxy struct {
	cfgPath string
	mu      sync.RWMutex
	cfg     *config.Config
	store   *stats.Store

	// per-protocol round-robin cursors
	rrMu   sync.Mutex
	cursor map[tierKey]int

	// webhook alerting state (consecutive failures + rate limit per upstream)
	alertMu    sync.Mutex
	alertState map[string]*alertState

	// circuit-breaker state used by the picker to avoid failing upstreams
	healthMu sync.Mutex
	health   map[string]*upstreamHealth

	client *http.Client // shared transport; no overall timeout so SSE isn't cut
}

func New(cfg *config.Config, store *stats.Store) *Proxy {
	// Timeout is disabled (0) on the Client because streamed responses can
	// legitimately outlive a fixed deadline. Per-attempt deadlines are set via
	// request context only for non-SSE requests.
	return &Proxy{
		cfgPath:    "",
		cfg:        cfg,
		store:      store,
		cursor:     map[tierKey]int{},
		alertState: map[string]*alertState{},
		health:     map[string]*upstreamHealth{},
		client: &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// SetConfigPath wires the config file path so Save() can persist changes made
// via the dashboard.
func (p *Proxy) SetConfigPath(path string) {
	p.mu.Lock()
	p.cfgPath = path
	p.mu.Unlock()
}

// ServeHTTP is the transparent entry point. It performs:
//
//	for each upstream in the protocol group:
//	  for attempt in 1..MaxAttempts:
//	    send verbatim request
//	    if success / non-retryable: stream back, return
//	    else if attempt remains: backoff & retry same upstream
//	    else: mark failover, next upstream
//
// If every upstream is exhausted, the last response body is streamed back
// (status code preserved) so the client sees an unmodified upstream error.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	proto := detectProtocol(r)
	ctx := r.Context()

	// Authenticate the caller against configured client keys before doing any
	// work. In open mode (no keys configured) clientKey is "" and the call
	// proceeds unauthenticated. Requests marked trusted came from the dashboard's
	// upstream test, which the admin layer already authenticated with a panel
	// JWT; running the client-key gate on them would reject that JWT as an
	// invalid client key.
	var clientKey string
	if !trusted(ctx) {
		var autherr error
		clientKey, autherr = p.authenticateClient(r.Header.Get("Authorization"))
		if autherr != nil {
			writeAuthError(w, autherr)
			return
		}
	}
	// Carry the client key on the request context so record()/token accounting
	// stay per-request safe under concurrency.
	ctx = context.WithValue(ctx, ctxKeyClient, clientKey)

	// Budget for the retry/failover phase. It is enforced as a check between
	// attempts rather than as a context deadline, so it can never truncate the
	// response body we end up streaming back -- a long SSE answer is legitimate
	// and must not be cut off by the retry budget.
	deadline := p.retryDeadline(start)
	r = r.WithContext(ctx)

	// Parse the auth header so the key can be swapped per upstream while
	// leaving the rest of the request byte-identical. We read the body now so
	// it can be replayed on retries; for SSE the body is a normal JSON body.
	origAuth := r.Header.Get("Authorization")
	origXAPIKey := r.Header.Get("x-api-key")
	body, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()

	// Snapshot the upstream group at call start; config changes during a call
	// apply to the next call.
	var group []config.Upstream
	if pinned := r.Header.Get("X-LLMPROXY-Upstream"); pinned != "" { // dashboard test pinned to one upstream
		group = p.pinnedGroup(proto, pinned)
	} else {
		group = p.snapshotGroup(proto)
	}
	group = p.limitRotations(group)

	attempts, failovers := 0, 0
	var lastBody bytes.Buffer
	var lastStatus int
	var lastClass ErrClass
	var lastDetail string
	var lastOK bool
	var lastUp config.Upstream // upstream that actually produced the final response
	timedOut := false          // retry budget ran out before any upstream answered

	for i, up := range group {
		attempts++ // first attempt on this upstream
		var retErr error
		var cls ErrClass
		var detail string

		for attempt := 1; attempt <= p.maxAttempts(); attempt++ {
			// Stop starting new attempts once the budget is spent; whatever we have
			// buffered so far becomes the response.
			if attempts > 0 && budgetExhausted(deadline) {
				timedOut = true
				break
			}
			if attempt > 1 {
				attempts++
			}
			resp, err := p.sendOne(ctx, up, r, body, origAuth, origXAPIKey, proto, attempt > 1)

			if err != nil {
				cls = classifyTransport(err)
				detail = err.Error()
				// Track this as the last-seen upstream/outcome so the final
				// fallback records a transport failure correctly (there is no
				// HTTP response to buffer).
				lastUp = up
				lastClass = cls
				lastDetail = detail
				lastOK = false
				lastStatus = http.StatusBadGateway
				lastBody.Reset()
				lastBody.WriteString(`{"error":{"message":"upstream transport error: ` + jsonEscape(detail) + `","type":"proxy_error"}}`)
				if !p.retryable(cls) || attempt >= p.maxAttempts() {
					retErr = err
					// This upstream is out of attempts: feed the breaker now, since
					// record() only runs once for the whole request and would
					// otherwise never see the upstreams we failed over from.
					p.recordHealth(up.Name, false, cls)
					if attempt >= p.maxAttempts() && i < len(group)-1 {
						failovers++ // exhausted this upstream at transport level -> failover
					}
					break
				}
				if d := p.backoff(attempt); !waitFits(deadline, d) || !sleep(ctx, d) {
					retErr = err
					timedOut = timedOut || !waitFits(deadline, d)
					break
				}
				continue
			}

			lastStatus = resp.StatusCode
			lastUp = up
			cls = classifyResponse(resp.StatusCode)
			detail = http.StatusText(resp.StatusCode)
			lastOK = cls == ErrNone

			// Success, or an error that belongs to the caller (400/404): hand it
			// back verbatim. Trying another upstream would not change the answer.
			if !p.retryable(cls) && !failoverable(cls) {
				committed, serr := p.streamBack(w, r, resp, up, start, proto, cls, detail, attempts, failovers)
				_ = resp.Body.Close()
				if serr == nil {
					return
				}
				slog.Warn("streaming back failed", "err", serr, "committed", committed)
				if committed {
					// Bytes have already reached the caller and headers cannot be
					// unwritten, so no recovery is possible for this request.
					return
				}
				// Nothing went out yet: this upstream still owes a usable answer,
				// so treat it as a failed attempt and honour the retry budget
				// instead of abandoning the caller with an empty response.
				scls := classifyTransport(serr)
				lastUp = up
				lastClass = scls
				lastDetail = serr.Error()
				lastOK = false
				lastStatus = http.StatusBadGateway
				lastBody.Reset()
				lastBody.WriteString(`{"error":{"message":"upstream stream error: ` + jsonEscape(serr.Error()) + `","type":"proxy_error"}}`)
				if cls == ErrNone && attempt < p.maxAttempts() {
					if d := p.backoff(attempt); waitFits(deadline, d) && sleep(ctx, d) {
						continue
					}
					timedOut = true
				}
				if i < len(group)-1 {
					failovers++
				}
				p.recordHealth(up.Name, false, scls)
				break
			}

			// Buffer the body so it can be replayed as the final response if every
			// remaining upstream fails too.
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			lastBody.Reset()
			lastBody.Write(b)
			lastClass = cls
			lastDetail = detail

			// Rejected credential (401/403): retrying the same key is pointless,
			// so skip the remaining attempts and move to the next upstream.
			if !p.retryable(cls) {
				p.recordHealth(up.Name, false, cls)
				if i < len(group)-1 {
					failovers++
				}
				break
			}

			if attempt >= p.maxAttempts() {
				// exhausted this upstream -> failover
				p.recordHealth(up.Name, false, cls)
				if i < len(group)-1 {
					failovers++
				}
				break
			}
			// An upstream that sends Retry-After has told us exactly how long to
			// wait; honour it instead of guessing with exponential backoff.
			wait := retryAfter(resp)
			if wait == 0 {
				wait = p.backoff(attempt)
			}
			if !waitFits(deadline, wait) || !sleep(ctx, wait) {
				timedOut = timedOut || !waitFits(deadline, wait)
				break
			}
		}

		_ = retErr
		// The budget also bounds failover: moving to the next upstream would start
		// a fresh round of attempts we have no time for.
		if budgetExhausted(deadline) {
			timedOut = true
		}
		// If we ran out of upstreams (or time), stream the last buffered body.
		if i >= len(group)-1 || timedOut {
			status := lastStatus
			if status == 0 {
				status = http.StatusBadGateway
			}
			body := lastBody.Bytes()
			cls := lastClass
			detail := lastDetail
			if timedOut {
				// Be explicit that the proxy gave up on its own clock rather than
				// relaying an upstream verdict, so this is distinguishable in the log.
				status = http.StatusGatewayTimeout
				cls = ErrTimeout
				detail = "retry budget exhausted"
				body = []byte(`{"error":{"message":"proxy retry budget exhausted before any upstream responded","type":"timeout","code":"retry_budget_exhausted"}}`)
			}
			writeRaw(w, r, status, body)
			p.record(r, lastUp, proto, start, status, lastOK, cls, detail, attempts, failovers, enrichment{})
			return
		}
	}

	// shouldn't reach here with a non-empty group; defensive fallback
	writeRaw(w, r, http.StatusBadGateway, nil)
}

// sendOne performs a single proxied request to one upstream, with the upstream's
// auth in place. retrying is true on attempts >1 of the same upstream, which
// lets us keep per-attempt handling uniform. clientProto is the protocol the
// caller spoke; when it differs from the upstream's protocol and the upstream is
// configured to bridge, the request/response are translated here.
func (p *Proxy) sendOne(ctx context.Context, up config.Upstream, r *http.Request, body []byte, origAuth, origXAPIKey string, clientProto config.Protocol, retrying bool) (*http.Response, error) {
	// An OpenAI-speaking caller reaching an Anthropic upstream with the bridge on
	// requires translating both ways -- but only for the one endpoint that has
	// an Anthropic equivalent. /v1/models, /v1/embeddings, etc. have no
	// counterpart to rewrite the path to and no chat-shaped body to translate
	// (a GET has no body at all), so they are forwarded to the upstream as-is.
	translate := up.Protocol == config.ProtocolAnthropic && up.TranslateToOpenAI &&
		clientProto == config.ProtocolOpenAI && routePath(r) == "/v1/chat/completions"

	targetPath := r.URL.RequestURI()
	if translate {
		var err error
		body, err = RequestOpenAIToAnthropic(body)
		if err != nil {
			return nil, err
		}
		targetPath = "/v1/messages"
	}

	outReq, err := http.NewRequestWithContext(ctx, r.Method, joinURL(up.BaseURL, targetPath), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	// Copy headers verbatim, dropping hop-by-hop ones and the routing hint.
	copyHeaders(outReq.Header, r.Header)
	outReq.Header.Del("X-LLMPROXY-Protocol")
	outReq.Header.Del("X-LLMPROXY-Upstream")
	// Strip any inbound proxy-delimited headers we must not leak.
	stripHopHeaders(outReq.Header)
	// The request body is either replayed from a buffer or rewritten (protocol
	// translation), so any inbound length/encoding header is now stale. Drop them
	// and let the transport recompute from outReq.Body; a leftover Content-Length
	// would make the upstream block waiting for bytes that never arrive.
	outReq.Header.Del("Content-Length")
	outReq.Header.Del("Transfer-Encoding")
	outReq.Header.Del("Content-Encoding")

	// Apply upstream auth based on protocol.
	switch up.Protocol {
	case config.ProtocolAnthropic:
		// Always present the upstream's own credential, even when the caller
		// authenticated some other way (the dashboard test sends only a panel
		// JWT). Keying off the inbound x-api-key instead would send this upstream
		// no usable credential at all.
		outReq.Header.Set("x-api-key", up.APIKey)
		// Anthropic authenticates via x-api-key, so any inbound Authorization is
		// the caller's credential to *this* proxy (a client key, or the panel JWT
		// on a dashboard test). It is never valid upstream and must not be leaked
		// to a third party. A gateway that wants Bearer instead can set it back
		// via the upstream's extra_headers, which are merged below.
		outReq.Header.Del("Authorization")
		// The Anthropic API requires an anthropic-version; an OpenAI client never
		// sends one, so supply the default when we are bridging.
		if translate && outReq.Header.Get("anthropic-version") == "" {
			outReq.Header.Set("anthropic-version", defaultAnthropicVersion)
		}
	default: // openai
		outReq.Header.Set("Authorization", "Bearer "+up.APIKey)
	}

	// Override the User-Agent only when the upstream asks for a specific one;
	// otherwise the caller's original UA is forwarded untouched (feature default).
	if up.UserAgent != "" {
		outReq.Header.Set("User-Agent", up.UserAgent)
	}

	// Merge configured extra headers (last, so operators can add any header they
	// need; auth headers above are intentionally not part of this set).
	for k, v := range up.ExtraHeaders {
		outReq.Header.Set(k, v)
	}

	resp, err := p.client.Do(outReq)
	if err != nil {
		return nil, err
	}
	if translate {
		resp = p.translateResponse(resp)
	}
	return resp, nil
}

// translateResponse rewrites an Anthropic upstream response into OpenAI form for
// an OpenAI-speaking caller, preserving streaming where the upstream used it.
// Non-2xx upstream errors keep their status code and are converted to the OpenAI
// error shape, so an OpenAI client still sees the real cause instead of an empty
// success body.
func (p *Proxy) translateResponse(resp *http.Response) *http.Response {
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return p.translateError(resp)
	}
	isSSE := strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
	if !isSSE {
		b, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			resp.Body = io.NopCloser(strings.NewReader(`{"error":{"message":"anthropic response read error","type":"proxy_error"}}`))
			resp.StatusCode = http.StatusBadGateway
			setBodyLen(resp, nil)
			return resp
		}
		conv, err := ResponseAnthropicToOpenAI(b)
		if err != nil {
			conv = []byte(`{"error":{"message":` + jsonEscape(err.Error()) + `,"type":"proxy_error"}}`)
			resp.StatusCode = http.StatusBadGateway
		}
		resp.Body = io.NopCloser(bytes.NewReader(conv))
		resp.Header.Set("Content-Type", "application/json")
		setBodyLen(resp, conv)
		return resp
	}

	// Streaming: wrap the Anthropic SSE body in a pull-based translator so the
	// caller receives OpenAI SSE incrementally as it reads. No goroutine / pipe is
	// involved, so there is no producer/consumer deadlock.
	resp.Body = &anthropicSSETranslator{
		src:          resp.Body,
		br:           bufio.NewReader(resp.Body),
		blockToolIdx: map[int]int{},
	}
	resp.Header.Set("Content-Type", "text/event-stream")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	resp.TransferEncoding = []string{"chunked"}
	return resp
}

// setBodyLen keeps the emitted Content-Length header in sync after the body was
// replaced. streamBack copies resp.Header verbatim, so a stale length inherited
// from the upstream body would make Go's server cut the translated response
// short (or hang the client waiting for the missing bytes).
func setBodyLen(resp *http.Response, body []byte) {
	if body == nil {
		resp.Header.Del("Content-Length")
		resp.ContentLength = -1
		return
	}
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	resp.ContentLength = int64(len(body))
	resp.Header.Del("Transfer-Encoding")
}

// translateError passes an upstream error response through with its status code,
// converting an Anthropic error body ({"type":"error","error":{...}}) into the
// OpenAI error shape so OpenAI clients surface the real cause. Bodies that are
// not Anthropic errors (gateways returning HTML/JSON of their own) are forwarded
// unchanged -- hiding them would only make diagnosis harder.
func (p *Proxy) translateError(resp *http.Response) *http.Response {
	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		b = []byte(`{"type":"error","error":{"type":"api_error","message":"anthropic response read error"}}`)
	}
	var ae struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	out := b
	if json.Unmarshal(b, &ae) == nil && ae.Type == "error" && ae.Error.Message != "" {
		typ := ae.Error.Type
		if typ == "" {
			typ = "api_error"
		}
		if nb, merr := json.Marshal(map[string]any{
			"error": map[string]any{"message": ae.Error.Message, "type": typ},
		}); merr == nil {
			out = nb
		}
		resp.Header.Set("Content-Type", "application/json")
	}
	// The original body is closed above; hand back the (possibly converted) bytes.
	resp.Body = io.NopCloser(bytes.NewReader(out))
	setBodyLen(resp, out)
	return resp
}

// streamBack writes the upstream response verbatim to the client and streams the
// body. For SSE / chunked responses it flushes after every read so chunks reach
// the client as they arrive instead of being buffered into one block. While
// forwarding, it tees the bytes into a bounded buffer so token/model/provider can
// be parsed afterwards without altering what the client receives.
//
// It returns whether anything was already committed to the client, plus any error
// that ended the transfer.
//
// Status and headers are deliberately withheld until the first body byte arrives.
// An upstream that dies before emitting anything therefore leaves the response
// uncommitted, so the caller can still retry under the normal policy instead of
// handing the caller a truncated answer. Once bytes have gone out that option is
// gone -- HTTP headers cannot be unwritten -- so a later truncation is recorded as
// a failure rather than silently counted as a success.
func (p *Proxy) streamBack(w http.ResponseWriter, r *http.Request, resp *http.Response, up config.Upstream, start time.Time, proto config.Protocol, cls ErrClass, detail string, attempts, failovers int) (committed bool, err error) {
	isSSE := strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)

	// Bounded side buffer for enrichment parsing (256 KiB). Beyond that we stop
	// teeing; token/model then stay unknown for that (rare, huge) response.
	const sideCap = 256 * 1024
	var side bytes.Buffer

	// Lazily emit status/headers on the first byte, so an upstream that never
	// produces a body cannot leave the client holding a half-written answer.
	wrote := false
	commit := func() {
		if wrote {
			return
		}
		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		wrote = true
	}

	var streamErr error
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			commit()
			if _, werr := w.Write(buf[:n]); werr != nil {
				streamErr = werr
				break
			}
			if flusher != nil {
				flusher.Flush() // push each chunk immediately (SSE-safe)
			}
			if side.Len() < sideCap {
				side.Write(buf[:n])
			}
		}
		if rerr != nil {
			// io.EOF is how a healthy stream ends; anything else means the
			// upstream died part-way through its answer.
			if rerr != io.EOF {
				streamErr = rerr
			}
			break
		}
	}
	// A clean transfer with an empty body still owes the client its status code.
	if streamErr == nil {
		commit()
	}

	ok := cls == ErrNone && streamErr == nil
	if streamErr != nil {
		cls = classifyTransport(streamErr)
		detail = "stream interrupted: " + streamErr.Error()
	}

	// Record only attempts that either reached the caller or finished cleanly. An
	// uncommitted failure gets retried and the retry records the outcome itself;
	// writing both would double-count one client request in the dashboard.
	if wrote || streamErr == nil {
		enr := enrichment{}
		if ok && side.Len() > 0 {
			if isSSE {
				enr = parseSSE(splitSSEData(side.Bytes()))
			} else {
				enr = parseBody(side.Bytes())
			}
		}
		p.record(r, up, proto, start, resp.StatusCode, ok, cls, detail, attempts, failovers, enr)
	}
	return wrote, streamErr
}

// splitSSEData splits an accumulated SSE byte stream into its `data:` lines.
func splitSSEData(b []byte) [][]byte {
	var out [][]byte
	for _, line := range bytes.Split(b, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if len(line) == 0 {
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			out = append(out, line)
		}
	}
	return out
}

// TestThrough routes a dashboard test request through the full proxy path
// (retry + failover) and writes the verbatim upstream response back to the
// dashboard. It reuses ServeHTTP behaviour but runs on the admin response
// writer.
func (p *Proxy) TestThrough(w http.ResponseWriter, r *http.Request) {
	// The admin layer has already authenticated this caller; mark it so the
	// client-key gate is skipped rather than rejecting the panel JWT.
	out := r.Clone(withTrusted(r.Context()))

	// The dashboard posts to /api/test, but the upstream must receive a real
	// inference path -- forwarding "/api/test" verbatim makes the upstream 404.
	// Rewrite it to the canonical path for the protocol under test.
	switch detectProtocol(r) {
	case config.ProtocolAnthropic:
		out.URL.Path = "/v1/messages"
	default:
		out.URL.Path = "/v1/chat/completions"
	}
	out.URL.RawQuery = ""

	p.ServeHTTP(w, out)
}

// pinnedGroup returns a single-upstream group when the dashboard pins a test
// to one named upstream; falls back to the normal group otherwise.
func (p *Proxy) pinnedGroup(proto config.Protocol, name string) []config.Upstream {
	p.mu.RLock()
	var found *config.Upstream
	for i := range p.cfg.Upstreams {
		u := p.cfg.Upstreams[i]
		if !u.Enabled || u.Name != name {
			continue
		}
		if u.Protocol == proto {
			f := u
			found = &f
			break
		}
		// Allow pinning a translate-enabled Anthropic upstream from an OpenAI test.
		if proto == config.ProtocolOpenAI && u.Protocol == config.ProtocolAnthropic && u.TranslateToOpenAI {
			f := u
			found = &f
			break
		}
	}
	p.mu.RUnlock()

	if found != nil {
		return []config.Upstream{*found}
	}
	// No pinned match: fall back to the normal rotation. This must run with the
	// read lock released -- snapshotGroup takes its own RLock, and re-entering a
	// read lock deadlocks whenever a writer is already queued.
	return p.snapshotGroup(proto)
}

// CurrentConfig returns a read-only copy of the running config for the admin API.
func (p *Proxy) CurrentConfig() *config.Config {
	p.mu.RLock()
	defer p.mu.RUnlock()
	c := *p.cfg
	// Deep-copy the slices: a shallow copy still aliases the live backing arrays,
	// which concurrent mutations (addUsedTokens runs on every proxied call) write
	// to under the lock while the caller reads them without it.
	c.Upstreams = append([]config.Upstream(nil), p.cfg.Upstreams...)
	c.ClientKeys = append([]config.ClientKey(nil), p.cfg.ClientKeys...)
	c.Users = append([]config.User(nil), p.cfg.Users...)
	for i := range c.Upstreams {
		if h := c.Upstreams[i].ExtraHeaders; h != nil {
			cp := make(map[string]string, len(h))
			for k, v := range h {
				cp[k] = v
			}
			c.Upstreams[i].ExtraHeaders = cp
		}
	}
	// Same reasoning for the pricing table: the struct copy above aliases the
	// live map, which UpdatePricing replaces wholesale under the lock.
	if m := p.cfg.Pricing.Models; m != nil {
		cp := make(map[string]config.ModelPrice, len(m))
		for k, v := range m {
			cp[k] = v
		}
		c.Pricing.Models = cp
	}
	return &c
}

// record logs one call into the stats store. enr carries best-effort
// token/model/provider parsed from the response (zero value when unavailable).
func (p *Proxy) record(r *http.Request, up config.Upstream, proto config.Protocol, start time.Time, status int, ok bool, cls ErrClass, detail string, attempts, failovers int, enr enrichment) {
	// A successful call carries no error classification. Some success paths
	// inherit a non-empty detail (e.g. http.StatusText(200) == "OK"); clearing
	// it here keeps the stored record honest so the dashboard never renders a
	// red "error" badge for a call that actually succeeded.
	if ok {
		cls = ErrNone
		detail = ""
	}
	clientKey := clientKeyFrom(r)
	// Charge tokens against the calling client key's quota.
	if enr.TotalTokens > 0 {
		p.addUsedTokens(clientKey, int64(enr.TotalTokens))
	}
	p.store.RecordCall(stats.Call{
		Time:       start,
		Protocol:   string(proto),
		Upstream:   up.Name,
		Router:     routePath(r),
		KeyPrefix:  keyPrefix(up.APIKey),
		ClientKey:  clientKey,
		StatusCode: status,
		DurationMs: time.Since(start).Milliseconds(),
		OK:         ok,
		ErrType:    string(cls),
		ErrDetail:  detail,
		Failovers:  failovers,
		Attempts:   attempts,

		PromptTokens:     enr.PromptTokens,
		CompletionTokens: enr.CompletionTokens,
		TotalTokens:      enr.TotalTokens,
		RealModel:        enr.RealModel,
		Provider:         enr.Provider,

		ClientIP:  clientIP(r),
		UserAgent: r.Header.Get("User-Agent"),
	})

	// Only successes are fed to the breaker here: failures are recorded per
	// upstream inside ServeHTTP as each one is exhausted, because record() runs
	// once per request and never sees the upstreams that were failed over from.
	// Counting a failure again here would double-count the last upstream.
	if ok {
		p.recordHealth(up.Name, true, cls)
	}
	p.notifyAlert(up.Name, ok, string(cls), detail)
}

// retryDeadline returns the instant after which no further attempt may start,
// or the zero Time when the budget is disabled. It is deliberately not a context
// deadline: attaching one to the request would also kill the response body we
// stream back, and a slow-but-valid SSE answer must survive.
func (p *Proxy) retryDeadline(start time.Time) time.Time {
	p.mu.RLock()
	sec := p.cfg.Retry.TotalTimeoutSec
	p.mu.RUnlock()
	if sec <= 0 {
		return time.Time{}
	}
	return start.Add(time.Duration(sec) * time.Second)
}

// budgetExhausted reports whether the retry budget has run out. A zero deadline
// means unlimited.
func budgetExhausted(deadline time.Time) bool {
	return !deadline.IsZero() && !time.Now().Before(deadline)
}

// waitFits reports whether a planned backoff would still land inside the budget.
// Sleeping past the deadline only delays the inevitable failure.
func waitFits(deadline time.Time, d time.Duration) bool {
	return deadline.IsZero() || time.Now().Add(d).Before(deadline)
}

func (p *Proxy) maxAttempts() int {
	p.mu.RLock()
	n := p.cfg.Retry.MaxAttempts
	p.mu.RUnlock()
	if n < 1 {
		return 1
	}
	return n
}

// failoverable reports whether another upstream is worth trying. Auth failures
// are not retried against the same upstream -- a rejected key stays rejected --
// but a different upstream may well have a working one.
func failoverable(cls ErrClass) bool {
	switch cls {
	case ErrRateLimit, ErrServer, ErrTimeout, ErrNetwork, ErrAuth:
		return true
	default:
		return false // success, or the caller's own bad request
	}
}

// retryable reports whether the same upstream is worth another attempt.
func (p *Proxy) retryable(cls ErrClass) bool {
	p.mu.RLock()
	rt := p.cfg.Retry
	p.mu.RUnlock()
	switch cls {
	case ErrRateLimit:
		return rt.RetryOn429
	case ErrServer:
		return rt.RetryOn5xx
	case ErrTimeout:
		return rt.RetryTimeout
	case ErrNetwork:
		return true // always retry transport errors
	default:
		return false // business errors never retried
	}
}

// backoff returns an exponential-backoff duration with jitter.
func (p *Proxy) backoff(attempt int) time.Duration {
	p.mu.RLock()
	c := p.cfg.Retry
	p.mu.RUnlock()
	base := c.BaseDelayMS
	if base <= 0 {
		base = 500
	}
	cap := c.MaxDelayMS
	if cap <= base {
		cap = base * 8
	}
	ms := float64(base) * math.Pow(2, float64(attempt-1))
	if ms > float64(cap) {
		ms = float64(cap)
	}
	jitter := float64(ms) * 0.2 * (randFloat() - 0.5)
	return time.Duration(ms+jitter) * time.Millisecond
}

func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func stripHopHeaders(h http.Header) {
	for _, k := range []string{"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		h.Del(k)
	}
}

// joinURL joins base URL and the original request URI (path+query). If the
// base already ends with a path prefix that the request URI repeats (commonly
// "/v1"), the duplicate is dropped so an upstream configured as
// "https://host/v1" and a request "/v1/chat/completions" produce
// "https://host/v1/chat/completions" rather than a doubled "/v1/v1/...".
func joinURL(base, reqURI string) string {
	base = strings.TrimRight(base, "/")
	reqURI = "/" + strings.TrimLeft(reqURI, "/")

	// Find the base's path suffix (everything after the host). If it is a
	// non-empty path like "/v1" and reqURI starts with that same segment,
	// strip it from reqURI to avoid duplication.
	if i := strings.Index(base, "://"); i >= 0 {
		if slash := strings.Index(base[i+3:], "/"); slash >= 0 {
			basePath := base[i+3+slash:] // e.g. "/v1"
			if basePath != "" && basePath != "/" {
				// Split query so we only match on the path part.
				path, query := reqURI, ""
				if q := strings.IndexByte(reqURI, '?'); q >= 0 {
					path, query = reqURI[:q], reqURI[q:]
				}
				if path == basePath || strings.HasPrefix(path, basePath+"/") {
					path = strings.TrimPrefix(path, basePath)
					reqURI = path + query
				}
			}
		}
	}
	return base + reqURI
}

// writeRaw emits a full response with an in-memory body. Used for final
// upstream errors where we may have retried but must present the last status.
func writeRaw(w http.ResponseWriter, r *http.Request, status int, body []byte) {
	if status == 0 {
		status = http.StatusBadGateway
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(body)
}

// keyPrefix keeps only first+last 4 chars of a key for display in the dashboard.
func keyPrefix(key string) string {
	if len(key) <= 8 {
		return "****"
	}
	return key[:4] + "…" + key[len(key)-4:]
}

// jsonEscape escapes a string so it can be embedded in a JSON string literal.
func jsonEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// retryAfter reads the Retry-After response header and returns the indicated
// wait duration. It understands both delay-seconds (e.g. "30") and HTTP-date
// formats. Returns 0 when the header is absent or unparseable.
func retryAfter(resp *http.Response) time.Duration {
	if resp == nil {
		return 0
	}
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0
	}
	// Try delay-seconds first.
	if secs, err := strconv.ParseFloat(v, 64); err == nil && secs > 0 {
		// Cap at 5 minutes so a misbehaving upstream cannot park a request forever.
		if d := time.Duration(secs * float64(time.Second)); d <= 5*time.Minute {
			return d
		}
		return 5 * time.Minute
	}
	// Fall back to HTTP-date.
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			if d > 5*time.Minute {
				return 5 * time.Minute
			}
			return d
		}
	}
	return 0
}
