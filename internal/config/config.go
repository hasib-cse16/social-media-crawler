package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the fully resolved runtime configuration. Everything comes from
// the environment so the binary stays deployable as a 12-factor service.
type Config struct {
	Env  string
	Addr string

	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration

	// UpstreamTimeout bounds a single outbound provider call.
	UpstreamTimeout time.Duration

	LogLevel string

	// DocsEnabled serves the Swagger UI and OpenAPI spec from the binary.
	DocsEnabled bool

	Database DatabaseConfig
	Auth     AuthConfig
	Lookup   LookupConfig

	YouTube YouTubeConfig
	Meta    MetaConfig
	TikTok  TikTokConfig
}

// AuthConfig covers sessions, password hashing and login abuse limits.
type AuthConfig struct {
	// SessionTTL is the absolute session lifetime.
	SessionTTL time.Duration

	// SessionIdleTTL expires a session that has gone quiet, so a forgotten
	// sign-in on a shared machine does not stay usable for the full TTL.
	SessionIdleTTL time.Duration

	// SessionTouchInterval is how stale a session's last-activity timestamp
	// must get before a request writes it again. Without a threshold, every
	// authenticated request writes a row.
	SessionTouchInterval time.Duration

	CookieName string

	// CookieSecure keeps cookies off plaintext connections. It is forced off in
	// development: a Secure cookie is simply never sent over http, and the
	// symptom — login succeeds, then every request is anonymous — is a
	// genuinely confusing afternoon.
	CookieSecure bool

	// CookieDomain is empty for a host-only cookie unless sessions must be
	// shared across subdomains.
	CookieDomain string

	// RegistrationOpen allows self-service sign-up.
	RegistrationOpen bool

	// LoginAttempts and LoginWindow bound failed sign-ins per email and per IP.
	LoginAttempts int
	LoginWindow   time.Duration

	// HashMemoryKiB, HashTime and HashThreads are the argon2id cost. Raising
	// them does not invalidate existing accounts: a hash carries the parameters
	// it was made with, and the next successful login upgrades it.
	HashMemoryKiB uint32
	HashTime      uint32
	HashThreads   uint8

	// HashConcurrency bounds simultaneous password hashes. Each one holds
	// HashMemoryKiB, so this is what caps the memory a burst of sign-ins can
	// take: at the defaults, 4 x 64 MiB. Zero means one per CPU.
	HashConcurrency int

	// TrustProxyHeaders honours X-Forwarded-For for the client address.
	//
	// It must stay off unless a proxy really is in front. With it on and no
	// proxy, any caller can set the header and choose which rate limit bucket
	// to spend — which is to say, turn the per-IP limit off.
	TrustProxyHeaders bool
}

// LookupConfig bounds what one lookup and one history list may cost.
type LookupConfig struct {
	// Timeout bounds a single lookup, including the provider call, so a slow
	// platform cannot hold a user-facing request open indefinitely.
	Timeout time.Duration

	// HistoryLimit is how many past lookups the dashboard lists.
	HistoryLimit int
}

// DatabaseConfig describes the PostgreSQL connection. The dashboard stores
// accounts, sessions and each user's lookup history, so unlike the provider
// credentials this is not optional: the service refuses to boot without it.
type DatabaseConfig struct {
	// URL is a libpq connection string or postgres:// URL.
	URL string

	// MaxConns bounds the pool. Postgres itself has a hard connection limit
	// (100 by default) shared across every replica, so this is sized per
	// replica with that budget in mind rather than set as high as it will go.
	MaxConns int32

	// MinConns keeps a few connections warm so the first request after an idle
	// period does not pay for a TCP handshake, TLS and authentication.
	MinConns int32

	// MaxConnLifetime recycles connections periodically. It bounds how long a
	// connection can hold stale session state, and it lets a rolling failover
	// move traffic to the new primary without waiting for idle timeouts.
	MaxConnLifetime time.Duration

	// ConnectTimeout bounds a single connection attempt.
	ConnectTimeout time.Duration

	// MigrateOnBoot applies pending migrations at startup. Turn it off where
	// migrations run as a separate deployment step.
	MigrateOnBoot bool
}

type YouTubeConfig struct {
	APIKey  string
	BaseURL string
}

func (c YouTubeConfig) Enabled() bool { return c.APIKey != "" }

type MetaConfig struct {
	// AccessToken is optional. It unlocks the Graph API path, which only works
	// for objects the token's app administers; everything else is read from the
	// public page. See internal/provider/meta.
	AccessToken string
	BaseURL     string
	APIVersion  string

	// On turns the provider off without removing it from the registry.
	On bool

	// PageFetch allows reading counters from public pages. Turning it off
	// leaves only the Graph API path, which means Instagram stops working
	// entirely and Facebook answers only for media the token administers.
	// It exists because scraping is a deployment-level policy decision.
	PageFetch bool

	// UserAgent is sent on page fetches. Meta serves a stripped page to clients
	// that do not look like a browser, so this must stay browser-like.
	UserAgent string

	// MaxAttempts bounds page fetch retries. Meta's failures are mostly
	// permanent login walls rather than TikTok's random challenges, so retrying
	// buys less here and the default is correspondingly lower.
	MaxAttempts int

	// RetryBackoff is the base delay between attempts; it grows linearly.
	RetryBackoff time.Duration
}

// Enabled reports whether the provider should serve at all. Unlike YouTube this
// is not tied to a credential: the public-page path needs none.
func (c MetaConfig) Enabled() bool { return c.On }

// HasToken reports whether the Graph API path is available.
func (c MetaConfig) HasToken() bool { return c.AccessToken != "" }

// PageFetchEnabled reports whether public pages may be read.
func (c MetaConfig) PageFetchEnabled() bool { return c.PageFetch }

type TikTokConfig struct {
	// BaseURL is the TikTok web origin. Public video pages are the only source
	// of view counts for arbitrary URLs; see internal/provider/tiktok.
	BaseURL string

	// UserAgent is sent on page fetches. TikTok serves a block page to clients
	// that do not look like a browser, so this must stay browser-like.
	UserAgent string

	// Enabled turns the provider off without removing it from the registry.
	On bool

	// MaxAttempts bounds how many times a page fetch is retried. TikTok serves
	// an anti-bot challenge page to a substantial fraction of requests
	// (measured at roughly a third), and a retry usually gets the real page,
	// so more than one attempt is required for a usable success rate.
	MaxAttempts int

	// RetryBackoff is the base delay between attempts; it grows linearly.
	// Keep it around a second: retrying every few hundred milliseconds trips
	// TikTok's rate limiter, which then serves a hard block stub to everything.
	RetryBackoff time.Duration
}

func (c TikTokConfig) Enabled() bool { return c.On }

// defaultMetaUserAgent is a desktop Chrome UA. Meta is not as fussy about the
// exact string as TikTok is, but a non-browser UA gets a stripped page with no
// payload in it, so a plausible one is still required.
const defaultMetaUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) " +
	"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

// defaultTikTokUserAgent is a desktop Chrome UA. The exact string matters more
// than it should: TikTok hard-blocks the canonical four-part version form
// ("Chrome/124.0.0.0"), which is what most scraping libraries send, while the
// three-part form ("Chrome/124.0") is served normally. Measured over repeated
// requests, the four-part UA got a block stub every time and this one did not.
// Override with TIKTOK_USER_AGENT if that changes.
const defaultTikTokUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) " +
	"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"

// Load reads configuration from the environment, applying defaults.
func Load() (*Config, error) {
	cfg := &Config{
		Env:             str("APP_ENV", "development"),
		Addr:            str("HTTP_ADDR", ":8080"),
		ReadTimeout:     dur("HTTP_READ_TIMEOUT", 10*time.Second),
		WriteTimeout:    dur("HTTP_WRITE_TIMEOUT", 20*time.Second),
		IdleTimeout:     dur("HTTP_IDLE_TIMEOUT", 60*time.Second),
		ShutdownTimeout: dur("HTTP_SHUTDOWN_TIMEOUT", 15*time.Second),
		UpstreamTimeout: dur("UPSTREAM_TIMEOUT", 8*time.Second),
		LogLevel:        str("LOG_LEVEL", "info"),
		DocsEnabled:     boolean("DOCS_ENABLED", true),
		Database: DatabaseConfig{
			URL:             str("DATABASE_URL", ""),
			MaxConns:        int32Val("DATABASE_MAX_CONNS", 10),
			MinConns:        int32Val("DATABASE_MIN_CONNS", 2),
			MaxConnLifetime: dur("DATABASE_CONN_MAX_LIFETIME", 30*time.Minute),
			ConnectTimeout:  dur("DATABASE_CONNECT_TIMEOUT", 5*time.Second),
			MigrateOnBoot:   boolean("MIGRATE_ON_BOOT", true),
		},
		Auth: AuthConfig{
			SessionTTL:           dur("SESSION_TTL", 720*time.Hour),
			SessionIdleTTL:       dur("SESSION_IDLE_TTL", 168*time.Hour),
			SessionTouchInterval: dur("SESSION_TOUCH_INTERVAL", 5*time.Minute),
			CookieName:           str("SESSION_COOKIE_NAME", "ss_session"),
			CookieSecure:         boolean("SESSION_COOKIE_SECURE", true),
			CookieDomain:         str("SESSION_COOKIE_DOMAIN", ""),
			RegistrationOpen:     boolean("REGISTRATION_OPEN", true),
			LoginAttempts:        intVal("LOGIN_ATTEMPTS", 5),
			LoginWindow:          dur("LOGIN_WINDOW", 15*time.Minute),
			HashMemoryKiB:        uint32(intVal("ARGON2_MEMORY_KIB", 64*1024)),
			HashTime:             uint32(intVal("ARGON2_TIME", 3)),
			HashThreads:          uint8(intVal("ARGON2_THREADS", 4)),
			HashConcurrency:      intVal("ARGON2_CONCURRENCY", 4),
			TrustProxyHeaders:    boolean("TRUST_PROXY_HEADERS", false),
		},
		Lookup: LookupConfig{
			Timeout:      dur("LOOKUP_TIMEOUT", 30*time.Second),
			HistoryLimit: intVal("LOOKUP_HISTORY_LIMIT", 100),
		},
		YouTube: YouTubeConfig{
			APIKey:  str("YOUTUBE_API_KEY", ""),
			BaseURL: str("YOUTUBE_API_BASE_URL", "https://www.googleapis.com/youtube/v3"),
		},
		Meta: MetaConfig{
			AccessToken:  str("META_ACCESS_TOKEN", ""),
			BaseURL:      str("META_API_BASE_URL", "https://graph.facebook.com"),
			APIVersion:   str("META_API_VERSION", "v21.0"),
			On:           boolean("META_ENABLED", true),
			PageFetch:    boolean("META_PAGE_FETCH", true),
			UserAgent:    str("META_USER_AGENT", defaultMetaUserAgent),
			MaxAttempts:  intVal("META_MAX_ATTEMPTS", 3),
			RetryBackoff: dur("META_RETRY_BACKOFF", time.Second),
		},
		TikTok: TikTokConfig{
			BaseURL:      str("TIKTOK_BASE_URL", "https://www.tiktok.com"),
			UserAgent:    str("TIKTOK_USER_AGENT", defaultTikTokUserAgent),
			On:           boolean("TIKTOK_ENABLED", true),
			MaxAttempts:  intVal("TIKTOK_MAX_ATTEMPTS", 4),
			RetryBackoff: dur("TIKTOK_RETRY_BACKOFF", time.Second),
		},
	}

	// A Secure cookie is never sent over http, so in development it would make
	// every sign-in appear to work and then fail. Forced rather than merely
	// defaulted, because the failure is confusing and the fix is not obvious.
	if cfg.Env == "development" {
		cfg.Auth.CookieSecure = false
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	if c.Addr == "" {
		return fmt.Errorf("HTTP_ADDR must not be empty")
	}
	if c.UpstreamTimeout <= 0 {
		return fmt.Errorf("UPSTREAM_TIMEOUT must be positive")
	}
	if c.Database.URL == "" {
		return fmt.Errorf("DATABASE_URL must be set\n\n" +
			"The dashboard stores users, tracked videos and the metric history in\n" +
			"PostgreSQL. For local development:\n" +
			"  make db-up      # starts postgres in docker\n" +
			"  make migrate    # applies the schema\n" +
			"Then set DATABASE_URL in .env (copy .env.example)")
	}
	if c.Database.MaxConns < 1 {
		return fmt.Errorf("DATABASE_MAX_CONNS must be at least 1")
	}
	if c.Database.MinConns < 0 || c.Database.MinConns > c.Database.MaxConns {
		return fmt.Errorf("DATABASE_MIN_CONNS must be between 0 and DATABASE_MAX_CONNS (%d)", c.Database.MaxConns)
	}
	if c.Auth.SessionTTL <= 0 {
		return fmt.Errorf("SESSION_TTL must be positive")
	}
	if c.Auth.SessionIdleTTL <= 0 || c.Auth.SessionIdleTTL > c.Auth.SessionTTL {
		return fmt.Errorf("SESSION_IDLE_TTL must be positive and no greater than SESSION_TTL (%s)", c.Auth.SessionTTL)
	}
	if c.Auth.CookieName == "" {
		return fmt.Errorf("SESSION_COOKIE_NAME must not be empty")
	}
	// argon2id's own lower bound. Below it the algorithm still runs but stops
	// being memory-hard, which is the entire reason it was chosen.
	if c.Auth.HashMemoryKiB < 8*1024 {
		return fmt.Errorf("ARGON2_MEMORY_KIB must be at least 8192 (8 MiB); %d is too weak to be worth the CPU",
			c.Auth.HashMemoryKiB)
	}
	if c.Auth.HashTime < 1 {
		return fmt.Errorf("ARGON2_TIME must be at least 1")
	}
	if c.Auth.HashThreads < 1 {
		return fmt.Errorf("ARGON2_THREADS must be at least 1")
	}
	if c.Lookup.Timeout <= 0 {
		return fmt.Errorf("LOOKUP_TIMEOUT must be positive")
	}
	if c.Lookup.HistoryLimit < 1 {
		return fmt.Errorf("LOOKUP_HISTORY_LIMIT must be at least 1")
	}
	return nil
}

func str(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func intVal(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func int32Val(key string, def int32) int32 {
	return int32(intVal(key, int(def)))
}

func boolean(key string, def bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func dur(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	// Bare integers are treated as seconds for operator convenience.
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second
	}
	return def
}
