package postgres

import (
	"errors"
	"strings"
	"testing"
	"testing/fstest"
)

// These tests cover the runner's validation rules without a database. The rules
// exist to catch mistakes at boot rather than halfway through a deploy, so they
// are worth testing against deliberately broken inputs — which is why
// loadMigrations takes an fs.FS.

func fsWith(files map[string]string) fstest.MapFS {
	out := fstest.MapFS{}
	for name, body := range files {
		out["migrations/"+name] = &fstest.MapFile{Data: []byte(body)}
	}
	return out
}

func TestLoadMigrationsOrdersByVersion(t *testing.T) {
	got, err := loadMigrations(fsWith(map[string]string{
		"0010_tenth.up.sql":  "SELECT 10;",
		"0002_second.up.sql": "SELECT 2;",
		"0001_first.up.sql":  "SELECT 1;",
	}))
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}

	// Lexical order would put 0010 second; numeric order must put it last.
	want := []int{1, 2, 10}
	if len(got) != len(want) {
		t.Fatalf("got %d migrations, want %d", len(got), len(want))
	}
	for i, v := range want {
		if got[i].version != v {
			t.Errorf("position %d = version %d, want %d", i, got[i].version, v)
		}
	}
	if got[0].name != "first" {
		t.Errorf("name = %q, want first", got[0].name)
	}
}

func TestLoadMigrationsChecksumsAreContentAddressed(t *testing.T) {
	a, err := loadMigrations(fsWith(map[string]string{"0001_x.up.sql": "SELECT 1;"}))
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	b, err := loadMigrations(fsWith(map[string]string{"0001_x.up.sql": "SELECT 2;"}))
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if a[0].checksum == b[0].checksum {
		t.Error("different bodies produced the same checksum")
	}

	again, _ := loadMigrations(fsWith(map[string]string{"0001_x.up.sql": "SELECT 1;"}))
	if a[0].checksum != again[0].checksum {
		t.Error("the same body produced different checksums")
	}
}

func TestLoadMigrationsRejects(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{
			// Two branches both adding "0007" is the most common way this goes
			// wrong, and it is silent unless checked.
			name:  "duplicate version",
			files: map[string]string{"0007_a.up.sql": "SELECT 1;", "0007_b.up.sql": "SELECT 2;"},
			want:  "duplicate migration version",
		},
		{
			name:  "unnumbered file",
			files: map[string]string{"create_users.sql": "SELECT 1;"},
			want:  "does not match",
		},
		{
			name:  "down migration",
			files: map[string]string{"0001_users.down.sql": "DROP TABLE users;"},
			want:  "does not match",
		},
		{
			name:  "three digit version",
			files: map[string]string{"001_users.up.sql": "SELECT 1;"},
			want:  "does not match",
		},
		{
			name:  "empty body",
			files: map[string]string{"0001_users.up.sql": "   \n\n"},
			want:  "is empty",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadMigrations(fsWith(tc.files))
			if err == nil {
				t.Fatalf("loadMigrations accepted %v", tc.files)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestVerifyChecksums(t *testing.T) {
	files := []migration{
		{version: 1, name: "first", checksum: "aaa"},
		{version: 2, name: "second", checksum: "bbb"},
	}

	t.Run("matching", func(t *testing.T) {
		err := verifyChecksums(files, map[int]appliedRecord{
			1: {name: "first", checksum: "aaa"},
		})
		if err != nil {
			t.Errorf("verifyChecksums: %v", err)
		}
	})

	t.Run("edited after being applied", func(t *testing.T) {
		err := verifyChecksums(files, map[int]appliedRecord{
			1: {name: "first", checksum: "different"},
		})
		if !errors.Is(err, ErrMigrationChecksumMismatch) {
			t.Errorf("error = %v, want ErrMigrationChecksumMismatch", err)
		}
	})

	t.Run("binary older than the schema", func(t *testing.T) {
		// Migration 9 ran, but this build does not contain it: a rollback to an
		// older image. Serving on a schema the code does not know about is
		// worse than refusing to start.
		err := verifyChecksums(files, map[int]appliedRecord{
			1: {name: "first", checksum: "aaa"},
			9: {name: "from_the_future", checksum: "zzz"},
		})
		if !errors.Is(err, ErrMigrationChecksumMismatch) {
			t.Fatalf("error = %v, want ErrMigrationChecksumMismatch", err)
		}
		if !strings.Contains(err.Error(), "older than the database schema") {
			t.Errorf("error = %q, want it to explain the version skew", err)
		}
	})
}

// The embedded set must satisfy its own rules; a malformed file committed to
// the repository would otherwise only surface at boot.
func TestEmbeddedMigrationsAreValid(t *testing.T) {
	files, err := loadMigrations(migrationFS)
	if err != nil {
		t.Fatalf("embedded migrations are invalid: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no embedded migrations found")
	}
	if files[0].version != 1 {
		t.Errorf("first migration is version %d, want 1", files[0].version)
	}
}
