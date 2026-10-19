package engine

import (
	"sync"
	"sync/atomic"
	"time"
)

// Progress tracks one download while its segments are still in flight.
//
// The counters are atomic because segment workers write them concurrently and
// the API goroutine reads them without joining those workers. The derived
// speed is smoothed with an exponential moving average so a UI does not jitter
// every time one segment happens to finish.
type Progress struct {
	total atomic.Int64
	done  atomic.Int64

	mu       sync.Mutex
	sampleAt time.Time
	sampleN  int64
	ema      float64 // bytes per second
}

// Snapshot is a consistent view of one download's progress.
//
// The tags are lowercase and mandatory: the UI reads `progress.done` and the
// browser host relays these objects verbatim, so default Go capitalisation
// would silently make every field undefined on the other side.
type Snapshot struct {
	Done    int64   `json:"done"`
	Total   int64   `json:"total"`
	Percent float64 `json:"percent"`
	Speed   float64 `json:"speed"` // bytes per second
}

// NewProgress starts counting with the transfer's known size.
func NewProgress(total int64) *Progress {
	p := &Progress{sampleAt: time.Now()}
	p.total.Store(total)
	return p
}

// Seed sets the starting point to bytes that were already on disk before this
// run, so a resumed download does not report a progress that drops backwards.
func (p *Progress) Seed(done int64) {
	if done < 0 {
		done = 0
	}
	p.done.Store(done)
	p.mu.Lock()
	p.sampleAt = time.Now()
	p.sampleN = done
	p.mu.Unlock()
}

// Add records bytes written to disk.
func (p *Progress) Add(n int64) {
	if p == nil {
		return
	}
	p.done.Add(n)
}

// Snapshot reads the current counters and refreshes the smoothed speed.
func (p *Progress) Snapshot() Snapshot {
	now := time.Now()
	done := p.done.Load()
	total := p.total.Load()

	p.mu.Lock()
	var speed float64
	if elapsed := now.Sub(p.sampleAt).Seconds(); elapsed >= 0.25 {
		inst := float64(done-p.sampleN) / elapsed
		if p.ema == 0 {
			p.ema = inst
		} else {
			// 0.7/0.3 keeps recent measurements in charge without dropping the
			// longer-term average.
			p.ema = 0.7*p.ema + 0.3*inst
		}
		p.sampleAt = now
		p.sampleN = done
	}
	speed = p.ema
	p.mu.Unlock()

	s := Snapshot{Done: done, Total: total, Speed: speed}
	if total > 0 {
		s.Percent = float64(done) / float64(total) * 100
	}
	return s
}
