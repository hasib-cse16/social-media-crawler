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

	YouTube YouTubeConfig
	Meta    MetaConfig
	TikTok  TikTokConfig
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
