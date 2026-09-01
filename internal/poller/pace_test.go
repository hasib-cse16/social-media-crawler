package poller

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestGateBoundsConcurrency(t *testing.T) {
	g := newGate(2, 0)
	ctx := context.Background()

	var mu sync.Mutex
	inFlight, peak := 0, 0

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := g.acquire(ctx); err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			defer g.release()

			mu.Lock()
			inFlight++
			peak = max(peak, inFlight)
			mu.Unlock()

			time.Sleep(2 * time.Millisecond)

			mu.Lock()
			inFlight--
			mu.Unlock()
		}()
	}
	wg.Wait()

	if peak > 2 {
		t.Errorf("peak concurrency = %d, want at most 2", peak)
	}
	if peak < 2 {
		t.Errorf("peak concurrency = %d; the gate serialised work it should have run in parallel", peak)
	}
}

// The gap is what the platforms actually notice. A concurrency limit alone does
// not provide one: a fast endpoint would still be hit as often as it answers.
func TestGateEnforcesAMinimumGap(t *testing.T) {
	const gap = 20 * time.Millisecond
	g := newGate(4, gap)
	ctx := context.Background()

	var mu sync.Mutex
	var starts []time.Time

	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := g.acquire(ctx); err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			defer g.release()

			mu.Lock()
			starts = append(starts, time.Now())
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(starts) != 5 {
		t.Fatalf("%d starts recorded, want 5", len(starts))
	}
	for i := range len(starts) {
		for j := i + 1; j < len(starts); j++ {
			if starts[i].After(starts[j]) {
				starts[i], starts[j] = starts[j], starts[i]
			}
		}
	}

	// Some slack for scheduling; the point is that they are spread, not
	// released as a burst.
	const slack = 5 * time.Millisecond
	for i := 1; i < len(starts); i++ {
		if actual := starts[i].Sub(starts[i-1]); actual < gap-slack {
			t.Errorf("start %d followed the previous one after %v, want at least %v", i, actual, gap)
		}
	}
	if total := starts[len(starts)-1].Sub(starts[0]); total < 3*gap {
		t.Errorf("five paced starts spanned only %v; they were not spread", total)
	}
}

// Several workers arriving together must each take a distinct place in the
// queue rather than all sleeping until the same instant and then firing as one
// burst — which is exactly what the gap exists to prevent.
func TestGateReservesDistinctSlots(t *testing.T) {
	g := newGate(8, 10*time.Millisecond)

	waits := make([]time.Duration, 4)
	for i := range waits {
		waits[i] = g.reserve()
	}

	for i := 1; i < len(waits); i++ {
		if waits[i] <= waits[i-1] {
			t.Errorf("reservation %d waits %v, not after the previous %v", i, waits[i], waits[i-1])
		}
	}
}

func TestGateRespectsContextCancellation(t *testing.T) {
	g := newGate(1, time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	// The first take is immediate; the second must wait out the hour-long gap
	// and be interrupted instead.
	if err := g.acquire(context.Background()); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	g.release()

	if err := g.acquire(ctx); err == nil {
		t.Error("acquire ignored a cancelled context")
		g.release()
	}

	// The slot must have been handed back, or the gate leaks capacity every
	// time a shutdown interrupts a wait.
	select {
	case g.slots <- struct{}{}:
		<-g.slots
	default:
		t.Error("the slot was not released when the wait was cancelled")
	}
}

func TestPauseUntilHoldsThePlatformBack(t *testing.T) {
	g := newGate(4, 0)
	until := time.Now().Add(time.Hour)

	g.pauseUntil(until)
	if got := g.pausedUntil(); !got.Equal(until) {
		t.Errorf("pausedUntil = %v, want %v", got, until)
	}

	// An earlier pause must not shorten a longer one already in effect.
	g.pauseUntil(time.Now().Add(time.Minute))
	if got := g.pausedUntil(); !got.Equal(until) {
		t.Errorf("a shorter pause overrode a longer one: %v", got)
	}
}

func TestJitterStaysWithinBoundsAndNeverGoesBackwards(t *testing.T) {
	from := time.Now()
	at := from.Add(time.Hour)

	var sawEarlier, sawLater bool
	for range 500 {
		got := jitter(from, at)

		if got.Before(from) {
			t.Fatalf("jitter produced %v, before the reference point %v", got, from)
		}
		offset := got.Sub(at)
		if maxOffset := time.Duration(float64(time.Hour) * jitterFraction); offset > maxOffset || offset < -maxOffset {
			t.Fatalf("offset %v exceeds ±%v", offset, maxOffset)
		}
		if offset < 0 {
			sawEarlier = true
		}
		if offset > 0 {
			sawLater = true
		}
	}

	// Both directions, or it is a delay rather than jitter.
	if !sawEarlier || !sawLater {
		t.Errorf("jitter only moved one way (earlier=%v later=%v)", sawEarlier, sawLater)
	}
}

// A time already due must not be jittered into the past and become
// retroactively overdue.
func TestJitterLeavesAnAlreadyDueTimeAlone(t *testing.T) {
	now := time.Now()

	for _, at := range []time.Time{now, now.Add(-time.Hour)} {
		if got := jitter(now, at); !got.Equal(at) {
			t.Errorf("jitter moved an already-due time from %v to %v", at, got)
		}
	}
}
