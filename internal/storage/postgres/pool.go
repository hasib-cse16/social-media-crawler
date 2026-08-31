// Package postgres is the persistence layer. It owns the connection pool, the
// migration runner and the repositories.
//
// Two rules hold the boundary this package exists to draw:
//
//   - Nothing above this package sees a pgx type. Repositories take and return
//     domain types, so the rest of the service does not know which database it
//     is talking to.
//   - Nothing above this package sees a Postgres error. Constraint violations,
//     no-rows and connection failures are translated into the domain sentinels
//     in errors.go, which is what lets api/errors.go stay the single place that
//     maps a failure onto an HTTP status.
package postgres

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/foodibd/socialstats/internal/config"
)

// DB wraps the connection pool. It is safe for concurrent use and is the only
// thing repositories need.
type DB struct {
	Pool *pgxpool.Pool
	log  *slog.Logger
}

// Connect builds the pool and verifies it can actually reach the database.
//
// The verification is the point: a pgxpool is lazy, so without an explicit ping
// a misconfigured DATABASE_URL produces a healthy-looking process that fails on
// the first real query, minutes later, in a request. Failing here instead means
// a bad deploy is caught by the startup probe rather than by a user.
func Connect(ctx context.Context, cfg config.DatabaseConfig, log *slog.Logger) (*DB, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		// The URL may carry a password, so the parse error is reported without
		// echoing what was parsed.
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}

	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MinConns = cfg.MinConns
	poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	poolCfg.ConnConfig.ConnectTimeout = cfg.ConnectTimeout

	// MaxConnLifetimeJitter spreads connection recycling out. Without it every
	// connection opened during a deploy expires in the same second half an hour
	// later, and the pool reconnects all at once.
	poolCfg.MaxConnLifetimeJitter = cfg.MaxConnLifetime / 5

	// Application name shows up in pg_stat_activity, which is where you look
	// when something is holding a lock.
	if poolCfg.ConnConfig.RuntimeParams == nil {
		poolCfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	poolCfg.ConnConfig.RuntimeParams["application_name"] = "socialstats"

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	log.Info("database connected",
		"host", poolCfg.ConnConfig.Host,
		"database", poolCfg.ConnConfig.Database,
		"max_conns", poolCfg.MaxConns,
	)

	return &DB{Pool: pool, log: log.With("component", "postgres")}, nil
}

// Ping reports whether the database is reachable right now. It is what /readyz
// calls, so it takes its own short timeout: a readiness probe that blocks for
// the caller's full request budget is worse than one that reports "not ready".
func (db *DB) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return db.Pool.Ping(ctx)
}

// Stats exposes pool counters for the health endpoint and for logging.
func (db *DB) Stats() PoolStats {
	s := db.Pool.Stat()
	return PoolStats{
		Total:        s.TotalConns(),
		Idle:         s.IdleConns(),
		Acquired:     s.AcquiredConns(),
		Max:          s.MaxConns(),
		AcquireCount: s.AcquireCount(),
	}
}

// PoolStats is a pgx-free view of the pool, so callers above this package do
// not import pgx just to read a number.
type PoolStats struct {
	Total        int32 `json:"total"`
	Idle         int32 `json:"idle"`
	Acquired     int32 `json:"acquired"`
	Max          int32 `json:"max"`
	AcquireCount int64 `json:"acquire_count"`
}

// Close releases every connection. Safe to call on a nil DB so shutdown paths
// do not need a guard.
func (db *DB) Close() {
	if db == nil || db.Pool == nil {
		return
	}
	db.Pool.Close()
}
