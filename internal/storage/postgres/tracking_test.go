package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/foodibd/socialstats/internal/domain"
)

// seedSnapshot writes a reading directly, so tests can build a history without
// pretending to run the poller.
func seedSnapshot(t *testing.T, db *DB, ctx context.Context, videoID int64, at time.Time, views uint64) {
	t.Helper()
	if _, err := db.Metrics().EnsureSnapshotPartition(ctx, at); err != nil {
		t.Fatalf("ensure partition: %v", err)
	}
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO metric_snapshots (video_id, captured_at, view_count)
		VALUES ($1, $2, $3)
		ON CONFLICT (video_id, captured_at) DO UPDATE SET view_count = excluded.view_count`,
		videoID, at, int64(views))
	if err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
}

func setLatest(t *testing.T, db *DB, ctx context.Context, videoID int64, at time.Time, views uint64) {
	t.Helper()
	_, err := db.Pool.Exec(ctx,
		`UPDATE videos SET latest_view_count = $2, latest_captured_at = $3 WHERE id = $1`,
		videoID, int64(views), at)
	if err != nil {
		t.Fatalf("set latest: %v", err)
	}
}

// ---------- tracking ----------

func TestTrackBringsAVideoIntoTheRotation(t *testing.T) {
	db, ctx := migrated(t)
	u := makeUser(t, db, ctx, "trackr@example.com")
	v := makeVideo(t, db, ctx, domain.PlatformYouTube, "t1")

	if v.Schedule.NextFetchAt != nil || v.Schedule.TrackerCount != 0 {
		t.Fatalf("a fresh video should be untracked and unscheduled: %+v", v.Schedule)
	}

	tracked, err := db.Tracking().Track(ctx, u.ID, v.ID, "My reel")
	if err != nil {
		t.Fatalf("Track: %v", err)
	}
	if tracked.Label != "My reel" {
		t.Errorf("label = %q", tracked.Label)
	}

	got, _ := db.Videos().ByID(ctx, v.ID)
	if got.Schedule.TrackerCount != 1 {
		t.Errorf("tracker count = %d, want 1", got.Schedule.TrackerCount)
	}
	// Scheduled immediately, so the first reading does not wait a whole
	// interval before the user sees anything.
	if got.Schedule.NextFetchAt == nil {
		t.Fatal("tracking a video did not schedule it")
	}
	if got.Schedule.NextFetchAt.After(time.Now().Add(time.Second)) {
		t.Errorf("first fetch scheduled for %v, want immediately", got.Schedule.NextFetchAt)
	}
}

// The fetch is shared, so the second person to track a video costs no extra
// upstream requests — the point of separating videos from tracked_videos.
func TestASharedVideoIsPolledOnce(t *testing.T) {
	db, ctx := migrated(t)
	alice := makeUser(t, db, ctx, "alice2@example.com")
	bob := makeUser(t, db, ctx, "bob2@example.com")
	v := makeVideo(t, db, ctx, domain.PlatformTikTok, "shared")

	for _, u := range []*domain.User{alice, bob} {
		if _, err := db.Tracking().Track(ctx, u.ID, v.ID, ""); err != nil {
			t.Fatalf("Track: %v", err)
		}
	}

	got, _ := db.Videos().ByID(ctx, v.ID)
	if got.Schedule.TrackerCount != 2 {
		t.Errorf("tracker count = %d, want 2", got.Schedule.TrackerCount)
	}

	claimed, err := db.Videos().ClaimDue(ctx, domain.PlatformTikTok, 10, time.Minute)
	if err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	if len(claimed) != 1 {
		t.Errorf("claimed %d rows for a video two people track, want 1", len(claimed))
	}

	// Alice leaving must not stop polling for Bob.
	if err := db.Tracking().Untrack(ctx, alice.ID, v.ID); err != nil {
		t.Fatalf("Untrack: %v", err)
	}
	got, _ = db.Videos().ByID(ctx, v.ID)
	if got.Schedule.TrackerCount != 1 {
		t.Errorf("tracker count = %d after one of two left, want 1", got.Schedule.TrackerCount)
	}
	if got.Schedule.NextFetchAt == nil {
		t.Error("polling stopped while a second user was still tracking")
	}

	// The last tracker leaving does stop it.
	if err := db.Tracking().Untrack(ctx, bob.ID, v.ID); err != nil {
		t.Fatalf("Untrack: %v", err)
	}
	got, _ = db.Videos().ByID(ctx, v.ID)
	if got.Schedule.TrackerCount != 0 || got.Schedule.NextFetchAt != nil {
		t.Errorf("video still polled with no trackers: %+v", got.Schedule)
	}
}

// Untracking archives rather than deletes, so re-adding restores the label and
// the history rather than starting over.
func TestUntrackThenRetrackRestores(t *testing.T) {
	db, ctx := migrated(t)
	u := makeUser(t, db, ctx, "again@example.com")
	v := makeVideo(t, db, ctx, domain.PlatformYouTube, "again")

	first, err := db.Tracking().Track(ctx, u.ID, v.ID, "Original label")
	if err != nil {
		t.Fatalf("Track: %v", err)
	}
	if err := db.Tracking().Untrack(ctx, u.ID, v.ID); err != nil {
		t.Fatalf("Untrack: %v", err)
	}
	if _, err := db.Tracking().Get(ctx, u.ID, v.ID); !errors.Is(err, domain.ErrRecordNotFound) {
		t.Errorf("archived tracking still visible: %v", err)
	}

	// Re-tracking with no label keeps the one the user chose before.
	again, err := db.Tracking().Track(ctx, u.ID, v.ID, "")
	if err != nil {
		t.Fatalf("re-Track: %v", err)
	}
	if again.Label != "Original label" {
		t.Errorf("label = %q, want the original preserved", again.Label)
	}
	if !again.AddedAt.Equal(first.AddedAt) {
		t.Errorf("added_at moved from %v to %v", first.AddedAt, again.AddedAt)
	}
	if again.Archived != nil {
		t.Error("re-tracked row is still archived")
	}
}

func TestUntrackingSomethingUntracked(t *testing.T) {
	db, ctx := migrated(t)
	u := makeUser(t, db, ctx, "noop@example.com")
	v := makeVideo(t, db, ctx, domain.PlatformYouTube, "noop")

	if err := db.Tracking().Untrack(ctx, u.ID, v.ID); !errors.Is(err, domain.ErrRecordNotFound) {
		t.Errorf("error = %v, want ErrRecordNotFound", err)
	}
}

// tracker_count is recomputed from the rows rather than incremented, so it
// cannot drift — and any drift that somehow exists repairs itself.
func TestTrackerCountSelfHeals(t *testing.T) {
	db, ctx := migrated(t)
	u := makeUser(t, db, ctx, "drift@example.com")
	v := makeVideo(t, db, ctx, domain.PlatformYouTube, "drift")

	if _, err := db.Tracking().Track(ctx, u.ID, v.ID, ""); err != nil {
		t.Fatalf("Track: %v", err)
	}
	// Corrupt the counter behind the repository's back.
	if _, err := db.Pool.Exec(ctx, `UPDATE videos SET tracker_count = 47 WHERE id = $1`, v.ID); err != nil {
		t.Fatalf("corrupt: %v", err)
	}

	other := makeUser(t, db, ctx, "drift2@example.com")
	if _, err := db.Tracking().Track(ctx, other.ID, v.ID, ""); err != nil {
		t.Fatalf("Track: %v", err)
	}

	got, _ := db.Videos().ByID(ctx, v.ID)
	if got.Schedule.TrackerCount != 2 {
		t.Errorf("tracker count = %d, want the recomputed 2 rather than 48", got.Schedule.TrackerCount)
	}
}

func TestTrackingUpdateAndCount(t *testing.T) {
	db, ctx := migrated(t)
	u := makeUser(t, db, ctx, "labels@example.com")
	v := makeVideo(t, db, ctx, domain.PlatformYouTube, "lbl")

	if _, err := db.Tracking().Track(ctx, u.ID, v.ID, "first"); err != nil {
		t.Fatalf("Track: %v", err)
	}
	updated, err := db.Tracking().Update(ctx, u.ID, v.ID, "second", "some notes")
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Label != "second" || updated.Notes != "some notes" {
		t.Errorf("updated = %+v", updated)
	}

	n, err := db.Tracking().CountForUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("CountForUser: %v", err)
	}
	if n != 1 {
		t.Errorf("count = %d, want 1", n)
	}
}

// ---------- the dashboard query ----------

func TestDashboardComputesGrowthAgainstABaseline(t *testing.T) {
	db, ctx := migrated(t)
	u := makeUser(t, db, ctx, "dash@example.com")
	v := makeVideo(t, db, ctx, domain.PlatformYouTube, "growth")
	if _, err := db.Tracking().Track(ctx, u.ID, v.ID, "Growing"); err != nil {
		t.Fatalf("Track: %v", err)
	}

	now := time.Now().UTC()
	seedSnapshot(t, db, ctx, v.ID, now.Add(-10*24*time.Hour), 100_000)
	seedSnapshot(t, db, ctx, v.ID, now.Add(-8*24*time.Hour), 150_000)
	seedSnapshot(t, db, ctx, v.ID, now.Add(-time.Hour), 220_000)
	setLatest(t, db, ctx, v.ID, now.Add(-time.Hour), 220_000)

	entries, err := db.Tracking().Dashboard(ctx, DashboardQuery{
		UserID: u.ID, Window: 7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("Dashboard: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("%d entries, want 1", len(entries))
	}

	e := entries[0]
	if e.Label != "Growing" {
		t.Errorf("label = %q", e.Label)
	}
	// The baseline is the last reading at or before the window's start, which
	// is the 8-day-old one, not the 10-day-old one.
	if e.ViewsGained == nil || *e.ViewsGained != 70_000 {
		t.Errorf("views gained = %v, want 70000 (220000 - 150000)", e.ViewsGained)
	}
	if e.BaselineAt == nil {
		t.Error("baseline timestamp missing; a delta with no reference point is uninterpretable")
	}
	if e.Video.Latest.ViewCount == nil || *e.Video.Latest.ViewCount != 220_000 {
		t.Errorf("latest = %v", e.Video.Latest.ViewCount)
	}
}

// The LEFT JOIN is what keeps newly added videos on the dashboard. An inner
// join would silently drop exactly the videos a user is most likely watching.
func TestDashboardKeepsVideosWithNoBaseline(t *testing.T) {
	db, ctx := migrated(t)
	u := makeUser(t, db, ctx, "fresh@example.com")
	v := makeVideo(t, db, ctx, domain.PlatformTikTok, "brandnew")
	if _, err := db.Tracking().Track(ctx, u.ID, v.ID, ""); err != nil {
		t.Fatalf("Track: %v", err)
	}

	now := time.Now().UTC()
	seedSnapshot(t, db, ctx, v.ID, now.Add(-time.Hour), 5_000)
	setLatest(t, db, ctx, v.ID, now.Add(-time.Hour), 5_000)

	entries, err := db.Tracking().Dashboard(ctx, DashboardQuery{UserID: u.ID, Window: 7 * 24 * time.Hour})
	if err != nil {
		t.Fatalf("Dashboard: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("%d entries, want the video kept despite having no week-old baseline", len(entries))
	}
	if entries[0].ViewsGained != nil {
		t.Errorf("views gained = %v, want nil when there is no baseline", *entries[0].ViewsGained)
	}
	if entries[0].Video.Latest.ViewCount == nil {
		t.Error("current counters missing")
	}
}

// Counters are not monotonic: platforms revise them downward, so a negative
// delta is a real measurement and must survive to the caller.
func TestDashboardReportsNegativeGrowth(t *testing.T) {
	db, ctx := migrated(t)
	u := makeUser(t, db, ctx, "shrink@example.com")
	v := makeVideo(t, db, ctx, domain.PlatformTikTok, "purged")
	if _, err := db.Tracking().Track(ctx, u.ID, v.ID, ""); err != nil {
		t.Fatalf("Track: %v", err)
	}

	now := time.Now().UTC()
	seedSnapshot(t, db, ctx, v.ID, now.Add(-8*24*time.Hour), 900_000)
	setLatest(t, db, ctx, v.ID, now, 850_000)

	entries, err := db.Tracking().Dashboard(ctx, DashboardQuery{UserID: u.ID, Window: 7 * 24 * time.Hour})
	if err != nil {
		t.Fatalf("Dashboard: %v", err)
	}
	if len(entries) != 1 || entries[0].ViewsGained == nil {
		t.Fatalf("entries = %+v", entries)
	}
	if *entries[0].ViewsGained != -50_000 {
		t.Errorf("views gained = %d, want -50000", *entries[0].ViewsGained)
	}
}

func TestDashboardFiltersSortsAndPages(t *testing.T) {
	db, ctx := migrated(t)
	u := makeUser(t, db, ctx, "sort@example.com")
	other := makeUser(t, db, ctx, "someone-else@example.com")

	now := time.Now().UTC()
	type seed struct {
		platform domain.Platform
		id       string
		views    uint64
	}
	for _, s := range []seed{
		{domain.PlatformYouTube, "yt-big", 900},
		{domain.PlatformYouTube, "yt-small", 100},
		{domain.PlatformTikTok, "tt-mid", 500},
	} {
		v := makeVideo(t, db, ctx, s.platform, s.id)
		if _, err := db.Tracking().Track(ctx, u.ID, v.ID, ""); err != nil {
			t.Fatalf("Track: %v", err)
		}
		setLatest(t, db, ctx, v.ID, now, s.views)
	}

	// Another user's video must never appear on this user's dashboard.
	theirs := makeVideo(t, db, ctx, domain.PlatformYouTube, "not-mine")
	if _, err := db.Tracking().Track(ctx, other.ID, theirs.ID, ""); err != nil {
		t.Fatalf("Track: %v", err)
	}

	all, err := db.Tracking().Dashboard(ctx, DashboardQuery{UserID: u.ID, Sort: SortViews})
	if err != nil {
		t.Fatalf("Dashboard: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("%d entries, want 3 (and none of the other user's)", len(all))
	}
	if *all[0].Video.Latest.ViewCount != 900 || *all[2].Video.Latest.ViewCount != 100 {
		t.Errorf("not sorted by views descending: %v", viewList(all))
	}

	byPlatform, err := db.Tracking().Dashboard(ctx, DashboardQuery{UserID: u.ID, Platform: domain.PlatformTikTok})
	if err != nil {
		t.Fatalf("Dashboard: %v", err)
	}
	if len(byPlatform) != 1 || byPlatform[0].Video.Platform != domain.PlatformTikTok {
		t.Errorf("platform filter returned %v", viewList(byPlatform))
	}

	page, err := db.Tracking().Dashboard(ctx, DashboardQuery{UserID: u.ID, Sort: SortViews, Limit: 2, Offset: 1})
	if err != nil {
		t.Fatalf("Dashboard: %v", err)
	}
	if len(page) != 2 || *page[0].Video.Latest.ViewCount != 500 {
		t.Errorf("paged result = %v", viewList(page))
	}
}

// An unrecognised sort falls back to the default rather than reaching the
// query text: the sort names a clause, it never becomes one.
func TestDashboardRejectsAnUnknownSort(t *testing.T) {
	db, ctx := migrated(t)
	u := makeUser(t, db, ctx, "inject@example.com")

	_, err := db.Tracking().Dashboard(ctx, DashboardQuery{
		UserID: u.ID,
		Sort:   DashboardSort("v.id; DROP TABLE users; --"),
	})
	if err != nil {
		t.Fatalf("Dashboard: %v", err)
	}

	var n int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
		t.Fatalf("users table is gone: %v", err)
	}
}

func viewList(entries []DashboardEntry) []uint64 {
	out := make([]uint64, 0, len(entries))
	for _, e := range entries {
		if e.Video.Latest.ViewCount != nil {
			out = append(out, *e.Video.Latest.ViewCount)
		} else {
			out = append(out, 0)
		}
	}
	return out
}

func TestSummarise(t *testing.T) {
	db, ctx := migrated(t)
	u := makeUser(t, db, ctx, "summary@example.com")
	now := time.Now().UTC()

	yt := makeVideo(t, db, ctx, domain.PlatformYouTube, "s-yt")
	tt := makeVideo(t, db, ctx, domain.PlatformTikTok, "s-tt")
	for _, v := range []*domain.Video{yt, tt} {
		if _, err := db.Tracking().Track(ctx, u.ID, v.ID, ""); err != nil {
			t.Fatalf("Track: %v", err)
		}
	}

	seedSnapshot(t, db, ctx, yt.ID, now.Add(-8*24*time.Hour), 1_000)
	setLatest(t, db, ctx, yt.ID, now, 1_500)
	setLatest(t, db, ctx, tt.ID, now, 400)

	s, err := db.Tracking().Summarise(ctx, u.ID, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("Summarise: %v", err)
	}
	if s.TrackedVideos != 2 {
		t.Errorf("tracked = %d, want 2", s.TrackedVideos)
	}
	if s.TotalViews != 1_900 {
		t.Errorf("total views = %d, want 1900", s.TotalViews)
	}
	if s.ViewsGained != 500 {
		t.Errorf("views gained = %d, want 500", s.ViewsGained)
	}
	if s.ByPlatform["youtube"] != 1 || s.ByPlatform["tiktok"] != 1 {
		t.Errorf("by platform = %v", s.ByPlatform)
	}
}

// Staleness is measured against each video's own interval, because a 6-hourly
// YouTube video and a 12-hourly TikTok one go stale at different speeds.
func TestSummariseCountsStaleAgainstEachVideosInterval(t *testing.T) {
	db, ctx := migrated(t)
	u := makeUser(t, db, ctx, "stale@example.com")
	now := time.Now().UTC()

	fast := makeVideo(t, db, ctx, domain.PlatformYouTube, "fast")
	slow := makeVideo(t, db, ctx, domain.PlatformTikTok, "slow")
	for _, v := range []*domain.Video{fast, slow} {
		if _, err := db.Tracking().Track(ctx, u.ID, v.ID, ""); err != nil {
			t.Fatalf("Track: %v", err)
		}
	}

	if _, err := db.Pool.Exec(ctx,
		`UPDATE videos SET fetch_interval = interval '1 hour' WHERE id = $1`, fast.ID); err != nil {
		t.Fatalf("set interval: %v", err)
	}
	if _, err := db.Pool.Exec(ctx,
		`UPDATE videos SET fetch_interval = interval '24 hours' WHERE id = $1`, slow.ID); err != nil {
		t.Fatalf("set interval: %v", err)
	}

	// Both last read 5 hours ago: stale for the hourly one, fine for the daily.
	setLatest(t, db, ctx, fast.ID, now.Add(-5*time.Hour), 10)
	setLatest(t, db, ctx, slow.ID, now.Add(-5*time.Hour), 10)

	s, err := db.Tracking().Summarise(ctx, u.ID, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("Summarise: %v", err)
	}
	if s.Stale != 1 {
		t.Errorf("stale = %d, want 1: a fixed cutoff would misreport one of the two", s.Stale)
	}
}

func TestDeletingAUserLeavesSharedVideosAlone(t *testing.T) {
	db, ctx := migrated(t)
	alice := makeUser(t, db, ctx, "leaving@example.com")
	bob := makeUser(t, db, ctx, "staying@example.com")
	v := makeVideo(t, db, ctx, domain.PlatformYouTube, "shared2")

	for _, u := range []*domain.User{alice, bob} {
		if _, err := db.Tracking().Track(ctx, u.ID, v.ID, ""); err != nil {
			t.Fatalf("Track: %v", err)
		}
	}
	seedSnapshot(t, db, ctx, v.ID, time.Now().UTC().Add(-time.Hour), 42)

	if err := db.Users().Delete(ctx, alice.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := db.Videos().ByID(ctx, v.ID); err != nil {
		t.Fatalf("the shared video went with the deleted user: %v", err)
	}
	history, _ := db.Metrics().History(ctx, v.ID, time.Now().Add(-24*time.Hour), time.Now(), BucketRaw)
	if len(history) != 1 {
		t.Errorf("history lost: %d snapshots", len(history))
	}

	entries, err := db.Tracking().Dashboard(ctx, DashboardQuery{UserID: bob.ID})
	if err != nil {
		t.Fatalf("Dashboard: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("bob's dashboard has %d entries, want 1", len(entries))
	}
}

// videoColumns is projected by hand in several queries; the count it is scanned
// with must match, or every read silently shifts by a column.
func TestVideoColumnCountMatchesTheProjection(t *testing.T) {
	db, ctx := migrated(t)

	rows, err := db.Pool.Query(ctx, `SELECT `+videoColumns+` FROM videos v LIMIT 0`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	if got := len(rows.FieldDescriptions()); got != videoColumnCount {
		t.Errorf("videoColumns projects %d columns, videoColumnCount says %d", got, videoColumnCount)
	}
	var s videoScan
	if got := len(s.dests()); got != videoColumnCount {
		t.Errorf("videoScan has %d destinations, want %d", got, videoColumnCount)
	}
}

func TestVideoReturningMatchesVideoColumns(t *testing.T) {
	db, ctx := migrated(t)

	rows, err := db.Pool.Query(ctx, `SELECT `+videoReturning+` FROM videos LIMIT 0`)
	if err != nil {
		t.Fatalf("videoReturning is not valid without an alias: %v", err)
	}
	defer rows.Close()

	if got := len(rows.FieldDescriptions()); got != videoColumnCount {
		t.Errorf("videoReturning projects %d columns, want %d", got, videoColumnCount)
	}
	_ = fmt.Sprint()
}
