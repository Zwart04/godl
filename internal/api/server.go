// Package api serves the local control API and the graphical page that drives
// it.
//
// Everything binds to loopback and every call carries the install token. That
// matters: without a header the browser's own same-origin policy would still
// stop a hostile page from reading responses, but it would happily let one
// *write* - a form or a fetch can change state without reading it. Requiring a
// header the page cannot obtain cross-origin closes that hole.
package api

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"godl/internal/category"
	"godl/internal/config"
	"godl/internal/engine"
	"godl/internal/queue"
	"godl/internal/state"
)

//go:embed web
var webFiles embed.FS

// Version is reported to the UI and to the native messaging host. It is a var
// so a release build can stamp it without touching the package.
var Version = config.Version

// tokenPlaceholder is swapped for the real token when index.html is served.
const tokenPlaceholder = "__GODL_TOKEN__"

// Server is the control plane: a JSON API plus the page that drives it.
type Server struct {
	Queue *queue.Queue
	Token string

	// Client transfers bytes, so it must carry no overall Timeout: a limit
	// there would abort a healthy download that simply takes a while.
	Client *http.Client

	// DownloadDir receives new downloads when the caller names no path.
	DownloadDir string
	// Categories sorts a new download into a folder by what it is, and backs
	// the list the window offers. An empty list means categories are off and
	// everything lands in DownloadDir directly.
	Categories []category.Category
	// Connections is the segment count used when a request does not say.
	Connections int
	// ProbeTimeout bounds the probe done before a download is queued. It is
	// applied to the probe context, not to the transfer client.
	ProbeTimeout time.Duration

	mu    sync.Mutex
	once  sync.Once
	web   fs.FS
	shell string
	prep  error

	// clipMu guards clipLink, the address the watcher has put forward and the
	// window has not yet answered.
	//
	// It is a separate mutex rather than mu because mu is held across work that
	// has nothing to do with this, and a captured link is offered from the
	// watcher's own goroutine while a request may be answering another one.
	clipMu   sync.Mutex
	clipLink string
}

// New builds a server with defaults suited to a local manager.
func New(q *queue.Queue, token string) *Server {
	return &Server{
		Queue:        q,
		Token:        token,
		Client:       &http.Client{},
		DownloadDir:  config.FilesDir(),
		Connections:  8,
		ProbeTimeout: 30 * time.Second,
	}
}

// prepare loads the embedded page once and bakes the token into it.
func (s *Server) prepare() error {
	s.once.Do(func() {
		sub, err := fs.Sub(webFiles, "web")
		if err != nil {
			s.prep = fmt.Errorf("embedded web assets: %w", err)
			return
		}
		raw, err := fs.ReadFile(sub, "index.html")
		if err != nil {
			s.prep = fmt.Errorf("embedded index.html: %w", err)
			return
		}
		s.web = sub
		// The token goes into the page itself: the UI runs same-origin, so it
		// can set the Authorization header, while a cross-origin page never
		// gets to see this document at all.
		s.shell = strings.ReplaceAll(string(raw), tokenPlaceholder, s.Token)
	})
	return s.prep
}

// Handler wires the routes.
func (s *Server) Handler() http.Handler {
	if err := s.prepare(); err != nil {
		panic("api: " + err.Error())
	}

	mux := http.NewServeMux()

	mux.Handle("GET /api/health", s.authed(http.HandlerFunc(s.handleHealth)))
	mux.Handle("GET /api/downloads", s.authed(http.HandlerFunc(s.handleList)))
	mux.Handle("GET /api/categories", s.authed(http.HandlerFunc(s.handleCategories)))
	mux.Handle("GET /api/clipboard", s.authed(http.HandlerFunc(s.handleClipboardOffer)))
	mux.Handle("POST /api/clipboard/accept", s.authed(http.HandlerFunc(s.handleClipboardAccept)))
	mux.Handle("POST /api/clipboard/ignore", s.authed(http.HandlerFunc(s.handleClipboardIgnore)))
	mux.Handle("POST /api/downloads", s.authed(http.HandlerFunc(s.handleAdd)))

	mux.Handle("POST /api/downloads/{id}/pause", s.authed(s.action("pause", func(q *queue.Queue, id string) error {
		return q.Pause(id)
	})))
	mux.Handle("POST /api/downloads/{id}/resume", s.authed(s.action("resume", func(q *queue.Queue, id string) error {
		return q.Resume(id)
	})))
	mux.Handle("POST /api/downloads/{id}/retry", s.authed(s.action("retry", func(q *queue.Queue, id string) error {
		return q.Retry(id)
	})))
	mux.Handle("POST /api/downloads/{id}/cancel", s.authed(s.action("cancel", func(q *queue.Queue, id string) error {
		return q.Cancel(id)
	})))
	mux.Handle("DELETE /api/downloads/{id}", s.authed(s.action("remove", func(q *queue.Queue, id string) error {
		return q.Remove(id)
	})))
	mux.Handle("POST /api/downloads/{id}/reveal", s.authed(s.action("reveal", func(q *queue.Queue, id string) error {
		d, err := q.Store.Load(id)
		if err != nil {
			return err
		}
		return reveal(d.Dest)
	})))
	mux.Handle("POST /api/downloads/{id}/speed-limit", s.authed(http.HandlerFunc(s.handleSpeedLimit)))
	mux.Handle("POST /api/downloads/{id}/schedule", s.authed(http.HandlerFunc(s.handleSchedule)))
	mux.Handle("POST /api/downloads/{id}/category", s.authed(http.HandlerFunc(s.handleCategory)))
	mux.Handle("POST /api/downloads/{id}/move-up", s.authed(s.action("move-up", func(q *queue.Queue, id string) error {
		return q.MoveUp(id)
	})))
	mux.Handle("POST /api/downloads/{id}/move-down", s.authed(s.action("move-down", func(q *queue.Queue, id string) error {
		return q.MoveDown(id)
	})))

	mux.Handle("GET /app.js", s.staticFile("app.js", "text/javascript; charset=utf-8"))
	mux.Handle("GET /app.css", s.staticFile("app.css", "text/css; charset=utf-8"))
	mux.Handle("GET /", http.HandlerFunc(s.serveIndex))

	return mux
}

// authed rejects any call that does not present the install token.
func (s *Server) authed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer"))
		got = strings.TrimSpace(got)
		if s.Token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
			writeErr(w, http.StatusUnauthorized, "missing or incorrect token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	// Anything that is not the page itself falls through to a 404 rather than
	// handing back HTML for a script that expected data.
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'")
	io.WriteString(w, s.shell)
}

func (s *Server) staticFile(name, contentType string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := fs.ReadFile(s.web, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(raw)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"version":      Version,
		"running":      s.Queue.Running(),
		"download_dir": s.DownloadDir,
	})
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"downloads": s.Queue.Snapshot()})
}

// AddRequest is what the UI and the browser host send when a link arrives.
type AddRequest struct {
	URL         string `json:"url"`
	Filename    string `json:"filename"`
	Connections int    `json:"connections"`
	// SpeedLimit is a bytes-per-second ceiling for this download alone.
	// Zero, or an omitted field, means no ceiling.
	SpeedLimit int64 `json:"speed_limit"`
	// Category names where this should land. Left empty, the file's extension
	// decides - which is the case for every link a browser offers.
	Category  string `json:"category"`
	Cookie    string `json:"cookie"`
	Referer   string `json:"referer"`
	UserAgent string `json:"user_agent"`
}

func (s *Server) handleAdd(w http.ResponseWriter, r *http.Request) {
	var req AddRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	s.add(w, r, req)
}

// add runs the whole of "a URL becomes a download" - validate, check the
// category, answer a duplicate, probe, name, file, save, start - and writes the
// one answer.
//
// It is its own function because there is exactly one shape to this work. The
// page's Add button, the browser's host, and a link copied to the clipboard are
// three ways of arriving at the same decision, and three copies of it would be
// three places for the safeguards to drift apart.
//
// It takes the request as well as the response writer because the probe runs on
// the caller's context: a window that has navigated away should not leave a
// probe going on its behalf.
func (s *Server) add(w http.ResponseWriter, r *http.Request, req AddRequest) {
	req.URL = strings.TrimSpace(req.URL)
	if !isHTTPURL(req.URL) {
		writeErr(w, http.StatusBadRequest, "only http and https URLs can be downloaded")
		return
	}
	if req.SpeedLimit < 0 {
		writeErr(w, http.StatusBadRequest, "a speed limit cannot be negative; send 0 for no limit")
		return
	}

	// A category the caller named is checked before the server is contacted.
	// It costs nothing to answer here, and a request that cannot be honoured
	// should not spend a probe finding that out.
	wantCat := strings.TrimSpace(req.Category)
	if wantCat != "" {
		if len(s.Categories) == 0 {
			writeErr(w, http.StatusNotImplemented, "this build puts every download in one folder; there are no categories to choose from")
			return
		}
		matched, ok := s.categoryName(wantCat)
		if !ok {
			writeErr(w, http.StatusBadRequest, "unknown category; choose one of: "+s.categoryNames())
			return
		}
		wantCat = matched
	}

	// A double-click must not queue the same file twice.
	for _, snap := range s.Queue.Snapshot() {
		if snap.URL == req.URL && snap.Status != state.StatusDone && snap.Status != state.StatusCancelled {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "duplicate": true, "download": snap})
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.probeTimeout())
	defer cancel()

	hints := engine.Hints{Cookie: req.Cookie, Referer: req.Referer, UserAgent: req.UserAgent}
	probe, err := engine.Probe(ctx, s.Client, req.URL, hints)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "could not reach the server: "+err.Error())
		return
	}
	if probe.TotalSize <= 0 {
		writeErr(w, http.StatusNotImplemented, "the server did not report a size; streaming downloads are not supported yet")
		return
	}

	name := sanitizeFilename(req.Filename)
	if name == "" {
		name = sanitizeFilename(probe.Name)
	}
	if name == "" {
		name = sanitizeFilename(nameFromURL(req.URL))
	}
	if name == "" {
		name = "download"
	}

	// Sorted by what the file is, unless the caller already knows - and that
	// answer was made above, before the server was contacted. Doing it here,
	// before the destination is picked, keeps the folder and the category the
	// same decision made once rather than two that could drift apart.
	cat := wantCat
	if cat == "" && len(s.Categories) > 0 {
		cat = category.Assign(s.Categories, name)
	}

	dir := s.folderFor(cat)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeErr(w, http.StatusInternalServerError, "could not create the download directory: "+err.Error())
		return
	}
	dest := state.UniquePath(dir, name)

	n := s.Connections
	if req.Connections > 0 {
		n = req.Connections
	}
	if n < 1 {
		n = 1
	}
	if !probe.Segmented {
		// IDM's own advice when a server cannot resume: fall back to one
		// connection rather than pretending otherwise.
		n = 1
	}

	d := &state.Download{
		ID:            state.NewID(),
		URL:           req.URL,
		Dest:          dest,
		PartPath:      dest + ".part",
		Category:      cat,
		TotalSize:     probe.TotalSize,
		Segmented:     probe.Segmented,
		ETag:          probe.ETag,
		LastModified:  probe.LastModified,
		SuggestedName: probe.Name,
		Segments:      state.PlanSegments(probe.TotalSize, n),
		Status:        state.StatusQueued,
		CreatedAt:     time.Now(),
		Cookie:        req.Cookie,
		Referer:       req.Referer,
		UserAgent:     req.UserAgent,
		SpeedLimit:    req.SpeedLimit,
	}
	if err := s.Queue.Store.Save(d); err != nil {
		writeErr(w, http.StatusInternalServerError, "could not save the download: "+err.Error())
		return
	}
	s.Queue.Kick()

	snap, _ := s.find(d.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "download": snap})
}

// action wraps a queue mutation with an existence check and a transition
// check, so a stray call cannot e.g. re-queue a download that already finished.
func (s *Server) action(name string, fn func(*queue.Queue, string) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		cur, ok := s.find(id)
		if !ok {
			writeErr(w, http.StatusNotFound, "no such download")
			return
		}
		if !actionAllowed(name, cur.Status) {
			writeErr(w, http.StatusConflict, fmt.Sprintf("cannot %s a download that is %s", name, cur.Status))
			return
		}
		if err := fn(s.Queue, id); err != nil {
			// A move with nowhere to go describes the queue, not a fault in
			// the request - answering 500 would tell the caller the server
			// broke when it was already where they asked for.
			code := http.StatusInternalServerError
			if errors.Is(err, queue.ErrAtEnd) {
				code = http.StatusConflict
			}
			writeErr(w, code, err.Error())
			return
		}
		if name == "resume" || name == "retry" {
			s.Queue.Kick()
		}
		next, _ := s.find(id)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "download": next})
	})
}

// handleSpeedLimit moves this download's ceiling.
//
// It is deliberately not routed through action(): that helper exists to reject
// a transition that does not apply to the current state, and a speed limit is
// not a transition. Lowering one while the transfer runs is the case the
// control exists for, so refusing it in every state but that one would make it
// useless.
func (s *Server) handleSpeedLimit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SpeedLimit int64 `json:"speed_limit"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.SpeedLimit < 0 {
		writeErr(w, http.StatusBadRequest, "a speed limit cannot be negative; send 0 for no limit")
		return
	}

	id := r.PathValue("id")
	if _, ok := s.find(id); !ok {
		writeErr(w, http.StatusNotFound, "no such download")
		return
	}
	if err := s.Queue.SetLimit(id, req.SpeedLimit); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	next, _ := s.find(id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "download": next})
}

// handleSchedule sets when a download may first begin.
//
// It is not routed through action() for the same reason speed-limit is not: a
// schedule is a value to store, not a transition to check. The state guard
// lives in actionAllowed instead, so a download that has no start left to
// postpone is refused before the body is even read.
func (s *Server) handleSchedule(w http.ResponseWriter, r *http.Request) {
	// The record is checked before its body is read. A caller who cannot
	// schedule this download cannot fix that by changing the payload, so
	// telling them so first saves them from composing a better-shaped one.
	id := r.PathValue("id")
	cur, ok := s.find(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "no such download")
		return
	}
	if !actionAllowed("schedule", cur.Status) {
		writeErr(w, http.StatusConflict, fmt.Sprintf("cannot schedule a download that is %s", cur.Status))
		return
	}

	var req struct {
		// StartsAt is an RFC 3339 timestamp, or the empty string to run it now.
		StartsAt string `json:"starts_at"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	var at *time.Time
	if raw := strings.TrimSpace(req.StartsAt); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "starts_at must be an RFC 3339 timestamp, or empty to run now")
			return
		}
		at = &parsed
	}
	if err := s.Queue.Schedule(id, at); err != nil {
		// Stopping a transfer did not complete in time. That is a disagreement
		// about who owns the record right now, not a fault in the server.
		code := http.StatusInternalServerError
		if errors.Is(err, queue.ErrBusy) {
			code = http.StatusConflict
		}
		writeErr(w, code, err.Error())
		return
	}
	// The scheduler may already be sitting in its sleep; a schedule moved
	// earlier has to reach it without waiting out the rest of the interval.
	s.Queue.Kick()

	next, _ := s.find(id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "download": next})
}

// OfferLink puts a link the watcher has just seen in front of the window.
//
// It replaces whatever has not been answered. The clipboard moves faster than
// a person does: copying three addresses in a row is ordinary, and only the
// last one is still a live offer. Nothing is lost by dropping the others,
// because none of them had been asked about.
//
// A link already in the list is not an offer at all. The add path would answer
// "duplicate", but a dialog opened to report that is a worse answer than not
// asking, and someone who genuinely wants it again can add it by hand.
func (s *Server) OfferLink(url string) {
	for _, snap := range s.Queue.Snapshot() {
		if snap.URL == url && snap.Status != state.StatusDone && snap.Status != state.StatusCancelled {
			return
		}
	}
	s.clipMu.Lock()
	defer s.clipMu.Unlock()
	s.clipLink = url
}

// takeLink hands back the waiting address and clears it, so one copy is
// offered once even with more than one view of the page open.
func (s *Server) takeLink() string {
	s.clipMu.Lock()
	defer s.clipMu.Unlock()
	link := s.clipLink
	s.clipLink = ""
	return link
}

// handleClipboardOffer reports the waiting link, or an empty url when there is
// nothing to answer. The window asks on a timer, and the empty answer is the
// ordinary one - so it is a normal response carrying no error, not a 404 that
// would put an error in the console every few seconds for the life of the app.
func (s *Server) handleClipboardOffer(w http.ResponseWriter, r *http.Request) {
	s.clipMu.Lock()
	defer s.clipMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"url": s.clipLink})
}

// answered records the status a handler wrote, so a caller can tell whether
// the request achieved anything.
//
// It wraps rather than rewrites: add() writes its own answer in several
// places, and a second copy of those status decisions would be a second place
// for them to drift apart.
type answered struct {
	http.ResponseWriter
	status int
}

func (a *answered) WriteHeader(code int) {
	a.status = code
	a.ResponseWriter.WriteHeader(code)
}

// handleClipboardAccept downloads the waiting link through the same path the
// page's own Add button takes: the same validation, the same probe, the same
// refusal when the server will not say what it has.
//
// The offer is let go only once a record exists. Taking it first would mean a
// server that could not be reached costs the link as well as the attempt, and
// the only way to try again would be to copy it a second time - which is a
// strange price for a copied link to carry. The compare on the way out stops a
// newer capture, put in place while this one was being probed, from being swept
// away by this one's success.
func (s *Server) handleClipboardAccept(w http.ResponseWriter, r *http.Request) {
	s.clipMu.Lock()
	link := s.clipLink
	s.clipMu.Unlock()
	if link == "" {
		writeErr(w, http.StatusNotFound, "there is no captured link waiting")
		return
	}

	rec := &answered{ResponseWriter: w}
	s.add(rec, r, AddRequest{URL: link})

	if rec.status != http.StatusCreated && rec.status != http.StatusOK {
		return // kept: the window shows what went wrong and may try again
	}

	s.clipMu.Lock()
	if s.clipLink == link {
		s.clipLink = ""
	}
	s.clipMu.Unlock()
}

// handleClipboardIgnore puts the waiting link down without downloading it.
//
// It succeeds whether or not anything was waiting: the window is clearing what
// it was showing, and reporting that nothing had been showing would turn a
// dismissal into an error in the console.
func (s *Server) handleClipboardIgnore(w http.ResponseWriter, r *http.Request) {
	s.takeLink()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleCategories is the list the window offers, in the order it shows them.
//
// It is served rather than built into the page so a categories.json can change
// what a download is sorted into without the interface having to be rebuilt.
func (s *Server) handleCategories(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"categories": s.Categories})
}

// folderFor names the directory a category's files land in, below the
// download directory.
//
// With no categories configured the download directory itself is returned,
// which is where everything landed before any of this existed.
func (s *Server) folderFor(name string) string {
	dir := s.DownloadDir
	if dir == "" {
		dir = config.FilesDir()
	}
	if len(s.Categories) == 0 {
		return dir
	}
	return filepath.Join(dir, category.FolderFor(s.Categories, name))
}

// categoryName matches a requested name against the list, and answers with the
// spelling the list uses.
//
// Matching rather than storing what was sent keeps one name per category on
// disk. "music", "Music" and " MUSIC " would otherwise each become their own
// entry the first time the records were read back and drawn.
func (s *Server) categoryName(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	for _, c := range s.Categories {
		if strings.EqualFold(c.Name, raw) {
			return c.Name, true
		}
	}
	return "", false
}

// categoryNames lists what is on offer, for a message when a name is refused.
func (s *Server) categoryNames() string {
	names := make([]string, 0, len(s.Categories))
	for _, c := range s.Categories {
		names = append(names, c.Name)
	}
	return strings.Join(names, ", ")
}

// handleCategory puts a download in another category, and moves its file.
//
// It is not routed through action(): a category is not a transition between
// states, and the state guard it would apply - "is this one running?" - is the
// queue's to make, because only the queue knows whether the worker has the
// file open.
func (s *Server) handleCategory(w http.ResponseWriter, r *http.Request) {
	// The record is checked before its body is read: a caller who cannot
	// change this one cannot fix that by sending a better-shaped payload.
	id := r.PathValue("id")
	if _, ok := s.find(id); !ok {
		writeErr(w, http.StatusNotFound, "no such download")
		return
	}
	if len(s.Categories) == 0 {
		writeErr(w, http.StatusNotImplemented, "this build puts every download in one folder; there are no categories to choose from")
		return
	}

	var req struct {
		Category string `json:"category"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	name, ok := s.categoryName(req.Category)
	if !ok {
		writeErr(w, http.StatusBadRequest, "unknown category; choose one of: "+s.categoryNames())
		return
	}

	if err := s.Queue.SetCategory(id, name, s.folderFor(name)); err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, queue.ErrInFlight) {
			code = http.StatusConflict
		}
		writeErr(w, code, err.Error())
		return
	}
	next, _ := s.find(id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "download": next})
}

func (s *Server) find(id string) (queue.Snapshot, bool) {
	for _, snap := range s.Queue.Snapshot() {
		if snap.ID == id {
			return snap, true
		}
	}
	return queue.Snapshot{}, false
}

func (s *Server) probeTimeout() time.Duration {
	if s.ProbeTimeout <= 0 {
		return 30 * time.Second
	}
	return s.ProbeTimeout
}

// actionAllowed lists the states each control accepts. Anything not listed is
// rejected rather than silently applied to a download that is somewhere else.
func actionAllowed(action string, status state.Status) bool {
	switch action {
	case "pause":
		return status == state.StatusRunning || status == state.StatusQueued
	case "resume":
		return status == state.StatusPaused
	case "retry":
		return status == state.StatusFailed
	case "cancel":
		return status != state.StatusDone && status != state.StatusCancelled
	case "remove":
		return true
	case "reveal":
		// Opening a folder changes nothing, so every state answers.
		return true
	case "move-up", "move-down":
		// Where a record sits in the list is not a state it is in, so a
		// finished download may be moved too - it costs nothing and keeps
		// one rule for the whole column.
		return true
	case "schedule":
		// Everything but a finished download. It is not about the state the
		// record is in so much as the one it should be in when the time
		// comes, and a transfer already under way has to be stopped first
		// rather than refused - the queue starts things the moment they are
		// added, so refusing anything not already waiting would leave this
		// control usable only in the milliseconds before the first byte.
		return status != state.StatusDone
	}
	return false
}

// reveal asks the shell to select the file inside its folder, which is how a
// download manager normally answers "where did that go?".
//
// The whole destination goes to explorer as one argument: a path with spaces
// would otherwise be split and the folder would fail to open. explorer also
// reports a nonzero exit code on perfectly ordinary success, so the command is
// started and abandoned instead of being waited on.
func reveal(dest string) error {
	if strings.TrimSpace(dest) == "" {
		return errors.New("this download has no destination recorded yet")
	}
	dir := filepath.Dir(dest)
	if _, err := os.Stat(dir); err != nil {
		return errors.New("the download folder does not exist yet")
	}
	return exec.Command("explorer.exe", "/select,"+dest).Start()
}

func isHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return u.Host != "" && (u.Scheme == "http" || u.Scheme == "https")
}

// nameFromURL takes the last path segment, without a query or fragment.
func nameFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	base := u.Path
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	return base
}

// sanitizeFilename reduces a name that came from a server or a browser to one
// safe path element: no separators, no traversal, nothing Windows reserves.
func sanitizeFilename(name string) string {
	if i := strings.LastIndexAny(name, "/\\"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20, r == 0x7f,
			r == '/', r == '\\', r == ':', r == '*', r == '?',
			r == '"', r == '<', r == '>', r == '|':
			return -1
		}
		return r
	}, name)
	name = strings.Trim(name, " .")
	if name == "" {
		return ""
	}
	// Windows refuses these stems whatever the extension, and also treats a
	// trailing dot as invisible.
	stem := name
	if i := strings.IndexByte(stem, '.'); i > 0 {
		stem = stem[:i]
	}
	upper := strings.ToUpper(stem)
	if upper == "CON" || upper == "PRN" || upper == "AUX" || upper == "NUL" ||
		(len(upper) == 4 && (strings.HasPrefix(upper, "COM") || strings.HasPrefix(upper, "LPT")) &&
			upper[3] >= '1' && upper[3] <= '9') {
		return "_" + name
	}
	return name
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"ok": false, "error": msg})
}

// IsNotFound reports whether the error means "no such record", which the UI
// shows as a refresh rather than an error banner.
func IsNotFound(err error) bool { return errors.Is(err, fs.ErrNotExist) }
