// Package engine tests the transfer engine against in-process HTTP servers.
//
// The behaviours exercised here are the ones the IDMan.exe analysis motivated:
// ranged requests that resume at a recorded offset, cross-segment integrity
// checking, and the degradation to a single connection when a server serves
// inconsistent bytes to different connections.
package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"godl/internal/state"
)

// bodySize is several chunks long on purpose: the read loop in fetchSegment
// iterates, and the 64-byte overlap discard has to cross a read boundary.
const bodySize = 700_000

func testBody(n int) []byte {
	b := make([]byte, n)
	// A fixed seed keeps a failing test reproducible.
	rnd := rand.New(rand.NewSource(1234))
	if _, err := rnd.Read(b); err != nil {
		panic(err)
	}
	return b
}

// ranged serves body with full Range support, the way a well-behaved origin does.
func ranged(body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "file.bin", time.Time{}, bytes.NewReader(body))
	}
}

// unranged ignores Range and always sends the whole file, which is what forces
// a single-connection download.
func unranged(body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}
}

// rangeRecorder records what the engine actually asked for, so a test can prove
// a resumed run did not start over from byte 0.
type rangeRecorder struct {
	mu          sync.Mutex
	headers     []string
	inflight    int
	maxInflight int
}

func (rec *rangeRecorder) wrap(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.headers = append(rec.headers, r.Header.Get("Range"))
		rec.inflight++
		if rec.inflight > rec.maxInflight {
			rec.maxInflight = rec.inflight
		}
		rec.mu.Unlock()
		defer func() {
			rec.mu.Lock()
			rec.inflight--
			rec.mu.Unlock()
		}()
		h(w, r)
	}
}

func (rec *rangeRecorder) ranges() []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]string(nil), rec.headers...)
}

func (rec *rangeRecorder) peak() int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.maxInflight
}

// parseRangeHeader reads "bytes=<start>-<end>". An open-ended "bytes=N-" is
// reported with end -1, matching net/http's own convention.
func parseRangeHeader(h string) (start, end int64, ok bool) {
	if !strings.HasPrefix(h, "bytes=") {
		return 0, 0, false
	}
	spec := strings.TrimPrefix(h, "bytes=")
	dash := strings.Index(spec, "-")
	if dash < 0 {
		return 0, 0, false
	}
	s, err := strconv.ParseInt(strings.TrimSpace(spec[:dash]), 10, 64)
	if err != nil {
		return 0, 0, false
	}
	e := int64(-1)
	if rest := strings.TrimSpace(spec[dash+1:]); rest != "" {
		e, err = strconv.ParseInt(rest, 10, 64)
		if err != nil {
			return 0, 0, false
		}
	}
	return s, e, true
}

func newDownload(url, dest string, size int64, connections int, segmented bool) *state.Download {
	n := connections
	if !segmented {
		n = 1
	}
	return &state.Download{
		ID:        state.NewID(),
		URL:       url,
		Dest:      dest,
		PartPath:  dest + ".part",
		TotalSize: size,
		Segmented: segmented,
		Segments:  state.PlanSegments(size, n),
		Status:    state.StatusQueued,
		CreatedAt: time.Now(),
	}
}

func seedPartFile(t *testing.T, path string, total int64, prefix []byte) {
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

func assertFileEquals(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s does not match the source: %d bytes on disk, %d expected", path, len(got), len(want))
	}
}

func TestProbeReportsSizeAndRangeSupport(t *testing.T) {
	body := testBody(4096)
	srv := httptest.NewServer(ranged(body))
	defer srv.Close()

	got, err := Probe(context.Background(), &http.Client{}, srv.URL+"/file.bin")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if got.TotalSize != int64(len(body)) {
		t.Fatalf("TotalSize = %d, want %d", got.TotalSize, len(body))
	}
	if !got.Segmented {
		t.Fatal("a server honouring Range must be reported as segmented")
	}
}

func TestProbeRecognisesServerWithoutRangeSupport(t *testing.T) {
	body := testBody(4096)
	srv := httptest.NewServer(unranged(body))
	defer srv.Close()

	got, err := Probe(context.Background(), &http.Client{}, srv.URL+"/file.bin")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if got.Segmented {
		t.Fatal("a server ignoring Range must not be reported as segmented")
	}
	if got.TotalSize != int64(len(body)) {
		t.Fatalf("TotalSize = %d, want %d", got.TotalSize, len(body))
	}
}

func TestSegmentedDownloadIsByteExact(t *testing.T) {
	body := testBody(bodySize)
	rec := &rangeRecorder{}
	srv := httptest.NewServer(rec.wrap(ranged(body)))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "out.bin")
	d := newDownload(srv.URL+"/file.bin", dest, int64(len(body)), 8, true)

	rep, err := (&Runner{}).Run(context.Background(), d, Options{Connections: 8})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.FallbackUsed {
		t.Fatalf("unexpected fallback: %s", rep.FallbackWhy)
	}
	if rep.Bytes != int64(len(body)) {
		t.Fatalf("report says %d bytes, want %d", rep.Bytes, len(body))
	}
	assertFileEquals(t, dest, body)

	if _, err := os.Stat(d.PartPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the part file survived a successful publish")
	}
	if d.Status != state.StatusDone {
		t.Fatalf("status = %q, want %q", d.Status, state.StatusDone)
	}
}

func TestResumeContinuesFromRecordedProgress(t *testing.T) {
	body := testBody(bodySize)
	rec := &rangeRecorder{}
	srv := httptest.NewServer(rec.wrap(ranged(body)))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "out.bin")
	d := newDownload(srv.URL+"/file.bin", dest, int64(len(body)), 4, true)

	// Pretend a previous run finished segment 0 and stopped partway into
	// segment 1. The bytes already on disk are the real ones, as a previous
	// run would have left them.
	const advanced = 5000
	d.Segments[0].Written = d.Segments[0].Len()
	d.Segments[0].Done = true
	d.Segments[1].Written = advanced
	seedPartFile(t, d.PartPath, d.TotalSize, body[:d.Segments[1].Start+advanced])

	rep, err := (&Runner{}).Run(context.Background(), d, Options{Connections: 4})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.FallbackUsed {
		t.Fatalf("unexpected fallback: %s", rep.FallbackWhy)
	}
	assertFileEquals(t, dest, body)

	// Nothing may be re-downloaded from the beginning: every request has to ask
	// for a range above byte 0.
	for _, h := range rec.ranges() {
		start, _, ok := parseRangeHeader(h)
		if !ok {
			t.Fatalf("request without a usable Range header: %q", h)
		}
		if start == 0 {
			t.Fatalf("resumed run re-requested from byte 0: %q", h)
		}
	}

	// And the half-finished segment resumes at the overlap-adjusted offset: the
	// request starts overlapLen bytes before the first byte still missing.
	want := fmt.Sprintf("bytes=%d-%d", d.Segments[1].Start-overlapLen+advanced, d.Segments[1].End)
	var found bool
	for _, h := range rec.ranges() {
		if h == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("segment 1 did not resume at %d; saw %v", d.Segments[1].Start+advanced, rec.ranges())
	}
}

func TestServerWithoutRangeSupportUsesOneConnection(t *testing.T) {
	body := testBody(bodySize)
	rec := &rangeRecorder{}
	srv := httptest.NewServer(rec.wrap(unranged(body)))
	defer srv.Close()

	// Probe first, the way "godl add" does, then watch only the transfer.
	probe, err := Probe(context.Background(), &http.Client{}, srv.URL+"/file.bin")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if probe.Segmented {
		t.Fatal("test server was expected to reject ranges")
	}
	before := len(rec.ranges())

	dir := t.TempDir()
	dest := filepath.Join(dir, "out.bin")
	d := newDownload(srv.URL+"/file.bin", dest, probe.TotalSize, 8, false)

	// Eight connections are requested but the server cannot honour ranges.
	if _, err := (&Runner{}).Run(context.Background(), d, Options{Connections: 8}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	assertFileEquals(t, dest, body)
	if got := len(d.Segments); got != 1 {
		t.Fatalf("planned %d segments against a server without range support", got)
	}
	if peak := rec.peak(); peak != 1 {
		t.Fatalf("peak concurrent requests = %d, want 1", peak)
	}
	for _, h := range rec.ranges()[before:] {
		if h != "" {
			t.Fatalf("sent a Range header to a server that cannot honour it: %q", h)
		}
	}
}

// inconsistent serves different bytes the first time a range above byte 0 is
// requested. That is the corruption IDM reacts to: a parallel download whose
// seams no longer agree with the origin.
type inconsistent struct {
	body []byte
	mu   sync.Mutex
	seen map[string]int
}

func (s *inconsistent) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Range")
		start, end, ok := parseRangeHeader(h)
		if !ok {
			http.Error(w, "range required", http.StatusBadRequest)
			return
		}
		if end < 0 || end > int64(len(s.body))-1 {
			end = int64(len(s.body)) - 1
		}
		data := append([]byte(nil), s.body[start:end+1]...)

		s.mu.Lock()
		s.seen[h]++
		first := s.seen[h] == 1
		s.mu.Unlock()

		// The first request for a download-sized range gets its final
		// overlapLen bytes replaced. That region is exactly the seam the next
		// segment's verifier re-reads, so the assembled file no longer agrees
		// with the origin — the corruption IDM reacts to. The 64-byte seam
		// probes are never touched, and repeats are served correctly, so the
		// single-connection retry can produce a good file.
		if first && start > 0 && len(data) > overlapLen {
			tail := data[len(data)-overlapLen:]
			for i := range tail {
				tail[i] ^= 0xff
			}
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(s.body)))
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data)
	}
}

func TestInconsistentServerTriggersSingleConnectionFallback(t *testing.T) {
	body := testBody(bodySize)
	rec := &rangeRecorder{}
	sim := &inconsistent{body: body, seen: map[string]int{}}
	srv := httptest.NewServer(rec.wrap(sim.handler()))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "out.bin")
	d := newDownload(srv.URL+"/file.bin", dest, int64(len(body)), 4, true)

	rep, err := (&Runner{}).Run(context.Background(), d, Options{Connections: 4})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !rep.FallbackUsed {
		t.Fatal("inconsistent per-connection data did not trigger the single-connection fallback")
	}
	if rep.FallbackWhy == "" {
		t.Fatal("the fallback was recorded without a reason")
	}
	if !d.SingleConnectionFallback || d.FallbackReason == "" {
		t.Fatal("the fallback was not persisted on the download record")
	}
	assertFileEquals(t, dest, body)

	// Every download-sized range must have been asked for twice: once on the
	// parallel attempt and once on the retry. The 64-byte seam probes are the
	// only requests that are not re-issued.
	counts := map[string]int{}
	for _, h := range rec.ranges() {
		counts[h]++
	}
	for h, n := range counts {
		start, end, ok := parseRangeHeader(h)
		if !ok || start == 0 || end < 0 {
			continue
		}
		if end-start+1 <= overlapLen {
			continue // a seam probe
		}
		if n < 2 {
			t.Fatalf("range %q was requested %d time(s) instead of being retried", h, n)
		}
	}
}

// dropping truncates every download-sized response while its flag is set,
// standing in for a connection that dies mid-transfer.
type dropping struct {
	body []byte
	mu   sync.Mutex
	drop bool
}

func (s *dropping) setDrop(v bool) {
	s.mu.Lock()
	s.drop = v
	s.mu.Unlock()
}

func (s *dropping) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start, end, ok := parseRangeHeader(r.Header.Get("Range"))
		if !ok {
			http.Error(w, "range required", http.StatusBadRequest)
			return
		}
		if end < 0 || end > int64(len(s.body))-1 {
			end = int64(len(s.body)) - 1
		}
		data := s.body[start : end+1]

		s.mu.Lock()
		drop := s.drop
		s.mu.Unlock()

		// The 64-byte seam probes are always answered in full, so the integrity
		// check can still judge an assembled file.
		if drop && len(data) > overlapLen {
			half := len(data) / 2
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, start+int64(half)-1, len(s.body)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(data[:half])
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(s.body)))
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data)
	}
}

func TestTruncatedTransferFailsThenResumes(t *testing.T) {
	body := testBody(bodySize)
	sim := &dropping{body: body, drop: true}
	srv := httptest.NewServer(sim.handler())
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "out.bin")
	d := newDownload(srv.URL+"/file.bin", dest, int64(len(body)), 4, true)

	if _, err := (&Runner{}).Run(context.Background(), d, Options{Connections: 4}); err == nil {
		t.Fatal("a server that closes early must fail the run")
	}
	if d.Status != state.StatusFailed {
		t.Fatalf("status = %q, want %q", d.Status, state.StatusFailed)
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a failed run published a file to the destination")
	}
	if progress := d.Completed(); progress <= 0 || progress >= d.TotalSize {
		t.Fatalf("completed bytes = %d, expected a partial result between 0 and %d", progress, d.TotalSize)
	}

	// The server recovers and the same record finishes the job.
	sim.setDrop(false)
	if _, err := (&Runner{}).Run(context.Background(), d, Options{Connections: 4}); err != nil {
		t.Fatalf("resume run: %v", err)
	}
	assertFileEquals(t, dest, body)
	if d.Status != state.StatusDone {
		t.Fatalf("status = %q, want %q", d.Status, state.StatusDone)
	}
}

func TestLimiterRefusesWorkAfterCancellation(t *testing.T) {
	lim := newLimiter(NewLimit(1024))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := lim.Wait(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait after cancellation = %v, want context.Canceled", err)
	}
	// Unlimited transfers must never block or fail - neither when the ceiling
	// is explicitly zero nor when the caller set none at all.
	for name, l := range map[string]*limiter{
		"zero": newLimiter(NewLimit(0)),
		"nil":  newLimiter(nil),
	} {
		if err := l.Wait(context.Background(), 1<<40); err != nil {
			t.Fatalf("%s Wait = %v, want nil", name, err)
		}
	}
}

// The whole point of Limit being a handle rather than a number is that moving
// it must affect a transfer already under way. If this ever regresses, a user
// who lowers the ceiling mid-download sees the number change in the interface
// and the rate stay exactly where it was.
func TestLimitCanBeMovedWhileATransferIsWaiting(t *testing.T) {
	lim := NewLimit(0)
	bucket := newLimiter(lim)

	if err := bucket.Wait(context.Background(), 1<<30); err != nil {
		t.Fatalf("unlimited Wait = %v, want nil", err)
	}

	// Impose a ceiling far below the chunk. The same bucket must now block
	// instead of letting those bytes through.
	lim.Set(1)
	limited, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	if err := bucket.Wait(limited, 1<<20); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait under a 1 byte/s ceiling = %v, want context.DeadlineExceeded", err)
	}

	// Lifting it again releases the very same wait, without rebuilding it.
	lim.Set(0)
	if err := bucket.Wait(context.Background(), 1<<30); err != nil {
		t.Fatalf("Wait after lifting the ceiling = %v, want nil", err)
	}
}

func TestLimitNormalisesWhatCannotBeHonoured(t *testing.T) {
	if got := NewLimit(-5).Get(); got != 0 {
		t.Fatalf("NewLimit(-5).Get() = %d, want 0: a negative rate cannot be honoured", got)
	}
	if got := (*Limit)(nil).Get(); got != 0 {
		t.Fatalf("nil Limit.Get() = %d, want 0 so a missing ceiling means unlimited", got)
	}

	l := NewLimit(256 * 1024)
	l.Set(-1)
	if got := l.Get(); got != 0 {
		t.Fatalf("Set(-1) left %d, want 0", got)
	}
	l.Set(1024 * 1024)
	if got := l.Get(); got != 1024*1024 {
		t.Fatalf("Get() = %d, want %d", got, 1024*1024)
	}

	// A nil handle must be safe to call, because Options.Limit is optional.
	var none *Limit
	none.Set(999)
	if got := none.Get(); got != 0 {
		t.Fatalf("Set on a nil Limit changed it to %d, want it to stay unlimited", got)
	}
}
