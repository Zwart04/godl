package queue

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

	// A paused download is treated as work to resume, so it is queued too.
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
	want = append(want, paused)

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

	// The record must be resumable: paused, with progress kept, and no file at
	// the destination. "running" would strand the download, because the queue
	// only ever picks up queued, paused and failed records.
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
