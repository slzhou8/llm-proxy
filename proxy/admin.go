package proxy

import (
	"fmt"

	"llmproxy/config"
)

// AdminCreds returns the current admin username and password hash for the web
// layer to verify logins against.
func (p *Proxy) AdminCreds() (username, passwordHash string) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cfg.Admin.Username, p.cfg.Admin.PasswordHash
}

// JWTSecret returns the configured JWT signing secret.
func (p *Proxy) JWTSecret() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cfg.JWTSecret
}

// SetAdmin updates the legacy admin username and/or password hash and persists.
// Empty arguments leave the corresponding field unchanged.
func (p *Proxy) SetAdmin(username, passwordHash string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if username != "" {
		p.cfg.Admin.Username = username
	}
	if passwordHash != "" {
		p.cfg.Admin.PasswordHash = passwordHash
	}
	return p.saveLocked()
}

// UserAuth returns a copy of the named account's password hash and role. It
// exists so the web layer never reads config.Users without holding the lock
// (SetUser mutates those records in place).
func (p *Proxy) UserAuth(username string) (passwordHash string, role config.Role, ok bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	u := p.cfg.FindUser(username)
	if u == nil {
		return "", "", false
	}
	return u.PasswordHash, u.Role, true
}

// --- multi-user dashboard accounts ---

// ListUsers returns the dashboard accounts with password hashes stripped, so
// the payload is safe to send to the browser.
func (p *Proxy) ListUsers() []config.User {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]config.User, 0, len(p.cfg.Users))
	for _, u := range p.cfg.Users {
		out = append(out, config.User{Username: u.Username, Role: u.Role})
	}
	return out
}

// AddUser creates a dashboard account. PasswordHash must already be hashed by
// the caller (the web layer owns bcrypt). An unknown role degrades to viewer so
// a malformed request can never escalate privileges.
func (p *Proxy) AddUser(u config.User) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if u.Username == "" {
		return fmt.Errorf("username required")
	}
	if u.PasswordHash == "" {
		return fmt.Errorf("password required")
	}
	if p.cfg.FindUser(u.Username) != nil {
		return fmt.Errorf("user %q already exists", u.Username)
	}
	if u.Role != config.RoleAdmin {
		u.Role = config.RoleViewer
	}
	p.cfg.Users = append(p.cfg.Users, u)
	return p.saveLocked()
}

// DeleteUser removes a dashboard account. The last remaining admin cannot be
// deleted, otherwise the panel would become permanently read-only.
func (p *Proxy) DeleteUser(username string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	idx := -1
	for i := range p.cfg.Users {
		if p.cfg.Users[i].Username == username {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("user %q not found", username)
	}
	admins := 0
	for _, u := range p.cfg.Users {
		if u.Role == config.RoleAdmin {
			admins++
		}
	}
	if p.cfg.Users[idx].Role == config.RoleAdmin && admins <= 1 {
		return fmt.Errorf("不能删除最后一个管理员账号")
	}
	p.cfg.Users = append(p.cfg.Users[:idx], p.cfg.Users[idx+1:]...)
	return p.saveLocked()
}

// SetUser updates an existing account's username and/or password hash, and
// keeps the legacy Admin field in sync when that account is the admin.
func (p *Proxy) SetUser(oldUsername, newUsername, passwordHash string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	u := p.cfg.FindUser(oldUsername)
	if u == nil {
		return fmt.Errorf("user %q not found", oldUsername)
	}
	if newUsername != "" && newUsername != oldUsername {
		if p.cfg.FindUser(newUsername) != nil {
			return fmt.Errorf("user %q already exists", newUsername)
		}
		u.Username = newUsername
	}
	if passwordHash != "" {
		u.PasswordHash = passwordHash
	}
	// keep legacy field consistent for older tooling / downgrades
	if u.Role == config.RoleAdmin {
		p.cfg.Admin.Username = u.Username
		p.cfg.Admin.PasswordHash = u.PasswordHash
	}
	return p.saveLocked()
}
