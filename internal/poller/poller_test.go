package poller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/foodibd/socialstats/internal/config"
	"github.com/foodibd/socialstats/internal/domain"
	"github.com/foodibd/socialstats/internal/storage/postgres"
	"github.com/foodibd/socialstats/internal/storage/postgres/pgtest"
	"github.com/foodibd/socialstats/internal/tracking"
)

// The poller is exercised against a real database, because the claim query is
// the part that has to be right and its correctness is entirely a property of
// FOR UPDATE SKIP LOCKED. Providers are faked: what is under test is what we do
// with a result, not how it was obtained.

type fakeProvider struct {
	platform domain.Platform
	calls    atomic.Int32

	mu     sync.Mutex
	starts []time.Time
	err    error
	views  uint64
	delay  time.Duration
}

func (f *fakeProvider) Platform() domain.Platform { return f.platform }
func (f *fakeProvider) Match(string) bool         { return true }
func (f *fakeProvider) Identify(string) (domain.VideoRef, error) {
	return domain.VideoRef{Platform: f.platform}, nil
}

func (f *fakeProvider) Stats(ctx context.Context, rawURL string) (*domain.VideoStats, error) {
	f.calls.Add(1)

	f.mu.Lock()
	f.starts = append(f.starts, time.Now())
	err, views, delay := f.err, f.views, f.delay
	f.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	return &domain.VideoStats{
		Platform: f.platform, VideoID: rawURL, CanonicalURL: rawURL,
		Title: "A video", ViewCount: domain.U64(views), FetchedAt: time.Now().UTC(),
	}, nil
}

func (f *fakeProvider) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeProvider) startTimes() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time(nil), f.starts...)
}

type fakeRegistry struct {
	providers map[domain.Platform]domain.Provider
}

func (r fakeRegistry) ByPlatform(p domain.Platform) (domain.Provider, error) {
	provider, ok := r.providers[p]
	if !ok {
		return nil, fmt.Errorf("%w: %s", domain.ErrUnsupported, p)
	}
	return provider, nil
}

var testPolicy = tracking.RefreshPolicy{
	Interval:               6 * time.Hour,
	MaxBackoff:             24 * time.Hour,
	FailuresBeforeRetiring: 3,
}

func newPoller(t *testing.T, providers map[domain.Platform]domain.Provider, mutate func(*Config)) (*Poller, *postgres.DB, context.Context, int64) {
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

	user, err := db.Users().Create(ctx, domain.NewUser{Email: "poll@example.com", PasswordHash: "x"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	platforms := map[domain.Platform]PlatformPolicy{}
	for p := range providers {
		platforms[p] = PlatformPolicy{Concurrency: 2, Refresh: 6 * time.Hour}
	}

	cfg := Config{
		Tick: time.Hour, Batch: 50, LockFor: time.Minute,
		RateLimitBackoff: time.Hour, ShutdownGrace: 5 * time.Second,
		Policy: testPolicy, Platforms: platforms,
	}
	if mutate != nil {
		mutate(&cfg)
	}

	return New(db.Videos(), fakeRegistry{providers: providers}, cfg, slog.New(slog.DiscardHandler)), db, ctx, user.ID
}

// trackVideo creates a video and puts it in the poll rotation, due now.
func trackVideo(t *testing.T, db *postgres.DB, ctx context.Context, userID int64, platform domain.Platform, id string) *domain.Video {
	t.Helper()

	video, err := db.Videos().Upsert(ctx, domain.NewVideo{
		Platform: platform, PlatformVideoID: id, CanonicalURL: "https://example.test/" + id,
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := db.Tracking().Track(ctx, userID, video.ID, ""); err != nil {
		t.Fatalf("track: %v", err)
	}
	fresh, err := db.Videos().ByID(ctx, video.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	return fresh
}

// The headline of this step: run the poller and history accumulates.
func TestTickCollectsHistory(t *testing.T) {
	yt := &fakeProvider{platform: domain.PlatformYouTube, views: 1_000}
	p, db, ctx, userID := newPoller(t, map[domain.Platform]domain.Provider{domain.PlatformYouTube: yt}, nil)

	video := trackVideo(t, db, ctx, userID, domain.PlatformYouTube, "abc")

	p.tick(ctx)

	if yt.calls.Load() != 1 {
		t.Fatalf("provider called %d times, want 1", yt.calls.Load())
	}

	got, err := db.Videos().ByID(ctx, video.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if got.Latest.ViewCount == nil || *got.Latest.ViewCount != 1_000 {
		t.Errorf("latest view count = %v, want 1000", got.Latest.ViewCount)
	}
	if got.Schedule.LastFetchStatus != domain.FetchOK {
		t.Errorf("status = %q", got.Schedule.LastFetchStatus)
	}
	if got.Schedule.LockedUntil != nil {
		t.Error("the claim was not released")
	}

	history, err := db.Metrics().History(ctx, video.ID,
		time.Now().Add(-time.Hour), time.Now().Add(time.Hour), postgres.BucketRaw)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("%d snapshots, want 1", len(history))
	}

	attempts, _ := db.Metrics().Attempts(ctx, video.ID, 10)
	if len(attempts) != 1 || attempts[0].Status != domain.AttemptOK {
		t.Errorf("attempts = %+v", attempts)
	}

	// A second tick must not refetch: the video is scheduled ~6h out.
	p.tick(ctx)
	if yt.calls.Load() != 1 {
		t.Errorf("provider called %d times after a second tick; the schedule was ignored", yt.calls.Load())
	}
}

// Several ticks over an accelerating clock build a real series.
func TestRepeatedTicksAccumulateASeries(t *testing.T) {
	yt := &fakeProvider{platform: domain.PlatformYouTube, views: 100}
	p, db, ctx, userID := newPoller(t, map[domain.Platform]domain.Provider{domain.PlatformYouTube: yt}, nil)

	video := trackVideo(t, db, ctx, userID, domain.PlatformYouTube, "series")

	for round := range 4 {
		yt.mu.Lock()
		yt.views = uint64(100 * (round + 1))
		yt.mu.Unlock()

		p.tick(ctx)

		// Bring the next fetch forward, standing in for time passing.
		if _, err := db.Pool.Exec(ctx, `UPDATE videos SET next_fetch_at = now() WHERE id = $1`, video.ID); err != nil {
			t.Fatalf("advance: %v", err)
		}
	}

	history, err := db.Metrics().History(ctx, video.ID,
		time.Now().Add(-time.Hour), time.Now().Add(time.Hour), postgres.BucketRaw)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(history) != 4 {
		t.Fatalf("%d snapshots after four ticks, want 4", len(history))
	}
	if *history[0].ViewCount != 100 || *history[3].ViewCount != 400 {
		t.Errorf("series runs %d..%d, want 100..400", *history[0].ViewCount, *history[3].ViewCount)
	}
}

func TestPlatformsArePolledIndependently(t *testing.T) {
	yt := &fakeProvider{platform: domain.PlatformYouTube, views: 10}
	tt := &fakeProvider{platform: domain.PlatformTikTok, views: 20}
	p, db, ctx, userID := newPoller(t, map[domain.Platform]domain.Provider{
		domain.PlatformYouTube: yt, domain.PlatformTikTok: tt,
	}, nil)

	trackVideo(t, db, ctx, userID, domain.PlatformYouTube, "y1")
	trackVideo(t, db, ctx, userID, domain.PlatformTikTok, "t1")

	p.tick(ctx)

	if yt.calls.Load() != 1 || tt.calls.Load() != 1 {
		t.Errorf("calls: youtube=%d tiktok=%d, want 1 each", yt.calls.Load(), tt.calls.Load())
	}
}

// A failure is recorded, the video keeps its last good reading, and it is
// scheduled to be tried again.
func TestFailedFetchIsRecordedAndRetried(t *testing.T) {
	yt := &fakeProvider{platform: domain.PlatformYouTube, views: 500}
	p, db, ctx, userID := newPoller(t, map[domain.Platform]domain.Provider{domain.PlatformYouTube: yt}, nil)

	video := trackVideo(t, db, ctx, userID, domain.PlatformYouTube, "flaky")
	p.tick(ctx)

	yt.setErr(domain.ErrBlocked)
	if _, err := db.Pool.Exec(ctx, `UPDATE videos SET next_fetch_at = now() WHERE id = $1`, video.ID); err != nil {
		t.Fatalf("advance: %v", err)
	}
	p.tick(ctx)

	got, _ := db.Videos().ByID(ctx, video.ID)
	if got.Latest.ViewCount == nil || *got.Latest.ViewCount != 500 {
		t.Errorf("latest = %v; a failed fetch erased the last good reading", got.Latest.ViewCount)
	}
	if got.Schedule.LastFetchStatus != domain.FetchBlocked {
		t.Errorf("status = %q, want blocked", got.Schedule.LastFetchStatus)
	}
	if got.Schedule.ConsecutiveFailures != 1 {
		t.Errorf("failures = %d, want 1", got.Schedule.ConsecutiveFailures)
	}
	if got.Schedule.NextFetchAt == nil {
		t.Error("a blocked video was left unscheduled")
	}
	if got.Schedule.Retired() {
		t.Error("a single block retired the video")
	}

	attempts, _ := db.Metrics().Attempts(ctx, video.ID, 10)
	if len(attempts) != 2 || attempts[0].Status != domain.AttemptBlocked {
		t.Errorf("attempts = %+v, want a blocked one on top", attempts)
	}
}

// Only repeated, unambiguous not-founds retire a video; the history stays.
func TestRepeatedNotFoundRetiresTheVideo(t *testing.T) {
	yt := &fakeProvider{platform: domain.PlatformYouTube, views: 5}
	p, db, ctx, userID := newPoller(t, map[domain.Platform]domain.Provider{domain.PlatformYouTube: yt}, nil)

	video := trackVideo(t, db, ctx, userID, domain.PlatformYouTube, "deleted")
	p.tick(ctx) // one good reading first

	yt.setErr(domain.ErrNotFound)
	for range testPolicy.FailuresBeforeRetiring {
		if _, err := db.Pool.Exec(ctx, `UPDATE videos SET next_fetch_at = now() WHERE id = $1`, video.ID); err != nil {
			t.Fatalf("advance: %v", err)
		}
		p.tick(ctx)
	}

	got, _ := db.Videos().ByID(ctx, video.ID)
	if !got.Schedule.Retired() {
		t.Fatalf("not retired after %d not-founds", testPolicy.FailuresBeforeRetiring)
	}
	if got.Schedule.NextFetchAt != nil {
		t.Error("a retired video is still scheduled")
	}
	if got.Latest.ViewCount == nil || *got.Latest.ViewCount != 5 {
		t.Error("retiring discarded the last known figures")
	}

	// It is out of the rotation for good.
	before := yt.calls.Load()
	p.tick(ctx)
	if yt.calls.Load() != before {
		t.Error("a retired video was fetched again")
	}
}

// A rate limit belongs to the platform, not to the video that happened to hit
// it: backing off one id sends the next tick into the same limit.
func TestRateLimitBacksOffTheWholePlatform(t *testing.T) {
	yt := &fakeProvider{platform: domain.PlatformYouTube, err: domain.ErrRateLimited}
	tt := &fakeProvider{platform: domain.PlatformTikTok, views: 1}
	p, db, ctx, userID := newPoller(t, map[domain.Platform]domain.Provider{
		domain.PlatformYouTube: yt, domain.PlatformTikTok: tt,
	}, nil)

	for i := range 4 {
		trackVideo(t, db, ctx, userID, domain.PlatformYouTube, fmt.Sprintf("y%d", i))
	}
	trackVideo(t, db, ctx, userID, domain.PlatformTikTok, "t1")

	p.tick(ctx)

	var due int
	if err := db.Pool.QueryRow(ctx, `
		SELECT count(*) FROM videos
		WHERE platform = 'youtube' AND next_fetch_at <= now() + interval '30 minutes'`).Scan(&due); err != nil {
		t.Fatalf("count: %v", err)
	}
	if due != 0 {
		t.Errorf("%d youtube videos are still due soon after a rate limit", due)
	}

	// The gate holds the platform back too, so the next tick does not
	// immediately re-claim.
	before := yt.calls.Load()
	p.tick(ctx)
	if yt.calls.Load() != before {
		t.Error("youtube was polled again during its backoff")
	}

	// TikTok is untouched.
	if tt.calls.Load() != 1 {
		t.Errorf("tiktok calls = %d; backing off youtube affected it", tt.calls.Load())
	}
	status := statusFor(p.Status(), domain.PlatformYouTube)
	if !status.BackedOff || status.ResumesAt == nil {
		t.Errorf("status does not report the backoff: %+v", status)
	}
}

// A missing credential will not fix itself on the next tick, and retrying it
// several hundred times an hour fills the logs and the audit table.
func TestMisconfiguredPlatformIsDisabled(t *testing.T) {
	yt := &fakeProvider{platform: domain.PlatformYouTube, err: domain.ErrMisconfigured}
	p, db, ctx, userID := newPoller(t, map[domain.Platform]domain.Provider{domain.PlatformYouTube: yt}, nil)

	trackVideo(t, db, ctx, userID, domain.PlatformYouTube, "nokey")
	p.tick(ctx)

	if yt.calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", yt.calls.Load())
	}

	for range 3 {
		p.tick(ctx)
	}
	if yt.calls.Load() != 1 {
		t.Errorf("calls = %d; a misconfigured platform kept being polled", yt.calls.Load())
	}

	status := statusFor(p.Status(), domain.PlatformYouTube)
	if status.Polling || status.Reason == "" {
		t.Errorf("status does not report the platform as disabled: %+v", status)
	}
}

// A platform with no provider must not leave its claimed rows locked.
func TestClaimsAreReleasedWhenThereIsNoProvider(t *testing.T) {
	yt := &fakeProvider{platform: domain.PlatformYouTube, views: 1}
	p, db, ctx, userID := newPoller(t, map[domain.Platform]domain.Provider{domain.PlatformYouTube: yt}, func(c *Config) {
		// Claim TikTok work, but register no TikTok provider.
		c.Platforms[domain.PlatformTikTok] = PlatformPolicy{Concurrency: 1, Refresh: time.Hour}
	})

	video := trackVideo(t, db, ctx, userID, domain.PlatformTikTok, "orphan")
	p.tick(ctx)

	got, _ := db.Videos().ByID(ctx, video.ID)
	if got.Schedule.LockedUntil != nil {
		t.Error("the claim was left locked; the video waits for the lock to expire for no reason")
	}
	if got.Schedule.NextFetchAt == nil {
		t.Error("the video lost its schedule")
	}
}

// SKIP LOCKED is the whole scaling story: several pollers must divide the work
// with no coordination and no video fetched twice.
func TestConcurrentPollersDoNotDuplicateWork(t *testing.T) {
	yt := &fakeProvider{platform: domain.PlatformYouTube, views: 1, delay: 5 * time.Millisecond}
	first, db, ctx, userID := newPoller(t, map[domain.Platform]domain.Provider{domain.PlatformYouTube: yt}, nil)

	const videos = 20
	for i := range videos {
		trackVideo(t, db, ctx, userID, domain.PlatformYouTube, fmt.Sprintf("v%02d", i))
	}

	// A second poller against the same database, as a second replica would be.
	second := New(db.Videos(), fakeRegistry{providers: map[domain.Platform]domain.Provider{
		domain.PlatformYouTube: yt,
	}}, first.cfg, slog.New(slog.DiscardHandler))

	var wg sync.WaitGroup
	for _, p := range []*Poller{first, second} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.tick(ctx)
		}()
	}
	wg.Wait()

	if got := yt.calls.Load(); got != videos {
		t.Errorf("%d fetches for %d videos; work was duplicated or dropped", got, videos)
	}

	var snapshots int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM metric_snapshots`).Scan(&snapshots); err != nil {
		t.Fatalf("count: %v", err)
	}
	if snapshots != videos {
		t.Errorf("%d snapshots, want %d", snapshots, videos)
	}
}

// Pacing is what the platforms notice, so it has to hold across the whole
// platform pass, not just within one worker.
func TestFetchesArePacedAcrossAPass(t *testing.T) {
	const gap = 15 * time.Millisecond
	tt := &fakeProvider{platform: domain.PlatformTikTok, views: 1}
	p, db, ctx, userID := newPoller(t, map[domain.Platform]domain.Provider{domain.PlatformTikTok: tt}, func(c *Config) {
		c.Platforms[domain.PlatformTikTok] = PlatformPolicy{Concurrency: 2, MinInterval: gap, Refresh: time.Hour}
	})

	for i := range 4 {
		trackVideo(t, db, ctx, userID, domain.PlatformTikTok, fmt.Sprintf("t%d", i))
	}

	start := time.Now()
	p.tick(ctx)
	elapsed := time.Since(start)

	if tt.calls.Load() != 4 {
		t.Fatalf("calls = %d, want 4", tt.calls.Load())
	}
	// Four fetches at one per 15ms cannot finish in under three gaps.
	if elapsed < 3*gap {
		t.Errorf("four paced fetches took %v, want at least %v", elapsed, 3*gap)
	}

	starts := tt.startTimes()
	for i := range len(starts) {
		for j := i + 1; j < len(starts); j++ {
			if starts[i].After(starts[j]) {
				starts[i], starts[j] = starts[j], starts[i]
			}
		}
	}
	const slack = 5 * time.Millisecond
	for i := 1; i < len(starts); i++ {
		if actual := starts[i].Sub(starts[i-1]); actual < gap-slack {
			t.Errorf("fetch %d followed the previous after %v, want at least %v", i, actual, gap)
		}
	}
}

// A fetch that finished must be recorded even though the process is stopping:
// the upstream request already happened, and dropping the result loses a
// reading and leaves the row locked until its claim expires.
func TestShutdownStillRecordsAFinishedFetch(t *testing.T) {
	yt := &fakeProvider{platform: domain.PlatformYouTube, views: 42}
	p, db, ctx, userID := newPoller(t, map[domain.Platform]domain.Provider{domain.PlatformYouTube: yt}, nil)

	video := trackVideo(t, db, ctx, userID, domain.PlatformYouTube, "shutdown")

	cancelled, cancel := context.WithCancel(ctx)
	provider, _ := p.fetch.ByPlatform(domain.PlatformYouTube)

	// Claim it the way a pass would, then cancel before recording.
	claimed, err := db.Videos().ClaimDue(cancelled, domain.PlatformYouTube, 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("ClaimDue: %v, %d", err, len(claimed))
	}
	p.process(cancelled, provider, claimed[0])
	cancel()

	got, _ := db.Videos().ByID(ctx, video.ID)
	if got.Latest.ViewCount == nil || *got.Latest.ViewCount != 42 {
		t.Errorf("latest = %v; the completed fetch was not recorded", got.Latest.ViewCount)
	}
	if got.Schedule.LockedUntil != nil {
		t.Error("the claim was left held")
	}
}

func TestRunStopsOnContextCancellation(t *testing.T) {
	yt := &fakeProvider{platform: domain.PlatformYouTube, views: 1}
	p, db, ctx, userID := newPoller(t, map[domain.Platform]domain.Provider{domain.PlatformYouTube: yt}, func(c *Config) {
		c.Tick = 10 * time.Millisecond
	})
	trackVideo(t, db, ctx, userID, domain.PlatformYouTube, "running")

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- p.Run(runCtx) }()

	// The first pass runs immediately rather than waiting a full tick, so a
	// restart does not silently delay every overdue video.
	deadline := time.After(2 * time.Second)
	for yt.calls.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("Run never polled")
		case <-time.After(5 * time.Millisecond):
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop when its context was cancelled")
	}
}

func TestUntrackedVideosAreNotPolled(t *testing.T) {
	yt := &fakeProvider{platform: domain.PlatformYouTube, views: 1}
	p, db, ctx, userID := newPoller(t, map[domain.Platform]domain.Provider{domain.PlatformYouTube: yt}, nil)

	video := trackVideo(t, db, ctx, userID, domain.PlatformYouTube, "leaving")
	if err := db.Tracking().Untrack(ctx, userID, video.ID); err != nil {
		t.Fatalf("Untrack: %v", err)
	}

	p.tick(ctx)
	if yt.calls.Load() != 0 {
		t.Errorf("an untracked video was polled %d times", yt.calls.Load())
	}
}

func statusFor(statuses []Status, platform domain.Platform) Status {
	for _, s := range statuses {
		if s.Platform == platform {
			return s
		}
	}
	return Status{}
}
