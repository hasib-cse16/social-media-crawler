package postgres

import (
	"context"

	"github.com/foodibd/socialstats/internal/domain"
)

// LookupRepo reads and writes the per-user lookup history.
//
// It has no update or refresh path on purpose: a lookup row is what a platform
// reported at a point in time, and rewriting it would make the timestamp next
// to the number a lie.
type LookupRepo struct{ db *DB }

// Lookups returns the lookup-history repository.
func (db *DB) Lookups() *LookupRepo { return &LookupRepo{db: db} }

// lookupColumns is the projection every lookup query shares, so a column added
// to the scan cannot be forgotten in one of three places.
const lookupColumns = `
	id, public_id::text, user_id, platform, video_id, url,
	title, published_at,
	channel_id, channel_title, channel_url, channel_email, channel_description,
	view_count, like_count, comment_count, share_count, save_count,
	looked_up_at`

type lookupRow interface {
	Scan(dest ...any) error
}

func scanLookup(row lookupRow) (*domain.Lookup, error) {
	var (
		l                                     domain.Lookup
		views, likes, comments, shares, saves *int64
	)
	err := row.Scan(
		&l.ID, &l.PublicID, &l.UserID, &l.Platform, &l.VideoID, &l.CanonicalURL,
		&l.Title, &l.PublishedAt,
		&l.ChannelID, &l.ChannelTitle, &l.ChannelURL, &l.ChannelEmail, &l.ChannelDescription,
		&views, &likes, &comments, &shares, &saves,
		&l.LookedUpAt,
	)
	if err != nil {
		return nil, translate(err)
	}
	l.ViewCount = countValue(views)
	l.LikeCount = countValue(likes)
	l.CommentCount = countValue(comments)
	l.ShareCount = countValue(shares)
	l.SaveCount = countValue(saves)
	return &l, nil
}

// Create appends one lookup.
func (r *LookupRepo) Create(ctx context.Context, in domain.Lookup) (*domain.Lookup, error) {
	const q = `
		INSERT INTO lookups (
			user_id, platform, video_id, url,
			title, published_at,
			channel_id, channel_title, channel_url, channel_email, channel_description,
			view_count, like_count, comment_count, share_count, save_count,
			looked_up_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
		RETURNING ` + lookupColumns

	return scanLookup(r.db.Pool.QueryRow(ctx, q,
		in.UserID, string(in.Platform), in.VideoID, in.CanonicalURL,
		in.Title, in.PublishedAt,
		in.ChannelID, in.ChannelTitle, in.ChannelURL, in.ChannelEmail, in.ChannelDescription,
		countArg(in.ViewCount), countArg(in.LikeCount), countArg(in.CommentCount),
		countArg(in.ShareCount), countArg(in.SaveCount),
		in.LookedUpAt,
	))
}

// ByPublicID returns one of this user's lookups.
//
// The user_id is part of the WHERE rather than checked after the read, so
// another user's public id is indistinguishable from one that does not exist.
func (r *LookupRepo) ByPublicID(ctx context.Context, userID int64, publicID string) (*domain.Lookup, error) {
	const q = `SELECT ` + lookupColumns + `
		FROM lookups WHERE user_id = $1 AND public_id = $2::uuid`
	return scanLookup(r.db.Pool.QueryRow(ctx, q, userID, publicID))
}

// Recent returns this user's lookups, newest first.
func (r *LookupRepo) Recent(ctx context.Context, userID int64, limit int) ([]domain.Lookup, error) {
	const q = `SELECT ` + lookupColumns + `
		FROM lookups WHERE user_id = $1
		ORDER BY looked_up_at DESC, id DESC
		LIMIT $2`

	rows, err := r.db.Pool.Query(ctx, q, userID, limit)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := make([]domain.Lookup, 0, limit)
	for rows.Next() {
		l, err := scanLookup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *l)
	}
	return out, translate(rows.Err())
}

// Delete removes one of this user's lookups.
func (r *LookupRepo) Delete(ctx context.Context, userID int64, publicID string) error {
	const q = `DELETE FROM lookups WHERE user_id = $1 AND public_id = $2::uuid`
	tag, err := r.db.Pool.Exec(ctx, q, userID, publicID)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrRecordNotFound
	}
	return nil
}
