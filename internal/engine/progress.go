package engine

import (
	"sync"
	"sync/atomic"
	"time"

	"godl/internal/state"
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
	// segs is one row per connection, owned by the workers while they run.
	// It sits behind the same mutex as the speed calculation because the
	// transfer dialog reads it every tenth of a second and the byte counters
	// are written from several goroutines at once - reading state.Segment
	// directly would race the very loop that is writing to it.
	segs []SegState
}

// SegState is one connection's share of the file, in the shape the transfer
// dialog's table draws it.
//
// The byte count is what makes the table worth reading: a column of claims
// with nothing behind them would be decoration. Phase is separate on purpose,
// because the difference between "Send GET…" and "Receiving data…" cannot be
// read off the byte count - a connection waiting on its response has written
// nothing exactly like one nobody has tried yet.
type SegState struct {
	Index int `json:"index"`
	// Start and End are where this connection's range sits in the file. They
	// are what makes the map under the progress bar a picture of this file
	// rather than a row of equal shares: a connection holding the last
	// quarter has to sit over the last quarter, or the strip is decoration.
	Start   int64  `json:"start"`
	End     int64  `json:"end"`
	Length  int64  `json:"length"`
	Written int64  `json:"written"`
	Done    bool   `json:"done"`
	Phase   string `json:"phase"`
}

// The phases a connection passes through, named the way the dialog reports
// them. The values go on the wire, so they are words rather than numbers -
// the browser host relays these objects verbatim.
const (
	PhaseQueued    = "queued"
	PhaseGet       = "get"
	PhaseReceiving = "receiving"
)

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

	// Segments is one row per connection while a transfer runs, and empty
	// when nothing is: an idle record has no workers to ask, and a table of
	// guesses would be worse than no table at all. The transfer dialog draws
	// both its map and its connection list from it.
	Segments []SegState `json:"segments,omitempty"`
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
//
// This is the aggregate alone. The transfer loop calls SegAdd, which reaches
// this: keeping two accounting calls in the same loop would be one more place
// for the progress bar and the connection table to disagree about one file.
func (p *Progress) Add(n int64) {
	if p == nil {
		return
	}
	p.done.Add(n)
}

// Attach copies the transfer's segment plan into the progress, seeding every
// connection with the bytes already on disk.
//
// Seeding is what makes a resumed download's table start where the file
// actually is instead of at zero, and what makes a plan that was reset - the
// single-connection fallback resets it - show an empty one again rather than
// the rows of a plan nobody is running any more.
func (p *Progress) Attach(segs []state.Segment) {
	if p == nil || len(segs) == 0 {
		return
	}
	rows := make([]SegState, len(segs))
	for i, s := range segs {
		rows[i] = SegState{
			Index:   s.Index,
			Start:   s.Start,
			End:     s.End,
			Length:  s.Len(),
			Written: s.Written,
			Done:    s.Done,
			Phase:   PhaseQueued,
		}
	}

	p.mu.Lock()
	p.segs = rows
	p.mu.Unlock()
}

// seg finds a connection's row. Callers hold p.mu; a nil result means no plan
// has been attached, which is the state an idle record is in and not an
// error worth reporting.
func (p *Progress) seg(index int) *SegState {
	for i := range p.segs {
		if p.segs[i].Index == index {
			return &p.segs[i]
		}
	}
	return nil
}

// SegGet records that a connection has sent its request and is waiting on the
// first byte - the line the dialog reads as "Send GET…".
func (p *Progress) SegGet(index int) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if s := p.seg(index); s != nil {
		s.Phase = PhaseGet
	}
	p.mu.Unlock()
}

// SegAdd records bytes this connection put on disk, advances the transfer's
// total, and marks the connection receiving.
//
// One call accounts for both on purpose: the bar reads the total and the table
// reads the row, and two counters fed by two statements in the same loop is
// how they come to disagree about the same file.
func (p *Progress) SegAdd(index int, n int64) {
	if p == nil || n <= 0 {
		return
	}
	p.Add(n)

	p.mu.Lock()
	if s := p.seg(index); s != nil {
		s.Written += n
		s.Phase = PhaseReceiving
	}
	p.mu.Unlock()
}

// SegDone marks a connection finished.
//
// It is called wherever the engine decides a segment is complete, including
// the path where a response ended early but exactly filled it - the row has
// to say so however the end arrived.
func (p *Progress) SegDone(index int) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if s := p.seg(index); s != nil {
		s.Done = true
		s.Phase = PhaseReceiving
	}
	p.mu.Unlock()
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
	var rows []SegState
	if len(p.segs) > 0 {
		rows = make([]SegState, len(p.segs))
		copy(rows, p.segs)
	}
	p.mu.Unlock()

	s := Snapshot{Done: done, Total: total, Speed: speed, Segments: rows}
	if total > 0 {
		s.Percent = float64(done) / float64(total) * 100
	}
	return s
}
