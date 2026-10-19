package engine

import (
	"context"
	"math"
	"sync"
	"time"
)

// limiter is a token bucket shared by every segment worker. IDM exposes one
// global speed limiter rather than a per-connection one, so we do the same:
// all workers draw from the same budget.
type limiter struct {
	rate  int64 // bytes per second; 0 means unlimited
	mu    sync.Mutex
	avail float64
	last  time.Time
}

func newLimiter(rate int64) *limiter {
	return &limiter{rate: rate, avail: float64(rate), last: time.Now()}
}

// Wait blocks until n bytes of budget are available.
func (l *limiter) Wait(ctx context.Context, n int64) error {
	if l == nil || l.rate <= 0 {
		return ctx.Err()
	}
	for {
		l.mu.Lock()
		now := time.Now()
		l.avail += now.Sub(l.last).Seconds() * float64(l.rate)
		l.last = now

		// A chunk can be larger than the whole per-second budget; the bucket
		// must be allowed to grow past the rate or Wait would never return.
		cap := math.Max(float64(l.rate), float64(n))
		if l.avail > cap {
			l.avail = cap
		}
		if l.avail >= float64(n) {
			l.avail -= float64(n)
			l.mu.Unlock()
			return ctx.Err()
		}

		need := float64(n) - l.avail
		wait := time.Duration(need / float64(l.rate) * float64(time.Second))
		l.mu.Unlock()

		if wait < time.Millisecond {
			wait = time.Millisecond
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}
