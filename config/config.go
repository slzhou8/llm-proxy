// Package config defines the proxy's persistent configuration model and
// loads/saves it to a JSON file. The web dashboard reads and writes this.
package config

import "encoding/json"

// Protocol identifies an upstream's request/response wire format.
type Protocol string

const (
	ProtocolOpenAI    Protocol = "openai"
	ProtocolAnthropic Protocol = "anthropic"
)

// Upstream is a single API endpoint + key. Only upstreams sharing the same
// Protocol are ever swapped during failover.
type Upstream struct {
	Name         string            `json:"name"`                    // display name, must be unique
	Protocol     Protocol          `json:"protocol"`                // openai | anthropic
	BaseURL      string            `json:"base_url"`                // e.g. https://api.openai.com / https://api.anthropic.com
	APIKey       string            `json:"api_key"`                 // key sent as Authorization Bearer / x-api-key
	ExtraHeaders map[string]string `json:"extra_headers,omitempty"` // optional per-upstream headers
	UserAgent    string            `json:"user_agent,omitempty"`    // if set, overrides the inbound User-Agent sent upstream
	// TranslateToOpenAI lets an OpenAI-speaking client reach an Anthropic
	// upstream: the proxy translates the request OpenAI->Anthropic on the way
	// out and the response Anthropic->OpenAI on the way back, for both
	// streaming and non-streaming calls. Ignored unless Protocol is anthropic.
	TranslateToOpenAI bool `json:"translate_to_openai,omitempty"`
	Enabled           bool `json:"enabled"`  // skip disabled upstreams during failover
	Priority          int  `json:"priority"` // higher is tried first; equal values share load round-robin
}

// RetryStrategy controls retry behaviour inside a single upstream.
type RetryStrategy struct {
	MaxAttempts  int   `json:"max_attempts"`  // total attempts per upstream (>=1)
	BaseDelayMS  int64 `json:"base_delay_ms"` // first retry backoff base
	MaxDelayMS   int64 `json:"max_delay_ms"`  // cap on backoff
	RetryOn429   bool  `json:"retry_on_429"`
	RetryOn5xx   bool  `json:"retry_on_5xx"`
	RetryTimeout bool  `json:"retry_on_timeout"`
	// TotalTimeoutSec caps the wall-clock time spent on one client request across
	// every retry and failover. Without it, retries and backoff can outlive the
	// caller's own timeout and keep burning upstream quota for a response nobody
	// will read. 0 disables the cap.
	//
	// Streaming is exempt once the upstream starts sending: a long SSE answer is
	// legitimate, so the deadline only guards the retry/failover phase.
	TotalTimeoutSec int `json:"total_timeout_sec"`
}

// FailoverStrategy controls how the proxy picks the next upstream.
type FailoverStrategy struct {
	Mode          string `json:"mode"`           // "round_robin" (future: "random", "skip_unhealthy")
	SkipUnhealthy bool   `json:"skip_unhealthy"` // avoid currently-throttled upstreams
	MaxRotations  int    `json:"max_rotations"`  // max failovers per request; 0 = unlimited
}

// AlertConfig controls webhook alerting when upstreams misbehave. Alerts fire
// when a single upstream fails FailStreak times in a row; CooldownSec prevents
// alert storms by rate-limiting per upstream.
type AlertConfig struct {
	URL         string `json:"url"`          // webhook endpoint (企业微信/钉钉)
	Enabled     bool   `json:"enabled"`      // master switch
	FailStreak  int    `json:"fail_streak"`  // consecutive failures before alerting
	CooldownSec int    `json:"cooldown_sec"` // min seconds between two alerts
}

// Role is a dashboard permission level.
type Role string

const (
	RoleAdmin  Role = "admin"  // full access: upstreams, keys, policy, alerts
	RoleViewer Role = "viewer" // read-only: monitoring and stats only
)

// AdminUser is a dashboard login account. Currently a single admin is used.
type AdminUser struct {
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"` // bcrypt hash
}

// User is a dashboard account with an explicit permission role. Multiple users
// are supported; the legacy single-Admin field is migrated into Users on load.
type User struct {
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"` // bcrypt hash
	Role         Role   `json:"role"`          // admin | viewer
}

// ClientKey is a credential issued to an application that calls the proxy. The
// proxy validates the caller's Authorization against these before forwarding.
type ClientKey struct {
	Name       string `json:"name"`        // display name, unique
	Key        string `json:"key"`         // the actual Authorization Bearer value (non-blank)
	CreatedAt  int64  `json:"created_at"`  // unix seconds
	ExpiresAt  int64  `json:"expires_at"`  // 0 = never expires
	TokenQuota int64  `json:"token_quota"` // 0 = unlimited; else reject once UsedTokens >= quota
	UsedTokens int64  `json:"used_tokens"` // cumulative tokens consumed
	Enabled    bool   `json:"enabled"`
}

// Config is the top-level persisted configuration.
type Config struct {
	AdminAddr  string           `json:"admin_addr"` // dashboard listen address, e.g. "127.0.0.1:18081"
	ProxyAddr  string           `json:"proxy_addr"` // local app points its SDK base URL here, e.g. ":18080"
	StatsFile  string           `json:"stats_file"` // file the stats store persists to
	LogBuffer  int              `json:"log_buffer"` // recent-call ring buffer size in web UI
	JWTSecret  string           `json:"jwt_secret"` // HMAC secret for dashboard JWTs; auto-generated if empty
	Admin      AdminUser        `json:"admin"`      // legacy single account; migrated into Users on startup
	Users      []User           `json:"users"`      // dashboard accounts with roles
	Retry      RetryStrategy    `json:"retry"`
	Failover   FailoverStrategy `json:"failover"`
	Alert      AlertConfig      `json:"alert"`   // webhook alerting
	Pricing    PricingConfig    `json:"pricing"` // per-model rates for cost estimation
	Upstreams  []Upstream       `json:"upstreams"`
	ClientKeys []ClientKey      `json:"client_keys"` // credentials for apps calling the proxy
}

// Default returns a sane baseline config.
func Default() *Config {
	return &Config{
		AdminAddr: "127.0.0.1:18081",
		ProxyAddr: ":18080",
		StatsFile: "stats.json",
		LogBuffer: 200,
		Admin:     AdminUser{Username: "admin"}, // password hash filled at startup
		Retry: RetryStrategy{
			MaxAttempts:     3,
			BaseDelayMS:     500,
			MaxDelayMS:      10000,
			RetryOn429:      true,
			RetryOn5xx:      true,
			RetryTimeout:    true,
			TotalTimeoutSec: 120, // cap one request at 2 minutes across all retries
		},
		Failover: FailoverStrategy{
			Mode:          "round_robin",
			SkipUnhealthy: true,
			MaxRotations:  0, // unlimited: try every upstream before giving up
		},
		Alert: AlertConfig{
			Enabled:     false, // opt-in: user supplies a webhook URL in the dashboard
			FailStreak:  3,     // alert after 3 consecutive failures on one upstream
			CooldownSec: 600,   // at most one alert per upstream per 10 minutes
		},
		Upstreams:  []Upstream{},
		ClientKeys: []ClientKey{},
	}
}

// FindUser returns the account with the given username, or nil if absent.
func (c *Config) FindUser(username string) *User {
	for i := range c.Users {
		if c.Users[i].Username == username {
			return &c.Users[i]
		}
	}
	return nil
}

// IsAdmin reports whether username holds admin privileges. Unknown users are
// denied, so the check fails closed.
func (c *Config) IsAdmin(username string) bool {
	u := c.FindUser(username)
	return u != nil && u.Role == RoleAdmin
}

// Load reads config from JSON path, creating a default file if missing.
func Load(path string) (*Config, error) {
	data, err := readFile(path)
	if err != nil {
		cfg := Default()
		cfg.Pricing = DefaultPricing()
		if werr := writeFile(path, cfg); werr != nil {
			return nil, werr
		}
		return cfg, nil
	}
	cfg := Default() // start from defaults then overlay
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	// Pricing is fixed up after the overlay instead of being seeded in
	// Default(): json.Unmarshal merges into an existing map rather than
	// replacing it, so a pre-seeded table would resurrect models the user
	// deleted in the dashboard. Whether the block was present at all cannot be
	// recovered from the struct (an absent block and an all-zero one are
	// identical once unmarshalled), so probe the raw JSON -- absent means never
	// configured and takes the full defaults, present is the user's own setting
	// with only missing sub-fields filled in.
	var probe struct {
		Pricing *json.RawMessage `json:"pricing"`
	}
	if json.Unmarshal(data, &probe) == nil && probe.Pricing == nil {
		cfg.Pricing = DefaultPricing()
	} else {
		cfg.Pricing.EnsureDefaults()
	}
	return cfg, nil
}

// Save writes cfg back to path atomically.
func (c *Config) Save(path string) error { return writeFile(path, c) }
