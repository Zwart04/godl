// Package queue schedules downloads.
//
// The IDMan.exe analysis showed queues persisted per-queue under
// Software\DownloadManager\Queue with q_<id>.dt files, driven by a QUEUE_TIMER
// that polled state. We keep the same shape - explicit persisted records, a
// polling scheduler, start/stop events - but use a worker pool instead of
// polling every record.
package queue

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"godl/internal/engine"
	"godl/internal/state"
)

// Event describes a queue lifecycle change.
type Event struct {
	Kind string // "started" | "finished" | "failed" | "paused"
	ID   string
	Err  error
}

// Snapshot is a race-free copy of one download, safe to hand to another
// goroutine. Nothing in it aliases a record that a worker is still mutating.
type Snapshot struct {
	ID             string          `json:"id"`
	URL            string          `json:"url"`
	Dest           string          `json:"dest"`
	TotalSize      int64           `json:"total_size"`
	Segmented      bool            `json:"segmented"`
	Status         state.Status    `json:"status"`
	Progress       engine.Snapshot `json:"progress"`
	Error          string          `json:"error,omitempty"`
	FallbackReason string          `json:"fallback_reason,omitempty"`
	Connections    int             `json:"connections"`
	CreatedAt      time.Time       `json:"created_at"`
}

// entry holds the live view of one running download.
//
// meta is written once before the worker goroutine starts and never again.
// status/error/fallback are published by the worker under mu. The byte counter
// lives in an atomic Progress. That is what lets Snapshot be called from any
// goroutine without racing the transfer.
type entry struct {
	meta Snapshot
	prog *engine.Progress
	stop context.CancelFunc

	mu        sync.Mutex
	status    state.Status
	err       string
	fallback  string
	discarded bool
}

func (e *entry) snapshot() Snapshot {
	e.mu.Lock()
	status, errStr, fb := e.status, e.err, e.fallback
	e.mu.Unlock()

	s := e.meta
	s.Status = status
	s.Error = errStr
	s.FallbackReason = fb
	s.Progress = e.prog.Snapshot()
	return s
}

func (e *entry) publish(status state.Status, errStr, fallback string) {
	e.mu.Lock()
	e.status, e.err, e.fallback = status, errStr, fallback
	e.mu.Unlock()
}

func (e *entry) markDiscarded() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.discarded {
		return true
	}
	e.discarded = true
	return false
}

// Queue runs at most Parallel downloads at once, oldest first.
type Queue struct {
	Store    *state.Store
	Parallel int
	// Poll is how long the scheduler waits before looking for new work while
	// idle. It is the daemon's equivalent of IDM's QUEUE_TIMER.
	Poll time.Duration

	mu      sync.Mutex
	live    map[string]*entry
	events  chan Event
	kick    chan struct{}
	running bool
	stop    context.CancelFunc
}

func New(store *state.Store, parallel int) *Queue {
	if parallel < 1 {
		parallel = 1
	}
	return &Queue{
		Store:    store,
		Parallel: parallel,
		live:     map[string]*entry{},
		events:   make(chan Event, 256),
		kick:     make(chan struct{}, 1),
		Poll:     500 * time.Millisecond,
	}
}

// Kick asks the scheduler to look for work now instead of waiting out the poll
// interval. Adding a download through the UI should not sit idle for half a
// second before it starts.
func (q *Queue) Kick() {
	select {
	case q.kick <- struct{}{}:
	default:
	}
}

// Events returns the lifecycle channel. It is never closed, so a consumer can
// range over it until its own context ends.
func (q *Queue) Events() <-chan Event { return q.events }

// Drain returns every buffered event without blocking.
func (q *Queue) Drain() []Event {
	var out []Event
	for {
		select {
		case e := <-q.events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// Running reports whether the scheduler loop is active.
func (q *Queue) Running() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.running
}

// Stop asks the scheduler to wind down and checkpoints in-flight downloads.
func (q *Queue) Stop() {
	q.mu.Lock()
	stop := q.stop
	q.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// Start processes whatever is queued and returns when the queue is empty.
func (q *Queue) Start(ctx context.Context, opt engine.Options) error {
	return q.loop(ctx, opt, true)
}

// Run keeps the queue alive until ctx ends, picking up work added meanwhile.
func (q *Queue) Run(ctx context.Context, opt engine.Options) error {
	return q.loop(ctx, opt, false)
}

func (q *Queue) loop(ctx context.Context, opt engine.Options, drain bool) error {
	q.mu.Lock()
	if q.running {
		q.mu.Unlock()
		return errors.New("queue is already running")
	}
	runCtx, cancel := context.WithCancel(ctx)
	q.stop = cancel
	q.running = true
	q.mu.Unlock()
	defer q.finish()

	// Only a dead process can leave "running" behind, and only before this
	// loop starts writing the status itself. Reaping once here avoids ever
	// overwriting a record a live worker owns.
	q.reapOrphans()

	wake := make(chan struct{}, 1)
	var wg sync.WaitGroup
	poll := q.Poll
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}

	for {
		queued := q.dispatch(runCtx, opt, &wg, wake)

		if drain && queued == 0 && q.liveCount() == 0 {
			return nil
		}

		select {
		case <-runCtx.Done():
			q.cancelAll()
			wg.Wait()
			return runCtx.Err()
		case <-wake:
		case <-q.kick:
		case <-time.After(poll):
		}
	}
}

// reapOrphans makes records left "running" by a killed process resumable.
func (q *Queue) reapOrphans() {
	if q.Store == nil {
		return
	}
	items, err := q.Store.List()
	if err != nil {
		return
	}
	for _, d := range items {
		if d.Status == state.StatusRunning {
			d.Status = state.StatusPaused
			d.Error = ""
			q.save(d)
		}
	}
}

// dispatch starts whatever work it can and returns how many queued records are
// still waiting for capacity.
func (q *Queue) dispatch(ctx context.Context, opt engine.Options, wg *sync.WaitGroup, wake chan struct{}) int {
	if q.Store == nil {
		return 0
	}
	items, err := q.Store.List()
	if err != nil {
		q.emit(Event{Kind: "failed", Err: err})
		return 0
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].CreatedAt.Before(items[j].CreatedAt)
	})

	queued := make([]*state.Download, 0, len(items))
	for _, d := range items {
		switch d.Status {
		case state.StatusQueued, state.StatusPaused, state.StatusFailed:
			queued = append(queued, d)
		}
	}

	q.mu.Lock()
	capacity := q.Parallel - len(q.live)
	q.mu.Unlock()

	launched := 0
	for _, d := range queued {
		if launched >= capacity {
			break
		}
		if q.busy(d.ID) {
			continue
		}
		q.launch(ctx, opt, d, wg, wake)
		launched++
	}
	return len(queued) - launched
}

func (q *Queue) launch(ctx context.Context, opt engine.Options, d *state.Download, wg *sync.WaitGroup, wake chan struct{}) {
	itemCtx, cancel := context.WithCancel(ctx)
	prog := engine.NewProgress(d.TotalSize)
	prog.Seed(d.Completed())

	meta := Snapshot{
		ID:        d.ID,
		URL:       d.URL,
		Dest:      d.Dest,
		TotalSize: d.TotalSize,
		Segmented: d.Segmented,
		Status:    state.StatusRunning,
		// The planned segment list, not the requested count: a server that
		// refuses ranges is downloaded on one connection and the UI has to
		// report the truth rather than the intention.
		Connections: len(d.Segments),
		CreatedAt:   d.CreatedAt,
	}
	e := &entry{meta: meta, prog: prog, stop: cancel}
	e.publish(state.StatusRunning, "", "")

	q.mu.Lock()
	q.live[d.ID] = e
	q.mu.Unlock()

	d.Status = state.StatusRunning
	d.Error = ""
	q.save(d)
	q.emit(Event{Kind: "started", ID: d.ID})

	wg.Add(1)
	go func() {
		defer wg.Done()

		runOpt := opt
		runOpt.Progress = prog
		_, err := (&engine.Runner{}).Run(itemCtx, d, runOpt)

		cancel()

		// The entry stays in the live set until every disk write below has
		// landed. Otherwise the scheduler can observe an empty queue while the
		// record still says "running" and hand control back to a caller that
		// reads a half-finished state.
		defer q.forget(d.ID, wake)

		// A record removed mid-flight must stay removed rather than being
		// resurrected by this final save.
		if e.markDiscarded() {
			return
		}

		switch {
		case err == nil:
			e.publish(state.StatusDone, "", "")
			d.Status = state.StatusDone
			d.Error = ""
			q.save(d)
			q.emit(Event{Kind: "finished", ID: d.ID})
		case itemCtx.Err() != nil:
			// The user asked us to stop: keep the progress, not the failure.
			e.publish(state.StatusPaused, "", "")
			d.Status = state.StatusPaused
			d.Error = ""
			q.save(d)
			q.emit(Event{Kind: "paused", ID: d.ID})
		default:
			fallback := ""
			if d.SingleConnectionFallback {
				fallback = d.FallbackReason
			}
			e.publish(state.StatusFailed, err.Error(), fallback)
			d.Status = state.StatusFailed
			d.Error = err.Error()
			q.save(d)
			q.emit(Event{Kind: "failed", ID: d.ID, Err: err})
		}
	}()
}

// Snapshot lists every download, running ones taken live and the rest from
// disk, oldest first.
func (q *Queue) Snapshot() []Snapshot {
	q.mu.Lock()
	live := make(map[string]*entry, len(q.live))
	for k, v := range q.live {
		live[k] = v
	}
	q.mu.Unlock()

	out := make([]Snapshot, 0, len(live)+4)
	seen := make(map[string]bool, len(live))

	if q.Store != nil {
		items, err := q.Store.List()
		if err == nil {
			for _, d := range items {
				if e, ok := live[d.ID]; ok {
					out = append(out, e.snapshot())
					seen[d.ID] = true
					continue
				}
				out = append(out, Snapshot{
					ID:             d.ID,
					URL:            d.URL,
					Dest:           d.Dest,
					TotalSize:      d.TotalSize,
					Segmented:      d.Segmented,
					Status:         d.Status,
					Connections:    len(d.Segments),
					Progress:       engine.Snapshot{Done: d.Completed(), Total: d.TotalSize, Percent: percent(d.Completed(), d.TotalSize)},
					Error:          d.Error,
					FallbackReason: d.FallbackReason,
					CreatedAt:      d.CreatedAt,
				})
			}
		}
	}
	for id, e := range live {
		if !seen[id] {
			out = append(out, e.snapshot())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// Pause stops a running download and leaves it resumable.
func (q *Queue) Pause(id string) error {
	if e := q.liveEntry(id); e != nil {
		e.stop()
		return nil
	}
	return q.rewrite(id, state.StatusPaused, false)
}

// Resume queues a paused download again.
func (q *Queue) Resume(id string) error {
	if q.busy(id) {
		return errors.New("download is already running")
	}
	return q.rewrite(id, state.StatusQueued, false)
}

// Retry queues a failed download again, keeping its progress.
func (q *Queue) Retry(id string) error {
	if q.busy(id) {
		return errors.New("download is already running")
	}
	return q.rewrite(id, state.StatusQueued, false)
}

// Cancel stops a download and discards the bytes it had gathered.
func (q *Queue) Cancel(id string) error {
	if e := q.liveEntry(id); e != nil {
		e.markDiscarded()
		e.stop()
	}
	d, err := q.load(id)
	if err != nil {
		return err
	}
	d.Status = state.StatusCancelled
	d.Error = ""
	if d.PartPath != "" {
		_ = os.Remove(d.PartPath)
	}
	return q.Store.Save(d)
}

// Remove deletes a download record and its partial file.
func (q *Queue) Remove(id string) error {
	if e := q.liveEntry(id); e != nil {
		e.markDiscarded()
		e.stop()
	}
	if d, err := q.load(id); err == nil && d.PartPath != "" {
		_ = os.Remove(d.PartPath)
	}
	return q.Store.Delete(id)
}

func (q *Queue) rewrite(id string, status state.Status, clearError bool) error {
	d, err := q.load(id)
	if err != nil {
		return err
	}
	d.Status = status
	if clearError {
		d.Error = ""
	}
	return q.Store.Save(d)
}

func (q *Queue) load(id string) (*state.Download, error) {
	if q.Store == nil {
		return nil, errors.New("no store configured")
	}
	if id == "" {
		return nil, errors.New("missing download id")
	}
	return q.Store.Load(id)
}

func (q *Queue) liveEntry(id string) *entry {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.live[id]
}

// forget drops a finished worker from the live set and nudges the scheduler.
// Callers use it as a deferred finaliser so the removal is the last thing that
// happens, after the record's outcome has been written.
func (q *Queue) forget(id string, wake chan struct{}) {
	q.mu.Lock()
	delete(q.live, id)
	q.mu.Unlock()
	wakeOnce(wake)
}

// save persists a record and reports a failure instead of dropping it. A write
// that is ignored leaves disk describing a run that is no longer true, and the
// next reader would trust that state.
func (q *Queue) save(d *state.Download) {
	if err := q.Store.Save(d); err != nil {
		q.emit(Event{Kind: "error", ID: d.ID, Err: fmt.Errorf("saving %s: %w", d.ID, err)})
	}
}

func (q *Queue) busy(id string) bool {
	return q.liveEntry(id) != nil
}

func (q *Queue) liveCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.live)
}

func (q *Queue) cancelAll() {
	q.mu.Lock()
	stops := make([]context.CancelFunc, 0, len(q.live))
	for _, e := range q.live {
		stops = append(stops, e.stop)
	}
	q.mu.Unlock()
	for _, stop := range stops {
		stop()
	}
}

func (q *Queue) emit(e Event) {
	select {
	case q.events <- e:
	default: // never let a slow observer block the transfer
	}
}

func (q *Queue) finish() {
	q.mu.Lock()
	q.running = false
	q.stop = nil
	q.mu.Unlock()
}

func wakeOnce(wake chan struct{}) {
	select {
	case wake <- struct{}{}:
	default:
	}
}

func percent(done, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(done) / float64(total) * 100
}
