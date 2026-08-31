package config

import (
	"testing"
	"time"
)

// minimalEnv sets only what validate() insists on, so each test can add the one
// variable it is about.
func minimalEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://localhost:5432/socialstats")
	t.Setenv("YOUTUBE_API_KEY", "test-key")
}

func TestDatabaseDefaults(t *testing.T) {
	minimalEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Database.MaxConns != 10 || cfg.Database.MinConns != 2 {
		t.Errorf("pool bounds = %d/%d, want 10/2", cfg.Database.MaxConns, cfg.Database.MinConns)
	}
	if cfg.Database.MaxConnLifetime != 30*time.Minute {
		t.Errorf("MaxConnLifetime = %v, want 30m", cfg.Database.MaxConnLifetime)
	}
	if !cfg.Database.MigrateOnBoot {
		t.Error("MigrateOnBoot should default to true")
	}
}

func TestDatabaseURLIsRequired(t *testing.T) {
	t.Setenv("YOUTUBE_API_KEY", "test-key")
	t.Setenv("DATABASE_URL", "")

	if _, err := Load(); err == nil {
		t.Fatal("Load should fail without DATABASE_URL")
	}
}

func TestPoolBoundsAreValidated(t *testing.T) {
	minimalEnv(t)
	t.Setenv("DATABASE_MAX_CONNS", "4")
	t.Setenv("DATABASE_MIN_CONNS", "8")

	if _, err := Load(); err == nil {
		t.Error("Load should reject a minimum above the maximum")
	}
}

// Non-positive integers fall back to the default rather than failing, which is
// how every other int in this config behaves (TIKTOK_MAX_ATTEMPTS=0 gives 4).
// Asserted here so the consistency is deliberate rather than incidental.
func TestNonPositivePoolSizeFallsBackToDefault(t *testing.T) {
	minimalEnv(t)
	t.Setenv("DATABASE_MAX_CONNS", "0")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Database.MaxConns != 10 {
		t.Errorf("MaxConns = %d, want the default 10", cfg.Database.MaxConns)
	}
}
