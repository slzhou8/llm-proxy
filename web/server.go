// Package web serves the admin dashboard (config management, call log,
// statistics) and the proxy listener.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"llmproxy/config"
	"llmproxy/proxy"
	"llmproxy/stats"
)

//go:embed static
var staticFS embed.FS

// Server holds two listeners: the admin dashboard+API and the proxy.
type Server struct {
	cfg     *config.Config
	proxy   *proxy.Proxy
	store   *stats.Store
	admin   *http.Server
	proxySrv *http.Server
	adminFS fs.FS
}

func NewServer(cfg *config.Config, p *proxy.Proxy, store *stats.Store) *Server {
	sub, _ := fs.Sub(staticFS, "static")
	return &Server{
		cfg:     cfg,
		proxy:   p,
		store:   store,
		adminFS: sub,
	}
}

// ListenAndServe starts both listeners and blocks until one fails.
func (s *Server) ListenAndServe() error {
	adminAddr := s.cfg.AdminAddr
	proxyAddr := s.cfg.ProxyAddr

	adminMux := http.NewServeMux()
	s.adminRoutes(adminMux)
	pmux := http.NewServeMux()
	pmux.Handle("/", cors(s.proxy))

	s.admin = &http.Server{Addr: adminAddr, Handler: adminMux, ReadHeaderTimeout: 10 * time.Second}
	s.proxySrv = &http.Server{Addr: proxyAddr, Handler: pmux, ReadHeaderTimeout: 10 * time.Second}

	errCh := make(chan error, 2)
	go func() { errCh <- s.admin.ListenAndServe() }()
	go func() { errCh <- s.proxySrv.ListenAndServe() }()

	slog.Info("admin dashboard listening", "addr", adminAddr)
	slog.Info("proxy listening", "addr", proxyAddr)

	err := <-errCh
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown gracefully stops both servers.
func (s *Server) Shutdown(ctx context.Context) error {
	var wg sync.WaitGroup
	var err error
	for _, srv := range []*http.Server{s.admin, s.proxySrv} {
		if srv == nil {
			continue
		}
		wg.Add(1)
		go func(srv *http.Server) {
			defer wg.Done()
			if e := srv.Shutdown(ctx); e != nil && !errors.Is(e, http.ErrServerClosed) {
				err = e
			}
		}(srv)
	}
	wg.Wait()
	return err
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
