package tracking

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/foodibd/socialstats/internal/domain"
)

var testPolicy = RefreshPolicy{
	Interval:               6 * time.Hour,
	MaxBackoff:             24 * time.Hour,
	FailuresBeforeRetiring: 3,
}

func videoWithFailures(n int) *domain.Video {
	return &domain.Video{ID: 1, Schedule: domain.FetchSchedule{ConsecutiveFailures: n}}
}

func TestOutcomeOnSuccess(t *testing.T) {
	at := time.Now().UTC()
	stats := &domain.VideoStats{Platform: domain.PlatformYouTube, VideoID: "abc", ViewCount: domain.U64(100)}

	out := testPolicy.Outcome(videoWithFailures(4), stats, nil, at, 120*time.Millisecond)

	if out.Status != domain.FetchOK || out.AttemptStatus != domain.AttemptOK {
		t.Errorf("status = %q/%q, want ok", out.Status, out.AttemptStatus)
	}
	if !out.Succeeded() {
		t.Error("Succeeded() = false on a successful fetch")
	}
	if out.ConsecutiveFailures != 0 {
		t.Errorf("failures = %d, want the counter reset", out.ConsecutiveFailures)
	}
	if out.NextFetchAt == nil || !out.NextFetchAt.Equal(at.Add(6*time.Hour)) {
		t.Errorf("next fetch = %v, want one interval out", out.NextFetchAt)
	}
	if out.UnavailableSince != nil {
		t.Error("a successful fetch retired the video")
	}
	if out.ErrorCode != "" {
		t.Errorf("error code = %q on success", out.ErrorCode)
	}
}

// A block is our problem, not evidence the video is gone. Conflating the two
// would quietly delete people's videos during a bad afternoon.
func TestBlockedNeverRetiresAVideo(t *testing.T) {
	at := time.Now().UTC()
	blocked := errors.New("wrapped: " + domain.ErrBlocked.Error())
	blocked = domain.ErrBlocked

	// Far past the retirement threshold; a block must still never retire.
	out := testPolicy.Outcome(videoWithFailures(20), nil, blocked, at, time.Second)

	if out.UnavailableSince != nil {
		t.Error("a blocked fetch retired the video")
	}
	if out.NextFetchAt == nil {
		t.Fatal("a blocked fetch stopped polling entirely")
	}
	if out.Status != domain.FetchBlocked || out.AttemptStatus != domain.AttemptBlocked {
		t.Errorf("status = %q/%q, want blocked", out.Status, out.AttemptStatus)
	}
	if out.ErrorCode != "upstream_blocked" {
		t.Errorf("error code = %q", out.ErrorCode)
	}
}

// A single not-found can be a region block or a brief privacy toggle, so it
// takes several in a row to stop polling.
func TestNotFoundRetiresOnlyAfterRepeats(t *testing.T) {
	at := time.Now().UTC()

	for failures := 0; failures < testPolicy.FailuresBeforeRetiring-1; failures++ {
		out := testPolicy.Outcome(videoWithFailures(failures), nil, domain.ErrNotFound, at, time.Second)
		if out.UnavailableSince != nil {
			t.Errorf("retired after %d consecutive not-founds, want %d",
				failures+1, testPolicy.FailuresBeforeRetiring)
		}
		if out.NextFetchAt == nil {
			t.Errorf("stopped polling after %d not-founds", failures+1)
		}
	}

	out := testPolicy.Outcome(videoWithFailures(testPolicy.FailuresBeforeRetiring-1), nil, domain.ErrNotFound, at, time.Second)
	if out.UnavailableSince == nil {
		t.Fatal("never retired despite repeated not-founds")
	}
	if out.NextFetchAt != nil {
		t.Error("a retired video is still scheduled")
	}
	if out.Status != domain.FetchNotFound {
		t.Errorf("status = %q", out.Status)
	}
}

func TestBackoffGrowsAndIsCapped(t *testing.T) {
	at := time.Now().UTC()

	var last time.Duration
	for failures := range 10 {
		out := testPolicy.Outcome(videoWithFailures(failures), nil, domain.ErrUpstreamFailure, at, time.Second)
		if out.NextFetchAt == nil {
			t.Fatalf("failures=%d: no next fetch", failures)
		}
		delay := out.NextFetchAt.Sub(at)

		if delay < last {
			t.Errorf("failures=%d: backoff shrank from %v to %v", failures, last, delay)
		}
		if delay > testPolicy.MaxBackoff {
			t.Errorf("failures=%d: backoff %v exceeds the cap %v", failures, delay, testPolicy.MaxBackoff)
		}
		last = delay
	}
	if last != testPolicy.MaxBackoff {
		t.Errorf("backoff settled at %v, want the cap %v", last, testPolicy.MaxBackoff)
	}
}

// A missing credential will not fix itself on the next tick, so it goes
// straight to the cap instead of grinding through the exponential ramp.
func TestMisconfiguredBacksOffImmediately(t *testing.T) {
	at := time.Now().UTC()

	out := testPolicy.Outcome(videoWithFailures(0), nil, domain.ErrMisconfigured, at, time.Millisecond)
	if out.NextFetchAt == nil {
		t.Fatal("no next fetch")
	}
	if got := out.NextFetchAt.Sub(at); got != testPolicy.MaxBackoff {
		t.Errorf("delay = %v, want the cap %v on the first failure", got, testPolicy.MaxBackoff)
	}
	if out.UnavailableSince != nil {
		t.Error("a configuration problem retired the video")
	}
}

func TestErrorCodes(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{nil, ""},
		{domain.ErrNotFound, "not_found"},
		{domain.ErrBlocked, "upstream_blocked"},
		{domain.ErrRateLimited, "rate_limited"},
		{domain.ErrMisconfigured, "provider_unavailable"},
		{domain.ErrInvalidURL, "invalid_url"},
		{domain.ErrUpstreamFailure, "upstream_error"},
		{context.DeadlineExceeded, "upstream_timeout"},
		{context.Canceled, "client_closed_request"},
		{errors.New("something else"), "internal_error"},
	}
	for _, tc := range tests {
		if got := errorCode(tc.err); got != tc.want {
			t.Errorf("errorCode(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// The sentinels have to survive wrapping, since that is how every provider
// actually returns them.
func TestOutcomeMatchesWrappedSentinels(t *testing.T) {
	at := time.Now().UTC()
	wrapped := domain.NewUpstreamError("tiktok", 200, "challenge page", domain.ErrBlocked)

	out := testPolicy.Outcome(videoWithFailures(9), nil, wrapped, at, time.Second)
	if out.AttemptStatus != domain.AttemptBlocked {
		t.Errorf("attempt status = %q, want blocked from the wrapped sentinel", out.AttemptStatus)
	}
	if out.UnavailableSince != nil {
		t.Error("a wrapped block retired the video")
	}
}

func TestOutcomeRecordsTheFailureDetail(t *testing.T) {
	at := time.Now().UTC()
	err := domain.NewUpstreamError("meta", 200, "login wall", domain.ErrBlocked)

	out := testPolicy.Outcome(videoWithFailures(0), nil, err, at, 3*time.Second)
	if out.ErrorDetail == "" {
		t.Error("no error detail recorded")
	}
	if out.Duration != 3*time.Second || !out.StartedAt.Equal(at) {
		t.Errorf("timing not carried through: %v, %v", out.StartedAt, out.Duration)
	}
}
