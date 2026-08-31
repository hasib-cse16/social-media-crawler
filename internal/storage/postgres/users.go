package postgres

import (
	"context"
	"strings"
	"time"

	"github.com/foodibd/socialstats/internal/domain"
)

// UserRepo reads and writes accounts.
type UserRepo struct{ db *DB }

// Users returns the account repository.
func (db *DB) Users() *UserRepo { return &UserRepo{db: db} }

// userColumns is the projection every user query shares, so a column added to
// the scan cannot be forgotten in one of four places.
//
// password_hash is deliberately absent: domain.User has nowhere to put it, and
// the only path that needs it is Credentials below.
const userColumns = `
	id, public_id::text, email::text, display_name, timezone, status,
	created_at, updated_at, last_login_at`

type userRow interface {
	Scan(dest ...any) error
}

func scanUser(row userRow) (*domain.User, error) {
	var u domain.User
	err := row.Scan(
		&u.ID, &u.PublicID, &u.Email, &u.DisplayName, &u.Timezone, &u.Status,
		&u.CreatedAt, &u.UpdatedAt, &u.LastLoginAt,
	)
	if err != nil {
		return nil, translate(err)
	}
	return &u, nil
}

// Create inserts an account. A duplicate email surfaces as domain.ErrConflict.
//
// The email is trimmed and lowercased on the way in even though the column is
// citext. citext makes comparison case-insensitive but preserves what was
// stored, and an address echoed back as the user typed it reads better than one
// normalised behind their back — while the stored form staying canonical means
// exports and logs do not show the same account four different ways.
func (r *UserRepo) Create(ctx context.Context, in domain.NewUser) (*domain.User, error) {
	tz := in.Timezone
	if tz == "" {
		tz = "UTC"
	}

	const q = `
		INSERT INTO users (email, password_hash, display_name, timezone)
		VALUES ($1, $2, $3, $4)
		RETURNING ` + userColumns

	return scanUser(r.db.Pool.QueryRow(ctx, q,
		normalizeEmail(in.Email), in.PasswordHash, strings.TrimSpace(in.DisplayName), tz))
}

// ByID looks an account up by its internal id.
func (r *UserRepo) ByID(ctx context.Context, id int64) (*domain.User, error) {
	const q = `SELECT ` + userColumns + ` FROM users WHERE id = $1`
	return scanUser(r.db.Pool.QueryRow(ctx, q, id))
}

// ByPublicID looks an account up by the uuid used in URLs.
func (r *UserRepo) ByPublicID(ctx context.Context, publicID string) (*domain.User, error) {
	const q = `SELECT ` + userColumns + ` FROM users WHERE public_id = $1::uuid`
	return scanUser(r.db.Pool.QueryRow(ctx, q, publicID))
}

// ByEmail looks an account up by address, case-insensitively.
func (r *UserRepo) ByEmail(ctx context.Context, email string) (*domain.User, error) {
	const q = `SELECT ` + userColumns + ` FROM users WHERE email = $1::citext`
	return scanUser(r.db.Pool.QueryRow(ctx, q, normalizeEmail(email)))
}

// Credentials fetches what the login path needs to verify a password.
//
// It is a separate method rather than a field on User so that a hash cannot
// travel anywhere it is not wanted: there is no way to accidentally serialise
// one, because the type that carries it never leaves the auth package.
func (r *UserRepo) Credentials(ctx context.Context, email string) (*domain.Credentials, error) {
	const q = `SELECT id, password_hash, status FROM users WHERE email = $1::citext`

	var c domain.Credentials
	err := r.db.Pool.QueryRow(ctx, q, normalizeEmail(email)).
		Scan(&c.UserID, &c.PasswordHash, &c.Status)
	if err != nil {
		return nil, translate(err)
	}
	return &c, nil
}

// UpdatePasswordHash replaces the stored hash, used both for a password change
// and for silently upgrading a hash whose parameters are out of date.
func (r *UserRepo) UpdatePasswordHash(ctx context.Context, userID int64, hash string) error {
	const q = `UPDATE users SET password_hash = $2, updated_at = now() WHERE id = $1`

	tag, err := r.db.Pool.Exec(ctx, q, userID, hash)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return errNoRowsAffected("user", userID)
	}
	return nil
}

// UpdateProfile changes the fields a user controls about themselves.
func (r *UserRepo) UpdateProfile(ctx context.Context, userID int64, displayName, timezone string) (*domain.User, error) {
	const q = `
		UPDATE users
		SET display_name = $2,
		    timezone     = coalesce(nullif($3, ''), timezone),
		    updated_at   = now()
		WHERE id = $1
		RETURNING ` + userColumns

	return scanUser(r.db.Pool.QueryRow(ctx, q, userID, strings.TrimSpace(displayName), timezone))
}

// TouchLastLogin records a successful sign-in.
func (r *UserRepo) TouchLastLogin(ctx context.Context, userID int64, at time.Time) error {
	const q = `UPDATE users SET last_login_at = $2 WHERE id = $1`

	tag, err := r.db.Pool.Exec(ctx, q, userID, at)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return errNoRowsAffected("user", userID)
	}
	return nil
}

// SetStatus suspends or reactivates an account.
func (r *UserRepo) SetStatus(ctx context.Context, userID int64, status domain.UserStatus) error {
	const q = `UPDATE users SET status = $2, updated_at = now() WHERE id = $1`

	tag, err := r.db.Pool.Exec(ctx, q, userID, status)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return errNoRowsAffected("user", userID)
	}
	return nil
}

// Delete removes an account. Sessions, tracking rows and everything keyed on
// the user go with it by cascade; the videos themselves and their history stay,
// because other users may be tracking them.
func (r *UserRepo) Delete(ctx context.Context, userID int64) error {
	tag, err := r.db.Pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return errNoRowsAffected("user", userID)
	}
	return nil
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}
