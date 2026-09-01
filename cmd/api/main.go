// Command api serves the social video stats HTTP API.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/foodibd/socialstats/internal/api"
	"github.com/foodibd/socialstats/internal/auth"
	"github.com/foodibd/socialstats/internal/config"
	"github.com/foodibd/socialstats/internal/domain"
	"github.com/foodibd/socialstats/internal/httpx"
	"github.com/foodibd/socialstats/internal/platform/httpclient"
	"github.com/foodibd/socialstats/internal/poller"
	"github.com/foodibd/socialstats/internal/provider"
	"github.com/foodibd/socialstats/internal/provider/meta"
	"github.com/foodibd/socialstats/internal/provider/tiktok"
	"github.com/foodibd/socialstats/internal/provider/youtube"
	"github.com/foodibd/socialstats/internal/stats"
	"github.com/foodibd/socialstats/internal/storage/postgres"
	"github.com/foodibd/socialstats/internal/tracking"
)

// version is stamped at build time: -ldflags "-X main.version=$(git rev-parse --short HEAD)".
var version = "dev"

// migrateOnly runs migrations and exits without serving. Deployments that apply
// the schema as a separate step — an init container, a release job — run the
// same binary with this flag, so migrations can never be applied by a build
// different from the one about to serve traffic.
var migrateOnly = flag.Bool("migrate-only", false, "apply pending migrations and exit")

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	flag.Parse()

	// A .env in the working directory (or up to a few parents up, so running
	// from an IDE works too) is loaded for local development. Real environment
	// variables always take precedence, so this is inert in production.
	envFile, err := config.LoadEnvFileFrom(".", config.DefaultEnvFile)
	if err != nil {
		return fmt.Errorf("load %s: %w", config.DefaultEnvFile, err)
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	log := newLogger(cfg)
	slog.SetDefault(log)
	log.Info("starting", "version", version, "env", cfg.Env, "docs", cfg.DocsEnabled)
	if envFile != "" {
		log.Info("loaded env file", "path", envFile)
	}

	// The signal context is established before anything that can block, so a
	// Ctrl-C during a slow database connect exits instead of hanging.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := postgres.Connect(ctx, cfg.Database, log)
	if err != nil {
		return fmt.Errorf("database: %w\n\n"+
			"Set DATABASE_URL in %s (copy .env.example). For local development:\n"+
			"  make db-up      # starts postgres in docker\n"+
			"  make migrate    # applies the schema",
			err, config.DefaultEnvFile)
	}
	defer db.Close()

	if *migrateOnly {
		if err := db.Migrate(ctx); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
		version, err := db.SchemaVersion(ctx)
		if err != nil {
			return fmt.Errorf("read schema version: %w", err)
		}
		log.Info("migrate-only complete", "schema_version", version)
		return nil
	}

	if cfg.Database.MigrateOnBoot {
		if err := db.Migrate(ctx); err != nil {
			// A schema the code does not match is not something to serve
			// traffic on top of.
			return fmt.Errorf("migrate: %w", err)
		}
	} else {
		version, err := db.SchemaVersion(ctx)
		if err != nil {
			return fmt.Errorf("read schema version: %w", err)
		}
		log.Info("migrations skipped", "reason", "MIGRATE_ON_BOOT=false", "schema_version", version)
	}

	client := httpclient.New(cfg.UpstreamTimeout)

	providers := make([]domain.Provider, 0, 3)

	yt, err := youtube.New(cfg.YouTube, client, log)
	if err != nil {
		// YouTube is the primary platform: refuse to boot half-configured.
		return fmt.Errorf("youtube provider: %w\n\n"+
			"Set YOUTUBE_API_KEY in %s (copy .env.example), or export it in your\n"+
			"run configuration. Get a key at console.cloud.google.com: enable\n"+
			"\"YouTube Data API v3\", then Credentials -> Create credentials -> API key.",
			err, config.DefaultEnvFile)
	}
	providers = append(providers, yt)

	// Meta and TikTok are registered even when disabled, so their URLs get a
	// clear 503 rather than "unsupported platform".
	//
	// Meta shares the pooled client: unlike TikTok it does not decide
	// per-connection whether to serve content, and its Graph API calls benefit
	// from connection reuse.
	providers = append(providers, meta.New(cfg.Meta, client, log))

	// TikTok gets its own client: it decides per-connection whether to serve a
	// challenge page, so retries must not reuse a poisoned connection. The
	// timeout here bounds a single attempt; the handler timeout below bounds the
	// whole retry sequence.
	tiktokClient := httpclient.NewWithoutConnectionReuse(cfg.UpstreamTimeout)
	providers = append(providers, tiktok.New(cfg.TikTok, tiktokClient, log))

	authSvc, authMW, err := buildAuth(cfg, db, log)
	if err != nil {
		return err
	}

	registry := provider.NewRegistry(providers...)
	cache := stats.NewCache(cacheTTL())
	service := stats.NewService(registry, cache, log)

	refreshPolicy := tracking.RefreshPolicy{
		Interval:               cfg.Tracking.RefreshInterval,
		MaxBackoff:             cfg.Tracking.MaxBackoff,
		FailuresBeforeRetiring: cfg.Tracking.FailuresBeforeRetiring,
	}

	trackingSvc := tracking.NewService(
		db.Videos(), db.Tracking(), db.Metrics(), registry,
		tracking.Config{
			MaxTrackedPerUser: cfg.Tracking.MaxPerUser,
			AddTimeout:        cfg.Tracking.AddTimeout,
			Policy:            refreshPolicy,
			// The per-platform refresh rate is set when a video is first
			// tracked, and the poller reads it back off the row. One source of
			// truth, on the video, because the fetch is shared.
			PlatformIntervals: refreshIntervals(cfg),
		}, log)

	go reapCache(ctx, cache, log)
	go reapSessions(ctx, authSvc, db, log)

	var poll *poller.Poller
	if cfg.Poll.Enabled {
		poll = poller.New(db.Videos(), registry, pollerConfig(cfg, refreshPolicy), log)
		go func() {
			if err := poll.Run(ctx); err != nil {
				log.Error("poller stopped", "error", err)
			}
		}()
	} else {
		log.Info("polling disabled", "reason", "POLL_ENABLED=false")
	}

	apiHandler := api.NewHandler(service, log, version, db)
	if poll != nil {
		apiHandler = apiHandler.WithPoller(poll.Reporter())
	}

	handler := api.NewRouter(
		apiHandler,
		log,
		api.RouterConfig{
			HandlerTimeout: handlerTimeout(cfg),
			DocsEnabled:    cfg.DocsEnabled,
			Auth:           api.NewAuthHandler(authSvc, authMW),
			Middleware:     authMW,
			Tracking:       api.NewTrackingHandler(trackingSvc),
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

// buildAuth assembles the authentication stack.
func buildAuth(cfg *config.Config, db *postgres.DB, log *slog.Logger) (*auth.Service, *auth.Middleware, error) {
	hasher := auth.NewHasher(auth.HashParams{
		Time:       cfg.Auth.HashTime,
		Memory:     cfg.Auth.HashMemoryKiB,
		Threads:    cfg.Auth.HashThreads,
		SaltLength: auth.DefaultHashParams.SaltLength,
		KeyLength:  auth.DefaultHashParams.KeyLength,
	}, cfg.Auth.HashConcurrency)

	svc, err := auth.NewService(
		db.Users(), db.Sessions(), db.RateLimits(), hasher,
		auth.Config{
			TTL:              cfg.Auth.SessionTTL,
			IdleTTL:          cfg.Auth.SessionIdleTTL,
			TouchInterval:    cfg.Auth.SessionTouchInterval,
			RegistrationOpen: cfg.Auth.RegistrationOpen,
			LoginAttempts:    cfg.Auth.LoginAttempts,
			LoginWindow:      cfg.Auth.LoginWindow,
			Cookie: auth.CookieConfig{
				Name:   cfg.Auth.CookieName,
				Secure: cfg.Auth.CookieSecure,
				Domain: cfg.Auth.CookieDomain,
				TTL:    cfg.Auth.SessionTTL,
			},
		}, log)
	if err != nil {
		return nil, nil, fmt.Errorf("auth: %w", err)
	}

	// Worth stating at startup: password hashing is deliberately expensive, and
	// the memory ceiling is a number people are surprised by when they size a
	// container.
	log.Info("auth ready",
		"argon2_memory_kib", cfg.Auth.HashMemoryKiB,
		"argon2_concurrency", cfg.Auth.HashConcurrency,
		"hash_memory_ceiling_mib", hasher.MemoryCeilingBytes()/(1<<20),
		"cookie_secure", cfg.Auth.CookieSecure,
		"registration_open", cfg.Auth.RegistrationOpen,
	)
	if !cfg.Auth.CookieSecure {
		log.Warn("session cookies are not marked Secure; this is only safe in development",
			"env", cfg.Env)
	}

	return svc, auth.NewMiddleware(svc, api.RespondError, cfg.Auth.TrustProxyHeaders), nil
}

// reapSessions deletes expired sessions and idle rate limit buckets.
func reapSessions(ctx context.Context, svc *auth.Service, db *postgres.DB, log *slog.Logger) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n, err := svc.ReapSessions(ctx); err != nil {
				log.ErrorContext(ctx, "session reaping failed", "error", err)
			} else if n > 0 {
				log.Debug("sessions reaped", "deleted", n)
			}
			// A bucket untouched for a day is full, and a full bucket is
			// indistinguishable from no bucket at all.
			if n, err := db.RateLimits().DeleteIdleBuckets(ctx, time.Now().Add(-24*time.Hour)); err != nil {
				log.ErrorContext(ctx, "rate limit sweep failed", "error", err)
			} else if n > 0 {
				log.Debug("rate limit buckets reaped", "deleted", n)
			}
		}
	}
}

// pollerConfig translates the environment-shaped config into the poller's own.
func pollerConfig(cfg *config.Config, policy tracking.RefreshPolicy) poller.Config {
	platforms := make(map[domain.Platform]poller.PlatformPolicy, len(cfg.Poll.Platforms))
	for platform, p := range cfg.Poll.Platforms {
		platforms[domain.Platform(platform)] = poller.PlatformPolicy{
			Concurrency: p.Concurrency,
			MinInterval: p.MinInterval,
			Refresh:     p.Refresh,
		}
	}

	return poller.Config{
		Tick:             cfg.Poll.Tick,
		Batch:            cfg.Poll.Batch,
		LockFor:          cfg.Poll.LockFor,
		RateLimitBackoff: cfg.Poll.RateLimitBackoff,
		ShutdownGrace:    cfg.ShutdownTimeout,
		Policy:           policy,
		Platforms:        platforms,
	}
}

// refreshIntervals is the rate each platform's videos are given when first
// tracked.
func refreshIntervals(cfg *config.Config) map[domain.Platform]time.Duration {
	out := make(map[domain.Platform]time.Duration, len(cfg.Poll.Platforms))
	for platform, p := range cfg.Poll.Platforms {
		out[domain.Platform(platform)] = p.Refresh
	}
	return out
}

// handlerTimeout must cover the slowest provider. The scraping providers retry
// page fetches, so their worst case is attempts x upstream timeout plus the
// backoff accumulated between them.
func handlerTimeout(cfg *config.Config) time.Duration {
	slowest := cfg.UpstreamTimeout

	if cfg.TikTok.Enabled() {
		slowest = max(slowest, retryBudget(cfg.UpstreamTimeout, cfg.TikTok.MaxAttempts, cfg.TikTok.RetryBackoff, 1))
	}
	if cfg.Meta.Enabled() {
		// Instagram can fetch twice in one request: the embed render, then the
		// post page when the embed carried no payload.
		slowest = max(slowest, retryBudget(cfg.UpstreamTimeout, cfg.Meta.MaxAttempts, cfg.Meta.RetryBackoff, 2))
	}
	return slowest + 2*time.Second
}

// retryBudget is the worst-case wall time for a number of sequential page fetches,
// each retried up to attempts times with linearly growing backoff.
func retryBudget(upstream time.Duration, maxAttempts int, backoff time.Duration, fetches int) time.Duration {
	attempts := time.Duration(max(maxAttempts, 1))
	waited := backoff * attempts * (attempts + 1) / 2 // linear growth
	return time.Duration(fetches) * (upstream*attempts + waited)
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
