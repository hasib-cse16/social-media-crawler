package tracking

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/foodibd/socialstats/internal/config"
	"github.com/foodibd/socialstats/internal/domain"
	"github.com/foodibd/socialstats/internal/storage/postgres"
	"github.com/foodibd/socialstats/internal/storage/postgres/pgtest"
)

// The service is exercised against a real database and a fake provider. The
// database is real because the dashboard's growth figures come out of a LATERAL
// join whose behaviour is the thing under test; the provider is fake because
// the point here is what we do with a fetch result, not how it was obtained.

// fakeProvider stands in for a platform.
type fakeProvider struct {
	platform domain.Platform
	stats    *domain.VideoStats
	err      error
	short    bool
	calls    atomic.Int32
}

func (f *fakeProvider) Platform() domain.Platform { return f.platform }
func (f *fakeProvider) Match(string) bool         { return true }

func (f *fakeProvider) Identify(rawURL string) (domain.VideoRef, error) {
	if f.short {
		return domain.VideoRef{}, fmt.Errorf("%w: %s", domain.ErrNeedsResolution, rawURL)
	}
	return domain.VideoRef{
		Platform:     f.platform,
		VideoID:      "vid-" + rawURL[len(rawURL)-3:],
		CanonicalURL: rawURL,
	}, nil
}

func (f *fakeProvider) Stats(_ context.Context, rawURL string) (*domain.VideoStats, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	// Derived from the URL rather than fixed, so two different URLs do not
	// collapse onto one video row and quietly make tracking look idempotent
	// when it is not.
	stats := *f.stats
	if !f.short {
		ref, _ := f.Identify(rawURL)
		stats.VideoID = ref.VideoID
		stats.CanonicalURL = ref.CanonicalURL
	}
	return &stats, nil
}

type fakeResolver struct {
	p   *fakeProvider
	err error
}

func (r fakeResolver) For(string) (domain.Provider, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.p, nil
}
func (r fakeResolver) Platforms() []domain.Platform { return []domain.Platform{r.p.platform} }

// seedSnapshot writes a reading directly, creating its partition first.
//
// Production only ever writes "now", and the migration keeps the current month
// plus two ahead. Tests write backwards — a fixture at now-20h crosses into last
// month on the first of any month — so they have to ensure the partition
// themselves rather than relying on the forward-looking set.
func seedSnapshot(t *testing.T, db *postgres.DB, ctx context.Context, videoID int64, at time.Time, views int64) {
	t.Helper()
	if _, err := db.Metrics().EnsureSnapshotPartition(ctx, at); err != nil {
		t.Fatalf("ensure partition for %s: %v", at.Format(time.RFC3339), err)
	}
	if _, err := db.Pool.Exec(ctx,
		`INSERT INTO metric_snapshots (video_id, captured_at, view_count) VALUES ($1, $2, $3)
		 ON CONFLICT (video_id, captured_at) DO UPDATE SET view_count = excluded.view_count`,
		videoID, at, views); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
}

func okStats(id string, views uint64) *domain.VideoStats {
	return &domain.VideoStats{
		Platform:     domain.PlatformYouTube,
		VideoID:      id,
		CanonicalURL: "https://www.youtube.com/watch?v=" + id,
		Title:        "A video",
		ChannelTitle: "A channel",
		ViewCount:    domain.U64(views),
		LikeCount:    domain.U64(views / 10),
		FetchedAt:    time.Now().UTC(),
	}
}

func newService(t *testing.T, p *fakeProvider, mutate func(*Config)) (*Service, *postgres.DB, context.Context, int64) {
	t.Helper()

	ctx := context.Background()
	db, err := postgres.Connect(ctx, config.DatabaseConfig{
		URL: pgtest.URL(t), MaxConns: 8, MinConns: 1,
		MaxConnLifetime: time.Minute, ConnectTimeout: 5 * time.Second,
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	user, err := db.Users().Create(ctx, domain.NewUser{Email: "user@example.com", PasswordHash: "x"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	cfg := Config{MaxTrackedPerUser: 10, Policy: testPolicy, AddTimeout: 5 * time.Second}
	if mutate != nil {
		mutate(&cfg)
	}

	svc := NewService(db.Videos(), db.Tracking(), db.Metrics(), fakeResolver{p: p}, cfg, slog.New(slog.DiscardHandler))
	return svc, db, ctx, user.ID
}

func TestAddFetchesImmediately(t *testing.T) {
	p := &fakeProvider{platform: domain.PlatformYouTube, stats: okStats("dQw4w9WgXcQ", 220_500)}
	svc, db, ctx, userID := newService(t, p, nil)

	entry, err := svc.Add(ctx, userID, "https://youtu.be/dQw4w9WgXcQ", "My video")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// The point of the synchronous fetch: the user sees a number, not an empty
	// row that fills in some time in the next six hours.
	if entry.Video.Latest.ViewCount == nil || *entry.Video.Latest.ViewCount != 220_500 {
		t.Errorf("view count = %v, want it present immediately", entry.Video.Latest.ViewCount)
	}
	if entry.Label != "My video" {
		t.Errorf("label = %q", entry.Label)
	}
	if entry.Video.Title != "A video" {
		t.Errorf("title = %q, want the fetched metadata applied", entry.Video.Title)
	}
	if !entry.Fresh {
		t.Error("a just-fetched video is not marked fresh")
	}
	if p.calls.Load() != 1 {
		t.Errorf("provider called %d times, want 1", p.calls.Load())
	}

	// A snapshot and an audit row were written too.
	history, err := db.Metrics().History(ctx, entry.Video.ID,
		time.Now().Add(-time.Hour), time.Now().Add(time.Hour), postgres.BucketRaw)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(history) != 1 {
		t.Errorf("%d snapshots after Add, want 1", len(history))
	}
	attempts, _ := db.Metrics().Attempts(ctx, entry.Video.ID, 10)
	if len(attempts) != 1 || attempts[0].Status != domain.AttemptOK {
		t.Errorf("attempts = %+v", attempts)
	}
}

// A TikTok challenge at 14:03 is not a reason to refuse to track a video.
func TestAddTracksEvenWhenTheFirstFetchFails(t *testing.T) {
	p := &fakeProvider{platform: domain.PlatformYouTube, err: domain.ErrBlocked}
	svc, db, ctx, userID := newService(t, p, nil)

	entry, err := svc.Add(ctx, userID, "https://youtu.be/dQw4w9WgXcQ", "")
	if err != nil {
		t.Fatalf("Add should succeed despite a failed fetch: %v", err)
	}

	if entry.Video.ID == 0 {
		t.Fatal("no video was created")
	}
	if entry.Video.PlatformVideoID != "vid-XcQ" {
		t.Errorf("video id = %q, want the one Identify produced without a fetch", entry.Video.PlatformVideoID)
	}
	if entry.Video.Latest.ViewCount != nil {
		t.Error("a failed fetch invented a view count")
	}
	if entry.Fresh {
		t.Error("a video that has never been read is marked fresh")
	}
	if entry.Video.Schedule.LastFetchStatus != domain.FetchBlocked {
		t.Errorf("status = %q, want the failure recorded on the row", entry.Video.Schedule.LastFetchStatus)
	}
	if entry.Video.Schedule.Retired() {
		t.Error("a single block retired the video")
	}

	// The failure is in the audit trail, which is where "why is this stale?"
	// gets answered.
	attempts, _ := db.Metrics().Attempts(ctx, entry.Video.ID, 10)
	if len(attempts) != 1 || attempts[0].Status != domain.AttemptBlocked {
		t.Errorf("attempts = %+v, want one blocked attempt", attempts)
	}

	// It is still scheduled, so the poller will try again.
	if entry.Video.Schedule.NextFetchAt == nil {
		t.Error("a blocked video was left unscheduled")
	}
}

// A short link has no id until the redirect is followed, so a failed fetch
// means we genuinely do not know which video was meant.
func TestAddRefusesAnUnresolvableShortLink(t *testing.T) {
	p := &fakeProvider{platform: domain.PlatformTikTok, short: true, err: domain.ErrBlocked}
	svc, _, ctx, userID := newService(t, p, nil)

	_, err := svc.Add(ctx, userID, "https://vm.tiktok.com/ZTRabcdef/", "")
	if err == nil {
		t.Fatal("Add accepted a short link it could not resolve")
	}
	if !errors.Is(err, domain.ErrBlocked) {
		t.Errorf("error = %v, want the underlying fetch failure", err)
	}
}

// A short link that does resolve is tracked under the identity the fetch
// returned, not the URL that was pasted.
func TestAddResolvesAShortLinkThroughTheFetch(t *testing.T) {
	stats := okStats("7249376077976472833", 1_000)
	stats.Platform = domain.PlatformTikTok
	stats.CanonicalURL = "https://www.tiktok.com/@user/video/7249376077976472833"

	p := &fakeProvider{platform: domain.PlatformTikTok, short: true, stats: stats}
	svc, _, ctx, userID := newService(t, p, nil)

	entry, err := svc.Add(ctx, userID, "https://vm.tiktok.com/ZTRabcdef/", "")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if entry.Video.PlatformVideoID != "7249376077976472833" {
		t.Errorf("video id = %q, want the resolved one", entry.Video.PlatformVideoID)
	}
	if entry.Video.CanonicalURL != stats.CanonicalURL {
		t.Errorf("canonical url = %q, want the resolved form rather than the short link", entry.Video.CanonicalURL)
	}
}

// Two users adding the same video must share one row, and therefore one fetch.
func TestAddingTheSameVideoTwiceSharesOneRow(t *testing.T) {
	p := &fakeProvider{platform: domain.PlatformYouTube, stats: okStats("dQw4w9WgXcQ", 100)}
	svc, db, ctx, userID := newService(t, p, nil)

	first, err := svc.Add(ctx, userID, "https://youtu.be/dQw4w9WgXcQ", "mine")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	other, err := db.Users().Create(ctx, domain.NewUser{Email: "other@example.com", PasswordHash: "x"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	second, err := svc.Add(ctx, other.ID, "https://youtu.be/dQw4w9WgXcQ", "theirs")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	if first.Video.ID != second.Video.ID {
		t.Errorf("two rows (%d, %d) for one video", first.Video.ID, second.Video.ID)
	}
	if first.Label == second.Label {
		t.Error("the label is shared between users; it should be per-tracking")
	}

	var count int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM videos`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("%d video rows, want 1", count)
	}
}

func TestAddRejects(t *testing.T) {
	t.Run("empty url", func(t *testing.T) {
		p := &fakeProvider{platform: domain.PlatformYouTube, stats: okStats("a", 1)}
		svc, _, ctx, userID := newService(t, p, nil)
		if _, err := svc.Add(ctx, userID, "   ", ""); !errors.Is(err, domain.ErrInvalidURL) {
			t.Errorf("error = %v, want ErrInvalidURL", err)
		}
	})

	t.Run("unsupported platform", func(t *testing.T) {
		p := &fakeProvider{platform: domain.PlatformYouTube}
		svc, _, ctx, userID := newService(t, p, nil)
		svc.resolver = fakeResolver{p: p, err: domain.ErrUnsupported}
		if _, err := svc.Add(ctx, userID, "https://example.com/x", ""); !errors.Is(err, domain.ErrUnsupported) {
			t.Errorf("error = %v, want ErrUnsupported", err)
		}
	})

	t.Run("over the per-user limit", func(t *testing.T) {
		p := &fakeProvider{platform: domain.PlatformYouTube, stats: okStats("a", 1)}
		svc, _, ctx, userID := newService(t, p, func(c *Config) { c.MaxTrackedPerUser = 2 })

		for i := range 2 {
			if _, err := svc.Add(ctx, userID, fmt.Sprintf("https://youtu.be/video%03d", i), ""); err != nil {
				t.Fatalf("Add %d: %v", i, err)
			}
		}
		_, err := svc.Add(ctx, userID, "https://youtu.be/video999", "")
		if !errors.Is(err, domain.ErrLimitReached) {
			t.Errorf("error = %v, want ErrLimitReached", err)
		}
	})
}

func TestListShowsGrowthAndOwnership(t *testing.T) {
	p := &fakeProvider{platform: domain.PlatformYouTube, stats: okStats("aaa", 5_000)}
	svc, db, ctx, userID := newService(t, p, nil)

	entry, err := svc.Add(ctx, userID, "https://youtu.be/videoaaa", "")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// A reading from before the window, so there is a baseline to grow from.
	seedSnapshot(t, db, ctx, entry.Video.ID, time.Now().UTC().Add(-8*24*time.Hour), 1_000)

	entries, err := svc.List(ctx, ListQuery{UserID: userID, Window: 7 * 24 * time.Hour})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("%d entries, want 1", len(entries))
	}
	if entries[0].ViewsGained == nil || *entries[0].ViewsGained != 4_000 {
		t.Errorf("views gained = %v, want 4000", entries[0].ViewsGained)
	}
	if entries[0].BaselineAt == nil {
		t.Error("no baseline timestamp; a delta with no reference point is uninterpretable")
	}

	// Another user's list must not contain it.
	other, _ := db.Users().Create(ctx, domain.NewUser{Email: "nosy@example.com", PasswordHash: "x"})
	theirs, err := svc.List(ctx, ListQuery{UserID: other.ID})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(theirs) != 0 {
		t.Errorf("another user's list has %d entries", len(theirs))
	}
}

func TestListSparkline(t *testing.T) {
	p := &fakeProvider{platform: domain.PlatformYouTube, stats: okStats("bbb", 100)}
	svc, db, ctx, userID := newService(t, p, nil)

	entry, err := svc.Add(ctx, userID, "https://youtu.be/videobbb", "")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	now := time.Now().UTC()
	for i := range 5 {
		seedSnapshot(t, db, ctx, entry.Video.ID, now.Add(-time.Duration(i+1)*time.Hour), int64(50+i))
	}

	without, _ := svc.List(ctx, ListQuery{UserID: userID})
	if len(without[0].Sparkline) != 0 {
		t.Error("a sparkline was attached when none was asked for")
	}

	with, err := svc.List(ctx, ListQuery{UserID: userID, Sparkline: 4})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(with[0].Sparkline) != 4 {
		t.Errorf("sparkline has %d points, want 4", len(with[0].Sparkline))
	}
	// Oldest first, because that is the direction a chart is drawn in.
	if len(with[0].Sparkline) > 1 &&
		!with[0].Sparkline[0].CapturedAt.Before(with[0].Sparkline[1].CapturedAt) {
		t.Error("sparkline is not in chronological order")
	}
}

// Videos are shared, so "this video exists" and "this caller may see it" are
// different questions.
func TestOwnershipIsCheckedNotAssumed(t *testing.T) {
	p := &fakeProvider{platform: domain.PlatformYouTube, stats: okStats("ccc", 10)}
	svc, db, ctx, userID := newService(t, p, nil)

	entry, err := svc.Add(ctx, userID, "https://youtu.be/videoccc", "")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	other, _ := db.Users().Create(ctx, domain.NewUser{Email: "intruder@example.com", PasswordHash: "x"})
	id := entry.Video.PublicID

	for name, call := range map[string]func() error{
		"get":     func() error { _, err := svc.Get(ctx, other.ID, id); return err },
		"update":  func() error { _, err := svc.Update(ctx, other.ID, id, "x", ""); return err },
		"remove":  func() error { return svc.Remove(ctx, other.ID, id) },
		"history": func() error { _, err := svc.HistoryFor(ctx, HistoryQuery{UserID: other.ID, PublicID: id}); return err },
		"refresh": func() error { _, err := svc.Refresh(ctx, other.ID, id); return err },
	} {
		t.Run(name, func(t *testing.T) {
			// Not-found rather than forbidden, so the endpoint does not confirm
			// that a video other people track exists.
			if err := call(); !errors.Is(err, domain.ErrRecordNotFound) {
				t.Errorf("error = %v, want ErrRecordNotFound", err)
			}
		})
	}
}

func TestUpdateAndRemove(t *testing.T) {
	p := &fakeProvider{platform: domain.PlatformYouTube, stats: okStats("ddd", 10)}
	svc, db, ctx, userID := newService(t, p, nil)

	entry, err := svc.Add(ctx, userID, "https://youtu.be/videoddd", "before")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	id := entry.Video.PublicID

	updated, err := svc.Update(ctx, userID, id, "after", "some notes")
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Label != "after" {
		t.Errorf("label = %q", updated.Label)
	}
	tracked, err := svc.Tracked(ctx, userID, id)
	if err != nil {
		t.Fatalf("Tracked: %v", err)
	}
	if tracked.Notes != "some notes" {
		t.Errorf("notes = %q", tracked.Notes)
	}

	if err := svc.Remove(ctx, userID, id); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if entries, _ := svc.List(ctx, ListQuery{UserID: userID}); len(entries) != 0 {
		t.Errorf("%d entries after removal", len(entries))
	}

	// The video and its history survive; polling stops.
	video, err := db.Videos().ByID(ctx, entry.Video.ID)
	if err != nil {
		t.Fatalf("the video was deleted with the tracking: %v", err)
	}
	if video.Schedule.NextFetchAt != nil {
		t.Error("an untracked video is still scheduled")
	}
	history, _ := db.Metrics().History(ctx, entry.Video.ID,
		time.Now().Add(-time.Hour), time.Now().Add(time.Hour), postgres.BucketRaw)
	if len(history) != 1 {
		t.Errorf("history was lost: %d snapshots", len(history))
	}
}

func TestHistory(t *testing.T) {
	p := &fakeProvider{platform: domain.PlatformYouTube, stats: okStats("eee", 10)}
	svc, db, ctx, userID := newService(t, p, nil)

	entry, err := svc.Add(ctx, userID, "https://youtu.be/videoeee", "")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	now := time.Now().UTC()
	for i := range 6 {
		seedSnapshot(t, db, ctx, entry.Video.ID, now.Add(-time.Duration(i*4)*time.Hour), int64(100+i*10))
	}

	h, err := svc.HistoryFor(ctx, HistoryQuery{
		UserID: userID, PublicID: entry.Video.PublicID,
		From: now.Add(-48 * time.Hour), To: now,
	})
	if err != nil {
		t.Fatalf("HistoryFor: %v", err)
	}
	if h.Source != "snapshots" {
		t.Errorf("source = %q", h.Source)
	}
	if len(h.Snapshots) == 0 {
		t.Error("no snapshots returned")
	}

	// A day bucket with no rollup yet falls back to the raw series rather than
	// returning an empty chart that looks like the video has no history.
	daily, err := svc.HistoryFor(ctx, HistoryQuery{
		UserID: userID, PublicID: entry.Video.PublicID,
		From: now.Add(-48 * time.Hour), To: now, Bucket: postgres.BucketDay,
	})
	if err != nil {
		t.Fatalf("HistoryFor: %v", err)
	}
	if daily.Source != "snapshots" || len(daily.Snapshots) == 0 {
		t.Errorf("day bucket with no rollup: source=%q, %d snapshots", daily.Source, len(daily.Snapshots))
	}

	// Once the rollup exists, the day bucket reads it and says so.
	if _, err := db.Metrics().Rollup(ctx, now.Add(-24*time.Hour)); err != nil {
		t.Fatalf("Rollup: %v", err)
	}
	rolled, err := svc.HistoryFor(ctx, HistoryQuery{
		UserID: userID, PublicID: entry.Video.PublicID,
		From: now.Add(-48 * time.Hour), To: now, Bucket: postgres.BucketDay,
	})
	if err != nil {
		t.Fatalf("HistoryFor: %v", err)
	}
	if rolled.Source != "daily_rollup" || len(rolled.Daily) == 0 {
		t.Errorf("source = %q, %d daily rows", rolled.Source, len(rolled.Daily))
	}
}

func TestHistoryRejectsAnInvertedRange(t *testing.T) {
	p := &fakeProvider{platform: domain.PlatformYouTube, stats: okStats("fff", 1)}
	svc, _, ctx, userID := newService(t, p, nil)

	entry, _ := svc.Add(ctx, userID, "https://youtu.be/videofff", "")
	now := time.Now()

	_, err := svc.HistoryFor(ctx, HistoryQuery{
		UserID: userID, PublicID: entry.Video.PublicID,
		From: now, To: now.Add(-time.Hour),
	})
	if !errors.Is(err, domain.ErrInvalidURL) {
		t.Errorf("error = %v, want a rejection", err)
	}
}

// Refresh moves the schedule rather than fetching inline: a synchronous
// refresh on demand is a way for one impatient user to spend everyone's budget.
func TestRefreshQueuesRatherThanFetching(t *testing.T) {
	p := &fakeProvider{platform: domain.PlatformYouTube, stats: okStats("ggg", 10)}
	svc, db, ctx, userID := newService(t, p, nil)

	entry, err := svc.Add(ctx, userID, "https://youtu.be/videoggg", "")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	before := p.calls.Load()

	// Push the next fetch well out, then ask for a refresh.
	if _, err := db.Pool.Exec(ctx,
		`UPDATE videos SET next_fetch_at = now() + interval '6 hours' WHERE id = $1`, entry.Video.ID); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if _, err := svc.Refresh(ctx, userID, entry.Video.PublicID); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	if p.calls.Load() != before {
		t.Error("Refresh fetched inline instead of queueing")
	}
	claimed, err := db.Videos().ClaimDue(ctx, domain.PlatformYouTube, 10, time.Minute)
	if err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	if len(claimed) != 1 {
		t.Errorf("%d videos due after a refresh, want 1", len(claimed))
	}
}

func TestRefreshRefusesARetiredVideo(t *testing.T) {
	p := &fakeProvider{platform: domain.PlatformYouTube, stats: okStats("hhh", 10)}
	svc, db, ctx, userID := newService(t, p, nil)

	entry, _ := svc.Add(ctx, userID, "https://youtu.be/videohhh", "")
	if _, err := db.Pool.Exec(ctx,
		`UPDATE videos SET unavailable_since = now(), next_fetch_at = NULL WHERE id = $1`,
		entry.Video.ID); err != nil {
		t.Fatalf("retire: %v", err)
	}

	if _, err := svc.Refresh(ctx, userID, entry.Video.PublicID); !errors.Is(err, domain.ErrGone) {
		t.Errorf("error = %v, want ErrGone", err)
	}
}

func TestSummarise(t *testing.T) {
	p := &fakeProvider{platform: domain.PlatformYouTube, stats: okStats("iii", 1_000)}
	svc, db, ctx, userID := newService(t, p, nil)

	for i := range 3 {
		if _, err := svc.Add(ctx, userID, fmt.Sprintf("https://youtu.be/video%03d", i), ""); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	// Give one of them a week-old baseline so it has growth to report.
	entries, _ := svc.List(ctx, ListQuery{UserID: userID})
	seedSnapshot(t, db, ctx, entries[0].Video.ID, time.Now().UTC().Add(-8*24*time.Hour), 400)

	summary, err := svc.Summarise(ctx, userID, 7*24*time.Hour, 3)
	if err != nil {
		t.Fatalf("Summarise: %v", err)
	}
	if summary.TrackedVideos != 3 {
		t.Errorf("tracked = %d, want 3", summary.TrackedVideos)
	}
	if summary.TotalViews != 3_000 {
		t.Errorf("total views = %d, want 3000", summary.TotalViews)
	}
	if summary.ViewsGained != 600 {
		t.Errorf("views gained = %d, want 600", summary.ViewsGained)
	}
	if len(summary.TopMovers) == 0 {
		t.Error("no top movers returned")
	}
	if summary.ByPlatform["youtube"] != 3 {
		t.Errorf("by platform = %v", summary.ByPlatform)
	}
}

// Staleness is judged against each video's own interval, so a six-hourly and a
// twelve-hourly video are not held to one cutoff.
func TestFreshnessUsesEachVideosInterval(t *testing.T) {
	p := &fakeProvider{platform: domain.PlatformYouTube, stats: okStats("jjj", 1)}
	svc, db, ctx, userID := newService(t, p, nil)

	entry, _ := svc.Add(ctx, userID, "https://youtu.be/videojjj", "")

	// Last read 8 hours ago, against a 6-hour interval: still inside 2x.
	if _, err := db.Pool.Exec(ctx,
		`UPDATE videos SET latest_captured_at = now() - interval '8 hours' WHERE id = $1`,
		entry.Video.ID); err != nil {
		t.Fatalf("age: %v", err)
	}
	got, _ := svc.Get(ctx, userID, entry.Video.PublicID)
	if !got.Fresh {
		t.Error("8h old against a 6h interval should still be fresh")
	}

	if _, err := db.Pool.Exec(ctx,
		`UPDATE videos SET latest_captured_at = now() - interval '20 hours' WHERE id = $1`,
		entry.Video.ID); err != nil {
		t.Fatalf("age: %v", err)
	}
	got, _ = svc.Get(ctx, userID, entry.Video.PublicID)
	if got.Fresh {
		t.Error("20h old against a 6h interval should be stale")
	}
}
