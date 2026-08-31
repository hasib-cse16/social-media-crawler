package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/foodibd/socialstats/internal/domain"
)

// TrackingRepo manages which users track which videos, and answers the
// dashboard's list query.
type TrackingRepo struct{ db *DB }

// Tracking returns the tracking repository.
func (db *DB) Tracking() *TrackingRepo { return &TrackingRepo{db: db} }

// Track adds a video to a user's list, or un-archives it if they had removed it
// before. It also brings the video into the poll rotation if this is its first
// active tracker.
//
// Both writes are one transaction because the second is what makes the first
// mean anything: a tracking row on a video nobody polls is a bookmark, not a
// tracked video.
func (r *TrackingRepo) Track(ctx context.Context, userID, videoID int64, label string) (*domain.TrackedVideo, error) {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return nil, translate(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// Re-tracking keeps the original created_at and only replaces the label
	// when a new one was given, so removing and re-adding a video does not
	// silently discard the name the user chose for it.
	const upsert = `
		INSERT INTO tracked_videos (user_id, video_id, label)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id, video_id) DO UPDATE
		SET archived_at = NULL,
		    label       = coalesce(nullif(excluded.label, ''), tracked_videos.label)
		RETURNING user_id, video_id, label, notes, created_at, archived_at`

	var t domain.TrackedVideo
	err = tx.QueryRow(ctx, upsert, userID, videoID, label).
		Scan(&t.UserID, &t.VideoID, &t.Label, &t.Notes, &t.AddedAt, &t.Archived)
	if err != nil {
		return nil, translate(err)
	}

	if err := syncTrackerCount(ctx, tx, videoID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, translate(err)
	}
	return &t, nil
}

// Untrack archives a user's tracking row and takes the video out of the poll
// rotation if that was its last active tracker.
//
// Archiving rather than deleting means the label and the date they added it
// survive, so re-adding a video is a restore rather than a fresh start.
func (r *TrackingRepo) Untrack(ctx context.Context, userID, videoID int64) error {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return translate(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	const q = `
		UPDATE tracked_videos SET archived_at = now()
		WHERE user_id = $1 AND video_id = $2 AND archived_at IS NULL`

	tag, err := tx.Exec(ctx, q, userID, videoID)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: user %d does not track video %d", domain.ErrRecordNotFound, userID, videoID)
	}

	if err := syncTrackerCount(ctx, tx, videoID); err != nil {
		return err
	}
	return translate(tx.Commit(ctx))
}

// syncTrackerCount recomputes a video's tracker count from the tracking rows
// and brings it into or out of the poll rotation accordingly.
//
// It recounts rather than incrementing on purpose. An increment is one missed
// error path away from a count that no longer matches reality — and a video
// with a phantom tracker is polled forever for nobody, while one whose count
// undershoots to zero stops being polled for someone who is still watching it.
// Deriving the value from the rows themselves means the number cannot drift,
// and any drift that somehow exists is repaired the next time anyone touches
// the video.
func syncTrackerCount(ctx context.Context, tx pgx.Tx, videoID int64) error {
	const q = `
		WITH active AS (
			SELECT count(*) AS n
			FROM tracked_videos
			WHERE video_id = $1 AND archived_at IS NULL
		)
		UPDATE videos v
		SET tracker_count = active.n,
		    next_fetch_at = CASE
		        -- Nobody is watching: stop polling, but keep the history.
		        WHEN active.n = 0 THEN NULL
		        -- Retired videos stay out of the rotation however many people
		        -- track them.
		        WHEN v.unavailable_since IS NOT NULL THEN NULL
		        -- Newly tracked: fetch immediately rather than making the first
		        -- reading wait a whole interval.
		        WHEN v.next_fetch_at IS NULL THEN now()
		        ELSE v.next_fetch_at
		    END
		FROM active
		WHERE v.id = $1`

	tag, err := tx.Exec(ctx, q, videoID)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return errNoRowsAffected("video", videoID)
	}
	return nil
}

// Get returns one user's tracking row for a video.
func (r *TrackingRepo) Get(ctx context.Context, userID, videoID int64) (*domain.TrackedVideo, error) {
	const q = `
		SELECT user_id, video_id, label, notes, created_at, archived_at
		FROM tracked_videos
		WHERE user_id = $1 AND video_id = $2 AND archived_at IS NULL`

	var t domain.TrackedVideo
	err := r.db.Pool.QueryRow(ctx, q, userID, videoID).
		Scan(&t.UserID, &t.VideoID, &t.Label, &t.Notes, &t.AddedAt, &t.Archived)
	if err != nil {
		return nil, translate(err)
	}
	return &t, nil
}

// Update changes the per-user fields on a tracking row.
func (r *TrackingRepo) Update(ctx context.Context, userID, videoID int64, label, notes string) (*domain.TrackedVideo, error) {
	const q = `
		UPDATE tracked_videos SET label = $3, notes = $4
		WHERE user_id = $1 AND video_id = $2 AND archived_at IS NULL
		RETURNING user_id, video_id, label, notes, created_at, archived_at`

	var t domain.TrackedVideo
	err := r.db.Pool.QueryRow(ctx, q, userID, videoID, label, notes).
		Scan(&t.UserID, &t.VideoID, &t.Label, &t.Notes, &t.AddedAt, &t.Archived)
	if err != nil {
		return nil, translate(err)
	}
	return &t, nil
}

// DashboardEntry is one row of a user's dashboard: the shared video, their own
// label for it, and how far it has moved over the requested window.
type DashboardEntry struct {
	Video *domain.Video `json:"video"`
	Label string        `json:"label,omitempty"`

	// ViewsGained is the current view count minus the last reading at or before
	// the start of the window. It is nil when there is no baseline — a video
	// added yesterday cannot have a week's growth — and it is signed, because
	// platforms revise counts downward and a negative here is a measurement
	// rather than a bug.
	ViewsGained *int64 `json:"views_gained,omitempty"`

	// BaselineAt is when the figure ViewsGained was measured against was taken.
	// Without it a delta is uninterpretable: "+1,200 since some unspecified
	// point" is not a fact anyone can use.
	BaselineAt *time.Time `json:"baseline_at,omitempty"`

	AddedAt time.Time `json:"added_at"`
}

// DashboardSort names an ordering for the dashboard list.
type DashboardSort string

const (
	SortViews       DashboardSort = "views"
	SortGained      DashboardSort = "gained"
	SortRecent      DashboardSort = "recent"
	SortTitle       DashboardSort = "title"
	SortLastFetched DashboardSort = "fetched"
)

// orderBy maps a sort onto SQL.
//
// A map of fixed fragments, not string interpolation: this is the one place a
// caller's input reaches the query text, and the only safe way to let it is for
// the input to select a clause rather than become one.
var orderBy = map[DashboardSort]string{
	SortViews:       `v.latest_view_count DESC NULLS LAST, v.id`,
	SortGained:      `views_gained DESC NULLS LAST, v.id`,
	SortRecent:      `t.created_at DESC, v.id`,
	SortTitle:       `coalesce(nullif(t.label, ''), v.title, v.canonical_url) ASC, v.id`,
	SortLastFetched: `v.latest_captured_at DESC NULLS LAST, v.id`,
}

// DashboardQuery selects and orders a user's tracked videos.
type DashboardQuery struct {
	UserID int64

	// Window is how far back the ViewsGained baseline is taken from.
	Window time.Duration

	// Platform filters to one platform when set.
	Platform domain.Platform

	Sort   DashboardSort
	Limit  int
	Offset int
}

// Dashboard returns a user's tracked videos with their current counters and
// growth over the requested window.
//
// This is the hot path, and the schema is shaped for it. The counters come from
// the denormalised latest_* columns, so the query does not touch the time
// series once per video; only the baseline does, through a LATERAL that rides
// the (video_id, captured_at) primary key.
//
// The join is LEFT on purpose. An INNER join would silently drop every video
// added more recently than the window from the user's own dashboard — the exact
// videos they are most likely to be watching — and it would look like the
// tracking had failed rather than like the delta was unknown.
func (r *TrackingRepo) Dashboard(ctx context.Context, q DashboardQuery) ([]DashboardEntry, error) {
	window := q.Window
	if window <= 0 {
		window = 7 * 24 * time.Hour
	}
	limit := q.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	order, ok := orderBy[q.Sort]
	if !ok {
		order = orderBy[SortViews]
	}

	sql := `
		SELECT ` + videoColumns + `,
		       t.label, t.created_at,
		       v.latest_view_count - baseline.view_count AS views_gained,
		       baseline.captured_at
		FROM tracked_videos t
		JOIN videos v ON v.id = t.video_id
		LEFT JOIN LATERAL (
			SELECT s.view_count, s.captured_at
			FROM metric_snapshots s
			WHERE s.video_id = v.id
			  AND s.captured_at <= now() - $2::interval
			ORDER BY s.captured_at DESC
			LIMIT 1
		) baseline ON true
		WHERE t.user_id = $1
		  AND t.archived_at IS NULL
		  AND ($3 = '' OR v.platform = $3)
		ORDER BY ` + order + `
		LIMIT $4 OFFSET $5`

	rows, err := r.db.Pool.Query(ctx, sql,
		q.UserID, intervalArg(window), string(q.Platform), limit, max(q.Offset, 0))
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	var out []DashboardEntry
	for rows.Next() {
		entry, err := scanDashboardEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, translate(rows.Err())
}

// scanDashboardEntry reads the shared video projection plus the four columns
// the dashboard query appends, in one pass.
func scanDashboardEntry(rows pgx.Rows) (DashboardEntry, error) {
	var (
		vs         videoScan
		label      string
		addedAt    time.Time
		gained     *int64
		baselineAt *time.Time
	)

	dests := append(vs.dests(), &label, &addedAt, &gained, &baselineAt)
	if err := rows.Scan(dests...); err != nil {
		return DashboardEntry{}, translate(err)
	}

	return DashboardEntry{
		Video:       vs.video(),
		Label:       label,
		AddedAt:     addedAt,
		ViewsGained: gained,
		BaselineAt:  baselineAt,
	}, nil
}

// CountForUser reports how many videos a user actively tracks.
func (r *TrackingRepo) CountForUser(ctx context.Context, userID int64) (int, error) {
	const q = `SELECT count(*) FROM tracked_videos WHERE user_id = $1 AND archived_at IS NULL`

	var n int
	if err := r.db.Pool.QueryRow(ctx, q, userID).Scan(&n); err != nil {
		return 0, translate(err)
	}
	return n, nil
}

// Summary is the headline block on the dashboard.
type Summary struct {
	TrackedVideos int               `json:"tracked_videos"`
	ByPlatform    map[string]int    `json:"by_platform"`
	TotalViews    int64             `json:"total_views"`
	ViewsGained   int64             `json:"views_gained"`
	Stale         int               `json:"stale"`
	Unavailable   int               `json:"unavailable"`
	Window        string            `json:"window"`
	Platforms     []domain.Platform `json:"-"`
}

// Summarise aggregates a user's tracked videos over a window.
//
// Stale counts videos whose newest reading is older than twice their own fetch
// interval — a per-video threshold rather than a fixed one, because a
// six-hourly YouTube video and a twelve-hourly TikTok video go stale at
// different speeds and a single cutoff would misreport one of them.
func (r *TrackingRepo) Summarise(ctx context.Context, userID int64, window time.Duration) (*Summary, error) {
	if window <= 0 {
		window = 7 * 24 * time.Hour
	}

	const q = `
		WITH tracked AS (
			SELECT v.*
			FROM tracked_videos t
			JOIN videos v ON v.id = t.video_id
			WHERE t.user_id = $1 AND t.archived_at IS NULL
		),
		gains AS (
			SELECT tr.id,
			       tr.latest_view_count - baseline.view_count AS gained
			FROM tracked tr
			LEFT JOIN LATERAL (
				SELECT s.view_count
				FROM metric_snapshots s
				WHERE s.video_id = tr.id AND s.captured_at <= now() - $2::interval
				ORDER BY s.captured_at DESC
				LIMIT 1
			) baseline ON true
		)
		SELECT
			(SELECT count(*) FROM tracked),
			(SELECT coalesce(sum(latest_view_count), 0) FROM tracked),
			(SELECT coalesce(sum(gained), 0) FROM gains),
			(SELECT count(*) FROM tracked
			  WHERE unavailable_since IS NULL
			    AND (latest_captured_at IS NULL
			         OR latest_captured_at < now() - (fetch_interval * 2))),
			(SELECT count(*) FROM tracked WHERE unavailable_since IS NOT NULL)`

	s := &Summary{ByPlatform: map[string]int{}, Window: window.String()}
	err := r.db.Pool.QueryRow(ctx, q, userID, intervalArg(window)).
		Scan(&s.TrackedVideos, &s.TotalViews, &s.ViewsGained, &s.Stale, &s.Unavailable)
	if err != nil {
		return nil, translate(err)
	}

	const byPlatform = `
		SELECT v.platform, count(*)
		FROM tracked_videos t
		JOIN videos v ON v.id = t.video_id
		WHERE t.user_id = $1 AND t.archived_at IS NULL
		GROUP BY v.platform
		ORDER BY v.platform`

	rows, err := r.db.Pool.Query(ctx, byPlatform, userID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	for rows.Next() {
		var platform string
		var n int
		if err := rows.Scan(&platform, &n); err != nil {
			return nil, translate(err)
		}
		s.ByPlatform[platform] = n
		s.Platforms = append(s.Platforms, domain.Platform(platform))
	}
	return s, translate(rows.Err())
}
