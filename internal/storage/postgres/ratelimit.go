package postgres

import (
	"context"
	"time"

	"github.com/foodibd/socialstats/internal/domain"
)

// RateLimitRepo is a token bucket kept in Postgres.
//
// It lives in the database rather than in each process's memory because the
// limit has to hold across replicas. An in-memory bucket silently multiplies
// the allowance by the number of pods, which makes the fleet size a security
// parameter nobody meant to choose — and one that goes up under load, exactly
// when it should not.
type RateLimitRepo struct{ db *DB }

// RateLimits returns the rate limit repository.
func (db *DB) RateLimits() *RateLimitRepo { return &RateLimitRepo{db: db} }

// Decision is the outcome of trying to spend a token.
type Decision struct {
	Allowed bool

	// Remaining is how many tokens are left after this attempt.
	Remaining float64

	// RetryAfter is how long until a token is available. Zero when allowed.
	RetryAfter time.Duration
}

// Take spends one token from the bucket for (scope, subject), refilling it for
// the time that has passed since the last attempt.
//
// capacity is the burst allowance and window is how long a full bucket takes to
// refill from empty, so "5 attempts per 15 minutes" is Take(..., 5, 15*time.Minute).
func (r *RateLimitRepo) Take(ctx context.Context, scope, subject string, capacity int, window time.Duration) (Decision, error) {
	if capacity <= 0 || window <= 0 {
		// A limit nobody configured is not a limit. Allowing is the right
		// failure mode here: this is called on the login path, and a
		// misconfigured limiter must not lock everyone out.
		return Decision{Allowed: true}, nil
	}

	refillPerSecond := float64(capacity) / window.Seconds()

	const q = `SELECT allowed, remaining, retry_after FROM take_rate_limit_token($1, $2, $3, $4)`

	var d Decision
	var retryAfterSeconds float64
	err := r.db.Pool.QueryRow(ctx, q, scope, subject, float64(capacity), refillPerSecond).
		Scan(&d.Allowed, &d.Remaining, &retryAfterSeconds)
	if err != nil {
		return Decision{}, translate(err)
	}

	d.RetryAfter = time.Duration(retryAfterSeconds * float64(time.Second))
	if d.RetryAfter < 0 {
		d.RetryAfter = 0
	}
	return d, nil
}

// Reset clears a bucket, used after a successful login so that a few mistyped
// passwords do not keep counting against someone who then got it right.
func (r *RateLimitRepo) Reset(ctx context.Context, scope, subject string) error {
	_, err := r.db.Pool.Exec(ctx,
		`DELETE FROM rate_limit_buckets WHERE scope = $1 AND subject = $2`, scope, subject)
	return translate(err)
}

// DeleteIdleBuckets prunes buckets untouched since the cutoff.
//
// A bucket that has not been touched for longer than its own refill window is
// full, and a full bucket is indistinguishable from no bucket at all — so the
// row is pure storage. Housekeeping sweeps them.
func (r *RateLimitRepo) DeleteIdleBuckets(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := r.db.Pool.Exec(ctx, `DELETE FROM rate_limit_buckets WHERE refilled_at < $1`, cutoff)
	if err != nil {
		return 0, translate(err)
	}
	return tag.RowsAffected(), nil
}

// compile-time check that Decision carries what the auth layer expects.
var _ = domain.ErrTooManyAttempts
