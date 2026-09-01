package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"llmproxy/config"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

// DefaultAdminPassword is the initial password for the admin account on first
// startup. The user should change it after logging in.
const DefaultAdminPassword = "ChangeMe123!"

// EnsureSecurity fills in security-critical config fields that must exist before
// the server starts: a random JWT secret and the admin password hash. It
// persists the config to path if anything changed. Call once at startup after
// config.Load.
func EnsureSecurity(cfg *config.Config, path string) error {
	changed := false
	if cfg.JWTSecret == "" {
		secret, err := randomSecret(32)
		if err != nil {
			return err
		}
		cfg.JWTSecret = secret
		changed = true
	}
	// Legacy single-account installs keep Admin populated; make sure it has a
	// username and password before migrating it into the Users list.
	if cfg.Admin.Username == "" {
		cfg.Admin.Username = "admin"
		changed = true
	}
	if cfg.Admin.PasswordHash == "" {
		h, err := hashPassword(DefaultAdminPassword)
		if err != nil {
			return err
		}
		cfg.Admin.PasswordHash = h
		changed = true
	}
	// Migrate / seed the multi-user list. The legacy admin becomes the first
	// admin account so existing credentials keep working unchanged.
	if len(cfg.Users) == 0 {
		cfg.Users = []config.User{{
			Username:     cfg.Admin.Username,
			PasswordHash: cfg.Admin.PasswordHash,
			Role:         config.RoleAdmin,
		}}
		changed = true
	}
	if changed {
		return cfg.Save(path)
	}
	return nil
}

// hashPassword returns a bcrypt hash of the plaintext password.
func hashPassword(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// checkPassword reports whether plain matches the stored bcrypt hash.
func checkPassword(hash, plain string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

// randomSecret returns a hex-encoded cryptographically-random string.
func randomSecret(nbytes int) (string, error) {
	b := make([]byte, nbytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

const jwtTTL = 24 * time.Hour

// jwtClaims is the identity decoded from a dashboard token.
type jwtClaims struct {
	Username string
	Role     config.Role
}

// issueJWT signs a token for username with the given role, valid for jwtTTL.
func (s *Server) issueJWT(username string, role config.Role) (string, error) {
	if role == "" {
		role = config.RoleViewer // fail closed: never grant admin implicitly
	}
	claims := jwt.MapClaims{
		"sub":  username,
		"role": string(role),
		"exp":  time.Now().Add(jwtTTL).Unix(),
		"iat":  time.Now().Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return tok.SignedString([]byte(s.proxy.JWTSecret()))
}

// parseJWT validates a token string and returns its subject and role.
func (s *Server) parseJWT(tokstr string) (*jwtClaims, error) {
	tok, err := jwt.Parse(tokstr, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return []byte(s.proxy.JWTSecret()), nil
	})
	if err != nil || !tok.Valid {
		return nil, errors.New("invalid token")
	}
	claims, ok := tok.Claims.(jwt.MapClaims)
	if !ok {
		return nil, errors.New("invalid claims")
	}
	sub, _ := claims["sub"].(string)
	if sub == "" {
		return nil, errors.New("no subject")
	}
	role, _ := claims["role"].(string)
	if role == "" {
		// Tokens issued before roles existed belong to the legacy admin.
		role = string(config.RoleAdmin)
	}
	return &jwtClaims{Username: sub, Role: config.Role(role)}, nil
}

// bearerToken extracts the token from an Authorization: Bearer <t> header.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(h), "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// ctxKeyUser carries the decoded identity from requireAuth to handlers.
type ctxKeyUserType struct{}

var ctxKeyUser = ctxKeyUserType{}

// userFrom returns the identity attached by requireAuth, or nil if absent.
func userFrom(r *http.Request) *jwtClaims {
	c, _ := r.Context().Value(ctxKeyUser).(*jwtClaims)
	return c
}

// requireAuth wraps a handler so only requests with a valid JWT pass. The
// decoded identity is attached to the request context for downstream handlers.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := bearerToken(r)
		if tok == "" {
			s.writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing token"})
			return
		}
		claims, err := s.parseJWT(tok)
		if err != nil {
			s.writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid or expired token"})
			return
		}
		// Re-check the account against live config rather than trusting the token
		// alone: a deleted account must lose access immediately, and the role must
		// come from config so a change takes effect without waiting out the TTL.
		_, role, ok := s.proxy.UserAuth(claims.Username)
		if !ok {
			s.writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "账号已不存在，请重新登录"})
			return
		}
		claims.Role = role
		ctx := context.WithValue(r.Context(), ctxKeyUser, claims)
		next(w, r.WithContext(ctx))
	}
}

// requireAdmin additionally requires the admin role: viewers may read
// monitoring data but must not mutate configuration or spend upstream quota.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return s.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		c := userFrom(r)
		if c == nil || c.Role != config.RoleAdmin {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "只读账号无权执行此操作"})
			return
		}
		next(w, r)
	})
}
