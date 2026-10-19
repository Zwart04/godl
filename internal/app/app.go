// Package app assembles the manager every front end needs: the record store,
// the scheduler, the control API and a loopback listener carrying the endpoint
// file the browser's messaging host reads.
//
// The desktop window and the headless server are one machine in two clothes,
// so the wiring lives here instead of being written twice and left to drift.
//
// Exactly one of them may exist per configuration directory. Two schedulers on
// the same records means two workers writing the same .part file, which is a
// corruption bug that looks like a random transfer failure weeks later. The
// single-instance lock below exists to make that impossible rather than
// unlikely.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"godl/internal/api"
	"godl/internal/category"
	"godl/internal/config"
	"godl/internal/engine"
	"godl/internal/queue"
)

// Options selects how the manager behaves.
type Options struct {
	// Parallel is how many downloads transfer at once.
	Parallel int
	// Connections is the segment count a new download asks for. Servers that
	// refuse ranges are still given one, regardless of this.
	Connections int

	// Speed limits are per download, not per manager, and are stored on each
	// record - see state.Download.SpeedLimit. A manager-wide cap would need
	// one bucket shared by every transfer, and nothing yet asks for one, so
	// there is deliberately no field here pretending otherwise.
}

// ErrAlreadyRunning reports that another manager owns this directory.
var ErrAlreadyRunning = errors.New("godl is already running")

// Instance is one running manager.
type Instance struct {
	// Queue is the scheduler. Both front ends drive the same one.
	Queue *queue.Queue
	// API is the control plane, served over loopback and, for the window,
	// mounted again at the window's own origin.
	API *api.Server
	// Endpoint is the loopback URL recorded for the native messaging host.
	Endpoint string

	cancel  context.CancelFunc
	runDone chan error
	httpSrv *http.Server
	release func()

	mu      sync.Mutex
	stopped bool
}

// Start brings a manager up: claims the single-instance lock, records an
// endpoint, then starts the scheduler and a loopback listener.
//
// Every failure path hands the lock back, so a caller that cannot start never
// leaves the directory looking occupied by a process that is not there.
func Start(o Options) (*Instance, error) {
	if o.Parallel < 1 {
		o.Parallel = 3
	}
	if o.Connections < 1 {
		o.Connections = 8
	}

	release, err := claimLock()
	if err != nil {
		return nil, err
	}
	bail := func(err error) (*Instance, error) {
		release()
		return nil, err
	}

	token, err := config.Token()
	if err != nil {
		return bail(fmt.Errorf("could not create the API token: %w", err))
	}
	if _, err := config.EnsureFilesDir(); err != nil {
		return bail(fmt.Errorf("could not create the download folder: %w", err))
	}

	// Port zero asks the OS for whatever is free: a fixed port would make the
	// app fail to start whenever an unrelated program happens to hold it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return bail(fmt.Errorf("could not listen on loopback: %w", err))
	}

	endpoint := "http://" + ln.Addr().String()
	if err := config.WriteEndpoint(endpoint); err != nil {
		_ = ln.Close()
		return bail(fmt.Errorf("could not record the endpoint: %w", err))
	}

	store := config.Store()
	q := queue.New(&store, o.Parallel)

	srv := api.New(q, token)
	srv.Connections = o.Connections
	// Read once at start-up. A categories.json edited while the app is open is
	// picked up next launch - reloading it per request would let a file being
	// half-saved decide where a download lands.
	srv.Categories = category.Load(config.Dir())

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() {
		runDone <- q.Run(ctx, engine.Options{Connections: o.Connections})
	}()

	// This listener is what the browser's host talks to. The window has its
	// own front door to the very same handler, so a page opened in a browser
	// and the desktop UI stay two views of one queue.
	hs := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = hs.Serve(ln) }()

	return &Instance{
		Queue:    q,
		API:      srv,
		Endpoint: endpoint,
		cancel:   cancel,
		runDone:  runDone,
		httpSrv:  hs,
		release:  release,
	}, nil
}

// Shutdown stops the manager and gives every claim on the directory back.
//
// The scheduler is waited for, not abandoned: it persists each record on its
// way out, and returning first is exactly how a closed app leaves a download
// marked "running" forever and refuses to touch it on the next launch.
func (i *Instance) Shutdown(timeout time.Duration) error {
	if i == nil {
		return nil
	}
	i.mu.Lock()
	if i.stopped {
		i.mu.Unlock()
		return nil
	}
	i.stopped = true
	i.mu.Unlock()

	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)

	var err error

	i.cancel()
	wait := time.Until(deadline)
	if wait < 0 {
		wait = 0
	}
	select {
	case <-i.runDone:
	case <-time.After(wait):
		err = errors.New("the queue did not stop in time")
	}

	// The endpoint goes before the lock so a host arriving in between finds
	// neither rather than a URL nobody is listening on.
	ctx, cancel := context.WithTimeout(context.Background(), time.Until(deadline))
	if ctx.Err() != nil {
		ctx, cancel = context.WithTimeout(context.Background(), time.Millisecond)
	}
	if herr := i.httpSrv.Shutdown(ctx); herr != nil && err == nil {
		err = herr
	}
	cancel()

	config.RemoveEndpoint()
	i.release()
	return err
}

// Lock claim ────────────────────────────────────────────────────────────────

const lockName = "manager.lock"

// claimLock takes the single-instance lock for this configuration directory
// and returns the function that gives it back.
//
// The file's existence is the claim, but existence alone is not evidence: a
// manager that was killed also leaves one behind. So an existing lock is only
// believed while the endpoint its owner recorded still answers.
//
// The grace period is what makes that test safe in both directions. A manager
// starting right now creates the lock a moment before it writes its endpoint,
// and reading that gap as "dead" would hand two schedulers the same records.
func claimLock() (func(), error) {
	if err := os.MkdirAll(config.Dir(), 0o700); err != nil {
		return nil, fmt.Errorf("could not create the configuration folder: %w", err)
	}
	path := filepath.Join(config.Dir(), lockName)

	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
			_ = f.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("could not claim the manager lock: %w", err)
		}
		if managerAnswers() {
			return nil, ErrAlreadyRunning
		}
		// Stale: the owner died without cleaning up. Drop it and try once
		// more so a crashed run does not lock the user out of the app.
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, ErrAlreadyRunning
		}
	}
	return nil, errors.New("could not claim the manager lock")
}

// managerAnswers reports whether a live manager is listening at the recorded
// endpoint, giving a manager that is still starting a moment to write it.
func managerAnswers() bool {
	deadline := time.Now().Add(1500 * time.Millisecond)
	for {
		if probeEndpoint() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(120 * time.Millisecond)
	}
}

// probeEndpoint asks the recorded endpoint for its health with the real token.
//
// Only an authenticated 200 counts. An endpoint file left behind by a crash may
// now point at some unrelated program, and that program must not be mistaken
// for our manager just because it answered on the port.
func probeEndpoint() bool {
	endpoint := strings.TrimSpace(config.ReadEndpoint())
	if endpoint == "" {
		return false
	}
	token, err := config.Token()
	if err != nil {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(endpoint, "/")+"/api/health", nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{
		Timeout: 2 * time.Second,
		// A redirect to another host is not our manager answering.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	res, err := client.Do(req)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
	return res.StatusCode == http.StatusOK
}
