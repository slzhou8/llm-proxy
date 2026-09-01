package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// alertState tracks consecutive failures and alert rate-limiting per upstream.
type alertState struct {
	streak    int       // consecutive failures seen so far
	lastAlert time.Time // when we last fired an alert for this upstream
}

// notifyAlert fires a webhook when an upstream fails repeatedly. It is called
// after every recorded call and is intentionally best-effort: delivery failures
// are logged, never propagated to the proxy caller.
//
// Alerts fire once a single upstream accumulates Alert.FailStreak consecutive
// failures, then are rate-limited per upstream by Alert.CooldownSec to avoid
// storming the webhook during an outage.
func (p *Proxy) notifyAlert(upstream string, ok bool, errType, detail string) {
	p.mu.RLock()
	a := p.cfg.Alert // AlertConfig is all scalars; copying it under the lock is enough
	p.mu.RUnlock()
	if !a.Enabled || a.URL == "" {
		return
	}
	streak := a.FailStreak
	if streak <= 0 {
		streak = 3
	}
	cooldown := time.Duration(a.CooldownSec) * time.Second
	if cooldown <= 0 {
		cooldown = 10 * time.Minute
	}

	p.alertMu.Lock()
	st := p.alertState[upstream]
	if st == nil {
		st = &alertState{}
		p.alertState[upstream] = st
	}
	if ok {
		// success resets the failure streak
		st.streak = 0
		p.alertMu.Unlock()
		return
	}
	st.streak++
	fire := st.streak >= streak && time.Since(st.lastAlert) >= cooldown
	curStreak := st.streak
	if fire {
		st.lastAlert = time.Now()
	}
	p.alertMu.Unlock()

	if !fire {
		return
	}
	go p.postAlert(a.URL, upstream, curStreak, errType, detail)
}

// postAlert delivers one alert to the configured webhook. The payload carries
// both `content` (企业微信 markdown) and `title`/`text` (钉钉 markdown) so the
// same body works with either provider.
func (p *Proxy) postAlert(url, upstream string, streak int, errType, detail string) {
	content := fmt.Sprintf("### LLM Proxy 告警\n"+
		"> **上游**：%s\n"+
		"> **连续失败**：%d 次\n"+
		"> **错误类型**：%s\n"+
		"> **详情**：%s\n"+
		"> **时间**：%s",
		upstream, streak, errType, detail, time.Now().Format("2006-01-02 15:04:05"))

	body, err := json.Marshal(map[string]any{
		"msgtype": "markdown",
		"markdown": map[string]string{
			"content": content,
			"title":   "LLM Proxy 告警",
			"text":    content,
		},
	})
	if err != nil {
		slog.Warn("alert: encode payload failed", "err", err)
		return
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		slog.Warn("alert: build request failed", "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		slog.Warn("alert: webhook post failed", "upstream", upstream, "err", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		slog.Warn("alert: webhook returned non-2xx", "upstream", upstream, "status", resp.StatusCode)
		return
	}
	slog.Info("alert: webhook delivered", "upstream", upstream, "streak", streak)
}
