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
	AccessToken string
	BaseURL     string
	APIVersion  string
}

func (c MetaConfig) Enabled() bool { return c.AccessToken != "" }

type TikTokConfig struct {
	BaseURL string
}

func (c TikTokConfig) Enabled() bool { return false } // not implemented yet

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
			AccessToken: str("META_ACCESS_TOKEN", ""),
			BaseURL:     str("META_API_BASE_URL", "https://graph.facebook.com"),
			APIVersion:  str("META_API_VERSION", "v21.0"),
		},
		TikTok: TikTokConfig{
			BaseURL: str("TIKTOK_API_BASE_URL", "https://open.tiktokapis.com"),
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
