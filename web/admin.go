package web

import (
	"encoding/json"
	"net/http"
	"strconv"

	"llmproxy/config"
	"llmproxy/proxy"
)

// adminRoutes registers the dashboard static files and its JSON API. Public
// routes (login, static assets) are open; all /api/* data routes require a
// valid admin JWT via requireAuth.
func (s *Server) adminRoutes(mux *http.ServeMux) {
	// Static dashboard (login page is served by the same SPA; it self-gates)
	mux.Handle("/", http.FileServer(http.FS(s.adminFS)))

	// Public: login
	mux.HandleFunc("/api/login", s.apiLogin)

	// Protected: everything below requires a valid JWT
	mux.HandleFunc("/api/password", s.requireAuth(s.apiPassword))
	mux.HandleFunc("/api/config", s.requireAuth(s.apiConfig))
	mux.HandleFunc("/api/upstreams", s.requireAuth(s.apiUpstreams))
	mux.HandleFunc("/api/upstreams/add", s.requireAdmin(s.apiUpstreamAdd))
	mux.HandleFunc("/api/upstreams/update", s.requireAdmin(s.apiUpstreamUpdate))
	mux.HandleFunc("/api/upstreams/delete", s.requireAdmin(s.apiUpstreamDelete))
	mux.HandleFunc("/api/clientkeys", s.requireAdmin(s.apiClientKeys)) // full secrets: admin only
	mux.HandleFunc("/api/clientkeys/add", s.requireAdmin(s.apiClientKeyAdd))
	mux.HandleFunc("/api/clientkeys/update", s.requireAdmin(s.apiClientKeyUpdate))
	mux.HandleFunc("/api/clientkeys/delete", s.requireAdmin(s.apiClientKeyDelete))
	mux.HandleFunc("/api/clientkeys/reset", s.requireAdmin(s.apiClientKeyReset))
	mux.HandleFunc("/api/test", s.requireAdmin(s.apiTest))
	mux.HandleFunc("/api/alert", s.requireAuth(s.apiAlert))
	mux.HandleFunc("/api/users", s.requireAdmin(s.apiUsers))
	mux.HandleFunc("/api/users/add", s.requireAdmin(s.apiUserAdd))
	mux.HandleFunc("/api/users/delete", s.requireAdmin(s.apiUserDelete))

	// Monitoring (protected)
	mux.HandleFunc("/api/calls", s.requireAuth(s.apiCalls))
	mux.HandleFunc("/api/stats", s.requireAuth(s.apiStats))
	mux.HandleFunc("/api/stats/range", s.requireAuth(s.apiStatsRange))
	mux.HandleFunc("/api/health", s.requireAuth(s.apiHealth))
	// /healthz is unauthenticated: it returns only {ok:true} so probes, load
	// balancers, and monitoring services can confirm the process is alive without
	// a dashboard token.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})
}

// apiLogin verifies username+password and returns a JWT on success.
func (s *Server) apiLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	hash, role, found := s.proxy.UserAuth(in.Username)
	if !found || !checkPassword(hash, in.Password) {
		s.writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "用户名或密码错误"})
		return
	}
	tok, err := s.issueJWT(in.Username, role)
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{
		"token": tok, "username": in.Username, "role": string(role),
	})
}

// apiPassword changes the password of the currently logged-in account. The
// caller must supply their existing password; the identity comes from the JWT
// rather than the request body so a user can never change someone else's.
func (s *Server) apiPassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	c := userFrom(r)
	if c == nil {
		s.writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid or expired token"})
		return
	}
	var in struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	hash, _, found := s.proxy.UserAuth(c.Username)
	if !found || !checkPassword(hash, in.OldPassword) {
		s.writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "当前密码不正确"})
		return
	}
	if len(in.NewPassword) < 6 {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "新密码至少 6 位"})
		return
	}
	newHash, err := hashPassword(in.NewPassword)
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := s.proxy.SetUser(c.Username, "", newHash); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) apiConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		c := s.proxy.CurrentConfig()
		s.writeJSON(w, http.StatusOK, struct {
			Retry     config.RetryStrategy    `json:"retry"`
			Failover  config.FailoverStrategy `json:"failover"`
			AdminAddr string                  `json:"admin_addr"`
			ProxyAddr string                  `json:"proxy_addr"`
		}{c.Retry, c.Failover, c.AdminAddr, c.ProxyAddr})
	case http.MethodPut:
		if c := userFrom(r); c == nil || c.Role != config.RoleAdmin {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "只读账号无权执行此操作"})
			return
		}
		var in struct {
			Retry    config.RetryStrategy    `json:"retry"`
			Failover config.FailoverStrategy `json:"failover"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if err := s.proxy.UpdatePolicy(in.Retry, in.Failover); err != nil {
			s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) apiUpstreams(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s.writeJSON(w, http.StatusOK, s.proxy.ListUpstreams())
}

func (s *Server) apiUpstreamAdd(w http.ResponseWriter, r *http.Request) {
	var u config.Upstream
	if err := json.NewDecoder(r.Body).Decode(&u); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.proxy.AddUpstream(u); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) apiUpstreamUpdate(w http.ResponseWriter, r *http.Request) {
	var u proxy.UpstreamPatch
	if err := json.NewDecoder(r.Body).Decode(&u); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.proxy.UpdateUpstream(u); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) apiUpstreamDelete(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.proxy.DeleteUpstream(in.Name); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) apiCalls(w http.ResponseWriter, r *http.Request) {
	n := 200
	if v := r.URL.Query().Get("n"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			n = parsed
		}
	}
	s.writeJSON(w, http.StatusOK, s.store.Recent(n))
}

func (s *Server) apiStats(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, s.store.Snapshot())
}

// --- client key handlers ---

func (s *Server) apiClientKeys(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s.writeJSON(w, http.StatusOK, s.proxy.ListClientKeys())
}

func (s *Server) apiClientKeyAdd(w http.ResponseWriter, r *http.Request) {
	var k config.ClientKey
	if err := json.NewDecoder(r.Body).Decode(&k); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.proxy.AddClientKey(k); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) apiClientKeyUpdate(w http.ResponseWriter, r *http.Request) {
	var k config.ClientKey
	if err := json.NewDecoder(r.Body).Decode(&k); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.proxy.UpdateClientKey(k); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) apiClientKeyDelete(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.proxy.DeleteClientKey(in.Name); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) apiClientKeyReset(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.proxy.ResetClientKeyUsage(in.Name); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// apiStatsRange returns per-day + summed aggregates for [from,to] inclusive.
// from/to are YYYY-MM-DD; missing params default to today.
func (s *Server) apiStatsRange(w http.ResponseWriter, r *http.Request) {
	from := r.URL.Query().Get("from")
	to := r.URL.Query().Get("to")
	if from == "" || to == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "from and to required (YYYY-MM-DD)"})
		return
	}
	s.writeJSON(w, http.StatusOK, s.store.Range(from, to))
}

// UpstreamHealth is the per-upstream rollup computed from recent calls.
type UpstreamHealth struct {
	Total    int     `json:"total"`              // calls sampled
	OK       int     `json:"ok"`                 // successful calls
	Failed   int     `json:"failed"`             // failed calls
	Rate     float64 `json:"rate"`               // success rate, 0-100
	AvgMS    int64   `json:"avg_ms"`             // average latency in ms
	LastErr  string  `json:"last_err,omitempty"` // most recent error type
	LastCode int     `json:"last_code,omitempty"`
}

// apiHealth aggregates the most recent n calls per upstream so the dashboard
// can show a health score without re-scanning the call log client-side.
// Query param n controls the sample size (default 200).
func (s *Server) apiHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	n := 200
	if v := r.URL.Query().Get("n"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			n = parsed
		}
	}
	calls := s.store.Recent(n)

	out := map[string]*UpstreamHealth{}
	for _, c := range calls {
		name := c.Upstream
		if name == "" {
			name = "(unknown)"
		}
		h := out[name]
		if h == nil {
			h = &UpstreamHealth{}
			out[name] = h
		}
		h.Total++
		h.AvgMS += c.DurationMs
		if c.OK {
			h.OK++
		} else {
			h.Failed++
			// keep the newest error seen (calls come oldest-first)
			h.LastErr = c.ErrType
			h.LastCode = c.StatusCode
		}
	}
	for _, h := range out {
		if h.Total > 0 {
			h.Rate = float64(h.OK) / float64(h.Total) * 100
			h.AvgMS = h.AvgMS / int64(h.Total)
		}
	}
	// Breaker state is keyed by upstream name so the dashboard can flag which
	// upstreams are currently being routed around.
	breakers := map[string]proxy.HealthSnapshot{}
	for _, b := range s.proxy.BreakerStates() {
		breakers[b.Upstream] = b
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"upstreams": out, "breakers": breakers, "sample": n})
}

// apiAlert reads (GET) or replaces (PUT) the webhook alerting configuration.
// A separate endpoint is used (rather than folding into /api/config) because an
// empty URL is a meaningful value meaning "disabled".
func (s *Server) apiAlert(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.writeJSON(w, http.StatusOK, s.proxy.CurrentConfig().Alert)
	case http.MethodPut:
		if c := userFrom(r); c == nil || c.Role != config.RoleAdmin {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "只读账号无权执行此操作"})
			return
		}
		var in config.AlertConfig
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if in.FailStreak <= 0 {
			in.FailStreak = 3
		}
		if in.CooldownSec <= 0 {
			in.CooldownSec = 600
		}
		if in.Enabled && in.URL == "" {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "启用告警时必须填写 Webhook URL"})
			return
		}
		if err := s.proxy.UpdateAlert(in); err != nil {
			s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// --- dashboard user management (admin only) ---

// apiUsers lists dashboard accounts. Password hashes are never sent.
func (s *Server) apiUsers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s.writeJSON(w, http.StatusOK, s.proxy.ListUsers())
}

// apiUserAdd creates a dashboard account with a role.
func (s *Server) apiUserAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		Username string      `json:"username"`
		Password string      `json:"password"`
		Role     config.Role `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if in.Username == "" || in.Password == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "用户名和密码必填"})
		return
	}
	if len(in.Password) < 6 {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "密码至少 6 位"})
		return
	}
	hash, err := hashPassword(in.Password)
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := s.proxy.AddUser(config.User{
		Username:     in.Username,
		PasswordHash: hash,
		Role:         in.Role,
	}); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// apiUserDelete removes a dashboard account. The last admin cannot be removed.
func (s *Server) apiUserDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		Username string `json:"username"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.proxy.DeleteUser(in.Username); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// apiTest lets the dashboard send a raw request through the proxy and see the
// raw result, for verifying an upstream works before enabling it.
func (s *Server) apiTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	// Reuse the proxy's forwarding path, but record the result and return it
	// verbatim to the dashboard. This requires building a synthetic upstream
	// request from the dashboard's JSON body.
	s.proxy.TestThrough(w, r)
}

// cors allows the dashboard (distinct port) to call the proxy port for test
// requests; it is only applied to the proxy listener.
func cors(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, x-api-key, anthropic-version, X-LLMPROXY-Protocol")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}
