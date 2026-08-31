package postgres

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/foodibd/socialstats/internal/config"
	"github.com/foodibd/socialstats/internal/domain"
)

// Integration tests. Half of what this package relies on — advisory locks,
// transactional DDL, SQLSTATE codes, ON CONFLICT — is behaviour a mock would
// simply assert back at us, so these run against a real PostgreSQL.
//
// They skip themselves when TEST_DATABASE_URL is unset, so `make test` stays
// fast and dependency-free. `make db-up && make test-db` runs them.

func testDB(t *testing.T) *DB {
	t.Helper()

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set; run `make db-up && make test-db`")
	}

	ctx := context.Background()
	db, err := Connect(ctx, config.DatabaseConfig{
		URL:             url,
		MaxConns:        8,
		MinConns:        1,
		MaxConnLifetime: time.Minute,
		ConnectTimeout:  5 * time.Second,
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("connect to TEST_DATABASE_URL: %v", err)
	}
	t.Cleanup(db.Close)

	// Every test starts from nothing, so a failed run cannot leave state that
	// makes the next one pass or fail for the wrong reason.
	if _, err := db.Pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	return db
}

func TestConnectRejectsAnUnreachableDatabase(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	// Port 1 has nothing listening. The point is that Connect fails here rather
	// than handing back a lazy pool that fails on the first real query.
	_, err := Connect(context.Background(), config.DatabaseConfig{
		URL:            "postgres://nobody@127.0.0.1:1/nothing?sslmode=disable",
		MaxConns:       2,
		MinConns:       0,
		ConnectTimeout: 2 * time.Second,
	}, slog.New(slog.DiscardHandler))

	if err == nil {
		t.Fatal("Connect succeeded against a dead address")
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	first, err := db.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if first == 0 {
		t.Fatal("schema version is 0 after migrating")
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	second, err := db.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if second != first {
		t.Errorf("version moved from %d to %d on a no-op run", first, second)
	}

	var applied int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	expected, err := loadMigrations(migrationFS)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if applied != len(expected) {
		t.Errorf("%d rows in schema_migrations, want %d", applied, len(expected))
	}
}

// A rolling deploy starts several replicas at once and they all migrate. The
// advisory lock is what stops them racing; without it the losers get duplicate
// object errors and crash-loop.
func TestMigrateIsSafeFromSeveralReplicas(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	const replicas = 5
	var wg sync.WaitGroup
	errs := make([]error, replicas)

	for i := range replicas {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = db.Migrate(ctx)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("replica %d: %v", i, err)
		}
	}

	var applied int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatalf("count: %v", err)
	}
	expected, _ := loadMigrations(migrationFS)
	if applied != len(expected) {
		t.Errorf("%d migrations recorded after %d concurrent runs, want %d", applied, replicas, len(expected))
	}
}

func TestMigrateRefusesAnEditedMigration(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Simulate someone editing an already-applied file.
	if _, err := db.Pool.Exec(ctx,
		`UPDATE schema_migrations SET checksum = 'tampered' WHERE version = 1`); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	err := db.Migrate(ctx)
	if !errors.Is(err, ErrMigrationChecksumMismatch) {
		t.Errorf("error = %v, want ErrMigrationChecksumMismatch", err)
	}
}

func TestSchemaVersionOnAnEmptyDatabase(t *testing.T) {
	db := testDB(t)

	// No schema_migrations table yet: that is "nothing has run", not an error.
	version, err := db.SchemaVersion(context.Background())
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if version != 0 {
		t.Errorf("version = %d on an empty database, want 0", version)
	}
}

func TestExtensionsMigrationEnablesCitext(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// citext exists to make users.email case-insensitive at the database level.
	// Asserting the behaviour rather than the extension's presence is what
	// actually matters.
	var equal bool
	if err := db.Pool.QueryRow(ctx,
		`SELECT 'Alice@Example.com'::citext = 'alice@example.com'::citext`).Scan(&equal); err != nil {
		t.Fatalf("citext comparison: %v", err)
	}
	if !equal {
		t.Error("citext did not compare case-insensitively")
	}

	var id string
	if err := db.Pool.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
		t.Fatalf("gen_random_uuid: %v", err)
	}
	if len(id) != 36 {
		t.Errorf("gen_random_uuid returned %q", id)
	}
}

func TestPingAndPoolStats(t *testing.T) {
	db := testDB(t)

	if err := db.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if s := db.Stats(); s.Max != 8 {
		t.Errorf("pool Max = %d, want 8", s.Max)
	}
}

// translate is the boundary that keeps pgx out of the rest of the service, so
// it is checked against errors Postgres actually produces rather than
// hand-built ones.
func TestTranslateMapsRealPostgresErrors(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	if _, err := db.Pool.Exec(ctx, `
		CREATE TABLE t (
			id   integer PRIMARY KEY,
			name text NOT NULL,
			n    integer CHECK (n > 0)
		)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO t VALUES (1, 'a', 1)`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tests := []struct {
		name string
		run  func() error
		want error
	}{
		{
			name: "no rows",
			run: func() error {
				var id int
				return db.Pool.QueryRow(ctx, `SELECT id FROM t WHERE id = 999`).Scan(&id)
			},
			want: domain.ErrRecordNotFound,
		},
		{
			name: "unique violation",
			run: func() error {
				_, err := db.Pool.Exec(ctx, `INSERT INTO t VALUES (1, 'b', 1)`)
				return err
			},
			want: domain.ErrConflict,
		},
		{
			name: "not null violation",
			run: func() error {
				_, err := db.Pool.Exec(ctx, `INSERT INTO t VALUES (2, NULL, 1)`)
				return err
			},
			want: domain.ErrStorage,
		},
		{
			name: "check violation",
			run: func() error {
				_, err := db.Pool.Exec(ctx, `INSERT INTO t VALUES (3, 'c', 0)`)
				return err
			},
			want: domain.ErrStorage,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := translate(tc.run())
			if !errors.Is(err, tc.want) {
				t.Errorf("translate = %v, want %v", err, tc.want)
			}
		})
	}
}

// A cancelled context is not a database error and must pass through unchanged,
// because callers do check for context.Canceled.
func TestTranslatePassesThroughContextErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if got := translate(ctx.Err()); !errors.Is(got, context.Canceled) {
		t.Errorf("translate = %v, want context.Canceled", got)
	}
	if got := translate(nil); got != nil {
		t.Errorf("translate(nil) = %v, want nil", got)
	}
}
