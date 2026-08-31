package postgres

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// The migration runner is deliberately small. A dedicated tool would be the
// largest dependency in this project and we need roughly a tenth of what one
// does: apply numbered SQL files in order, exactly once, safely, from several
// replicas at the same time.
//
// Three properties it guarantees, each of which exists because of a specific
// way schema management goes wrong:
//
//   - One transaction per migration. A half-applied migration is not a state
//     anyone should have to reason about at 3am.
//   - A session advisory lock around the whole run. A rolling deploy starts
//     several replicas at once and they would otherwise race to apply the same
//     file; the loser gets a duplicate-object error and crash-loops.
//   - Checksums. Editing an already-applied migration is refused at boot rather
//     than silently ignored, because the alternative is two environments that
//     disagree about what the schema is and no way to tell.
//
// Migrations are forward-only: there are no .down.sql files. Rolling a schema
// back in production is almost always the wrong move, and the recovery path is
// a new forward migration.

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationLockID is the key for pg_advisory_lock. It is an arbitrary constant;
// what matters is only that every replica of this service uses the same one and
// that no other application picks it by accident.
const migrationLockID int64 = 8_231_744_509_112_233

// migrationNameRe matches "0001_create_users.up.sql".
var migrationNameRe = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.up\.sql$`)

// ErrMigrationChecksumMismatch means an applied migration file was edited after
// the fact. It is deliberately fatal.
var ErrMigrationChecksumMismatch = errors.New("applied migration has been modified")

type migration struct {
	version  int
	name     string
	sql      string
	checksum string
}

// Migrate applies every pending migration, in order.
func (db *DB) Migrate(ctx context.Context) error {
	files, err := loadMigrations(migrationFS)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		db.log.WarnContext(ctx, "no migrations found")
		return nil
	}

	// A dedicated connection, because an advisory lock is held by the session
	// that took it. Taking it on a pooled connection and releasing it on a
	// different one would leave the lock held until that connection closed.
	conn, err := db.Pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockID); err != nil {
		return fmt.Errorf("take migration lock: %w", err)
	}
	defer func() {
		// Best effort: if this fails the session is broken, and closing it
		// releases the lock anyway.
		if _, err := conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrationLockID); err != nil {
			db.log.WarnContext(ctx, "releasing migration lock failed", "error", err)
		}
	}()

	if err := ensureMigrationsTable(ctx, conn.Conn()); err != nil {
		return err
	}

	applied, err := appliedMigrations(ctx, conn.Conn())
	if err != nil {
		return err
	}

	if err := verifyChecksums(files, applied); err != nil {
		return err
	}

	pending := 0
	for _, m := range files {
		if _, done := applied[m.version]; done {
			continue
		}
		if err := applyOne(ctx, conn.Conn(), m); err != nil {
			return fmt.Errorf("migration %04d_%s: %w", m.version, m.name, err)
		}
		db.log.InfoContext(ctx, "migration applied", "version", m.version, "name", m.name)
		pending++
	}

	if pending == 0 {
		db.log.InfoContext(ctx, "database schema up to date", "version", files[len(files)-1].version)
	} else {
		db.log.InfoContext(ctx, "migrations complete", "applied", pending, "version", files[len(files)-1].version)
	}
	return nil
}

// SchemaVersion reports the highest applied migration, or 0 when none have run.
// It is useful in /healthz for spotting a replica that booted against an older
// schema than the rest of the fleet.
func (db *DB) SchemaVersion(ctx context.Context) (int, error) {
	var version *int
	err := db.Pool.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&version)
	if err != nil {
		// The table not existing is not an error; it means nothing has run.
		if strings.Contains(err.Error(), "does not exist") {
			return 0, nil
		}
		return 0, err
	}
	if version == nil {
		return 0, nil
	}
	return *version, nil
}

func ensureMigrationsTable(ctx context.Context, conn *pgx.Conn) error {
	const ddl = `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    integer     PRIMARY KEY,
			name       text        NOT NULL,
			checksum   text        NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`
	if _, err := conn.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	return nil
}

// appliedRecord is what the ledger remembers about a migration that has run.
type appliedRecord struct {
	name     string
	checksum string
}

func appliedMigrations(ctx context.Context, conn *pgx.Conn) (map[int]appliedRecord, error) {
	rows, err := conn.Query(ctx, `SELECT version, name, checksum FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()

	out := make(map[int]appliedRecord)
	for rows.Next() {
		var version int
		var rec appliedRecord
		if err := rows.Scan(&version, &rec.name, &rec.checksum); err != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}
		out[version] = rec
	}
	return out, rows.Err()
}

// verifyChecksums refuses to continue when a migration that has already run no
// longer matches the file on disk.
//
// A migration recorded in the database but missing from the binary is also
// refused: it means this build is older than the schema it is pointed at, which
// during a rollback is exactly when you want to be told rather than to have the
// application quietly run against a schema it does not understand.
func verifyChecksums(files []migration, applied map[int]appliedRecord) error {
	onDisk := make(map[int]migration, len(files))
	for _, m := range files {
		onDisk[m.version] = m
	}

	versions := make([]int, 0, len(applied))
	for v := range applied {
		versions = append(versions, v)
	}
	sort.Ints(versions)

	for _, v := range versions {
		rec := applied[v]
		m, ok := onDisk[v]
		if !ok {
			return fmt.Errorf("%w: migration %04d_%s is recorded as applied but is not in this build; "+
				"this binary is older than the database schema",
				ErrMigrationChecksumMismatch, v, rec.name)
		}
		if m.checksum != rec.checksum {
			return fmt.Errorf("%w: %04d_%s (recorded %s, on disk %s); "+
				"applied migrations are immutable, write a new one instead",
				ErrMigrationChecksumMismatch, v, m.name, short(rec.checksum), short(m.checksum))
		}
	}
	return nil
}

// applyOne runs a single migration and records it in the same transaction, so
// the schema change and the ledger entry cannot disagree.
func applyOne(ctx context.Context, conn *pgx.Conn, m migration) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if _, err := tx.Exec(ctx, m.sql); err != nil {
		return err
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`,
		m.version, m.name, m.checksum)
	if err != nil {
		return fmt.Errorf("record migration: %w", err)
	}

	return tx.Commit(ctx)
}

// loadMigrations reads and validates the migration files.
//
// It takes an fs.FS rather than reading the embedded one directly so the
// validation rules below can be tested against deliberately malformed sets
// without committing broken files to the repository.
func loadMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}

	out := make([]migration, 0, len(entries))
	seen := make(map[int]string, len(entries))

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		match := migrationNameRe.FindStringSubmatch(e.Name())
		if match == nil {
			return nil, fmt.Errorf("migration %q does not match NNNN_name.up.sql", e.Name())
		}

		version, err := strconv.Atoi(match[1])
		if err != nil {
			return nil, fmt.Errorf("migration %q: bad version: %w", e.Name(), err)
		}
		if prev, dup := seen[version]; dup {
			// Two people adding migration 0007 on separate branches is the
			// single most common way this goes wrong, and it is silent unless
			// checked.
			return nil, fmt.Errorf("duplicate migration version %04d: %s and %s", version, prev, e.Name())
		}
		seen[version] = e.Name()

		body, err := fs.ReadFile(fsys, "migrations/"+e.Name())
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		if len(strings.TrimSpace(string(body))) == 0 {
			return nil, fmt.Errorf("migration %s is empty", e.Name())
		}

		sum := sha256.Sum256(body)
		out = append(out, migration{
			version:  version,
			name:     match[2],
			sql:      string(body),
			checksum: hex.EncodeToString(sum[:]),
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

func short(checksum string) string {
	if len(checksum) <= 12 {
		return checksum
	}
	return checksum[:12]
}
