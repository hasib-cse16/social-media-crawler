// Package tracking is the application layer for the dashboard: which videos a
// user follows, what their counters are doing, and when they were last read.
package tracking

import (
	"context"
	"errors"
	"time"

	"github.com/foodibd/socialstats/internal/domain"
)

// RefreshPolicy decides when a video should next be fetched, and when to stop.
//
// It is deliberately separate from the fetching and from the storing. Backoff,
// jitter and the rule for retiring a video are judgement calls that will be
// tuned; the transaction that writes a result is not. Keeping them apart means
// the poller in the next step can supply a different policy without touching
// either.
type RefreshPolicy struct {
	// Interval is the base gap between successful fetches.
	Interval time.Duration

	// MaxBackoff caps the exponential growth after repeated failures.
	MaxBackoff time.Duration

	// FailuresBeforeRetiring is how many consecutive, unambiguous "this video
	// does not exist" answers it takes before polling stops.
	//
	// More than one, because a single not-found can be a bad afternoon rather
	// than a deleted video — a region block, a brief privacy toggle, an
	// upstream hiccup that surfaces as a 404.
	FailuresBeforeRetiring int
}

// DefaultPolicy is the fallback when nothing more specific is configured.
var DefaultPolicy = RefreshPolicy{
	Interval:               6 * time.Hour,
	MaxBackoff:             24 * time.Hour,
	FailuresBeforeRetiring: 3,
}

// Outcome turns one fetch attempt into the record that will be written.
//
// The mapping from a provider's error to a status is where the platform-level
// distinctions the provider packages worked out get preserved or thrown away,
// so each branch is explicit rather than falling through to a default.
func (p RefreshPolicy) Outcome(
	video *domain.Video,
	stats *domain.VideoStats,
	fetchErr error,
	startedAt time.Time,
	duration time.Duration,
) domain.FetchOutcome {
	out := domain.FetchOutcome{
		StartedAt: startedAt,
		Duration:  duration,
	}

	if fetchErr == nil && stats != nil {
		next := startedAt.Add(p.Interval)
		out.Stats = stats
		out.Status = domain.FetchOK
		out.AttemptStatus = domain.AttemptOK
		out.ConsecutiveFailures = 0
		out.NextFetchAt = &next
		return out
	}

	failures := video.Schedule.ConsecutiveFailures + 1
	out.ConsecutiveFailures = failures
	out.ErrorCode = errorCode(fetchErr)
	out.ErrorDetail = fetchErr.Error()

	switch {
	case errors.Is(fetchErr, domain.ErrNotFound):
		out.Status = domain.FetchNotFound
		out.AttemptStatus = domain.AttemptNotFound

		if failures >= p.FailuresBeforeRetiring {
			// Repeatedly and unambiguously gone: stop polling, keep every
			// snapshot already collected.
			retired := startedAt
			out.UnavailableSince = &retired
			out.NextFetchAt = nil
			return out
		}

	case errors.Is(fetchErr, domain.ErrBlocked):
		// A challenge page or a login wall is our problem, not evidence the
		// video no longer exists. Conflating the two would quietly delete
		// people's Facebook videos from their dashboards during a bad
		// afternoon, and they would never come back on their own.
		out.Status = domain.FetchBlocked
		out.AttemptStatus = domain.AttemptBlocked

	case errors.Is(fetchErr, domain.ErrRateLimited):
		out.Status = domain.FetchError
		out.AttemptStatus = domain.AttemptRateLimited

	case errors.Is(fetchErr, context.DeadlineExceeded):
		out.Status = domain.FetchError
		out.AttemptStatus = domain.AttemptTimeout

	case errors.Is(fetchErr, domain.ErrMisconfigured):
		// A missing credential is not going to fix itself on the next tick, and
		// retrying it four hundred times an hour helps nobody. Back off to the
		// cap immediately.
		out.Status = domain.FetchError
		out.AttemptStatus = domain.AttemptError
		next := startedAt.Add(p.MaxBackoff)
		out.NextFetchAt = &next
		return out

	default:
		out.Status = domain.FetchError
		out.AttemptStatus = domain.AttemptError
	}

	next := startedAt.Add(p.backoff(failures))
	out.NextFetchAt = &next
	return out
}

// backoff grows exponentially and is capped.
//
// Jitter is applied by the caller rather than here, so that Outcome stays a
// pure function of its inputs and can be tested without a source of randomness.
func (p RefreshPolicy) backoff(failures int) time.Duration {
	interval := p.Interval
	if interval <= 0 {
		interval = DefaultPolicy.Interval
	}
	maxBackoff := p.MaxBackoff
	if maxBackoff <= 0 {
		maxBackoff = DefaultPolicy.MaxBackoff
	}

	delay := interval
	for range min(max(failures-1, 0), 16) {
		delay *= 2
		if delay >= maxBackoff {
			return maxBackoff
		}
	}
	return min(delay, maxBackoff)
}

// errorCode names a failure in the same vocabulary the HTTP layer uses, so
// provider health can be grouped without parsing message text.
func errorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, domain.ErrNotFound):
		return "not_found"
	case errors.Is(err, domain.ErrBlocked):
		return "upstream_blocked"
	case errors.Is(err, domain.ErrRateLimited):
		return "rate_limited"
	case errors.Is(err, domain.ErrMisconfigured):
		return "provider_unavailable"
	case errors.Is(err, domain.ErrNotImplemented):
		return "not_implemented"
	case errors.Is(err, domain.ErrInvalidURL):
		return "invalid_url"
	case errors.Is(err, context.DeadlineExceeded):
		return "upstream_timeout"
	case errors.Is(err, context.Canceled):
		return "client_closed_request"
	case errors.Is(err, domain.ErrUpstreamFailure):
		return "upstream_error"
	default:
		return "internal_error"
	}
}
