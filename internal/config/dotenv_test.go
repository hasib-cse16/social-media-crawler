package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeEnv(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}
	return path
}

func TestLoadEnvFile(t *testing.T) {
	path := writeEnv(t, t.TempDir(), `
# a comment
APP_ENV=staging
export EXPORTED_KEY=exported
QUOTED="double quoted"
SINGLE='single quoted'
WITH_ESCAPE="line\nbreak"
INLINE_COMMENT=value # trailing note
SPACED   =   padded
EMPTY=
malformed line with no equals
`)

	// t.Setenv registers cleanup for each key, restoring the environment.
	for _, k := range []string{"APP_ENV", "EXPORTED_KEY", "QUOTED", "SINGLE", "WITH_ESCAPE", "INLINE_COMMENT", "SPACED", "EMPTY"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}

	if err := LoadEnvFile(path); err != nil {
		t.Fatalf("LoadEnvFile: %v", err)
	}

	want := map[string]string{
		"APP_ENV":        "staging",
		"EXPORTED_KEY":   "exported",
		"QUOTED":         "double quoted",
		"SINGLE":         "single quoted",
		"WITH_ESCAPE":    "line\nbreak",
		"INLINE_COMMENT": "value",
		"SPACED":         "padded",
		"EMPTY":          "",
	}
	for k, v := range want {
		if got := os.Getenv(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestRealEnvWinsOverFile(t *testing.T) {
	path := writeEnv(t, t.TempDir(), "YOUTUBE_API_KEY=from-file\n")

	t.Setenv("YOUTUBE_API_KEY", "from-environment")
	if err := LoadEnvFile(path); err != nil {
		t.Fatalf("LoadEnvFile: %v", err)
	}
	if got := os.Getenv("YOUTUBE_API_KEY"); got != "from-environment" {
		t.Errorf("YOUTUBE_API_KEY = %q, want the exported value to win", got)
	}
}

func TestMissingFileIsNotAnError(t *testing.T) {
	if err := LoadEnvFile(filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Errorf("LoadEnvFile on a missing file = %v, want nil", err)
	}
}

func TestLoadEnvFileFromWalksUp(t *testing.T) {
	root := t.TempDir()
	writeEnv(t, root, "WALKED_UP_KEY=found\n")

	nested := filepath.Join(root, "cmd", "api")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	t.Setenv("WALKED_UP_KEY", "")
	os.Unsetenv("WALKED_UP_KEY")

	found, err := LoadEnvFileFrom(nested, ".env")
	if err != nil {
		t.Fatalf("LoadEnvFileFrom: %v", err)
	}
	if found == "" {
		t.Fatal("LoadEnvFileFrom did not find the parent .env")
	}
	if got := os.Getenv("WALKED_UP_KEY"); got != "found" {
		t.Errorf("WALKED_UP_KEY = %q, want %q", got, "found")
	}
}

func TestLoadEnvFileFromReportsNothingFound(t *testing.T) {
	found, err := LoadEnvFileFrom(t.TempDir(), ".env.does-not-exist")
	if err != nil {
		t.Fatalf("LoadEnvFileFrom: %v", err)
	}
	if found != "" {
		t.Errorf("found = %q, want empty", found)
	}
}

func TestLoadAppliesEnvFileValues(t *testing.T) {
	dir := t.TempDir()
	writeEnv(t, dir, "HTTP_ADDR=:9999\nCACHE_TTL=30s\nDOCS_ENABLED=false\n"+
		"DATABASE_URL=postgres://localhost/test\n")

	for _, k := range []string{"HTTP_ADDR", "DOCS_ENABLED"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}

	if _, err := LoadEnvFileFrom(dir, ".env"); err != nil {
		t.Fatalf("LoadEnvFileFrom: %v", err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != ":9999" {
		t.Errorf("Addr = %q, want :9999", cfg.Addr)
	}
	if cfg.DocsEnabled {
		t.Error("DocsEnabled = true, want false from the env file")
	}
}
