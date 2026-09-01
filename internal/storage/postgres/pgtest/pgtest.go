// Package pgtest hands integration tests an isolated PostgreSQL schema.
//
// It deliberately returns a connection string rather than an open database:
// depending on the storage package would make it unusable from that package's
// own tests, which is where most of it is needed.
//
// Every caller gets a private schema instead of sharing `public`. That matters
// more than it looks — `go test ./...` runs packages in parallel, and two
// packages that each reset a shared schema will drop the other's tables
// mid-migration. The failure is intermittent, blames whichever test happened to
// be running, and costs an afternoon.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// URL returns a connection string pointing at a fresh, empty schema that is
// dropped when the test ends. It skips the test when TEST_DATABASE_URL is unset,
// so `make test` stays fast and dependency-free.
func URL(t *testing.T) string {
	t.Helper()

	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL is not set; run `make db-up && make test-db`")
	}

	requireCleanPublic(t, base)

	ctx := context.Background()
	schema := "t_" + randomSuffix(t)

	// The schema must exist before a connection can put it on its search_path,
	// so it is created over a throwaway connection first.
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect to TEST_DATABASE_URL: %v", err)
	}
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+quoteIdent(schema)); err != nil {
		_ = admin.Close(ctx)
		t.Fatalf("create schema %s: %v", schema, err)
	}
	_ = admin.Close(ctx)

	t.Cleanup(func() { dropSchema(t, base, schema) })

	return withSearchPath(t, base, schema)
}

// requireCleanPublic fails loudly when the test database has application tables
// in `public`.
//
// `public` is on the search_path because the extensions live there, so anything
// else in it is visible to every test schema — and an unqualified query that
// finds a leftover table there reads the wrong data while looking entirely
// correct. That failure mode is genuinely hard to spot from the outside, so it
// is checked once, up front, with a message that says what to do about it.
func requireCleanPublic(t *testing.T, base string) {
	t.Helper()

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		return // The caller is about to fail on this anyway.
	}
	defer func() { _ = conn.Close(ctx) }()

	var leftovers []string
	rows, err := conn.Query(ctx,
		`SELECT tablename FROM pg_tables WHERE schemaname = 'public' ORDER BY tablename`)
	if err != nil {
		return
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err == nil {
			leftovers = append(leftovers, name)
		}
	}
	rows.Close()

	if len(leftovers) > 0 {
		t.Fatalf("TEST_DATABASE_URL has application tables in the public schema: %s\n"+
			"They are visible to every test schema and will be read by unqualified queries.\n"+
			"Something migrated straight into public — most likely the app was pointed at\n"+
			"the test database. Clean it with:\n"+
			"  docker compose exec postgres-test psql -U socialstats -d socialstats_test \\\n"+
			"    -c 'DROP SCHEMA public CASCADE; CREATE SCHEMA public;'",
			strings.Join(leftovers, ", "))
	}
}

func dropSchema(t *testing.T, base, schema string) {
	t.Helper()

	// The test's own context may be cancelled by now, and the pool using this
	// schema is closed, so cleanup gets its own connection and context.
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Logf("could not connect to drop schema %s: %v", schema, err)
		return
	}
	defer func() { _ = conn.Close(ctx) }()

	if _, err := conn.Exec(ctx, `DROP SCHEMA IF EXISTS `+quoteIdent(schema)+` CASCADE`); err != nil {
		t.Logf("could not drop schema %s: %v", schema, err)
	}
}

// withSearchPath appends the libpq option that pins a connection's search_path.
//
// The test's own schema comes first, so every table the migrations create lands
// there and disappears with it. `public` follows because extensions live there:
// citext and pgcrypto are database-wide objects installed into one schema (see
// migration 0001), and the types they provide have to stay resolvable from
// whichever schema is in front.
func withSearchPath(t *testing.T, raw, schema string) string {
	t.Helper()

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}

	q := u.Query()
	q.Set("options", "-csearch_path="+schema+",public")
	u.RawQuery = q.Encode()
	return u.String()
}

func randomSuffix(t *testing.T) string {
	t.Helper()

	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("generate schema name: %v", err)
	}
	return hex.EncodeToString(buf)
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
