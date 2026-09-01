package proxy

import (
	"fmt"
	"strings"
	"time"

	"llmproxy/config"
)

// ListClientKeys returns all client keys with their full secret. The dashboard
// is behind admin authentication and these are credentials the admin issued, so
// the admin is allowed to reveal and copy them; the frontend masks by default
// and reveals on demand.
func (p *Proxy) ListClientKeys() []config.ClientKey {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]config.ClientKey, len(p.cfg.ClientKeys))
	copy(out, p.cfg.ClientKeys)
	return out
}

// AddClientKey appends a client key. Key must be non-blank and name unique.
func (p *Proxy) AddClientKey(k config.ClientKey) error {
	if strings.TrimSpace(k.Key) == "" {
		return fmt.Errorf("API key must not be empty")
	}
	if strings.TrimSpace(k.Name) == "" {
		return fmt.Errorf("name must not be empty")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, ex := range p.cfg.ClientKeys {
		if ex.Name == k.Name {
			return fmt.Errorf("client key %q already exists", k.Name)
		}
		if ex.Key == k.Key {
			return fmt.Errorf("this key value is already in use")
		}
	}
	if k.CreatedAt == 0 {
		k.CreatedAt = time.Now().Unix()
	}
	p.cfg.ClientKeys = append(p.cfg.ClientKeys, k)
	return p.saveLocked()
}

// UpdateClientKey merges non-empty fields into the key with matching Name.
// An empty Key field keeps the existing secret (so the masked value from the
// dashboard never overwrites the real key). UsedTokens is preserved unless
// explicitly reset via ResetClientKeyUsage.
func (p *Proxy) UpdateClientKey(k config.ClientKey) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.cfg.ClientKeys {
		if p.cfg.ClientKeys[i].Name == k.Name {
			ex := p.cfg.ClientKeys[i]
			if strings.TrimSpace(k.Key) != "" {
				ex.Key = k.Key
			}
			ex.ExpiresAt = k.ExpiresAt
			ex.TokenQuota = k.TokenQuota
			ex.Enabled = k.Enabled
			p.cfg.ClientKeys[i] = ex
			return p.saveLocked()
		}
	}
	return fmt.Errorf("client key %q not found", k.Name)
}

// DeleteClientKey removes a client key by name.
func (p *Proxy) DeleteClientKey(name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.cfg.ClientKeys {
		if p.cfg.ClientKeys[i].Name == name {
			p.cfg.ClientKeys = append(p.cfg.ClientKeys[:i], p.cfg.ClientKeys[i+1:]...)
			return p.saveLocked()
		}
	}
	return fmt.Errorf("client key %q not found", name)
}

// ResetClientKeyUsage zeroes a key's UsedTokens counter.
func (p *Proxy) ResetClientKeyUsage(name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.cfg.ClientKeys {
		if p.cfg.ClientKeys[i].Name == name {
			p.cfg.ClientKeys[i].UsedTokens = 0
			return p.saveLocked()
		}
	}
	return fmt.Errorf("client key %q not found", name)
}
