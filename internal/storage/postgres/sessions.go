package postgres

import (
	"context"
	"time"

	"github.com/foodibd/socialstats/internal/domain"
)

// SessionRepo reads and writes server-side sessions.
//
// Every method is keyed on the SHA-256 of the session token, never the token.
// Hashing is the auth package's job; this package stores what it is handed and
// has no way to reverse it.
type SessionRepo struct{ db *DB }

// Sessions returns the session repository.
func (db *DB) Sessions() *SessionRepo { return &SessionRepo{db: db} }

// Create issues a session.
func (r *SessionRepo) Create(ctx context.Context, in domain.NewSession) (*domain.Session, error) {
	const q = `
		INSERT INTO sessions (token_hash, user_id, expires_at, user_agent, ip)
		VALUES ($1, $2, $3, $4, nullif($5, '')::inet)
		RETURNING user_id, created_at, last_seen_at, expires_at, user_agent, coalesce(host(ip), '')`

	var s domain.Session
	err := r.db.Pool.QueryRow(ctx, q, in.TokenHash, in.UserID, in.ExpiresAt, in.UserAgent, in.IP).
		Scan(&s.UserID, &s.CreatedAt, &s.LastSeenAt, &s.ExpiresAt, &s.UserAgent, &s.IP)
	if err != nil {
		return nil, translate(err)
	}
	return &s, nil
}

// Lookup resolves a token hash to its session and the user it belongs to.
//
// Both expiry rules are applied in SQL, so an expired or idle session comes back
// as domain.ErrRecordNotFound rather than as a valid-looking row the caller has
// to remember to check. A suspended account is filtered here for the same
// reason: there is exactly one place that decides whether a session is usable,
// and it is not spread across every handler.
//
// One round trip returns both records because the auth middleware needs both on
// every single request.
func (r *SessionRepo) Lookup(ctx context.Context, tokenHash []byte, idleTTL time.Duration) (*domain.Session, *domain.User, error) {
	const q = `
		SELECT s.user_id, s.created_at, s.last_seen_at, s.expires_at,
		       s.user_agent, coalesce(host(s.ip), ''),
		       u.id, u.public_id::text, u.email::text, u.display_name, u.timezone,
		       u.status, u.created_at, u.updated_at, u.last_login_at
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1
		  AND s.expires_at > now()
		  AND s.last_seen_at > now() - $2::interval
		  AND u.status = 'active'`

	var s domain.Session
	var u domain.User
	err := r.db.Pool.QueryRow(ctx, q, tokenHash, intervalArg(idleTTL)).Scan(
		&s.UserID, &s.CreatedAt, &s.LastSeenAt, &s.ExpiresAt, &s.UserAgent, &s.IP,
		&u.ID, &u.PublicID, &u.Email, &u.DisplayName, &u.Timezone,
		&u.Status, &u.CreatedAt, &u.UpdatedAt, &u.LastLoginAt,
	)
	if err != nil {
		return nil, nil, translate(err)
	}
	return &s, &u, nil
}

// Touch advances last_seen_at, but only when it is already older than
// minInterval.
//
// Idle expiry needs a write on activity, and a write on every authenticated
// request would make the sessions table the busiest thing in the database for
// no benefit — the difference between a session last seen 3 seconds ago and 30
// is not worth a WAL record. The threshold moves that decision into SQL, where
// it costs one index lookup and usually updates nothing.
//
// It reports whether a row was actually written, which is only interesting to
// tests.
func (r *SessionRepo) Touch(ctx context.Context, tokenHash []byte, minInterval time.Duration) (bool, error) {
	const q = `
		UPDATE sessions
		SET last_seen_at = now()
		WHERE token_hash = $1
		  AND last_seen_at < now() - $2::interval`

	tag, err := r.db.Pool.Exec(ctx, q, tokenHash, intervalArg(minInterval))
	if err != nil {
		return false, translate(err)
	}
	return tag.RowsAffected() > 0, nil
}

// Delete revokes one session. Deleting a session that is already gone is not an
// error: logging out twice is not a failure.
func (r *SessionRepo) Delete(ctx context.Context, tokenHash []byte) error {
	_, err := r.db.Pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, tokenHash)
	return translate(err)
}

// DeleteAllForUser is "log out everywhere". Server-side sessions give this for
// free; it is the capability a stateless token would have cost us.
func (r *SessionRepo) DeleteAllForUser(ctx context.Context, userID int64) (int64, error) {
	tag, err := r.db.Pool.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, userID)
	if err != nil {
		return 0, translate(err)
	}
	return tag.RowsAffected(), nil
}

// DeleteExpired reaps sessions past either bound: the absolute expiry, or the
// idle window measured from last activity.
func (r *SessionRepo) DeleteExpired(ctx context.Context, idleTTL time.Duration) (int64, error) {
	const q = `
		DELETE FROM sessions
		WHERE expires_at <= now()
		   OR last_seen_at <= now() - $1::interval`

	tag, err := r.db.Pool.Exec(ctx, q, intervalArg(idleTTL))
	if err != nil {
		return 0, translate(err)
	}
	return tag.RowsAffected(), nil
}

// CountForUser reports how many live sessions an account has, for a "signed in
// on N devices" display.
func (r *SessionRepo) CountForUser(ctx context.Context, userID int64, idleTTL time.Duration) (int, error) {
	const q = `
		SELECT count(*) FROM sessions
		WHERE user_id = $1
		  AND expires_at > now()
		  AND last_seen_at > now() - $2::interval`

	var n int
	if err := r.db.Pool.QueryRow(ctx, q, userID, intervalArg(idleTTL)).Scan(&n); err != nil {
		return 0, translate(err)
	}
	return n, nil
}

// intervalArg renders a Duration as a Postgres interval literal.
//
// Seconds, always: a Duration is an exact span, and formatting it as anything
// month-shaped would hand Postgres a unit whose length depends on the calendar.
func intervalArg(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return strconvSeconds(d) + " seconds"
}
