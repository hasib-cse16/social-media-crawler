package poller

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/foodibd/socialstats/internal/domain"
	"github.com/foodibd/socialstats/internal/tracking"
)

// VideoStore is the persistence the poller needs. *postgres.VideoRepo satisfies it.
type VideoStore interface {
	ClaimDue(ctx context.Context, platform domain.Platform, limit int, lockFor time.Duration) ([]*domain.Video, error)
	Record(ctx context.Context, videoID int64, out domain.FetchOutcome) error
	Release(ctx context.Context, videoID int64) error
	BackoffPlatform(ctx context.Context, platform domain.Platform, until time.Time) (int64, error)
}

// Fetcher resolves a URL to the provider that owns it. *provider.Registry
// satisfies it.
type Fetcher interface {
	ByPlatform(p domain.Platform) (domain.Provider, error)
}

// PlatformPolicy is how one platform is polled.
type PlatformPolicy struct {
	// Concurrency bounds fetches in flight for this platform.
	Concurrency int

	// MinInterval is the smallest gap between the starts of two fetches.
	MinInterval time.Duration

	// Refresh is the interval given to videos on this platform when they are
	// first tracked.
	Refresh time.Duration
}

// Config tunes the poll loop.
type Config struct {
	// Tick is how often the poller looks for work. It is not the refresh rate:
	// a video's own interval decides when it is due, and this decides how
	// promptly "due" is noticed.
	Tick time.Duration

	// Batch is how many videos are claimed per platform per tick.
	Batch int

	// LockFor is how long a claim is held. It must exceed the slowest possible
	// fetch, or a worker's video becomes claimable while it is still working on
	// it and gets fetched twice.
	LockFor time.Duration

	// RateLimitBackoff is how long a whole platform is held back after it says
	// we are going too fast.
	RateLimitBackoff time.Duration

	// ShutdownGrace is how long Run waits for in-flight fetches to finish
	// recording before returning.
	ShutdownGrace time.Duration

	// Policy decides scheduling and retirement from a fetch result.
	Policy tracking.RefreshPolicy

	// Platforms is the per-platform pacing. A platform absent from this map is
	// not polled.
	Platforms map[domain.Platform]PlatformPolicy
}

// Poller refreshes tracked videos on a schedule.
type Poller struct {
	videos VideoStore
	fetch  Fetcher
	cfg    Config
	log    *slog.Logger

	gates map[domain.Platform]*gate

	// disabled holds platforms taken out of the rotation because polling them
	// cannot succeed — a missing credential, a provider switched off. Retrying
	// those on every tick would fill the logs and the audit table with the same
	// answer several hundred times an hour.
	mu       sync.Mutex
	disabled map[domain.Platform]string

	now func() time.Time
}

// New builds a poller.
func New(videos VideoStore, fetch Fetcher, cfg Config, log *slog.Logger) *Poller {
	if cfg.Tick <= 0 {
		cfg.Tick = time.Minute
	}
	if cfg.Batch <= 0 {
		cfg.Batch = 50
	}
	if cfg.LockFor <= 0 {
		cfg.LockFor = 5 * time.Minute
	}
	if cfg.RateLimitBackoff <= 0 {
		cfg.RateLimitBackoff = time.Hour
	}
	if cfg.ShutdownGrace <= 0 {
		cfg.ShutdownGrace = 30 * time.Second
	}

	gates := make(map[domain.Platform]*gate, len(cfg.Platforms))
	for platform, policy := range cfg.Platforms {
		gates[platform] = newGate(policy.Concurrency, policy.MinInterval)
	}

	return &Poller{
		videos:   videos,
		fetch:    fetch,
		cfg:      cfg,
		log:      log.With("component", "poller"),
		gates:    gates,
		disabled: map[domain.Platform]string{},
		now:      time.Now,
	}
}

// Run polls until ctx is cancelled.
//
// Nothing here coordinates with other replicas, and nothing needs to: the claim
// query uses FOR UPDATE SKIP LOCKED, so any number of pollers divide the work
// between them with no leader election and no chance of two fetching the same
// video.
func (p *Poller) Run(ctx context.Context) error {
	for platform, policy := range p.cfg.Platforms {
		p.log.Info("polling platform",
			"platform", platform,
			"concurrency", policy.Concurrency,
			"min_interval", policy.MinInterval,
			"refresh", policy.Refresh)
	}

	ticker := time.NewTicker(p.cfg.Tick)
	defer ticker.Stop()

	// The first pass runs immediately. Waiting a full tick after a deploy means
	// a restart quietly delays every overdue video by up to that long.
	p.tick(ctx)

	for {
		select {
		case <-ctx.Done():
			p.log.Info("poller stopping, waiting for in-flight fetches", "grace", p.cfg.ShutdownGrace)
			return nil
		case <-ticker.C:
			p.tick(ctx)
		}
	}
}

// tick runs one pass across every platform.
//
// Platforms are polled concurrently because they are independent: a TikTok
// challenge should not delay YouTube, and TikTok's two-second gap should not be
// paid by anything else.
func (p *Poller) tick(ctx context.Context) {
	var wg sync.WaitGroup
	for platform := range p.cfg.Platforms {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.pollPlatform(ctx, platform)
		}()
	}
	wg.Wait()
}

// pollPlatform claims and processes one platform's due videos.
func (p *Poller) pollPlatform(ctx context.Context, platform domain.Platform) {
	if reason, off := p.isDisabled(platform); off {
		p.log.DebugContext(ctx, "platform is not being polled", "platform", platform, "reason", reason)
		return
	}

	// Held back by its own rate limit: claiming now would lock rows we are not
	// going to fetch for another hour, keeping them out of any other replica's
	// reach for no reason.
	if until := p.gates[platform].pausedUntil(); until.After(p.now()) {
		p.log.DebugContext(ctx, "platform is backed off", "platform", platform, "until", until)
		return
	}

	videos, err := p.videos.ClaimDue(ctx, platform, p.cfg.Batch, p.cfg.LockFor)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			p.log.ErrorContext(ctx, "could not claim work", "platform", platform, "error", err)
		}
		return
	}
	if len(videos) == 0 {
		return
	}

	provider, err := p.fetch.ByPlatform(platform)
	if err != nil {
		p.log.ErrorContext(ctx, "no provider for a platform we claimed work for",
			"platform", platform, "error", err)
		p.releaseAll(ctx, videos)
		return
	}

	p.log.InfoContext(ctx, "claimed videos to refresh", "platform", platform, "count", len(videos))

	started := p.now()
	var wg sync.WaitGroup
	for _, video := range videos {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.process(ctx, provider, video)
		}()
	}
	wg.Wait()

	p.log.InfoContext(ctx, "finished a platform pass",
		"platform", platform, "count", len(videos), "duration_ms", p.now().Sub(started).Milliseconds())
}

// process fetches one video and records the result.
func (p *Poller) process(ctx context.Context, provider domain.Provider, video *domain.Video) {
	gate := p.gates[video.Platform]
	if err := gate.acquire(ctx); err != nil {
		// Shutting down before this one started. Hand the claim back so it is
		// picked up promptly rather than waiting for the lock to expire.
		p.release(ctx, video)
		return
	}
	defer gate.release()

	startedAt := p.now()
	stats, fetchErr := provider.Stats(ctx, video.CanonicalURL)
	duration := p.now().Sub(startedAt)

	// A cancelled fetch is a shutdown, not a failure of the video. Recording it
	// would burn a retry and skew the provider health figures with an outcome
	// the platform had no part in.
	if errors.Is(fetchErr, context.Canceled) && ctx.Err() != nil {
		p.release(context.WithoutCancel(ctx), video)
		return
	}

	outcome := p.cfg.Policy.Outcome(video, stats, fetchErr, startedAt, duration)

	// Jitter is applied here rather than inside the policy, so that Outcome
	// stays a pure function of its inputs and can be tested without a source of
	// randomness.
	if outcome.NextFetchAt != nil {
		at := jitter(startedAt, *outcome.NextFetchAt)
		outcome.NextFetchAt = &at
	}

	// Recording must not be abandoned because the process is stopping: the
	// fetch already happened, and dropping the result loses a reading and
	// leaves the row locked until its claim expires.
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.cfg.ShutdownGrace)
	defer cancel()

	if err := p.videos.Record(recordCtx, video.ID, outcome); err != nil {
		p.log.ErrorContext(ctx, "could not record a fetch result",
			"video_id", video.ID, "platform", video.Platform, "error", err)
		p.release(recordCtx, video)
		return
	}

	p.reactTo(recordCtx, video, outcome, fetchErr)
}

// reactTo handles the failures that are about the platform rather than about
// one video.
func (p *Poller) reactTo(ctx context.Context, video *domain.Video, outcome domain.FetchOutcome, fetchErr error) {
	switch {
	case fetchErr == nil:
		p.log.DebugContext(ctx, "video refreshed",
			"video_id", video.ID, "platform", video.Platform,
			"views", viewCount(outcome.Stats))

	case errors.Is(fetchErr, domain.ErrRateLimited):
		// A rate limit belongs to the platform, not to the video that happened
		// to hit it. Backing off only that video sends the next tick straight
		// into the same limit with a different id.
		until := p.now().Add(p.cfg.RateLimitBackoff)
		p.gates[video.Platform].pauseUntil(until)

		moved, err := p.videos.BackoffPlatform(ctx, video.Platform, until)
		if err != nil {
			p.log.ErrorContext(ctx, "could not back off a platform", "platform", video.Platform, "error", err)
			return
		}
		p.log.WarnContext(ctx, "platform rate limited, backing the whole platform off",
			"platform", video.Platform, "videos_rescheduled", moved, "until", until)

	case errors.Is(fetchErr, domain.ErrMisconfigured):
		// Not going to fix itself on the next tick. Retrying a missing API key
		// several hundred times an hour fills the logs and the audit table with
		// the same answer and helps nobody.
		p.disable(video.Platform, fetchErr.Error())

	case outcome.UnavailableSince != nil:
		p.log.InfoContext(ctx, "video retired after repeated not-found answers",
			"video_id", video.ID, "platform", video.Platform,
			"failures", outcome.ConsecutiveFailures)

	default:
		p.log.WarnContext(ctx, "refresh failed",
			"video_id", video.ID, "platform", video.Platform,
			"failures", outcome.ConsecutiveFailures,
			"next_attempt", outcome.NextFetchAt,
			"error", fetchErr)
	}
}

func (p *Poller) release(ctx context.Context, video *domain.Video) {
	if err := p.videos.Release(ctx, video.ID); err != nil {
		// Not fatal: the claim expires on its own, just later than we would
		// have liked.
		p.log.WarnContext(ctx, "could not release a claim", "video_id", video.ID, "error", err)
	}
}

func (p *Poller) releaseAll(ctx context.Context, videos []*domain.Video) {
	for _, video := range videos {
		p.release(ctx, video)
	}
}

// disable takes a platform out of the rotation for the life of the process.
func (p *Poller) disable(platform domain.Platform, reason string) {
	p.mu.Lock()
	_, already := p.disabled[platform]
	p.disabled[platform] = reason
	p.mu.Unlock()

	if !already {
		// Logged once, loudly. Repeating it every tick would bury it.
		p.log.Error("platform disabled for the rest of this process",
			"platform", platform,
			"reason", reason,
			"remedy", "fix the configuration and restart")
	}
}

func (p *Poller) isDisabled(platform domain.Platform) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	reason, off := p.disabled[platform]
	return reason, off
}

// Status reports what the poller is doing, for the health endpoint.
type Status struct {
	Platform    domain.Platform `json:"platform"`
	Polling     bool            `json:"polling"`
	Reason      string          `json:"reason,omitempty"`
	BackedOff   bool            `json:"backed_off,omitempty"`
	ResumesAt   *time.Time      `json:"resumes_at,omitempty"`
	Concurrency int             `json:"concurrency"`
	Refresh     string          `json:"refresh"`
}

// Status describes each platform's polling state.
func (p *Poller) Status() []Status {
	out := make([]Status, 0, len(p.cfg.Platforms))
	now := p.now()

	for platform, policy := range p.cfg.Platforms {
		reason, off := p.isDisabled(platform)
		s := Status{
			Platform:    platform,
			Polling:     !off,
			Reason:      reason,
			Concurrency: policy.Concurrency,
			Refresh:     policy.Refresh.String(),
		}
		if until := p.gates[platform].pausedUntil(); until.After(now.Add(time.Second)) {
			s.BackedOff = true
			s.ResumesAt = &until
		}
		out = append(out, s)
	}
	return out
}

func viewCount(stats *domain.VideoStats) any {
	if stats == nil || stats.ViewCount == nil {
		return nil
	}
	return *stats.ViewCount
}

// statusReporter adapts Status to the api package's PollReporter, which returns
// `any` so that package does not import this one.
type statusReporter struct{ p *Poller }

func (s statusReporter) Status() any { return s.p.Status() }

// Reporter returns a view of the poller for the health endpoint.
func (p *Poller) Reporter() interface{ Status() any } { return statusReporter{p: p} }
