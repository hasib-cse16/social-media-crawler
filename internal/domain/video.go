package domain

import "time"

// Counters is the set of public metrics a platform may report.
//
// Every field is a pointer for the same reason it is in VideoStats: nil means
// "this platform does not report it, or the uploader hides it", which is not
// the same fact as zero. Collapsing the two would make an Instagram photo post
// look like a video nobody watched.
type Counters struct {
	ViewCount    *uint64 `json:"view_count,omitempty"`
	LikeCount    *uint64 `json:"like_count,omitempty"`
	CommentCount *uint64 `json:"comment_count,omitempty"`
	ShareCount   *uint64 `json:"share_count,omitempty"`
	SaveCount    *uint64 `json:"save_count,omitempty"`
}

// Counters extracts just the metrics from a provider result.
func (v *VideoStats) Counters() Counters {
	return Counters{
		ViewCount:    v.ViewCount,
		LikeCount:    v.LikeCount,
		CommentCount: v.CommentCount,
		ShareCount:   v.ShareCount,
		SaveCount:    v.SaveCount,
	}
}

// FetchStatus is the outcome of the most recent attempt to refresh a video.
type FetchStatus string

const (
	FetchPending  FetchStatus = "pending" // never fetched
	FetchOK       FetchStatus = "ok"
	FetchNotFound FetchStatus = "not_found"
	FetchBlocked  FetchStatus = "blocked"
	FetchError    FetchStatus = "error"
)

// FetchSchedule is the poller's state for one video. It lives on the video
// rather than per-user because the fetch is shared: one video is fetched once
// however many people track it.
type FetchSchedule struct {
	// TrackerCount is how many users actively track this video. At zero the
	// video is not polled at all.
	TrackerCount int

	// Interval is how often this video should be refreshed.
	Interval time.Duration

	// NextFetchAt is nil when the video is not scheduled: nobody tracks it, or
	// it has been retired.
	NextFetchAt *time.Time

	// LockedUntil is held by the worker that claimed this video. Expiry is what
	// makes a worker dying mid-fetch recoverable.
	LockedUntil *time.Time

	ConsecutiveFailures int
	LastFetchAt         *time.Time
	LastFetchStatus     FetchStatus
	LastFetchError      string

	// UnavailableSince is set only when the platform said, repeatedly and
	// unambiguously, that the video is gone. History is kept; polling stops.
	// It is never set from a block or a login wall, which are our problem
	// rather than evidence the video no longer exists.
	UnavailableSince *time.Time
}

// Retired reports whether this video has been taken out of the poll rotation
// because the platform says it no longer exists.
func (s FetchSchedule) Retired() bool { return s.UnavailableSince != nil }

// Video is the shared record for one video on one platform.
type Video struct {
	ID       int64  `json:"-"`
	PublicID string `json:"id"`

	Platform        Platform `json:"platform"`
	PlatformVideoID string   `json:"video_id"`
	CanonicalURL    string   `json:"canonical_url"`

	Title        string     `json:"title,omitempty"`
	ChannelID    string     `json:"channel_id,omitempty"`
	ChannelTitle string     `json:"channel_title,omitempty"`
	PublishedAt  *time.Time `json:"published_at,omitempty"`

	// Latest is the denormalised current reading, written in the same
	// transaction as the snapshot it came from.
	Latest Counters `json:"latest"`

	// LatestCapturedAt is when Latest was measured. Nil means never fetched.
	LatestCapturedAt *time.Time `json:"latest_captured_at,omitempty"`

	Schedule    FetchSchedule `json:"-"`
	FirstSeenAt time.Time     `json:"first_seen_at"`
}

// NewVideo identifies a video well enough to create or find its row. Metadata
// and counters arrive later, from a fetch.
type NewVideo struct {
	Platform        Platform
	PlatformVideoID string
	CanonicalURL    string
}

// TrackedVideo is one user's tracking of one video.
type TrackedVideo struct {
	UserID   int64      `json:"-"`
	VideoID  int64      `json:"-"`
	Label    string     `json:"label,omitempty"`
	Notes    string     `json:"notes,omitempty"`
	AddedAt  time.Time  `json:"added_at"`
	Archived *time.Time `json:"archived_at,omitempty"`
}

// Snapshot is one reading of a video's counters at a point in time.
type Snapshot struct {
	VideoID    int64     `json:"-"`
	CapturedAt time.Time `json:"captured_at"`
	Counters
}

// DailyMetric summarises one video's day, once the raw snapshots for it have
// aged out.
type DailyMetric struct {
	VideoID int64     `json:"-"`
	Day     time.Time `json:"day"`

	FirstViewCount *uint64 `json:"first_view_count,omitempty"`
	LastViewCount  *uint64 `json:"last_view_count,omitempty"`
	LastLike       *uint64 `json:"last_like_count,omitempty"`
	LastComment    *uint64 `json:"last_comment_count,omitempty"`
	LastShare      *uint64 `json:"last_share_count,omitempty"`
	LastSave       *uint64 `json:"last_save_count,omitempty"`

	// ViewDelta is last minus first, and is signed on purpose: platforms revise
	// view counts downward, so a negative is a real measurement.
	ViewDelta *int64 `json:"view_delta,omitempty"`

	// SampleCount is how many raw snapshots this row summarises. One sample
	// means the delta is meaningless, and charts should say so rather than
	// drawing a confident flat line across a gap in coverage.
	SampleCount int `json:"sample_count"`
}

// AttemptStatus is the recorded outcome of a single fetch, at finer grain than
// FetchStatus so provider health can be broken down.
type AttemptStatus string

const (
	AttemptOK          AttemptStatus = "ok"
	AttemptNotFound    AttemptStatus = "not_found"
	AttemptBlocked     AttemptStatus = "blocked"
	AttemptRateLimited AttemptStatus = "rate_limited"
	AttemptTimeout     AttemptStatus = "timeout"
	AttemptError       AttemptStatus = "error"
)

// FetchAttempt is one row of the audit trail.
type FetchAttempt struct {
	VideoID     int64         `json:"-"`
	Platform    Platform      `json:"platform"`
	StartedAt   time.Time     `json:"started_at"`
	Duration    time.Duration `json:"-"`
	DurationMS  int           `json:"duration_ms"`
	Status      AttemptStatus `json:"status"`
	ErrorCode   string        `json:"error_code,omitempty"`
	ErrorDetail string        `json:"error_detail,omitempty"`
}

// FetchOutcome is everything one fetch attempt produced, together with the
// caller's decision about what to do next.
//
// The scheduling fields are supplied rather than computed by storage on
// purpose: backoff, jitter and the rule for retiring a video are policy, and
// policy belongs in the poller. Storage's job is to write all of it atomically.
type FetchOutcome struct {
	// Stats is the provider's result, and is nil unless Status is FetchOK.
	Stats *VideoStats

	Status        FetchStatus
	AttemptStatus AttemptStatus
	ErrorCode     string
	ErrorDetail   string

	StartedAt time.Time
	Duration  time.Duration

	// ConsecutiveFailures is the new value, not a delta.
	ConsecutiveFailures int

	// NextFetchAt nil means "do not poll this again".
	NextFetchAt *time.Time

	// UnavailableSince, when set, retires the video.
	UnavailableSince *time.Time
}

// Succeeded reports whether this outcome carries usable stats.
func (o FetchOutcome) Succeeded() bool { return o.Status == FetchOK && o.Stats != nil }
