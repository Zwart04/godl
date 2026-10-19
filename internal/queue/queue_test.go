package queue

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"godl/internal/engine"
	"godl/internal/state"
)

func rangedServer(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "file.bin", time.Time{}, bytes.NewReader(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestQueueRunsEveryPendingDownload(t *testing.T) {
	body := make([]byte, 300_000)
	for i := range body {
		body[i] = byte(i * 7)
	}
	srv := rangedServer(t, body)

	dir := t.TempDir()
	store := &state.Store{Dir: filepath.Join(dir, "records")}

	var want []*state.Download
	for i, name := range []string{"one.bin", "two.bin"} {
		dest := filepath.Join(dir, name)
		d := &state.Download{
			ID:        state.NewID(),
			URL:       srv.URL + "/file.bin",
			Dest:      dest,
			PartPath:  dest + ".part",
			TotalSize: int64(len(body)),
			Segmented: true,
			Segments:  state.PlanSegments(int64(len(body)), 4),
			Status:    state.StatusQueued,
			CreatedAt: time.Now(),
		}
		if i == 0 {
			// A finished download must not be picked up again.
			d.Status = state.StatusDone
			d.Dest = filepath.Join(dir, "already-done.bin")
			d.PartPath = d.Dest + ".part"
			if err := store.Save(d); err != nil {
				t.Fatalf("Save: %v", err)
			}
			continue
		}
		if err := store.Save(d); err != nil {
			t.Fatalf("Save: %v", err)
		}
		want = append(want, d)
	}

	// A paused download is held, not work to start: the scheduler used to pick
	// these up too, which is what made the Pause button undo itself within half
	// a second. It must be left exactly as it was found.
	pausedDest := filepath.Join(dir, "paused.bin")
	paused := &state.Download{
		ID:        state.NewID(),
		URL:       srv.URL + "/file.bin",
		Dest:      pausedDest,
		PartPath:  pausedDest + ".part",
		TotalSize: int64(len(body)),
		Segmented: true,
		Segments:  state.PlanSegments(int64(len(body)), 2),
		Status:    state.StatusPaused,
		CreatedAt: time.Now(),
	}
	paused.Segments[0].Written = paused.Segments[0].Len()
	paused.Segments[0].Done = true
	seedPart(t, paused.PartPath, paused.TotalSize, body[:paused.Segments[1].Start])
	if err := store.Save(paused); err != nil {
		t.Fatalf("Save: %v", err)
	}

	q := New(store, 2)
	if err := q.Start(context.Background(), engine.Options{Connections: 4}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var started, finished, failed int
	for _, ev := range q.Drain() {
		switch ev.Kind {
		case "started":
			started++
		case "finished":
			finished++
		case "failed":
			failed++
			t.Errorf("download %s failed: %v", ev.ID, ev.Err)
		}
	}
	if started != len(want) || finished != len(want) {
		t.Fatalf("got %d started and %d finished events, want %d of each", started, finished, len(want))
	}
	if q.Running() {
		t.Fatal("the queue still reports itself as running after Start returned")
	}

	for _, submitted := range want {
		// The queue works on its own copies loaded from disk, so the outcome
		// has to be read back from the store rather than from the structs the
		// test submitted.
		d, err := store.Load(submitted.ID)
		if err != nil {
			t.Fatalf("load %s: %v", submitted.ID, err)
		}
		if d.Status != state.StatusDone {
			t.Fatalf("%s: status = %q, want %q", d.ID, d.Status, state.StatusDone)
		}
		got, err := os.ReadFile(d.Dest)
		if err != nil {
			t.Fatalf("read %s: %v", d.Dest, err)
		}
		if !bytes.Equal(got, body) {
			t.Fatalf("%s does not match the source", d.Dest)
		}
	}

	// The held record is untouched: same status, same partial file still on
	// disk with the bytes it had, and no destination published.
	held, err := store.Load(paused.ID)
	if err != nil {
		t.Fatalf("load paused: %v", err)
	}
	if held.Status != state.StatusPaused {
		t.Fatalf("paused download restarted itself: status = %q", held.Status)
	}
	if _, err := os.Stat(held.PartPath); err != nil {
		t.Fatalf("a held download lost its partial file: %v", err)
	}
	if _, err := os.Stat(held.Dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a held download published a file to its destination")
	}

	// A second Start must be a no-op: nothing is left to do.
	if err := q.Start(context.Background(), engine.Options{Connections: 4}); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if evs := q.Drain(); len(evs) != 0 {
		t.Fatalf("second Start emitted %d events, want none", len(evs))
	}
}

func TestQueueStopIsNotAFailure(t *testing.T) {
	body := make([]byte, 4_000_000)
	srv := rangedServer(t, body)

	dir := t.TempDir()
	store := &state.Store{Dir: filepath.Join(dir, "records")}
	dest := filepath.Join(dir, "slow.bin")
	d := &state.Download{
		ID:        state.NewID(),
		URL:       srv.URL + "/file.bin",
		Dest:      dest,
		PartPath:  dest + ".part",
		TotalSize: int64(len(body)),
		Segmented: true,
		Segments:  state.PlanSegments(int64(len(body)), 4),
		Status:    state.StatusQueued,
		// Throttled on the record, which is how a real limit reaches the
		// engine: it keeps the transfer in flight long enough for Stop to
		// land mid-flight rather than after the file has finished.
		SpeedLimit: 256 * 1024,
		CreatedAt:  time.Now(),
	}
	if err := store.Save(d); err != nil {
		t.Fatalf("Save: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	q := New(store, 1)

	done := make(chan error, 1)
	go func() {
		// The throttle lives on the record above, so this exercises the same
		// path a user's setting takes.
		done <- q.Start(ctx, engine.Options{Connections: 4})
	}()

	time.Sleep(150 * time.Millisecond)
	q.Stop()
	cancel()

	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Start after Stop: %v", err)
	}

	// The record must survive as paused, with progress kept, and no file at
	// the destination. "running" would strand it, because a record only ever
	// starts when the queue reads it as queued - and a transfer the user
	// stopped has to come back as something they must ask for again.
	saved, err := store.Load(d.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if saved.Status != state.StatusPaused {
		t.Fatalf("status after Stop = %q, want %q", saved.Status, state.StatusPaused)
	}
	if saved.Completed() <= 0 {
		t.Fatal("a paused download lost its progress")
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a paused download published a file to the destination")
	}
}

// A ceiling belongs to the record, but the control the interface offers is
// only useful if it bites on the transfer already running. This asserts both
// halves: that the new value reaches disk, and that the running worker starts
// obeying it without being restarted.
func TestSpeedLimitCanBeChangedWhileTheTransferRuns(t *testing.T) {
	body := make([]byte, 400_000)
	for i := range body {
		body[i] = byte(i % 251)
	}
	srv := rangedServer(t, body)

	dir := t.TempDir()
	store := &state.Store{Dir: filepath.Join(dir, "records")}
	dest := filepath.Join(dir, "capped.bin")
	d := &state.Download{
		ID:        state.NewID(),
		URL:       srv.URL + "/file.bin",
		Dest:      dest,
		PartPath:  dest + ".part",
		TotalSize: int64(len(body)),
		Segmented: true,
		Segments:  state.PlanSegments(int64(len(body)), 4),
		Status:    state.StatusQueued,
		// One byte per second. The transfer cannot finish while this holds,
		// which is what makes the assertions below deterministic.
		SpeedLimit: 1,
		CreatedAt:  time.Now(),
	}
	if err := store.Save(d); err != nil {
		t.Fatalf("Save: %v", err)
	}

	q := New(store, 1)

	waitFor := func(what string, timeout time.Duration, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			if ok() {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	lookup := func(id string) (Snapshot, bool) {
		for _, s := range q.Snapshot() {
			if s.ID == id {
				return s, true
			}
		}
		return Snapshot{}, false
	}

	runDone := make(chan error, 1)
	go func() { runDone <- q.Start(context.Background(), engine.Options{Connections: 4}) }()

	waitFor("the transfer to start", 5*time.Second, func() bool { return q.liveEntry(d.ID) != nil })

	e := q.liveEntry(d.ID)
	if got := e.limit.Get(); got != 1 {
		t.Fatalf("running ceiling = %d, want the record's 1", got)
	}
	if snap, ok := lookup(d.ID); !ok || snap.SpeedLimit != 1 {
		t.Fatalf("snapshot speed_limit = %v/%d, want 1", ok, snap.SpeedLimit)
	}

	// Move it. The record must follow, and so must the bucket the worker
	// actually draws from.
	if err := q.SetLimit(d.ID, 512*1024); err != nil {
		t.Fatalf("SetLimit: %v", err)
	}
	if got := e.limit.Get(); got != 512*1024 {
		t.Fatalf("running ceiling after SetLimit = %d, want %d: the change did not reach the worker", got, 512*1024)
	}
	if saved, err := store.Load(d.ID); err != nil || saved.SpeedLimit != 512*1024 {
		t.Fatalf("record after SetLimit = %v/%d, want 512*1024", err, safeSpeed(saved))
	}
	if snap, ok := lookup(d.ID); !ok || snap.SpeedLimit != 512*1024 {
		t.Fatalf("snapshot after SetLimit = %v/%d, want %d", ok, snap.SpeedLimit, 512*1024)
	}

	// Lifting the ceiling must be what lets it finish - proof that the running
	// worker is reading the value rather than the one it was launched with.
	if err := q.SetLimit(d.ID, 0); err != nil {
		t.Fatalf("SetLimit(0): %v", err)
	}
	if err := <-runDone; err != nil {
		t.Fatalf("Start: %v", err)
	}

	final, err := store.Load(d.ID)
	if err != nil {
		t.Fatalf("Load after completion: %v", err)
	}
	if final.Status != state.StatusDone {
		t.Fatalf("status = %q, want %q: the download never finished, so lifting the ceiling did nothing",
			final.Status, state.StatusDone)
	}
	if final.SpeedLimit != 0 {
		t.Fatalf("recorded speed_limit = %d, want 0", final.SpeedLimit)
	}
	got, err := os.ReadFile(final.Dest)
	if err != nil {
		t.Fatalf("read %s: %v", final.Dest, err)
	}
	if !bytes.Equal(got, body) {
		t.Fatal("the file does not match the source after the limit was moved")
	}
}

// safeSpeed reports a download's limit without dereferencing a nil record, so
// the assertions above stay readable when the load itself failed.
func safeSpeed(d *state.Download) int64 {
	if d == nil {
		return -1
	}
	return d.SpeedLimit
}

// Pause has to hold.
//
// The scheduler is deliberately willing to start a paused record: that is what
// lets a queue drain past one download, and what makes a record resumable after
// a restart. So a pause that merely stops the current worker would be undone by
// the very next pass, well under a second later, and the button would look
// like it had done nothing at all.
func TestPauseHoldsAgainstTheScheduler(t *testing.T) {
	body := make([]byte, 4<<20)
	for i := range body {
		body[i] = byte(i * 13)
	}
	srv := rangedServer(t, body)

	dir := t.TempDir()
	store := &state.Store{Dir: filepath.Join(dir, "records")}
	dest := filepath.Join(dir, "slow.bin")
	d := &state.Download{
		ID:        state.NewID(),
		URL:       srv.URL + "/file.bin",
		Dest:      dest,
		PartPath:  dest + ".part",
		TotalSize: int64(len(body)),
		Segmented: true,
		Segments:  state.PlanSegments(int64(len(body)), 4),
		Status:    state.StatusQueued,
		// One kilobyte a second, so the transfer is still under way long after
		// the scheduler has had several chances to notice it stop.
		SpeedLimit: 1024,
		CreatedAt:  time.Now(),
	}
	if err := store.Save(d); err != nil {
		t.Fatalf("Save: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := New(store, 1)
	go func() { _ = q.Start(ctx, engine.Options{Connections: 4}) }()
	defer q.Stop()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !q.busy(d.ID) {
		time.Sleep(20 * time.Millisecond)
	}
	if !q.busy(d.ID) {
		t.Fatal("the download never started")
	}

	// And wait for a byte to have really moved. Pausing the instant the
	// transfer is registered would leave the record with nothing to keep, and
	// the assertion further down would then be about scheduling luck rather
	// than about Pause - which is a flake, not a test.
	//
	// It is read from the live transfer rather than from disk: the record is
	// written periodically, so disk would put the clock back into it.
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && progressOf(q, d.ID) <= 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if got := progressOf(q, d.ID); got <= 0 {
		t.Fatalf("the download never moved a byte (progress = %d)", got)
	}

	if err := q.Pause(d.ID); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	// Several scheduler passes rather than one, because one pass could be
	// caught in the gap between stopping the worker and writing its record.
	time.Sleep(1500 * time.Millisecond)

	saved, err := store.Load(d.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if saved.Status != state.StatusPaused {
		t.Fatalf("status = %q after Pause, want %q - the scheduler restarted it",
			saved.Status, state.StatusPaused)
	}
	if q.busy(d.ID) {
		t.Fatal("a paused download is still being transferred")
	}
	if saved.Completed() <= 0 {
		t.Fatal("the pause discarded the progress it should have kept")
	}
}

// A failure the user did not ask to retry has to wait for them.
//
// The scheduler started failed records on every pass, so a host that could not
// be reached was retried as fast as the network could answer it: one probe
// after another for as long as the app stayed open. Retry exists precisely
// because retrying is a decision someone makes.
func TestAFailedDownloadWaitsForRetry(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	store := &state.Store{Dir: filepath.Join(dir, "records")}
	dest := filepath.Join(dir, "never.bin")
	d := &state.Download{
		ID:        state.NewID(),
		URL:       srv.URL + "/never.bin",
		Dest:      dest,
		PartPath:  dest + ".part",
		TotalSize: 1 << 20,
		Segments:  state.PlanSegments(1<<20, 4),
		Status:    state.StatusQueued,
		CreatedAt: time.Now(),
	}
	if err := store.Save(d); err != nil {
		t.Fatalf("Save: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := New(store, 1)
	go func() { _ = q.Start(ctx, engine.Options{Connections: 4}) }()
	defer q.Stop()

	time.Sleep(1200 * time.Millisecond)

	saved, err := store.Load(d.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if saved.Status != state.StatusFailed {
		t.Fatalf("status = %q after a rejected transfer, want %q", saved.Status, state.StatusFailed)
	}
	if q.busy(d.ID) {
		t.Fatal("the failed download is still being transferred")
	}
	if n := hits.Load(); n > 6 {
		t.Fatalf("%d requests reached the server in one attempt window - the scheduler is retrying it", n)
	}
}

// The list is the promise. A download moved up has to be the one the queue
// starts next, not merely the one that moved inside the window.
func TestMoveDecidesWhatRunsNext(t *testing.T) {
	body := make([]byte, 400_000)
	for i := range body {
		body[i] = byte(i * 3)
	}
	srv := rangedServer(t, body)

	dir := t.TempDir()
	store := &state.Store{Dir: filepath.Join(dir, "records")}

	base := time.Now()
	mk := func(name string, age time.Duration) *state.Download {
		dest := filepath.Join(dir, name)
		d := &state.Download{
			ID:        state.NewID(),
			URL:       srv.URL + "/file.bin",
			Dest:      dest,
			PartPath:  dest + ".part",
			TotalSize: int64(len(body)),
			Segmented: true,
			Segments:  state.PlanSegments(int64(len(body)), 2),
			Status:    state.StatusQueued,
			// Spaced apart so the arrival order is a fact rather than a
			// tiebreak the clock might settle either way.
			CreatedAt: base.Add(age),
		}
		if err := store.Save(d); err != nil {
			t.Fatalf("Save %s: %v", name, err)
		}
		return d
	}
	a := mk("a.bin", 0)
	b := mk("b.bin", time.Second)
	c := mk("c.bin", 2*time.Second)

	q := New(store, 1)
	if err := q.MoveUp(c.ID); err != nil {
		t.Fatalf("first MoveUp: %v", err)
	}
	if err := q.MoveUp(c.ID); err != nil {
		t.Fatalf("second MoveUp: %v", err)
	}

	if got, want := orderOf(q), []string{c.ID, a.ID, b.ID}; !equalIDs(got, want) {
		t.Fatalf("list order = %v, want %v", got, want)
	}

	// Moving the first one up has nowhere to go, and says so as much rather
	// than reporting a server fault.
	if err := q.MoveUp(c.ID); !errors.Is(err, ErrAtEnd) {
		t.Fatalf("MoveUp at the head = %v, want ErrAtEnd", err)
	}
	if err := q.MoveDown(b.ID); !errors.Is(err, ErrAtEnd) {
		t.Fatalf("MoveDown at the foot = %v, want ErrAtEnd", err)
	}

	if err := q.Start(context.Background(), engine.Options{Connections: 4}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	var ran []string
	for _, ev := range q.Drain() {
		if ev.Kind == "started" {
			ran = append(ran, ev.ID)
		}
	}
	if !equalIDs(ran, []string{c.ID, a.ID, b.ID}) {
		t.Fatalf("ran in order %v, want %v", ran, []string{c.ID, a.ID, b.ID})
	}
}

// A download set for later waits for its clock, and does not hold the queue
// open in the meantime.
func TestScheduleHoldsUntilItsTime(t *testing.T) {
	body := make([]byte, 300_000)
	for i := range body {
		body[i] = byte(i * 11)
	}
	srv := rangedServer(t, body)

	dir := t.TempDir()
	store := &state.Store{Dir: filepath.Join(dir, "records")}
	dest := filepath.Join(dir, "later.bin")
	d := &state.Download{
		ID:        state.NewID(),
		URL:       srv.URL + "/file.bin",
		Dest:      dest,
		PartPath:  dest + ".part",
		TotalSize: int64(len(body)),
		Segmented: true,
		Segments:  state.PlanSegments(int64(len(body)), 4),
		Status:    state.StatusQueued,
		CreatedAt: time.Now(),
	}
	if err := store.Save(d); err != nil {
		t.Fatalf("Save: %v", err)
	}

	q := New(store, 1)
	future := time.Now().Add(time.Hour)
	if err := q.Schedule(d.ID, &future); err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	// Start has to come back on its own. If a future schedule were counted as
	// work waiting for a slot, the loop would sit here until the clock came
	// round - an hour of a caller that asked to drain the queue.
	done := make(chan error, 1)
	go func() { done <- q.Start(context.Background(), engine.Options{Connections: 4}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start is waiting for a download scheduled an hour from now")
	}

	saved, err := store.Load(d.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if saved.Status != state.StatusQueued {
		t.Fatalf("status = %q, want the scheduled download left %q", saved.Status, state.StatusQueued)
	}
	if saved.StartsAt == nil || !saved.StartsAt.Equal(future) {
		t.Fatalf("starts_at = %v, want %v", saved.StartsAt, future)
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a download scheduled for an hour away downloaded now")
	}

	// Clearing the schedule is what makes it start.
	if err := q.Schedule(d.ID, nil); err != nil {
		t.Fatalf("clear schedule: %v", err)
	}
	if err := q.Start(context.Background(), engine.Options{Connections: 4}); err != nil {
		t.Fatalf("Start after clearing: %v", err)
	}
	saved, err = store.Load(d.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if saved.Status != state.StatusDone {
		t.Fatalf("status = %q after the schedule was cleared, want %q", saved.Status, state.StatusDone)
	}
	if saved.StartsAt != nil {
		t.Fatal("the schedule was not cleared from the record")
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatal("the scheduled download does not match the source")
	}
}

// A category is a claim about where a file belongs, so changing one has to
// move the file, not merely the label. Nothing is written over: the new path
// is worked out against what is really on disk before anything moves.
func TestChangingACategoryMovesTheFileThatWasThere(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(dir, "Other")
	music := filepath.Join(dir, "Music")

	store := &state.Store{Dir: filepath.Join(dir, "records")}
	dest := filepath.Join(other, "song.flac")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// The bytes are already there, as they are for a finished download.
	if err := os.WriteFile(dest, []byte("ID3 bytes"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	d := &state.Download{
		ID:        state.NewID(),
		URL:       "https://example.com/song.flac",
		Dest:      dest,
		PartPath:  dest + ".part",
		Category:  "Other",
		Status:    state.StatusDone,
		CreatedAt: time.Now(),
	}
	if err := store.Save(d); err != nil {
		t.Fatalf("Save: %v", err)
	}

	q := New(store, 1)
	if err := q.SetCategory(d.ID, "Music", music); err != nil {
		t.Fatalf("SetCategory: %v", err)
	}

	moved := filepath.Join(music, "song.flac")
	got, err := os.ReadFile(moved)
	if err != nil {
		t.Fatalf("the file did not move: %v", err)
	}
	if string(got) != "ID3 bytes" {
		t.Fatalf("the move changed the contents to %q", got)
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the file was left behind at its old path")
	}

	saved, err := store.Load(d.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if saved.Category != "Music" {
		t.Errorf("category = %q, want %q", saved.Category, "Music")
	}
	if saved.Dest != moved {
		t.Errorf("dest = %q, want the file's new home %q", saved.Dest, moved)
	}
	if saved.PartPath != moved+".part" {
		t.Errorf("part_path = %q, want it beside the moved file", saved.PartPath)
	}
}

// Nothing has been written yet for a download still waiting its turn, so
// changing its category is a matter of rewriting where it will go.
func TestChangingACategoryOfAQueuedDownloadTouchesNoFile(t *testing.T) {
	dir := t.TempDir()
	store := &state.Store{Dir: filepath.Join(dir, "records")}
	dest := filepath.Join(dir, "Other", "setup.exe")

	d := &state.Download{
		ID:        state.NewID(),
		URL:       "https://example.com/setup.exe",
		Dest:      dest,
		PartPath:  dest + ".part",
		Category:  "Other",
		Status:    state.StatusQueued,
		CreatedAt: time.Now(),
	}
	if err := store.Save(d); err != nil {
		t.Fatalf("Save: %v", err)
	}

	q := New(store, 1)
	want := filepath.Join(dir, "Programs")
	if err := q.SetCategory(d.ID, "Programs", want); err != nil {
		t.Fatalf("SetCategory: %v", err)
	}

	saved, err := store.Load(d.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if saved.Dest != filepath.Join(want, "setup.exe") {
		t.Errorf("dest = %q, want it under %q", saved.Dest, want)
	}
	if saved.Status != state.StatusQueued {
		t.Errorf("status = %q, want a queued download to stay queued", saved.Status)
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an old path appeared where nothing had been written")
	}
}

// The worker has the file open, and Windows will not rename a file that is
// open. Refusing is the honest answer; half-moving a transfer under way is not.
func TestChangingACategoryRefusesATransferUnderWay(t *testing.T) {
	body := make([]byte, 4_000_000)
	srv := rangedServer(t, body)

	dir := t.TempDir()
	store := &state.Store{Dir: filepath.Join(dir, "records")}
	dest := filepath.Join(dir, "Other", "slow.bin")
	// handleAdd creates the category's folder before writing the record; this
	// is that, so the test is about the guard rather than about directories.
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	d := &state.Download{
		ID:        state.NewID(),
		URL:       srv.URL + "/file.bin",
		Dest:      dest,
		PartPath:  dest + ".part",
		Category:  "Other",
		TotalSize: int64(len(body)),
		Segmented: true,
		Segments:  state.PlanSegments(int64(len(body)), 4),
		Status:    state.StatusQueued,
		// Kept in flight long enough for the call to land mid-transfer.
		SpeedLimit: 256 * 1024,
		CreatedAt:  time.Now(),
	}
	if err := store.Save(d); err != nil {
		t.Fatalf("Save: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	q := New(store, 1)
	done := make(chan error, 1)
	go func() { done <- q.Start(ctx, engine.Options{Connections: 4}) }()

	time.Sleep(150 * time.Millisecond)

	// The refusal only means something if there really is a transfer to refuse
	// for. Without this the test could pass or fail on a download that never
	// started, which would say nothing at all about the guard.
	before, err := store.Load(d.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if before.Status != state.StatusRunning {
		t.Fatalf("status = %q, want %q - the download was not running, so this proves nothing",
			before.Status, state.StatusRunning)
	}

	err = q.SetCategory(d.ID, "Music", filepath.Join(dir, "Music"))
	if !errors.Is(err, ErrInFlight) {
		t.Fatalf("SetCategory while running = %v, want ErrInFlight", err)
	}

	// The refusal must leave the record exactly as it was. Writing part of it
	// before noticing the transfer was running is what makes a half-moved
	// download that no state can be resumed from.
	saved, err := store.Load(d.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if saved.Dest != dest {
		t.Errorf("dest = %q, want the refused call to leave it at %q", saved.Dest, dest)
	}
	if saved.Category != "Other" {
		t.Errorf("category = %q, want the refused call to leave it alone", saved.Category)
	}

	q.Stop()
	cancel()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Start after Stop: %v", err)
	}
}

// A category folder is exactly the sort of thing someone tidies away while
// downloads still point at it. The download should simply make it again.
func TestStartingADownloadRecreatesTheFolderItBelongsIn(t *testing.T) {
	body := make([]byte, 100_000)
	srv := rangedServer(t, body)

	dir := t.TempDir()
	store := &state.Store{Dir: filepath.Join(dir, "records")}
	dest := filepath.Join(dir, "Programs", "app.bin")
	d := &state.Download{
		ID:        state.NewID(),
		URL:       srv.URL + "/file.bin",
		Dest:      dest,
		PartPath:  dest + ".part",
		TotalSize: int64(len(body)),
		Segmented: true,
		Segments:  state.PlanSegments(int64(len(body)), 4),
		Status:    state.StatusQueued,
		CreatedAt: time.Now(),
	}
	if err := store.Save(d); err != nil {
		t.Fatalf("Save: %v", err)
	}

	q := New(store, 1)
	if err := q.Start(context.Background(), engine.Options{Connections: 4}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("the folder was not made again, so the download did not land: %v", err)
	}
}

// A folder that cannot be created will not appear by the next pass either, so
// the record has to be recorded as failed. Leaving it queued would have the
// scheduler attempting it again for as long as the app stayed open - the same
// shape as a host that cannot be reached, and just as pointless.
func TestADownloadWhoseFolderCannotBeMadeFailsOnceNotForever(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	store := &state.Store{Dir: filepath.Join(dir, "records")}

	// A plain file sitting where the category folder should be. MkdirAll
	// cannot work past that, which is the whole point of this case.
	blocked := filepath.Join(dir, "Programs")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	dest := filepath.Join(blocked, "app.bin")
	d := &state.Download{
		ID:        state.NewID(),
		URL:       srv.URL + "/file.bin",
		Dest:      dest,
		PartPath:  dest + ".part",
		TotalSize: 1 << 20,
		Segments:  state.PlanSegments(1<<20, 4),
		Status:    state.StatusQueued,
		CreatedAt: time.Now(),
	}
	if err := store.Save(d); err != nil {
		t.Fatalf("Save: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := New(store, 1)
	go func() { _ = q.Start(ctx, engine.Options{Connections: 4}) }()
	defer q.Stop()

	time.Sleep(1200 * time.Millisecond)

	saved, err := store.Load(d.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if saved.Status != state.StatusFailed {
		t.Fatalf("status = %q, want %q - a queued record is one the scheduler keeps trying",
			saved.Status, state.StatusFailed)
	}
	if saved.Error == "" {
		t.Fatal("the failure was recorded without saying why")
	}
	if q.busy(d.ID) {
		t.Fatal("the download is still being transferred")
	}
	if n := hits.Load(); n > 0 {
		t.Fatalf("%d requests reached the server - it failed before ever opening a connection", n)
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a file appeared somewhere it should not")
	}
}

// progressOf reports how many bytes a download has really moved, read from the
// live transfer rather than from the record - the record is written
// periodically, so reading it would race the clock instead of asking the
// transfer. It answers -1 for a download the queue has no live entry for.
func progressOf(q *Queue, id string) int64 {
	for _, s := range q.Snapshot() {
		if s.ID == id {
			return s.Progress.Done
		}
	}
	return -1
}

func orderOf(q *Queue) []string {
	out := make([]string, 0, 4)
	for _, s := range q.Snapshot() {
		out = append(out, s.ID)
	}
	return out
}

func equalIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func seedPart(t *testing.T, path string, total int64, prefix []byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create part file: %v", err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Truncate(total); err != nil {
		t.Fatalf("size part file: %v", err)
	}
	if _, err := f.WriteAt(prefix, 0); err != nil {
		t.Fatalf("seed part file: %v", err)
	}
}
