package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/foodibd/socialstats/internal/domain"
)

// MetricRepo reads the time series and owns the jobs that keep it bounded.
type MetricRepo struct{ db *DB }

// Metrics returns the metric repository.
func (db *DB) Metrics() *MetricRepo { return &MetricRepo{db: db} }

// Bucket is the resolution a history query is returned at.
type Bucket string

const (
	// BucketRaw returns every stored snapshot.
	BucketRaw Bucket = "raw"
	// BucketHour returns the last reading in each hour.
	BucketHour Bucket = "hour"
	// BucketDay returns the last reading in each day.
	BucketDay Bucket = "day"
)

// History returns a video's readings between two times.
//
// Bucketing takes the *last* reading in each bucket rather than an average,
// because these are cumulative counters, not rates: the mean of 220,000 and
// 221,000 is not a view count anything ever had. Last-in-bucket is a real
// measurement that happens to be the one at the end of the period.
func (r *MetricRepo) History(ctx context.Context, videoID int64, from, to time.Time, bucket Bucket) ([]domain.Snapshot, error) {
	var q string
	switch bucket {
	case BucketRaw, "":
		q = `
			SELECT captured_at, view_count, like_count, comment_count, share_count, save_count
			FROM metric_snapshots
			WHERE video_id = $1 AND captured_at >= $2 AND captured_at <= $3
			ORDER BY captured_at`
	case BucketHour, BucketDay:
		// DISTINCT ON is the cheapest way to say "last row per bucket" in
		// Postgres, and it rides the (video_id, captured_at) primary key.
		q = fmt.Sprintf(`
			SELECT DISTINCT ON (date_trunc('%s', captured_at))
			       captured_at, view_count, like_count, comment_count, share_count, save_count
			FROM metric_snapshots
			WHERE video_id = $1 AND captured_at >= $2 AND captured_at <= $3
			ORDER BY date_trunc('%s', captured_at), captured_at DESC`, bucket, bucket)
	default:
		return nil, fmt.Errorf("%w: unknown bucket %q", domain.ErrInvalidURL, bucket)
	}

	rows, err := r.db.Pool.Query(ctx, q, videoID, from, to)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out, err := scanSnapshots(rows, videoID)
	if err != nil {
		return nil, err
	}

	// DISTINCT ON orders by the bucket expression; callers want chronological.
	if bucket == BucketHour || bucket == BucketDay {
		sortSnapshots(out)
	}
	return out, nil
}

// Recent returns a video's last n readings, oldest first. It is what a
// sparkline needs, and it reads backwards off the primary key.
func (r *MetricRepo) Recent(ctx context.Context, videoID int64, limit int) ([]domain.Snapshot, error) {
	if limit <= 0 {
		return nil, nil
	}

	const q = `
		SELECT captured_at, view_count, like_count, comment_count, share_count, save_count
		FROM (
			SELECT * FROM metric_snapshots
			WHERE video_id = $1
			ORDER BY captured_at DESC
			LIMIT $2
		) recent
		ORDER BY captured_at`

	rows, err := r.db.Pool.Query(ctx, q, videoID, limit)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()
	return scanSnapshots(rows, videoID)
}

func scanSnapshots(rows pgx.Rows, videoID int64) ([]domain.Snapshot, error) {
	var out []domain.Snapshot
	for rows.Next() {
		var s domain.Snapshot
		var views, likes, comments, shares, saves *int64

		if err := rows.Scan(&s.CapturedAt, &views, &likes, &comments, &shares, &saves); err != nil {
			return nil, translate(err)
		}
		s.VideoID = videoID
		s.Counters = domain.Counters{
			ViewCount:    countValue(views),
			LikeCount:    countValue(likes),
			CommentCount: countValue(comments),
			ShareCount:   countValue(shares),
			SaveCount:    countValue(saves),
		}
		out = append(out, s)
	}
	return out, translate(rows.Err())
}

func sortSnapshots(s []domain.Snapshot) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j].CapturedAt.Before(s[j-1].CapturedAt); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// Daily returns the rolled-up figures for a video over a date range.
func (r *MetricRepo) Daily(ctx context.Context, videoID int64, from, to time.Time) ([]domain.DailyMetric, error) {
	const q = `
		SELECT day, first_view_count, last_view_count, last_like_count,
		       last_comment_count, last_share_count, last_save_count,
		       view_delta, sample_count
		FROM metric_daily
		WHERE video_id = $1 AND day >= $2::date AND day <= $3::date
		ORDER BY day`

	rows, err := r.db.Pool.Query(ctx, q, videoID, from, to)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	var out []domain.DailyMetric
	for rows.Next() {
		var d domain.DailyMetric
		var first, last, likes, comments, shares, saves *int64

		if err := rows.Scan(&d.Day, &first, &last, &likes, &comments, &shares, &saves,
			&d.ViewDelta, &d.SampleCount); err != nil {
			return nil, translate(err)
		}
		d.VideoID = videoID
		d.FirstViewCount = countValue(first)
		d.LastViewCount = countValue(last)
		d.LastLike = countValue(likes)
		d.LastComment = countValue(comments)
		d.LastShare = countValue(shares)
		d.LastSave = countValue(saves)
		out = append(out, d)
	}
	return out, translate(rows.Err())
}

// Rollup builds or rebuilds metric_daily for one UTC day, across every video.
//
// It is idempotent, so running it hourly for today and again for yesterday
// costs nothing and means a missed run repairs itself on the next one rather
// than leaving a permanent hole in the long-range charts.
//
// view_delta is last minus first and is allowed to be negative: platforms
// revise view counts downward, and recording that honestly is the whole point
// of storing a signed value.
func (r *MetricRepo) Rollup(ctx context.Context, day time.Time) (int64, error) {
	const q = `
		INSERT INTO metric_daily (
			video_id, day,
			first_view_count, last_view_count,
			last_like_count, last_comment_count, last_share_count, last_save_count,
			view_delta, sample_count
		)
		SELECT
			video_id,
			$1::date,
			(array_agg(view_count    ORDER BY captured_at))[1],
			(array_agg(view_count    ORDER BY captured_at DESC))[1],
			(array_agg(like_count    ORDER BY captured_at DESC))[1],
			(array_agg(comment_count ORDER BY captured_at DESC))[1],
			(array_agg(share_count   ORDER BY captured_at DESC))[1],
			(array_agg(save_count    ORDER BY captured_at DESC))[1],
			(array_agg(view_count ORDER BY captured_at DESC))[1]
				- (array_agg(view_count ORDER BY captured_at))[1],
			count(*)
		FROM metric_snapshots
		WHERE captured_at >= $1::date
		  AND captured_at <  ($1::date + interval '1 day')
		GROUP BY video_id
		ON CONFLICT (video_id, day) DO UPDATE
		SET first_view_count   = excluded.first_view_count,
		    last_view_count    = excluded.last_view_count,
		    last_like_count    = excluded.last_like_count,
		    last_comment_count = excluded.last_comment_count,
		    last_share_count   = excluded.last_share_count,
		    last_save_count    = excluded.last_save_count,
		    view_delta         = excluded.view_delta,
		    sample_count       = excluded.sample_count`

	tag, err := r.db.Pool.Exec(ctx, q, day)
	if err != nil {
		return 0, translate(err)
	}
	return tag.RowsAffected(), nil
}

// EnsureSnapshotPartition creates the monthly partition covering `at` if it is
// missing, returning its name.
//
// The work happens in a SQL function so creation is one atomic statement that
// tolerates two workers racing across a month boundary; see migration 0004.
func (r *MetricRepo) EnsureSnapshotPartition(ctx context.Context, at time.Time) (string, error) {
	var name string
	err := r.db.Pool.QueryRow(ctx, `SELECT ensure_metric_snapshot_partition($1)`, at.UTC()).Scan(&name)
	if err != nil {
		return "", translate(err)
	}
	return name, nil
}

// EnsureSnapshotPartitions creates partitions for the current month and the
// next `ahead` months. Running ahead of time is what keeps a missing partition
// from ever being the reason a reading is lost.
func (r *MetricRepo) EnsureSnapshotPartitions(ctx context.Context, from time.Time, ahead int) ([]string, error) {
	out := make([]string, 0, ahead+1)
	for i := 0; i <= ahead; i++ {
		name, err := r.EnsureSnapshotPartition(ctx, from.UTC().AddDate(0, i, 0))
		if err != nil {
			return out, err
		}
		out = append(out, name)
	}
	return out, nil
}

// DropSnapshotPartitionsBefore removes whole partitions that end at or before
// the cutoff, returning what it dropped.
//
// Retention by DROP TABLE rather than DELETE is the reason the table is
// partitioned at all: it is instant, leaves no bloat and generates almost no
// WAL, where deleting several million rows every month does all three.
//
// A partition is only dropped when its entire range is past the cutoff, so a
// partially expired month stays until it is completely expired. Losing 29 days
// of data to reclaim one is not a trade worth making.
func (r *MetricRepo) DropSnapshotPartitionsBefore(ctx context.Context, cutoff time.Time) ([]string, error) {
	const q = `
		SELECT c.relname
		FROM pg_class c
		JOIN pg_inherits i ON i.inhrelid = c.oid
		JOIN pg_class p ON p.oid = i.inhparent
		WHERE p.relname = 'metric_snapshots'
		  AND split_part(
		        rtrim(split_part(pg_get_expr(c.relpartbound, c.oid), ' TO (', 2), ')'),
		        '''', 2)::timestamptz <= $1
		ORDER BY c.relname`

	rows, err := r.db.Pool.Query(ctx, q, cutoff.UTC())
	if err != nil {
		return nil, translate(err)
	}

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, translate(err)
		}
		names = append(names, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, translate(err)
	}

	for _, name := range names {
		// The name comes from pg_class, not from a caller, but it is still
		// quoted rather than interpolated raw: the habit is what makes the one
		// place that does take user input obvious.
		if _, err := r.db.Pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, pgQuoteIdent(name))); err != nil {
			return names, translate(err)
		}
		r.db.log.InfoContext(ctx, "dropped expired snapshot partition", "partition", name)
	}
	return names, nil
}

// Attempts returns a video's most recent fetch attempts, newest first.
func (r *MetricRepo) Attempts(ctx context.Context, videoID int64, limit int) ([]domain.FetchAttempt, error) {
	if limit <= 0 {
		limit = 20
	}

	const q = `
		SELECT platform, started_at, duration_ms, status, error_code, error_detail
		FROM fetch_attempts
		WHERE video_id = $1
		ORDER BY started_at DESC
		LIMIT $2`

	rows, err := r.db.Pool.Query(ctx, q, videoID, limit)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	var out []domain.FetchAttempt
	for rows.Next() {
		var a domain.FetchAttempt
		var code, detail *string

		if err := rows.Scan(&a.Platform, &a.StartedAt, &a.DurationMS, &a.Status, &code, &detail); err != nil {
			return nil, translate(err)
		}
		a.VideoID = videoID
		a.Duration = time.Duration(a.DurationMS) * time.Millisecond
		a.ErrorCode = nullableString(code)
		a.ErrorDetail = nullableString(detail)
		out = append(out, a)
	}
	return out, translate(rows.Err())
}

// PlatformHealth is a platform's fetch success rate over a window.
type PlatformHealth struct {
	Platform  domain.Platform `json:"platform"`
	Attempts  int             `json:"attempts"`
	Succeeded int             `json:"succeeded"`
	Blocked   int             `json:"blocked"`
	NotFound  int             `json:"not_found"`
	Errored   int             `json:"errored"`
	P50MS     int             `json:"p50_ms"`
	P95MS     int             `json:"p95_ms"`
}

// SuccessRate is the fraction of attempts that returned data, or 0 when there
// were none.
func (h PlatformHealth) SuccessRate() float64 {
	if h.Attempts == 0 {
		return 0
	}
	return float64(h.Succeeded) / float64(h.Attempts)
}

// Health summarises fetch outcomes per platform over a window.
//
// This is what the audit table exists for: "is the TikTok provider degrading?"
// is otherwise answerable only by grepping logs, and by then the answer is a
// week old.
func (r *MetricRepo) Health(ctx context.Context, since time.Time) ([]PlatformHealth, error) {
	const q = `
		SELECT platform,
		       count(*),
		       count(*) FILTER (WHERE status = 'ok'),
		       count(*) FILTER (WHERE status = 'blocked'),
		       count(*) FILTER (WHERE status = 'not_found'),
		       count(*) FILTER (WHERE status IN ('error', 'timeout', 'rate_limited')),
		       coalesce(percentile_disc(0.5)  WITHIN GROUP (ORDER BY duration_ms), 0),
		       coalesce(percentile_disc(0.95) WITHIN GROUP (ORDER BY duration_ms), 0)
		FROM fetch_attempts
		WHERE started_at >= $1
		GROUP BY platform
		ORDER BY platform`

	rows, err := r.db.Pool.Query(ctx, q, since)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	var out []PlatformHealth
	for rows.Next() {
		var h PlatformHealth
		if err := rows.Scan(&h.Platform, &h.Attempts, &h.Succeeded, &h.Blocked,
			&h.NotFound, &h.Errored, &h.P50MS, &h.P95MS); err != nil {
			return nil, translate(err)
		}
		out = append(out, h)
	}
	return out, translate(rows.Err())
}

// DeleteAttemptsBefore prunes the audit trail.
func (r *MetricRepo) DeleteAttemptsBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := r.db.Pool.Exec(ctx, `DELETE FROM fetch_attempts WHERE started_at < $1`, cutoff)
	if err != nil {
		return 0, translate(err)
	}
	return tag.RowsAffected(), nil
}

// pgQuoteIdent double-quotes an identifier for interpolation into DDL, which
// cannot take a bind parameter.
func pgQuoteIdent(name string) string {
	out := make([]byte, 0, len(name)+2)
	out = append(out, '"')
	for i := range len(name) {
		if name[i] == '"' {
			out = append(out, '"')
		}
		out = append(out, name[i])
	}
	return string(append(out, '"'))
}
