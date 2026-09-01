package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"llmproxy/config"
	"llmproxy/proxy"
	"llmproxy/stats"
	"llmproxy/web"
)

func main() {
	configPath := flag.String("config", "config.json", "path to config file")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("failed to load config", "err", err)
		os.Exit(1)
	}

	// Initialize security-critical fields (JWT secret, admin password hash).
	if err := web.EnsureSecurity(cfg, *configPath); err != nil {
		slog.Error("failed to init security", "err", err)
		os.Exit(1)
	}

	store, err := stats.NewStore(cfg.StatsFile, cfg.LogBuffer)
	if err != nil {
		slog.Error("failed to init stats", "err", err)
		os.Exit(1)
	}
	go store.Run()

	p := proxy.New(cfg, store)
	p.SetConfigPath(*configPath)
	go p.WatchConfig()

	srv := web.NewServer(cfg, p, store)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		slog.Info("shutting down")
		_ = srv.Shutdown(context.Background())
	case err := <-errCh:
		if err != nil {
			slog.Error("server error", "err", err)
			os.Exit(1)
		}
	}
	store.Stop()
}
