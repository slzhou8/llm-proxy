package proxy

import (
	"os"
	"time"

	"llmproxy/config"
)

// WatchConfig polls the config file every second; on change it atomically
// swaps the in-memory config so edits made on disk (or by the dashboard) take
// effect without a restart. Call in a goroutine; it returns only when the
// process is stopping or the file disappears permanently.
func (p *Proxy) WatchConfig() {
	if p.cfgPath == "" {
		return
	}
	lastMod := fileModTime(p.cfgPath)
	for {
		time.Sleep(time.Second)
		mod := fileModTime(p.cfgPath)
		if mod.IsZero() {
			continue
		}
		if mod.Equal(lastMod) {
			continue
		}
		lastMod = mod
		cfg, err := config.Load(p.cfgPath)
		if err != nil {
			continue
		}
		p.mu.Lock()
		p.cfg = cfg
		p.mu.Unlock()
	}
}

// Save persists the current in-memory config to the file and reloads it.
// Used by the dashboard. The file write goes through config.Save so the on-disk
// and in-memory states stay consistent.
func (p *Proxy) Save() error {
	p.mu.Lock()
	path := p.cfgPath
	cfg := p.cfg
	p.mu.Unlock()
	if path == "" {
		return nil
	}
	return cfg.Save(path)
}

func fileModTime(path string) time.Time {
	st, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}
