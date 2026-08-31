package postgres

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/foodibd/socialstats/internal/domain"
)

// translate converts a pgx or Postgres error into a domain sentinel.
//
// This function is the boundary. Everything above this package matches on
// domain.Err*, never on pgx.ErrNoRows or a five-character SQLSTATE, which is
// what keeps the store swappable and keeps api/errors.go as the only place that
// knows about HTTP status codes.
//
// The original error is always wrapped, not discarded: the sentinel is for
// control flow, the wrapped text is for the log line that tells you which
// constraint actually fired.
func translate(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %w", domain.ErrRecordNotFound, err)
	}

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		// Not a database error at all: a cancelled context, a dead connection,
		// a scan into the wrong type. Passed through unchanged so the caller
		// can still see context.Canceled, which callers do check for.
		return err
	}

	// Every branch wraps the sentinel *and* the original with %w. Both have to
	// survive: callers match on the sentinel for control flow, and code that
	// needs the SQLSTATE — the writer's missing-partition retry, for one —
	// reaches the *pgconn.PgError through errors.As. Formatting the original
	// with %v instead would silently break the second kind of caller, and it is
	// the kind whose failure looks like a data loss rather than an error.
	switch pgErr.Code {
	case "23505": // unique_violation
		return fmt.Errorf("%w: constraint %s: %w", domain.ErrConflict, pgErr.ConstraintName, err)
	case "23503": // foreign_key_violation
		// A dangling reference is a bug in our code, not a user's mistake, so
		// it maps to a storage failure rather than a 4xx-shaped sentinel.
		return fmt.Errorf("%w: foreign key %s: %w", domain.ErrStorage, pgErr.ConstraintName, err)
	case "23514": // check_violation
		return fmt.Errorf("%w: check %s: %w", domain.ErrStorage, pgErr.ConstraintName, err)
	case "23502": // not_null_violation
		return fmt.Errorf("%w: column %s must not be null: %w", domain.ErrStorage, pgErr.ColumnName, err)
	case "40001", "40P01": // serialization_failure, deadlock_detected
		// Both are retryable by definition: the transaction did not happen and
		// running it again is expected to work. Callers that can retry check
		// for this with IsRetryable.
		return fmt.Errorf("%w: %s is retryable: %w", domain.ErrStorage, pgErr.Code, err)
	case "57014": // query_canceled
		return fmt.Errorf("%w: statement timeout: %w", domain.ErrStorage, err)
	default:
		return fmt.Errorf("%w: %s: %w", domain.ErrStorage, pgErr.Code, err)
	}
}

// IsRetryable reports whether err describes a transaction that can simply be
// run again — a serialization failure or a deadlock. Anything else, including a
// constraint violation, will fail identically on a retry.
func IsRetryable(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "40001" || pgErr.Code == "40P01"
}
