package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/foodibd/socialstats/internal/domain"
)

// VideoRepo reads and writes the shared video records and their poll schedule.
type VideoRepo struct{ db *DB }

// Videos returns the video repository.
func (db *DB) Videos() *VideoRepo { return &VideoRepo{db: db} }

// videoColumns is the projection every video query shares.
//
// fetch_interval is read as seconds rather than scanned as an interval: an
// interval's month component has no fixed length and a time.Duration implies
// one, so the conversion is done in SQL where the assumption is visible.
const videoColumns = `
	v.id, v.public_id::text, v.platform, v.platform_video_id, v.canonical_url,
	v.title, v.channel_id, v.channel_title, v.published_at,
	v.latest_view_count, v.latest_like_count, v.latest_comment_count,
	v.latest_share_count, v.latest_save_count, v.latest_captured_at,
	v.tracker_count, extract(epoch from v.fetch_interval)::bigint,
	v.next_fetch_at, v.locked_until, v.consecutive_failures,
	v.last_fetch_at, v.last_fetch_status, v.last_fetch_error,
	v.unavailable_since, v.first_seen_at`

// videoReturning is videoColumns without the table alias.
//
// Which of the two a query needs depends on its shape. A plain INSERT ...
// RETURNING has no alias to reference, so it needs the bare names. An
// UPDATE videos v ... FROM (...) does have one, and needs it: without the
// prefix, "id" is ambiguous between the table and the joined subquery.
//
// It is derived rather than written out a second time so videoColumns stays the
// single definition of how a video is read. Twenty-five column names maintained
// in two places is twenty-five chances for the two to drift.
var videoReturning = strings.ReplaceAll(videoColumns, "v.", "")

type scanner interface {
	Scan(dest ...any) error
}

// videoScan holds the intermediate values a video row needs on the way in:
// nullable text, bigint counters that become *uint64, and an interval already
// reduced to seconds by SQL.
//
// It exists so that the destination list can be shared. The dashboard query
// selects videoColumns plus three of its own, and building its scan from
// dests() rather than repeating twenty-five destinations means videoColumns
// stays the single definition of how a video is read — add a column there and
// every caller picks it up or fails to compile.
type videoScan struct {
	v domain.Video

	title, channelID, channelTitle, lastFetchError *string
	views, likes, comments, shares, saves          *int64
	intervalSeconds                                int64
}

// videoColumnCount is how many columns videoColumns projects. It is asserted
// against the real projection in the tests rather than trusted.
const videoColumnCount = 25

func (s *videoScan) dests() []any {
	return []any{
		&s.v.ID, &s.v.PublicID, &s.v.Platform, &s.v.PlatformVideoID, &s.v.CanonicalURL,
		&s.title, &s.channelID, &s.channelTitle, &s.v.PublishedAt,
		&s.views, &s.likes, &s.comments, &s.shares, &s.saves, &s.v.LatestCapturedAt,
		&s.v.Schedule.TrackerCount, &s.intervalSeconds,
		&s.v.Schedule.NextFetchAt, &s.v.Schedule.LockedUntil, &s.v.Schedule.ConsecutiveFailures,
		&s.v.Schedule.LastFetchAt, &s.v.Schedule.LastFetchStatus, &s.lastFetchError,
		&s.v.Schedule.UnavailableSince, &s.v.FirstSeenAt,
	}
}

// video finalises the conversions the database cannot do for us.
func (s *videoScan) video() *domain.Video {
	v := s.v
	v.Title = nullableString(s.title)
	v.ChannelID = nullableString(s.channelID)
	v.ChannelTitle = nullableString(s.channelTitle)
	v.Latest = domain.Counters{
		ViewCount:    countValue(s.views),
		LikeCount:    countValue(s.likes),
		CommentCount: countValue(s.comments),
		ShareCount:   countValue(s.shares),
		SaveCount:    countValue(s.saves),
	}
	v.Schedule.Interval = secondsToDuration(s.intervalSeconds)
	v.Schedule.LastFetchError = nullableString(s.lastFetchError)
	return &v
}

func scanVideo(row scanner) (*domain.Video, error) {
	var s videoScan
	if err := row.Scan(s.dests()...); err != nil {
		return nil, translate(err)
	}
	return s.video(), nil
}

func scanVideos(rows pgx.Rows) ([]*domain.Video, error) {
	defer rows.Close()

	var out []*domain.Video
	for rows.Next() {
		v, err := scanVideo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, translate(err)
	}
	return out, nil
}

// Upsert finds or creates the row for a video.
//
// (platform, platform_video_id) is the deduplication point, which is why the
// URL parsers run before this: two users pasting youtu.be/X and
// youtube.com/watch?v=X must land on one row, or they get two independent
// fetches and two divergent histories of the same video.
//
// The conflict branch refreshes canonical_url and nothing else. Metadata and
// counters belong to Record, and an upsert that blanked a title because this
// caller happened not to know it would be a slow way to lose data.
func (r *VideoRepo) Upsert(ctx context.Context, in domain.NewVideo) (*domain.Video, error) {
	// RETURNING rather than a data-modifying CTE followed by a SELECT: the
	// outer query of such a CTE reads the snapshot taken before the insert, so
	// it would not see the row that was just written.
	q := `
		INSERT INTO videos (platform, platform_video_id, canonical_url)
		VALUES ($1, $2, $3)
		ON CONFLICT (platform, platform_video_id) DO UPDATE
		SET canonical_url = excluded.canonical_url
		RETURNING ` + videoReturning

	return scanVideo(r.db.Pool.QueryRow(ctx, q, in.Platform, in.PlatformVideoID, in.CanonicalURL))
}

// ByID looks a video up by internal id.
func (r *VideoRepo) ByID(ctx context.Context, id int64) (*domain.Video, error) {
	const q = `SELECT ` + videoColumns + ` FROM videos v WHERE v.id = $1`
	return scanVideo(r.db.Pool.QueryRow(ctx, q, id))
}

// ByPublicID looks a video up by the uuid used in URLs.
func (r *VideoRepo) ByPublicID(ctx context.Context, publicID string) (*domain.Video, error) {
	const q = `SELECT ` + videoColumns + ` FROM videos v WHERE v.public_id = $1::uuid`
	return scanVideo(r.db.Pool.QueryRow(ctx, q, publicID))
}

// ByPlatformID looks a video up by its natural key.
func (r *VideoRepo) ByPlatformID(ctx context.Context, platform domain.Platform, videoID string) (*domain.Video, error) {
	const q = `SELECT ` + videoColumns + `
		FROM videos v WHERE v.platform = $1 AND v.platform_video_id = $2`
	return scanVideo(r.db.Pool.QueryRow(ctx, q, platform, videoID))
}

// ClaimDue takes ownership of up to limit videos that are ready to be fetched,
// locking them for lockFor.
//
// FOR UPDATE SKIP LOCKED is the entire scaling story for the poller: run five
// replicas and they divide the work between them with no coordination, no
// leader election and no chance of two workers fetching the same video. The
// locked_until column covers the remaining case — a worker dying mid-fetch —
// because the lock expires on its own and the row becomes claimable again,
// with no liveness tracking of workers anywhere.
func (r *VideoRepo) ClaimDue(
	ctx context.Context,
	platform domain.Platform,
	limit int,
	lockFor time.Duration,
) ([]*domain.Video, error) {
	if limit <= 0 {
		return nil, nil
	}

	q := `
		UPDATE videos v
		SET locked_until = now() + $3::interval
		FROM (
			SELECT id FROM videos
			WHERE platform = $1
			  AND next_fetch_at IS NOT NULL
			  AND next_fetch_at <= now()
			  AND unavailable_since IS NULL
			  AND (locked_until IS NULL OR locked_until < now())
			ORDER BY next_fetch_at
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		) due
		WHERE v.id = due.id
		RETURNING ` + videoColumns

	rows, err := r.db.Pool.Query(ctx, q, platform, limit, intervalArg(lockFor))
	if err != nil {
		return nil, translate(err)
	}
	return scanVideos(rows)
}

// Release drops a claim without recording a result, for a worker that decided
// not to fetch after all. The video keeps its existing next_fetch_at, so it is
// picked up again on the following tick rather than skipped.
func (r *VideoRepo) Release(ctx context.Context, videoID int64) error {
	_, err := r.db.Pool.Exec(ctx, `UPDATE videos SET locked_until = NULL WHERE id = $1`, videoID)
	return translate(err)
}

// Record persists one fetch attempt: the snapshot, the denormalised current
// values, the audit row and the next schedule — in a single transaction.
//
// These writes must not be separable, and keeping them in one method is what
// makes the denormalised latest_* columns safe to read:
//
//   - A snapshot without the latest_* update leaves the dashboard showing stale
//     numbers with nothing to detect it by.
//   - A schedule update without the snapshot silently drops a data point, and
//     the gap looks identical to the poller never having run.
//   - An audit row without either makes provider health say a fetch succeeded
//     when nothing was stored.
//
// A failed fetch still writes: the audit row and the new schedule are recorded,
// and only the snapshot and latest_* are skipped. "We tried at 14:00 and Meta
// served a login wall" is exactly what the fetch log exists to remember.
func (r *VideoRepo) Record(ctx context.Context, videoID int64, out domain.FetchOutcome) error {
	err := r.record(ctx, videoID, out)

	// A snapshot whose month has no partition fails with a check violation.
	// Rather than lose the reading, create the partition and try once more:
	// this is the writer's own safety net for the case where the housekeeping
	// job has not run, and it is the reason the schema has no DEFAULT
	// partition to hide the problem in.
	if err != nil && isMissingPartition(err) && out.Succeeded() {
		at := snapshotTime(out)
		if _, cerr := r.db.Metrics().EnsureSnapshotPartition(ctx, at); cerr != nil {
			return fmt.Errorf("%w (and creating its partition failed: %v)", err, cerr)
		}
		r.db.log.WarnContext(ctx, "created a missing snapshot partition on demand",
			"video_id", videoID, "captured_at", at,
			"hint", "the partition housekeeping job is not keeping up")
		return r.record(ctx, videoID, out)
	}
	return err
}

func (r *VideoRepo) record(ctx context.Context, videoID int64, out domain.FetchOutcome) error {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return translate(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if out.Succeeded() {
		if err := insertSnapshot(ctx, tx, videoID, snapshotTime(out), out.Stats.Counters()); err != nil {
			return err
		}
		if err := updateVideoLatest(ctx, tx, videoID, out); err != nil {
			return err
		}
	}

	if err := updateVideoSchedule(ctx, tx, videoID, out); err != nil {
		return err
	}
	if err := insertAttempt(ctx, tx, videoID, out); err != nil {
		return err
	}

	return translate(tx.Commit(ctx))
}

func insertSnapshot(ctx context.Context, tx pgx.Tx, videoID int64, at time.Time, c domain.Counters) error {
	const q = `
		INSERT INTO metric_snapshots
			(video_id, captured_at, view_count, like_count, comment_count, share_count, save_count)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (video_id, captured_at) DO UPDATE
		SET view_count    = excluded.view_count,
		    like_count    = excluded.like_count,
		    comment_count = excluded.comment_count,
		    share_count   = excluded.share_count,
		    save_count    = excluded.save_count`

	// The conflict branch handles a re-fetch landing on the same microsecond,
	// which a manual refresh right after a scheduled one can do. Overwriting is
	// right: it is the same reading, measured twice.
	_, err := tx.Exec(ctx, q, videoID, at,
		countArg(c.ViewCount), countArg(c.LikeCount), countArg(c.CommentCount),
		countArg(c.ShareCount), countArg(c.SaveCount))
	return translate(err)
}

func updateVideoLatest(ctx context.Context, tx pgx.Tx, videoID int64, out domain.FetchOutcome) error {
	c := out.Stats.Counters()

	// Metadata uses coalesce(nullif(...)) so a fetch that came back without a
	// title does not erase the one we already had. Counters are written
	// straight through, because a counter that has stopped being reported
	// should stop being reported here too.
	const q = `
		UPDATE videos
		SET title                = coalesce(nullif($2, ''), title),
		    channel_id           = coalesce(nullif($3, ''), channel_id),
		    channel_title        = coalesce(nullif($4, ''), channel_title),
		    published_at         = coalesce($5, published_at),
		    canonical_url        = coalesce(nullif($6, ''), canonical_url),
		    latest_view_count    = $7,
		    latest_like_count    = $8,
		    latest_comment_count = $9,
		    latest_share_count   = $10,
		    latest_save_count    = $11,
		    latest_captured_at   = $12
		WHERE id = $1`

	tag, err := tx.Exec(ctx, q, videoID,
		out.Stats.Title, out.Stats.ChannelID, out.Stats.ChannelTitle,
		out.Stats.PublishedAt, out.Stats.CanonicalURL,
		countArg(c.ViewCount), countArg(c.LikeCount), countArg(c.CommentCount),
		countArg(c.ShareCount), countArg(c.SaveCount),
		snapshotTime(out))
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return errNoRowsAffected("video", videoID)
	}
	return nil
}

func updateVideoSchedule(ctx context.Context, tx pgx.Tx, videoID int64, out domain.FetchOutcome) error {
	// The claim is released here — locked_until goes back to NULL — so the same
	// transaction that records the result also hands the row back. A worker
	// that crashes before commit leaves the lock in place to expire, which is
	// exactly what should happen.
	const q = `
		UPDATE videos
		SET last_fetch_at        = $2,
		    last_fetch_status    = $3,
		    last_fetch_error     = $4,
		    consecutive_failures = $5,
		    next_fetch_at        = $6,
		    unavailable_since    = $7,
		    locked_until         = NULL
		WHERE id = $1`

	tag, err := tx.Exec(ctx, q, videoID,
		out.StartedAt, out.Status, emptyToNil(out.ErrorDetail),
		out.ConsecutiveFailures, out.NextFetchAt, out.UnavailableSince)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return errNoRowsAffected("video", videoID)
	}
	return nil
}

func insertAttempt(ctx context.Context, tx pgx.Tx, videoID int64, out domain.FetchOutcome) error {
	const q = `
		INSERT INTO fetch_attempts
			(video_id, platform, started_at, duration_ms, status, error_code, error_detail)
		SELECT $1, v.platform, $2, $3, $4, $5, $6
		FROM videos v WHERE v.id = $1`

	ms := out.Duration.Milliseconds()
	if ms < 0 {
		ms = 0
	}

	_, err := tx.Exec(ctx, q, videoID, out.StartedAt, ms, out.AttemptStatus,
		emptyToNil(out.ErrorCode), emptyToNil(truncateDetail(out.ErrorDetail)))
	return translate(err)
}

// Reschedule moves a video's next fetch, used to bring a manual refresh forward
// or to push a whole platform back after a rate limit.
func (r *VideoRepo) Reschedule(ctx context.Context, videoID int64, at time.Time) error {
	const q = `
		UPDATE videos SET next_fetch_at = $2
		WHERE id = $1 AND unavailable_since IS NULL AND tracker_count > 0`

	tag, err := r.db.Pool.Exec(ctx, q, videoID, at)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		// Retired or untracked videos are not scheduled, and asking to
		// reschedule one is a no-op rather than a failure.
		return nil
	}
	return nil
}

// BackoffPlatform pushes every scheduled video on a platform past `until`.
//
// A rate limit is a property of the platform, not of the video that happened to
// hit it. Backing off only that video would send the next tick straight into
// the same limit with a different id.
func (r *VideoRepo) BackoffPlatform(ctx context.Context, platform domain.Platform, until time.Time) (int64, error) {
	const q = `
		UPDATE videos
		SET next_fetch_at = $2, locked_until = NULL
		WHERE platform = $1
		  AND next_fetch_at IS NOT NULL
		  AND next_fetch_at < $2
		  AND unavailable_since IS NULL`

	tag, err := r.db.Pool.Exec(ctx, q, platform, until)
	if err != nil {
		return 0, translate(err)
	}
	return tag.RowsAffected(), nil
}

// snapshotTime is when a reading was taken. The provider's own FetchedAt is
// preferred over the database clock so a slow write does not misdate the
// measurement.
func snapshotTime(out domain.FetchOutcome) time.Time {
	if out.Stats != nil && !out.Stats.FetchedAt.IsZero() {
		return out.Stats.FetchedAt.UTC()
	}
	if !out.StartedAt.IsZero() {
		return out.StartedAt.UTC()
	}
	return time.Now().UTC()
}

// isMissingPartition recognises the error a range-partitioned insert produces
// when no partition covers the row.
func isMissingPartition(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "23514" && strings.Contains(pgErr.Message, "no partition of relation")
}

// truncateDetail bounds what a provider's error text can cost us. Some of them
// include a page snippet, and an unbounded column filled from upstream output
// is a slow storage leak.
func truncateDetail(s string) string {
	const limit = 1000
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}
