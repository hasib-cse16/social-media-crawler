package config

import (
	"bufio"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// DefaultEnvFile is the dotenv file loaded at startup when present.
const DefaultEnvFile = ".env"

// LoadEnvFile reads a dotenv file into the process environment.
//
// Real environment variables always win: a value already exported is never
// overwritten, so a container's or CI's configuration takes precedence over a
// stray .env left in the working tree. A missing file is not an error — the file
// is a local-development convenience, and production is expected to inject real
// environment variables.
//
// Supported: KEY=value, `export KEY=value`, # comments, blank lines, and single-
// or double-quoted values (escape sequences are expanded only inside double
// quotes, matching shell behaviour).
func LoadEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		key, value, ok := parseEnvLine(scanner.Text())
		if !ok {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// LoadEnvFileFrom walks up from dir looking for a dotenv file, so the service
// starts the same way whether it is run from the repository root or from an
// IDE whose working directory is a subdirectory. It returns the file it loaded,
// or "" when none was found.
func LoadEnvFileFrom(dir, name string) (string, error) {
	for i := 0; i < 5; i++ { // repo roots are never far up
		candidate := filepath.Join(dir, name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, LoadEnvFile(candidate)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", nil
}

func parseEnvLine(line string) (key, value string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	line = strings.TrimPrefix(line, "export ")

	key, value, found := strings.Cut(line, "=")
	if !found {
		return "", "", false
	}

	key = strings.TrimSpace(key)
	if key == "" {
		return "", "", false
	}

	return key, unquote(strings.TrimSpace(value)), true
}

func unquote(v string) string {
	if len(v) >= 2 {
		switch {
		case v[0] == '"' && v[len(v)-1] == '"':
			// Only double quotes expand escapes, as in a shell.
			v = v[1 : len(v)-1]
			v = strings.ReplaceAll(v, `\n`, "\n")
			v = strings.ReplaceAll(v, `\t`, "\t")
			v = strings.ReplaceAll(v, `\"`, `"`)
			return v
		case v[0] == '\'' && v[len(v)-1] == '\'':
			return v[1 : len(v)-1]
		}
	}
	// An unquoted value ends at an inline comment.
	if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return v
}
