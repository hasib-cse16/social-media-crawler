// Command api serves the social video stats HTTP API.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/foodibd/socialstats/internal/api"
	"github.com/foodibd/socialstats/internal/config"
	"github.com/foodibd/socialstats/internal/domain"
	"github.com/foodibd/socialstats/internal/httpx"
	"github.com/foodibd/socialstats/internal/platform/httpclient"
	"github.com/foodibd/socialstats/internal/provider"
	"github.com/foodibd/socialstats/internal/provider/meta"
	"github.com/foodibd/socialstats/internal/provider/tiktok"
	"github.com/foodibd/socialstats/internal/provider/youtube"
	"github.com/foodibd/socialstats/internal/stats"
)

// version is stamped at build time: -ldflags "-X main.version=$(git rev-parse --short HEAD)".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	log := newLogger(cfg)
	slog.SetDefault(log)
	log.Info("starting", "version", version, "env", cfg.Env, "docs", cfg.DocsEnabled)

	client := httpclient.New(cfg.UpstreamTimeout)

	providers := make([]domain.Provider, 0, 3)

	yt, err := youtube.New(cfg.YouTube, client, log)
	if err != nil {
		// YouTube is the primary platform: refuse to boot half-configured.
		return fmt.Errorf("youtube provider: %w", err)
	}
	providers = append(providers, yt)

	// Meta and TikTok are registered unconditionally so their URLs get a clear
	// 501/503 instead of "unsupported platform" while they are being built out.
	providers = append(providers, meta.New(cfg.Meta, client, log))
	providers = append(providers, tiktok.New(cfg.TikTok, client, log))

	registry := provider.NewRegistry(providers...)
	cache := stats.NewCache(cacheTTL())
	service := stats.NewService(registry, cache, log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go reapCache(ctx, cache, log)

	handler := api.NewRouter(
		api.NewHandler(service, log, version),
		log,
		api.RouterConfig{
			HandlerTimeout: cfg.UpstreamTimeout + 2*time.Second,
			DocsEnabled:    cfg.DocsEnabled,
		},
	)

	server := httpx.NewServer(httpx.ServerConfig{
		Addr:            cfg.Addr,
		ReadTimeout:     cfg.ReadTimeout,
		WriteTimeout:    cfg.WriteTimeout,
		IdleTimeout:     cfg.IdleTimeout,
		ShutdownTimeout: cfg.ShutdownTimeout,
	}, handler, log)

	if err := server.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}

func cacheTTL() time.Duration {
	if v := os.Getenv("CACHE_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return 60 * time.Second
}

// reapCache evicts expired entries so memory stays bounded.
func reapCache(ctx context.Context, cache *stats.Cache, log *slog.Logger) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n := cache.Reap(); n > 0 {
				log.Debug("cache reaped", "evicted", n)
			}
		}
	}
}

func newLogger(cfg *config.Config) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}

	// Human-readable locally, JSON everywhere else so logs are queryable.
	if cfg.Env == "development" {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}
