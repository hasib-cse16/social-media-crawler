// Package poller keeps tracked videos refreshed.
//
// It is the only thing in the service that talks to a platform on its own
// initiative, so it is also the only place that has to be a good citizen about
// how often it does so. Two mechanisms do that work, and they are different:
//
//	concurrency  how many fetches a platform may have in flight at once, which
//	             bounds memory and connections
//	min gap      how closely fetches may follow one another, which is what the
//	             platforms themselves actually notice
//
// A concurrency limit of 1 does not imply a gap — a fast platform would still
// be hit as often as it can answer — so both are needed.
package poller

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"
)

// gate paces one platform: at most `slots` fetches in flight, and at least
// `minGap` between the starts of any two.
type gate struct {
	slots chan struct{}

	mu          sync.Mutex
	minGap      time.Duration
	nextAllowed time.Time
	now         func() time.Time
}

func newGate(concurrency int, minGap time.Duration) *gate {
	if concurrency < 1 {
		concurrency = 1
	}
	return &gate{
		slots:  make(chan struct{}, concurrency),
		minGap: minGap,
		now:    time.Now,
	}
}

// acquire waits for a slot and for the minimum gap to elapse.
//
// The gap is reserved before the wait, not after: several workers arriving at
// once each take a distinct slot in the queue rather than all sleeping until the
// same instant and then firing together — which is the burst the gap exists to
// prevent.
func (g *gate) acquire(ctx context.Context) error {
	select {
	case g.slots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}

	wait := g.reserve()
	if wait <= 0 {
		return nil
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		g.release()
		return ctx.Err()
	}
}

func (g *gate) release() {
	select {
	case <-g.slots:
	default:
	}
}

// reserve claims the next start time and reports how long to wait for it.
func (g *gate) reserve() time.Duration {
	if g.minGap <= 0 {
		return 0
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.now()
	if g.nextAllowed.Before(now) {
		g.nextAllowed = now
	}
	at := g.nextAllowed
	g.nextAllowed = at.Add(g.minGap)
	return at.Sub(now)
}

// pauseUntil holds the platform back until t, used when the platform itself has
// told us to slow down.
func (g *gate) pauseUntil(t time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if t.After(g.nextAllowed) {
		g.nextAllowed = t
	}
}

// pausedUntil reports when this platform may next be fetched.
func (g *gate) pausedUntil() time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.nextAllowed
}

// jitterFraction is how far a scheduled fetch is nudged either way.
//
// Without it, everything that failed during an outage retries in the same
// second when the outage ends — the fleet synchronises itself into exactly the
// thundering herd the backoff was meant to avoid. It also keeps videos added in
// one batch from staying in lockstep forever.
const jitterFraction = 0.2

// jitter spreads a scheduled time by ±jitterFraction of the delay from now.
//
// It only ever moves a time later than `from`, so a jittered fetch cannot be
// pulled into the past and become immediately due.
func jitter(from, at time.Time) time.Time {
	delay := at.Sub(from)
	if delay <= 0 {
		return at
	}

	spread := float64(delay) * jitterFraction
	offset := time.Duration((rand.Float64()*2 - 1) * spread)

	jittered := at.Add(offset)
	if jittered.Before(from) {
		return from
	}
	return jittered
}
