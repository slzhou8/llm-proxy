// Package stats records every proxied call and daily aggregates, keeping a
// bounded in-memory view for the dashboard and periodically persisting to disk.
package stats

import (
	"encoding/json"
	"log/slog"
	"os"
	"sync"
	"time"
)

// Call is one proxied request/response exchange. API keys are never stored;
// only a fingerprint is.
type Call struct {
	Time       time.Time `json:"time"`
	Protocol   string    `json:"protocol"`
	Upstream   string    `json:"upstream"`
	Router     string    `json:"router"`     // matched route (e.g. /v1/chat/completions)
	KeyPrefix  string    `json:"key_prefix"` // first 4 + last 4 of the upstream key
	ClientKey  string    `json:"client_key"` // name of the calling client key ("" in open mode)
	StatusCode int       `json:"status_code"`
	DurationMs int64     `json:"duration_ms"`
	OK         bool      `json:"ok"`
	ErrType    string    `json:"err_type,omitempty"` // rate_limit | timeout | network | server_error | business
	ErrDetail  string    `json:"err_detail,omitempty"`
	Failovers  int       `json:"failovers"` // number of upstream switches during this call
	Attempts   int       `json:"attempts"`

	// Enrichment parsed from the response body (best-effort; zero/empty if the
	// upstream did not return it).
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	TotalTokens      int    `json:"total_tokens"`
	RealModel        string `json:"real_model,omitempty"` // model the upstream actually used (model=auto -> resolved)
	Provider         string `json:"provider,omitempty"`   // upstream-reported platform/provider

	// Request-side metadata.
	ClientIP  string `json:"client_ip,omitempty"`
	UserAgent string `json:"user_agent,omitempty"`
}

// Record is the persisted snapshot.
type Record struct {
	Calls   []Call             `json:"calls"`
	ByDay   map[string]*DayAgg `json:"by_day"`
	SavedAt time.Time          `json:"saved_at"`
}

// DayAgg is per-day roll-up. Keyed by YYYY-MM-DD.
type DayAgg struct {
	Total            int64            `json:"total"`
	OK               int64            `json:"ok"`
	Failed           int64            `json:"failed"`
	RateLimit        int64            `json:"rate_limit"`
	Timeout          int64            `json:"timeout"`
	Network          int64            `json:"network"`
	ServerErr        int64            `json:"server_err"`
	BusinessErr      int64            `json:"business_err"`
	PromptTokens     int64            `json:"prompt_tokens"`
	CompletionTokens int64            `json:"completion_tokens"`
	TotalTokens      int64            `json:"total_tokens"`
	DurationSum      int64            `json:"duration_sum"`     // sum of duration_ms; avg latency = DurationSum/Total
	ByHour           [24]int64        `json:"by_hour"`          // request count per hour (local time), for same-day trend
	ByKey            map[string]int64 `json:"by_key,omitempty"` // client key name -> total tokens
	ByModel          map[string]*ModelAgg `json:"by_model,omitempty"` // real model -> call/token roll-up
}

// ModelAgg is the per-day roll-up for a single real model (the model the
// upstream actually served, which may differ from the requested one when the
// client asked for "auto" or a router alias).
type ModelAgg struct {
	Total       int64 `json:"total"`
	OK          int64 `json:"ok"`
	Failed      int64 `json:"failed"`
	TotalTokens int64 `json:"total_tokens"`
}

// Store is concurrency-safe. It keeps a bounded recent-call buffer purely in
// memory and a full day-rollup, and persists the whole state periodically.
type Store struct {
	mu      sync.RWMutex
	callBuf []Call // ring buffer of most recent calls
	bufCap  int
	byDay   map[string]*DayAgg

	file    string
	persist time.Duration
	stopCh  chan struct{}
	doneCh  chan struct{}
}

func NewStore(file string, bufCap int) (*Store, error) {
	if bufCap <= 0 {
		bufCap = 200
	}
	s := &Store{
		bufCap:  bufCap,
		byDay:   map[string]*DayAgg{},
		file:    file,
		persist: 15 * time.Second,
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
	}
	s.load()
	return s, nil
}

// RecordCall appends a call and updates day aggregates.
func (s *Store) RecordCall(c Call) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.callBuf) >= s.bufCap {
		s.callBuf = s.callBuf[1:]
	}
	s.callBuf = append(s.callBuf, c)

	day := c.Time.Format("2006-01-02")
	d := s.byDay[day]
	if d == nil {
		d = &DayAgg{}
		s.byDay[day] = d
	}
	d.Total++
	if c.OK {
		d.OK++
	} else {
		d.Failed++
	}
	d.PromptTokens += int64(c.PromptTokens)
	d.CompletionTokens += int64(c.CompletionTokens)
	d.TotalTokens += int64(c.TotalTokens)
	d.DurationSum += c.DurationMs
	d.ByHour[c.Time.Hour()]++
	if c.TotalTokens > 0 {
		if d.ByKey == nil {
			d.ByKey = map[string]int64{}
		}
		name := c.ClientKey
		if name == "" {
			name = "(open)" // unauthenticated / open mode
		}
		d.ByKey[name] += int64(c.TotalTokens)
	}
	switch c.ErrType {
	case "rate_limit":
		d.RateLimit++
	case "timeout":
		d.Timeout++
	case "network":
		d.Network++
	case "server_error":
		d.ServerErr++
	case "business":
		d.BusinessErr++
	}
	if c.RealModel != "" {
		m := d.ByModel[c.RealModel]
		if m == nil {
			m = &ModelAgg{}
			d.ByModel[c.RealModel] = m
		}
		m.Total++
		if c.OK {
			m.OK++
		} else {
			m.Failed++
		}
		m.TotalTokens += int64(c.TotalTokens)
	}
}

// Recent returns up to n most recent calls in chronological order.
func (s *Store) Recent(n int) []Call {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if n > len(s.callBuf) {
		n = len(s.callBuf)
	}
	start := len(s.callBuf) - n
	out := make([]Call, n)
	copy(out, s.callBuf[start:])
	return out
}

// Snapshot returns a copy of the day rollup.
func (s *Store) Snapshot() map[string]*DayAgg {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]*DayAgg, len(s.byDay))
	for k, v := range s.byDay {
		c := *v
		if v.ByKey != nil {
			c.ByKey = make(map[string]int64, len(v.ByKey))
			for kk, n := range v.ByKey {
				c.ByKey[kk] = n
			}
		}
		if v.ByModel != nil {
			c.ByModel = make(map[string]*ModelAgg, len(v.ByModel))
			for kk, n := range v.ByModel {
				c.ByModel[kk] = &ModelAgg{Total: n.Total, OK: n.OK, Failed: n.Failed, TotalTokens: n.TotalTokens}
			}
		}
		out[k] = &c
	}
	return out
}

// RangeResult is the response for a windowed stats query.
type RangeResult struct {
	From  string             `json:"from"`  // YYYY-MM-DD inclusive
	To    string             `json:"to"`    // YYYY-MM-DD inclusive
	Days  map[string]*DayAgg `json:"days"`  // per-day within window
	Total DayAgg             `json:"total"` // summed across window
}

// Range returns per-day aggregates and their sum for [from, to] inclusive,
// where from/to are YYYY-MM-DD strings. Days with no data are omitted from the
// map but contribute zero to the total.
func (s *Store) Range(from, to string) RangeResult {
	s.mu.RLock()
	defer s.mu.RUnlock()
	res := RangeResult{From: from, To: to, Days: map[string]*DayAgg{}}
	for day, v := range s.byDay {
		if day < from || day > to {
			continue
		}
		c := *v
		if v.ByKey != nil {
			c.ByKey = make(map[string]int64, len(v.ByKey))
			for k, n := range v.ByKey {
				c.ByKey[k] = n
			}
		}
		if v.ByModel != nil {
			c.ByModel = make(map[string]*ModelAgg, len(v.ByModel))
			for k, n := range v.ByModel {
				c.ByModel[k] = &ModelAgg{Total: n.Total, OK: n.OK, Failed: n.Failed, TotalTokens: n.TotalTokens}
			}
		}
		res.Days[day] = &c
		res.Total.Total += v.Total
		res.Total.OK += v.OK
		res.Total.Failed += v.Failed
		res.Total.RateLimit += v.RateLimit
		res.Total.Timeout += v.Timeout
		res.Total.Network += v.Network
		res.Total.ServerErr += v.ServerErr
		res.Total.BusinessErr += v.BusinessErr
		res.Total.PromptTokens += v.PromptTokens
		res.Total.CompletionTokens += v.CompletionTokens
		res.Total.TotalTokens += v.TotalTokens
		res.Total.DurationSum += v.DurationSum
		for h := 0; h < 24; h++ {
			res.Total.ByHour[h] += v.ByHour[h]
		}
		for k, n := range v.ByKey {
			if res.Total.ByKey == nil {
				res.Total.ByKey = map[string]int64{}
			}
			res.Total.ByKey[k] += n
		}
		for k, n := range v.ByModel {
			if res.Total.ByModel == nil {
				res.Total.ByModel = map[string]*ModelAgg{}
			}
			a := res.Total.ByModel[k]
			if a == nil {
				a = &ModelAgg{}
				res.Total.ByModel[k] = a
			}
			a.Total += n.Total
			a.OK += n.OK
			a.Failed += n.Failed
			a.TotalTokens += n.TotalTokens
		}
	}
	return res
}

// Run is the persist loop; call in a goroutine.
func (s *Store) Run() {
	defer close(s.doneCh)
	t := time.NewTicker(s.persist)
	defer t.Stop()
	for {
		select {
		case <-s.stopCh:
			s.save()
			return
		case <-t.C:
			s.save()
		}
	}
}

// Stop flushes and stops the persist loop.
func (s *Store) Stop() { close(s.stopCh); <-s.doneCh }

func (s *Store) save() {
	s.mu.RLock()
	r := Record{Calls: s.callBuf, ByDay: s.byDay, SavedAt: time.Now()}
	s.mu.RUnlock()
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return
	}
	// Atomic write: temp file + fsync + rename so a crash never leaves a
	// half-written JSON (which would cause load() to return no-data on restart).
	if err := writeAtomic(s.file, data); err != nil {
		slog.Warn("stats: failed to persist", "err", err)
	}
}

func (s *Store) load() {
	data, err := os.ReadFile(s.file)
	if err != nil {
		return
	}
	var r Record
	if err := json.Unmarshal(data, &r); err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.callBuf = r.Calls
	if len(s.callBuf) > s.bufCap {
		s.callBuf = s.callBuf[len(s.callBuf)-s.bufCap:]
	}
	s.byDay = r.ByDay
	if s.byDay == nil {
		s.byDay = map[string]*DayAgg{}
	}
}
