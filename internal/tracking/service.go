package tracking

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/foodibd/socialstats/internal/domain"
	"github.com/foodibd/socialstats/internal/storage/postgres"
)

// The dependencies, declared by the consumer so the Postgres repositories and
// the provider registry satisfy them implicitly and the import arrow keeps
// pointing inward.

// VideoStore is the video persistence this service needs.
type VideoStore interface {
	Upsert(ctx context.Context, in domain.NewVideo) (*domain.Video, error)
	ByID(ctx context.Context, id int64) (*domain.Video, error)
	ByPublicID(ctx context.Context, publicID string) (*domain.Video, error)
	Record(ctx context.Context, videoID int64, out domain.FetchOutcome) error
	Reschedule(ctx context.Context, videoID int64, at time.Time) error
}

// TrackStore is the per-user tracking persistence.
type TrackStore interface {
	Track(ctx context.Context, userID, videoID int64, label string) (*domain.TrackedVideo, error)
	Untrack(ctx context.Context, userID, videoID int64) error
	Get(ctx context.Context, userID, videoID int64) (*domain.TrackedVideo, error)
	Update(ctx context.Context, userID, videoID int64, label, notes string) (*domain.TrackedVideo, error)
	Dashboard(ctx context.Context, q postgres.DashboardQuery) ([]postgres.DashboardEntry, error)
	CountForUser(ctx context.Context, userID int64) (int, error)
	Summarise(ctx context.Context, userID int64, window time.Duration) (*postgres.Summary, error)
}

// MetricStore is the time series this service reads.
type MetricStore interface {
	History(ctx context.Context, videoID int64, from, to time.Time, bucket postgres.Bucket) ([]domain.Snapshot, error)
	Recent(ctx context.Context, videoID int64, limit int) ([]domain.Snapshot, error)
	Daily(ctx context.Context, videoID int64, from, to time.Time) ([]domain.DailyMetric, error)
	Attempts(ctx context.Context, videoID int64, limit int) ([]domain.FetchAttempt, error)
}

// Resolver turns a URL into a provider. provider.Registry satisfies it.
type Resolver interface {
	For(rawURL string) (domain.Provider, error)
	Platforms() []domain.Platform
}

// Config bounds what one account may do.
type Config struct {
	// MaxTrackedPerUser caps a user's list.
	//
	// It exists because tracking is what creates polling work, and polling work
	// is spent against upstream quota and anti-bot budgets that everyone on the
	// deployment shares. Without a cap, one account pasting a channel's back
	// catalogue degrades the service for all of them.
	MaxTrackedPerUser int

	// Policy decides when a tracked video is next fetched.
	Policy RefreshPolicy

	// AddTimeout bounds the synchronous fetch performed when a video is added.
	AddTimeout time.Duration
}

// Service is the tracking application layer.
type Service struct {
	videos   VideoStore
	tracking TrackStore
	metrics  MetricStore
	resolver Resolver
	cfg      Config
	log      *slog.Logger

	now func() time.Time
}

func NewService(videos VideoStore, tracking TrackStore, metrics MetricStore, resolver Resolver, cfg Config, log *slog.Logger) *Service {
	if cfg.MaxTrackedPerUser <= 0 {
		cfg.MaxTrackedPerUser = 200
	}
	if cfg.Policy.Interval <= 0 {
		cfg.Policy = DefaultPolicy
	}
	if cfg.AddTimeout <= 0 {
		cfg.AddTimeout = 30 * time.Second
	}
	return &Service{
		videos:   videos,
		tracking: tracking,
		metrics:  metrics,
		resolver: resolver,
		cfg:      cfg,
		log:      log.With("component", "tracking"),
		now:      time.Now,
	}
}

// Entry is one video on a user's dashboard.
type Entry struct {
	Video   *domain.Video `json:"video"`
	Label   string        `json:"label,omitempty"`
	AddedAt time.Time     `json:"added_at"`

	// ViewsGained is signed, and nil when there is no baseline to compare
	// against. A video added yesterday has no week-old reading, and reporting
	// zero growth would be a claim rather than an absence.
	ViewsGained *int64     `json:"views_gained,omitempty"`
	BaselineAt  *time.Time `json:"baseline_at,omitempty"`

	// Sparkline is the recent history, oldest first, when it was asked for.
	Sparkline []domain.Snapshot `json:"sparkline,omitempty"`

	// Fresh reports whether the last reading is recent enough to be worth
	// showing without a caveat, measured against this video's own interval.
	Fresh bool `json:"fresh"`
}

// Add starts tracking a URL for a user, fetching it immediately.
//
// The synchronous fetch is what makes this feel like a product rather than a
// queue: a user pastes a URL and sees the number, instead of an empty row that
// fills in at some point in the next six hours.
//
// A failed fetch still tracks the video. A TikTok challenge at 14:03 is not a
// reason to refuse to track a video, and the failure is recorded in the audit
// trail and on the row where the dashboard can show it.
func (s *Service) Add(ctx context.Context, userID int64, rawURL, label string) (*Entry, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, fmt.Errorf("%w: no url given", domain.ErrInvalidURL)
	}

	count, err := s.tracking.CountForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	if count >= s.cfg.MaxTrackedPerUser {
		return nil, fmt.Errorf("%w: you are tracking %d videos, the limit is %d",
			domain.ErrLimitReached, count, s.cfg.MaxTrackedPerUser)
	}

	provider, err := s.resolver.For(rawURL)
	if err != nil {
		return nil, err
	}

	ref, identifyErr := provider.Identify(rawURL)

	// A short link has no id until the redirect is followed, so for those the
	// fetch is not an optimisation — it is the only way to know which video is
	// being tracked. Everything else is identified first, so a failed fetch
	// still leaves a correctly identified row.
	var (
		stats     *domain.VideoStats
		fetchErr  error
		startedAt = s.now()
		duration  time.Duration
	)

	needsResolution := errors.Is(identifyErr, domain.ErrNeedsResolution)
	if identifyErr != nil && !needsResolution {
		return nil, identifyErr
	}

	fetchCtx, cancel := context.WithTimeout(ctx, s.cfg.AddTimeout)
	stats, fetchErr = provider.Stats(fetchCtx, rawURL)
	duration = s.now().Sub(startedAt)
	cancel()

	switch {
	case fetchErr == nil:
		// The fetched identity wins: it is canonical, and for a short link it
		// is the only one there is.
		ref = stats.Ref()
	case needsResolution:
		return nil, fmt.Errorf("could not resolve %q: %w", rawURL, fetchErr)
	}

	video, err := s.videos.Upsert(ctx, domain.NewVideo{
		Platform:        ref.Platform,
		PlatformVideoID: ref.VideoID,
		CanonicalURL:    firstNonEmpty(ref.CanonicalURL, rawURL),
	})
	if err != nil {
		return nil, err
	}

	tracked, err := s.tracking.Track(ctx, userID, video.ID, strings.TrimSpace(label))
	if err != nil {
		return nil, err
	}

	// Recorded after tracking, so the schedule this writes is not immediately
	// overwritten by Track bringing the video into the rotation.
	outcome := s.cfg.Policy.Outcome(video, stats, fetchErr, startedAt, duration)
	if err := s.videos.Record(ctx, video.ID, outcome); err != nil {
		// The video is tracked and the user can see it; failing the whole
		// request because the reading could not be filed would be worse.
		s.log.ErrorContext(ctx, "could not record the first fetch", "video_id", video.ID, "error", err)
	}
	if fetchErr != nil {
		s.log.WarnContext(ctx, "first fetch failed, tracking anyway",
			"video_id", video.ID, "platform", ref.Platform, "error", fetchErr)
	}

	fresh, err := s.videos.ByID(ctx, video.ID)
	if err != nil {
		return nil, err
	}
	return s.entry(fresh, tracked), nil
}

// ListQuery selects and orders a user's tracked videos.
type ListQuery struct {
	UserID   int64
	Window   time.Duration
	Platform domain.Platform
	Sort     postgres.DashboardSort
	Limit    int
	Offset   int

	// Sparkline, when positive, attaches that many recent readings to each
	// entry. It is off by default because it costs one query per video, which
	// is fine for a page of forty and wasteful for a JSON client that only
	// wants the numbers.
	Sparkline int
}

// List returns a user's tracked videos with their current counters and growth.
func (s *Service) List(ctx context.Context, q ListQuery) ([]Entry, error) {
	rows, err := s.tracking.Dashboard(ctx, postgres.DashboardQuery{
		UserID:   q.UserID,
		Window:   q.Window,
		Platform: q.Platform,
		Sort:     q.Sort,
		Limit:    q.Limit,
		Offset:   q.Offset,
	})
	if err != nil {
		return nil, err
	}

	out := make([]Entry, 0, len(rows))
	for _, row := range rows {
		entry := Entry{
			Video:       row.Video,
			Label:       row.Label,
			AddedAt:     row.AddedAt,
			ViewsGained: row.ViewsGained,
			BaselineAt:  row.BaselineAt,
			Fresh:       s.isFresh(row.Video),
		}

		if q.Sparkline > 0 {
			points, err := s.metrics.Recent(ctx, row.Video.ID, q.Sparkline)
			if err != nil {
				// A missing sparkline is a cosmetic loss; the numbers beside it
				// are the point of the row.
				s.log.WarnContext(ctx, "could not load a sparkline", "video_id", row.Video.ID, "error", err)
			} else {
				entry.Sparkline = points
			}
		}
		out = append(out, entry)
	}
	return out, nil
}

// Get returns one tracked video, and fails when the caller does not track it.
//
// Ownership is checked by looking up the tracking row rather than by trusting
// the id in the URL. Videos are shared between users, so "this video exists" and
// "this user may see it" are different questions.
func (s *Service) Get(ctx context.Context, userID int64, publicID string) (*Entry, error) {
	video, tracked, err := s.owned(ctx, userID, publicID)
	if err != nil {
		return nil, err
	}
	return s.entry(video, tracked), nil
}

// Tracked returns the caller's tracking row for a video, which is what a PATCH
// needs in order to leave unsent fields alone.
func (s *Service) Tracked(ctx context.Context, userID int64, publicID string) (*domain.TrackedVideo, error) {
	_, tracked, err := s.owned(ctx, userID, publicID)
	return tracked, err
}

// Update changes the label and notes a user keeps on a video.
func (s *Service) Update(ctx context.Context, userID int64, publicID, label, notes string) (*Entry, error) {
	video, _, err := s.owned(ctx, userID, publicID)
	if err != nil {
		return nil, err
	}

	tracked, err := s.tracking.Update(ctx, userID, video.ID, strings.TrimSpace(label), strings.TrimSpace(notes))
	if err != nil {
		return nil, err
	}
	return s.entry(video, tracked), nil
}

// Remove stops tracking a video for one user.
//
// The video's history survives, because other users may be tracking it and
// because re-adding it later should restore what was collected rather than
// starting from nothing.
func (s *Service) Remove(ctx context.Context, userID int64, publicID string) error {
	video, _, err := s.owned(ctx, userID, publicID)
	if err != nil {
		return err
	}
	return s.tracking.Untrack(ctx, userID, video.ID)
}

// HistoryQuery selects a slice of one video's time series.
type HistoryQuery struct {
	UserID   int64
	PublicID string
	From     time.Time
	To       time.Time
	Bucket   postgres.Bucket
}

// History is a video's readings over a window.
type History struct {
	Video     *domain.Video        `json:"video"`
	From      time.Time            `json:"from"`
	To        time.Time            `json:"to"`
	Bucket    postgres.Bucket      `json:"bucket"`
	Snapshots []domain.Snapshot    `json:"snapshots,omitempty"`
	Daily     []domain.DailyMetric `json:"daily,omitempty"`

	// Source names where the figures came from, because the two are not
	// equivalent: raw snapshots are individual measurements, daily rows are
	// summaries of days whose raw data has since been expired.
	Source string `json:"source"`
}

// HistoryFor returns a video's readings, reading the daily rollup for ranges
// older than the raw retention window.
//
// The switch is explicit in the response rather than hidden, because a chart
// drawn from daily summaries has different properties from one drawn from
// six-hourly measurements and the caller should be able to say so.
func (s *Service) HistoryFor(ctx context.Context, q HistoryQuery) (*History, error) {
	video, _, err := s.owned(ctx, q.UserID, q.PublicID)
	if err != nil {
		return nil, err
	}

	to := q.To
	if to.IsZero() {
		to = s.now()
	}
	from := q.From
	if from.IsZero() {
		from = to.Add(-7 * 24 * time.Hour)
	}
	if !from.Before(to) {
		return nil, fmt.Errorf("%w: 'from' must be before 'to'", domain.ErrInvalidURL)
	}

	out := &History{Video: video, From: from, To: to, Bucket: q.Bucket}

	if q.Bucket == postgres.BucketDay {
		daily, err := s.metrics.Daily(ctx, video.ID, from, to)
		if err != nil {
			return nil, err
		}
		if len(daily) > 0 {
			out.Daily = daily
			out.Source = "daily_rollup"
			return out, nil
		}
		// No rollup yet — a young deployment, or a day that has not been rolled
		// up. Fall through to the raw series rather than returning an empty
		// chart that looks like the video has no history.
	}

	bucket := q.Bucket
	if bucket == "" {
		bucket = postgres.BucketRaw
	}
	snapshots, err := s.metrics.History(ctx, video.ID, from, to, bucket)
	if err != nil {
		return nil, err
	}
	out.Snapshots = snapshots
	out.Bucket = bucket
	out.Source = "snapshots"
	return out, nil
}

// Attempts returns a video's recent fetch attempts, for the "why is this stale?"
// question the dashboard has to be able to answer.
func (s *Service) Attempts(ctx context.Context, userID int64, publicID string, limit int) ([]domain.FetchAttempt, error) {
	video, _, err := s.owned(ctx, userID, publicID)
	if err != nil {
		return nil, err
	}
	return s.metrics.Attempts(ctx, video.ID, limit)
}

// Refresh brings a video's next fetch forward, for a user who does not want to
// wait for the schedule.
//
// It moves the schedule rather than fetching inline: a synchronous refresh on
// demand is a way for one impatient user to spend everyone's TikTok budget, and
// the poller is already the thing that paces platform access.
func (s *Service) Refresh(ctx context.Context, userID int64, publicID string) (*Entry, error) {
	video, tracked, err := s.owned(ctx, userID, publicID)
	if err != nil {
		return nil, err
	}
	if video.Schedule.Retired() {
		return nil, fmt.Errorf("%w: this video is no longer available on its platform", domain.ErrGone)
	}
	if err := s.videos.Reschedule(ctx, video.ID, s.now()); err != nil {
		return nil, err
	}
	return s.entry(video, tracked), nil
}

// Summary is the headline block plus the movers.
type Summary struct {
	*postgres.Summary
	TopMovers []Entry `json:"top_movers,omitempty"`
}

// Summarise aggregates a user's tracked videos and picks out what moved most.
func (s *Service) Summarise(ctx context.Context, userID int64, window time.Duration, movers int) (*Summary, error) {
	base, err := s.tracking.Summarise(ctx, userID, window)
	if err != nil {
		return nil, err
	}

	out := &Summary{Summary: base}
	if movers > 0 && base.TrackedVideos > 0 {
		top, err := s.List(ctx, ListQuery{
			UserID: userID, Window: window,
			Sort: postgres.SortGained, Limit: movers,
		})
		if err != nil {
			return nil, err
		}
		out.TopMovers = top
	}
	return out, nil
}

// Platforms lists what this deployment can track.
func (s *Service) Platforms() []domain.Platform { return s.resolver.Platforms() }

// owned resolves a public video id and confirms the caller tracks it.
//
// A video the user does not track reports ErrRecordNotFound rather than a
// permission error, so the endpoint does not confirm the existence of videos
// other people are tracking.
func (s *Service) owned(ctx context.Context, userID int64, publicID string) (*domain.Video, *domain.TrackedVideo, error) {
	video, err := s.videos.ByPublicID(ctx, strings.TrimSpace(publicID))
	if err != nil {
		return nil, nil, err
	}

	tracked, err := s.tracking.Get(ctx, userID, video.ID)
	if err != nil {
		if errors.Is(err, domain.ErrRecordNotFound) {
			return nil, nil, fmt.Errorf("%w: you are not tracking that video", domain.ErrRecordNotFound)
		}
		return nil, nil, err
	}
	return video, tracked, nil
}

func (s *Service) entry(video *domain.Video, tracked *domain.TrackedVideo) *Entry {
	e := &Entry{Video: video, Fresh: s.isFresh(video)}
	if tracked != nil {
		e.Label = tracked.Label
		e.AddedAt = tracked.AddedAt
	}
	return e
}

// isFresh measures staleness against the video's own interval rather than a
// fixed cutoff, because a six-hourly YouTube video and a twelve-hourly TikTok
// video go stale at different speeds and one threshold would misreport one of
// them.
func (s *Service) isFresh(video *domain.Video) bool {
	if video.LatestCapturedAt == nil {
		return false
	}
	interval := video.Schedule.Interval
	if interval <= 0 {
		interval = s.cfg.Policy.Interval
	}
	return s.now().Sub(*video.LatestCapturedAt) <= 2*interval
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
