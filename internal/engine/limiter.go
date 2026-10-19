package engine

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// Limit is a bytes-per-second ceiling that a caller may move while a transfer
// is already running.
//
// The bucket reads it on every wait rather than once at the start. That is the
// whole point: lowering the ceiling from the interface has to bite on the chunk
// in flight, not on the next download the user starts afterwards.
//
// A nil *Limit places no ceiling at all, so callers that do not care about
// throttling can leave the field unset instead of constructing a zero value.
type Limit struct {
	bps atomic.Int64

	mu sync.Mutex
	// ch is closed and replaced on every change. Without it a worker already
	// asleep would have to wait out a deadline computed from the rate that no
	// longer exists - at one byte per second, a deadline measured in days -
	// before noticing that the ceiling had been lifted.
	ch chan struct{}
}

// NewLimit returns a Limit holding bps.
//
// A negative rate cannot be honoured, and silently doing nothing about one
// would be worse than normalising it here - so anything below zero is stored
// as unlimited rather than as a ceiling that Wait can never satisfy.
func NewLimit(bps int64) *Limit {
	l := &Limit{ch: make(chan struct{})}
	l.Set(bps)
	return l
}

// Set moves the ceiling. Zero removes it.
//
// Every real change wakes the workers waiting on the old rate, which is what
// makes the new one take effect now rather than at the end of a sleep that was
// sized by the old one.
func (l *Limit) Set(bps int64) {
	if l == nil {
		return
	}
	if bps < 0 {
		bps = 0
	}
	if l.bps.Swap(bps) == bps {
		return // no change: waking anyone here would be pure churn
	}

	l.mu.Lock()
	if l.ch == nil {
		l.ch = make(chan struct{})
	} else {
		close(l.ch)
		l.ch = make(chan struct{})
	}
	l.mu.Unlock()
}

// Get returns the ceiling in bytes per second, zero meaning unlimited.
// A nil receiver reports unlimited for the same reason.
func (l *Limit) Get() int64 {
	if l == nil {
		return 0
	}
	return l.bps.Load()
}

// signal returns a channel that is closed by the next change.
//
// It is called while the bucket lock is held, so the change cannot slip into
// the gap between computing a deadline and going to sleep on it. The lock
// order is always bucket first, then this one - Set takes nothing else - so
// the two can never deadlock.
func (l *Limit) signal() <-chan struct{} {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ch == nil {
		l.ch = make(chan struct{})
	}
	return l.ch
}

// limiter is a token bucket shared by every segment worker of one transfer.
// IDM exposes one limiter rather than a per-connection one, so we do the same:
// all workers draw from the same budget.
type limiter struct {
	// rate is read fresh on every pass, which is what lets a running transfer
	// be re-limited without restarting it.
	rate *Limit

	mu    sync.Mutex
	avail float64
	last  time.Time
}

// newLimiter starts the bucket full, so a limited transfer may spend its first
// second's worth immediately instead of waiting a full second to move a byte.
func newLimiter(rate *Limit) *limiter {
	return &limiter{rate: rate, avail: float64(rate.Get()), last: time.Now()}
}

// Wait blocks until n bytes of budget are available.
func (l *limiter) Wait(ctx context.Context, n int64) error {
	if l == nil || l.rate.Get() <= 0 {
		return ctx.Err()
	}
	for {
		bps := l.rate.Get()
		if bps <= 0 {
			// The ceiling was lifted while this call was asleep. Returning is
			// correct, and the stale surplus does not matter: the clamp below
			// runs before any budget is granted, so re-imposing a limit later
			// cannot hand out a burst accrued while unlimited.
			return ctx.Err()
		}

		l.mu.Lock()
		now := time.Now()
		l.avail += now.Sub(l.last).Seconds() * float64(bps)
		l.last = now

		// A chunk can be larger than the whole per-second budget; the bucket
		// must be allowed to grow past the rate or Wait would never return.
		//
		// Re-clamping on every pass also does the second job of making a
		// lowered ceiling bite at once: without it the surplus accrued at the
		// old, higher rate would be spent before the new one took effect.
		cap := math.Max(float64(bps), float64(n))
		if l.avail > cap {
			l.avail = cap
		}
		if l.avail >= float64(n) {
			l.avail -= float64(n)
			l.mu.Unlock()
			return ctx.Err()
		}

		// Take the wake-up signal before releasing the lock, so a change made
		// a moment from now cannot fall into the gap between choosing this
		// deadline and sleeping on it.
		changed := l.rate.signal()
		need := float64(n) - l.avail
		wait := time.Duration(need / float64(bps) * float64(time.Second))
		if wait < time.Millisecond {
			wait = time.Millisecond
		}
		l.mu.Unlock()

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-changed:
			// The ceiling moved. Abandon the deadline sized by the rate that
			// no longer applies and recompute against the new one.
			timer.Stop()
		case <-timer.C:
		}
	}
}
