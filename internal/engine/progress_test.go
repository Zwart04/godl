package engine

import (
	"sync"
	"testing"

	"godl/internal/state"
)

// One row per connection, seeded from what is already on disk. The seeding is
// what makes a resumed download's table start where the file is instead of at
// zero, which is the difference between a table that reports the truth and one
// that reports a fresh start it is not having.
func TestProgressCarriesOneRowPerConnection(t *testing.T) {
	p := NewProgress(1000)
	p.Attach([]state.Segment{
		{Index: 0, Start: 0, End: 499},
		{Index: 1, Start: 500, End: 999, Written: 250},
	})

	s := p.Snapshot()
	if len(s.Segments) != 2 {
		t.Fatalf("segments = %d rows, want 2", len(s.Segments))
	}
	if got := s.Segments[0].Length; got != 500 {
		t.Errorf("row 0 length = %d, want 500", got)
	}
	if got := s.Segments[1].Written; got != 250 {
		t.Errorf("row 1 written = %d, want the 250 bytes already on disk", got)
	}
	if got := s.Segments[0].Phase; got != PhaseQueued {
		t.Errorf("row 0 phase = %q, want %q", got, PhaseQueued)
	}
	if got := s.Segments[1].Done; got {
		t.Error("row 1 is marked complete while it is only half written")
	}
}

// The table the dialog draws is the whole reason this exists: bytes have to
// reach the row, and the phase has to say what a connection is doing rather
// than only what it has managed to carry so far.
func TestProgressReportsWhatEachConnectionIsDoing(t *testing.T) {
	p := NewProgress(600)
	p.Attach([]state.Segment{{Index: 0, Start: 0, End: 599}})

	p.SegGet(0)
	if got := p.Snapshot().Segments[0].Phase; got != PhaseGet {
		t.Fatalf("phase after the request = %q, want %q", got, PhaseGet)
	}

	p.SegAdd(0, 300)
	s := p.Snapshot()
	if got := s.Segments[0].Written; got != 300 {
		t.Errorf("written = %d, want 300", got)
	}
	if got := s.Segments[0].Phase; got != PhaseReceiving {
		t.Errorf("phase = %q, want %q", got, PhaseReceiving)
	}
	// The aggregate has to agree with the rows. A progress bar and a table
	// that disagree about the same file is worse than either on its own.
	if s.Done != 300 {
		t.Errorf("aggregate done = %d, want 300", s.Done)
	}

	p.SegDone(0)
	if s := p.Snapshot(); !s.Segments[0].Done {
		t.Error("row 0 is still marked running after SegDone")
	}
}

// Attaching a plan twice replaces it. The single-connection fallback resets
// the transfer's plan mid-flight, and the dialog must show the plan now in
// force rather than the rows of the one that was thrown away.
func TestProgressAttachingAPlanReplacesTheOldOne(t *testing.T) {
	p := NewProgress(300)
	p.Attach([]state.Segment{
		{Index: 0, Start: 0, End: 99},
		{Index: 1, Start: 100, End: 199},
		{Index: 2, Start: 200, End: 299},
	})
	p.SegAdd(1, 50)

	p.Attach([]state.Segment{{Index: 0, Start: 0, End: 299}})
	s := p.Snapshot()
	if len(s.Segments) != 1 {
		t.Fatalf("segments = %d rows, want the plan now in force (1)", len(s.Segments))
	}
	if s.Segments[0].Written != 0 {
		t.Errorf("written = %d, want 0: a reset plan starts again", s.Segments[0].Written)
	}
}

// Nothing attached is the state an idle record is in, not an error: the dialog
// polls whatever the list happens to hold, and every call has to fall through
// quietly rather than panic on a record nobody is transferring.
func TestProgressWithoutAPlanIsQuiet(t *testing.T) {
	p := NewProgress(100)
	p.SegGet(0)
	p.SegAdd(0, 10)
	p.SegDone(0)
	p.Attach(nil)

	if s := p.Snapshot(); len(s.Segments) != 0 {
		t.Errorf("segments = %v, want none", s.Segments)
	}

	var nothing *Progress
	nothing.SegGet(0)
	nothing.SegAdd(0, 10)
	nothing.SegDone(0)
	nothing.Attach([]state.Segment{{Index: 0, Start: 0, End: 9}})
}

// Several connections write at once while the dialog polls them, which is
// exactly how the transfer dialog and the web table both read this. Run with
// -race this is the test that would catch reading state.Segment directly.
func TestProgressIsSafeWhileAPollingDialogReadsIt(t *testing.T) {
	const rows, perRow = 8, 50
	p := NewProgress(int64(rows * perRow * 10))

	segs := make([]state.Segment, rows)
	for i := range segs {
		segs[i] = state.Segment{Index: i, Start: int64(i * 1000), End: int64(i*1000 + 999)}
	}
	p.Attach(segs)

	stop := make(chan struct{})
	var poller sync.WaitGroup
	poller.Add(1)
	go func() {
		defer poller.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = p.Snapshot()
			}
		}
	}()

	var workers sync.WaitGroup
	for i := 0; i < rows; i++ {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			p.SegGet(i)
			for n := 0; n < perRow; n++ {
				p.SegAdd(i, 10)
			}
			p.SegDone(i)
		}(i)
	}
	workers.Wait()
	close(stop)
	poller.Wait()

	s := p.Snapshot()
	if len(s.Segments) != rows {
		t.Fatalf("segments = %d rows, want %d", len(s.Segments), rows)
	}
	var rowBytes int64
	for i, row := range s.Segments {
		if row.Written != perRow*10 {
			t.Errorf("row %d written = %d, want %d", i, row.Written, perRow*10)
		}
		if !row.Done {
			t.Errorf("row %d is not marked done", i)
		}
		rowBytes += row.Written
	}
	if s.Done != rowBytes {
		t.Errorf("aggregate done = %d, but the rows add up to %d", s.Done, rowBytes)
	}
}
