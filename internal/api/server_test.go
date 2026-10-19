package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"godl/internal/queue"
	"godl/internal/state"
)

const testToken = "test-token-0123456789"

type fixture struct {
	server   *Server
	handler  http.Handler
	store    *state.Store
	filesDir string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	store := &state.Store{Dir: filepath.Join(dir, "records")}
	files := filepath.Join(dir, "files")

	q := queue.New(store, 4)
	s := New(q, testToken)
	s.DownloadDir = files
	s.ProbeTimeout = 15 * time.Second
	s.Connections = 4

	return &fixture{server: s, handler: s.Handler(), store: store, filesDir: files}
}

// ranged serves a file that honours Range, which is what segmented downloads
// need.
func ranged(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "file.bin", time.Time{}, bytes.NewReader(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// unrange ignores the Range header entirely and answers 200 with the whole
// body, which is how a server that cannot resume behaves.
func unrange(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="plain.bin"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func call(t *testing.T, f *fixture, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == "" {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

type addResponse struct {
	OK        bool   `json:"ok"`
	Duplicate bool   `json:"duplicate"`
	Error     string `json:"error"`
	Download  struct {
		ID   string       `json:"id"`
		Dest string       `json:"dest"`
		URL  string       `json:"url"`
		Seg  bool         `json:"segmented"`
		Stat state.Status `json:"status"`
	} `json:"download"`
}

func add(t *testing.T, f *fixture, rawURL, filename string, connections int) (*httptest.ResponseRecorder, addResponse) {
	t.Helper()
	payload, err := json.Marshal(AddRequest{
		URL:         rawURL,
		Filename:    filename,
		Connections: connections,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec := call(t, f, http.MethodPost, "/api/downloads", testToken, string(payload))
	var out addResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode add response %q: %v", rec.Body.String(), err)
	}
	return rec, out
}

func TestEveryAPIRouteRequiresTheToken(t *testing.T) {
	f := newFixture(t)

	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"list", http.MethodGet, "/api/downloads", ""},
		{"health", http.MethodGet, "/api/health", ""},
		{"add", http.MethodPost, "/api/downloads", `{"url":"http://127.0.0.1:9/x"}`},
		{"pause", http.MethodPost, "/api/downloads/none/pause", ""},
		{"remove", http.MethodDelete, "/api/downloads/none", ""},
		{"speed-limit", http.MethodPost, "/api/downloads/none/speed-limit", `{"speed_limit":1024}`},
		{"schedule", http.MethodPost, "/api/downloads/none/schedule", `{"starts_at":"2030-01-01T00:00:00Z"}`},
		{"move-up", http.MethodPost, "/api/downloads/none/move-up", ""},
		{"move-down", http.MethodPost, "/api/downloads/none/move-down", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name+"/missing", func(t *testing.T) {
			if got := call(t, f, tc.method, tc.path, "", tc.body); got.Code != http.StatusUnauthorized {
				t.Fatalf("no token: status = %d, want 401", got.Code)
			}
		})
		t.Run(tc.name+"/wrong", func(t *testing.T) {
			if got := call(t, f, tc.method, tc.path, "not-the-token", tc.body); got.Code != http.StatusUnauthorized {
				t.Fatalf("wrong token: status = %d, want 401", got.Code)
			}
		})
		t.Run(tc.name+"/absent-header", func(t *testing.T) {
			// A token smuggled in a query string must not be accepted: query
			// strings end up in logs and browser history.
			req := httptest.NewRequest(tc.method, tc.path+"?token="+testToken, nil)
			rec := httptest.NewRecorder()
			f.handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("query token: status = %d, want 401", rec.Code)
			}
		})
	}
}

func TestPageIsServedWithTheTokenBakedIn(t *testing.T) {
	f := newFixture(t)

	rec := call(t, f, http.MethodGet, "/", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /: status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, testToken) {
		t.Error("the page does not carry the token, so the UI could not call the API")
	}
	if strings.Contains(body, tokenPlaceholder) {
		t.Error("the placeholder survived into the served page")
	}
	if got := rec.Header().Get("Content-Security-Policy"); !strings.Contains(got, "script-src 'self'") {
		t.Errorf("CSP = %q, want it to restrict scripts to our own origin", got)
	}

	// The page itself needs no token; its script does, and that is what the
	// cross-origin reader cannot obtain.
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}

	for _, tc := range []struct {
		path, wantType string
	}{
		{"/app.js", "text/javascript"},
		{"/app.css", "text/css"},
	} {
		rec := call(t, f, http.MethodGet, tc.path, "", "")
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: status = %d, want 200", tc.path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, tc.wantType) {
			t.Errorf("GET %s: Content-Type = %q, want %q", tc.path, ct, tc.wantType)
		}
	}

	if got := call(t, f, http.MethodGet, "/does-not-exist", "", ""); got.Code != http.StatusNotFound {
		t.Errorf("unknown path: status = %d, want 404", got.Code)
	}
}

func TestAddListAndRemove(t *testing.T) {
	body := bytes.Repeat([]byte{0x5a}, 200_000)
	srv := ranged(t, body)
	f := newFixture(t)

	rec, added := add(t, f, srv.URL+"/file.bin", "", 4)
	if rec.Code != http.StatusCreated {
		t.Fatalf("add: status = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	if !added.OK || added.Download.ID == "" {
		t.Fatalf("add returned %+v", added)
	}
	if added.Download.Stat != state.StatusQueued {
		t.Errorf("status = %q, want %q", added.Download.Stat, state.StatusQueued)
	}
	if !strings.HasPrefix(added.Download.Dest, f.filesDir) {
		t.Errorf("dest = %q, want it inside %q", added.Download.Dest, f.filesDir)
	}
	if !added.Download.Seg {
		t.Error("segmented = false, but the server honours Range")
	}

	d, err := f.store.Load(added.Download.ID)
	if err != nil {
		t.Fatalf("load record: %v", err)
	}
	if got := len(d.Segments); got != 4 {
		t.Errorf("segments = %d, want 4", got)
	}
	if d.PartPath != d.Dest+".part" {
		t.Errorf("part path = %q, want %q", d.PartPath, d.Dest+".part")
	}

	list := call(t, f, http.MethodGet, "/api/downloads", testToken, "")
	if list.Code != http.StatusOK {
		t.Fatalf("list: status = %d", list.Code)
	}
	var listing struct {
		Downloads []queue.Snapshot `json:"downloads"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &listing); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listing.Downloads) != 1 {
		t.Fatalf("listed %d downloads, want 1", len(listing.Downloads))
	}
	if listing.Downloads[0].ID != added.Download.ID {
		t.Errorf("listed %q, want %q", listing.Downloads[0].ID, added.Download.ID)
	}

	del := call(t, f, http.MethodDelete, "/api/downloads/"+added.Download.ID, testToken, "")
	if del.Code != http.StatusOK {
		t.Fatalf("delete: status = %d (%s)", del.Code, del.Body.String())
	}
	if _, err := f.store.Load(added.Download.ID); err == nil {
		t.Error("the record is still on disk after delete")
	}

	list = call(t, f, http.MethodGet, "/api/downloads", testToken, "")
	if err := json.Unmarshal(list.Body.Bytes(), &listing); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listing.Downloads) != 0 {
		t.Errorf("listed %d downloads after delete, want 0", len(listing.Downloads))
	}
}

func TestAddRefusesURLsItCannotDownload(t *testing.T) {
	f := newFixture(t)

	for _, raw := range []string{
		"ftp://example.com/file.zip",
		"file:///C:/Windows/System32/cmd.exe",
		"https://",
		"javascript:alert(1)",
		"  ",
	} {
		rec, _ := add(t, f, raw, "", 4)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%q: status = %d, want 400", raw, rec.Code)
		}
	}
}

func TestAddQueuesAnActiveURLOnlyOnce(t *testing.T) {
	body := bytes.Repeat([]byte{1}, 4096)
	srv := ranged(t, body)
	f := newFixture(t)

	rec, first := add(t, f, srv.URL+"/file.bin", "", 4)
	if rec.Code != http.StatusCreated {
		t.Fatalf("first add: status = %d", rec.Code)
	}

	rec, second := add(t, f, srv.URL+"/file.bin", "", 4)
	if rec.Code != http.StatusOK {
		t.Fatalf("second add: status = %d", rec.Code)
	}
	if !second.Duplicate {
		t.Fatal("second add of the same URL was not reported as a duplicate")
	}
	if second.Download.ID != first.Download.ID {
		t.Errorf("duplicate points at %q, want the existing %q", second.Download.ID, first.Download.ID)
	}

	// Once the first one is gone the URL may be queued again.
	if del := call(t, f, http.MethodDelete, "/api/downloads/"+first.Download.ID, testToken, ""); del.Code != http.StatusOK {
		t.Fatalf("delete: status = %d", del.Code)
	}
	if _, rec := add(t, f, srv.URL+"/file.bin", "", 4); rec.Duplicate {
		t.Error("URL still treated as a duplicate after the record was removed")
	}
}

func TestFilenameCannotEscapeTheDownloadDirectory(t *testing.T) {
	body := bytes.Repeat([]byte{7}, 1024)
	srv := ranged(t, body)
	f := newFixture(t)

	for i, tc := range []struct{ given, want string }{
		{"../../escape.txt", "escape.txt"},
		{`..\..\escape.txt`, "escape.txt"},
		{"/etc/passwd", "passwd"},
		{"CON.txt", "_CON.txt"},
		{"  spaced.bin  ", "spaced.bin"},
		{"", ""}, // falls back to the last URL segment
	} {
		// Each case needs its own URL or the duplicate check would short-circuit
		// the second one before its filename was ever looked at.
		rec, added := add(t, f, srv.URL+"/case-"+strconv.Itoa(i), tc.given, 4)
		if rec.Code != http.StatusCreated {
			t.Fatalf("add %q: status = %d (%s)", tc.given, rec.Code, rec.Body.String())
		}
		got := filepath.Base(added.Download.Dest)
		if tc.want == "" {
			if filepath.Dir(added.Download.Dest) != f.filesDir {
				t.Errorf("%q escaped the download directory: %q", tc.given, added.Download.Dest)
			}
			continue
		}
		if got != tc.want {
			t.Errorf("filename %q became %q, want %q", tc.given, got, tc.want)
		}
		if dir := filepath.Dir(added.Download.Dest); dir != f.filesDir {
			t.Errorf("filename %q landed in %q, want %q", tc.given, dir, f.filesDir)
		}
	}
}

func TestServerWithoutRangesGetsOneConnection(t *testing.T) {
	body := bytes.Repeat([]byte{3}, 80_000)
	srv := unrange(t, body)
	f := newFixture(t)

	rec, added := add(t, f, srv.URL+"/anything", "", 8)
	if rec.Code != http.StatusCreated {
		t.Fatalf("add: status = %d (%s)", rec.Code, rec.Body.String())
	}
	if added.Download.Seg {
		t.Error("segmented = true, but the server ignored Range")
	}

	d, err := f.store.Load(added.Download.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := len(d.Segments); got != 1 {
		t.Errorf("segments = %d, want 1 - the server cannot resume, so parallelism would be a lie", got)
	}
	if d.TotalSize != int64(len(body)) {
		t.Errorf("total = %d, want %d", d.TotalSize, len(body))
	}
}

func TestControlsRejectTransitionsThatMakeNoSense(t *testing.T) {
	body := bytes.Repeat([]byte{9}, 2048)
	srv := ranged(t, body)
	f := newFixture(t)

	_, added := add(t, f, srv.URL+"/file.bin", "", 4)
	id := added.Download.ID
	at := func(code int, action string) {
		t.Helper()
		rec := call(t, f, http.MethodPost, "/api/downloads/"+id+"/"+action, testToken, "")
		if rec.Code != code {
			t.Errorf("%s: status = %d, want %d (%s)", action, rec.Code, code, rec.Body.String())
		}
	}

	// Queued: nothing is running, so there is nothing to resume or retry.
	at(http.StatusConflict, "resume")
	at(http.StatusConflict, "retry")
	at(http.StatusOK, "pause") // -> paused

	// Paused: resuming is the point, retrying still means nothing failed.
	at(http.StatusConflict, "retry")
	at(http.StatusOK, "resume") // -> queued

	at(http.StatusOK, "cancel") // -> cancelled

	// Cancelled: it stays stopped and can only be discarded.
	at(http.StatusConflict, "cancel")
	at(http.StatusConflict, "resume")

	del := call(t, f, http.MethodDelete, "/api/downloads/"+id, testToken, "")
	if del.Code != http.StatusOK {
		t.Errorf("remove cancelled: status = %d, want 200", del.Code)
	}

	missing := call(t, f, http.MethodPost, "/api/downloads/does-not-exist/pause", testToken, "")
	if missing.Code != http.StatusNotFound {
		t.Errorf("pause unknown: status = %d, want 404", missing.Code)
	}
}

// A finished download must not be restartable: re-queueing it would fetch it
// all over again into a file that is already correct.
func TestFinishedDownloadCanBeRemovedButNotRestarted(t *testing.T) {
	f := newFixture(t)

	finished := &state.Download{
		ID:        state.NewID(),
		URL:       "http://example.com/already-here.zip",
		Dest:      filepath.Join(f.filesDir, "already-here.zip"),
		PartPath:  filepath.Join(f.filesDir, "already-here.zip.part"),
		TotalSize: 10,
		Segments:  state.PlanSegments(10, 1),
		Status:    state.StatusDone,
		CreatedAt: time.Now(),
	}
	if err := f.store.Save(finished); err != nil {
		t.Fatalf("save: %v", err)
	}

	for action, want := range map[string]int{
		"pause":    http.StatusConflict,
		"resume":   http.StatusConflict,
		"retry":    http.StatusConflict,
		"cancel":   http.StatusConflict,
		"schedule": http.StatusConflict,
	} {
		rec := call(t, f, http.MethodPost, "/api/downloads/"+finished.ID+"/"+action, testToken, "")
		if rec.Code != want {
			t.Errorf("%s done: status = %d, want %d", action, rec.Code, want)
		}
	}

	rec := call(t, f, http.MethodDelete, "/api/downloads/"+finished.ID, testToken, "")
	if rec.Code != http.StatusOK {
		t.Errorf("remove done: status = %d, want 200", rec.Code)
	}
}

func TestAddRejectsAnOversizedBody(t *testing.T) {
	f := newFixture(t)
	huge := `{"url":"http://example.com/x","cookie":"` + strings.Repeat("a", 2<<20) + `"}`
	rec := call(t, f, http.MethodPost, "/api/downloads", testToken, huge)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized body: status = %d, want 400", rec.Code)
	}
}

type limitResponse struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error"`
	Download struct {
		ID         string `json:"id"`
		SpeedLimit int64  `json:"speed_limit"`
	} `json:"download"`
}

type listResponse struct {
	Downloads []struct {
		ID         string `json:"id"`
		SpeedLimit int64  `json:"speed_limit"`
	} `json:"downloads"`
}

func TestSpeedLimitIsRecordedAtAddAndChangeableAfterwards(t *testing.T) {
	f := newFixture(t)
	body := bytes.Repeat([]byte("godl"), 32_000)
	srv := ranged(t, body)

	// Chosen as the download is created, the way the add sheet offers it.
	payload, err := json.Marshal(AddRequest{
		URL:         srv.URL + "/file.bin",
		Filename:    "capped.bin",
		Connections: 4,
		SpeedLimit:  256 * 1024,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec := call(t, f, http.MethodPost, "/api/downloads", testToken, string(payload))
	if rec.Code != http.StatusCreated {
		t.Fatalf("add: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created limitResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode add response: %v", err)
	}
	if created.Download.SpeedLimit != 256*1024 {
		t.Fatalf("speed_limit on add = %d, want %d", created.Download.SpeedLimit, 256*1024)
	}
	stored, err := f.store.Load(created.Download.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if stored.SpeedLimit != 256*1024 {
		t.Fatalf("recorded speed_limit = %d, want %d", stored.SpeedLimit, 256*1024)
	}

	// Changed afterwards, without disturbing anything else about the transfer.
	rec = call(t, f, http.MethodPost, "/api/downloads/"+created.Download.ID+"/speed-limit",
		testToken, `{"speed_limit": 1048576}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("change: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var changed limitResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &changed); err != nil {
		t.Fatalf("decode change response: %v", err)
	}
	if changed.Download.SpeedLimit != 1048576 {
		t.Fatalf("speed_limit after change = %d, want 1048576", changed.Download.SpeedLimit)
	}
	stored, err = f.store.Load(created.Download.ID)
	if err != nil {
		t.Fatalf("load after change: %v", err)
	}
	if stored.SpeedLimit != 1048576 {
		t.Fatalf("record after change = %d, want 1048576", stored.SpeedLimit)
	}
	// The list is what the interface reads; it has to agree with the record.
	rec = call(t, f, http.MethodGet, "/api/downloads", testToken, "")
	var listed listResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	found := false
	for _, item := range listed.Downloads {
		if item.ID == created.Download.ID {
			found = true
			if item.SpeedLimit != 1048576 {
				t.Fatalf("listed speed_limit = %d, want 1048576", item.SpeedLimit)
			}
		}
	}
	if !found {
		t.Fatal("the download is missing from the list")
	}

	// Zero means no ceiling, and must be distinguishable from "not set".
	rec = call(t, f, http.MethodPost, "/api/downloads/"+created.Download.ID+"/speed-limit",
		testToken, `{"speed_limit": 0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if stored, err = f.store.Load(created.Download.ID); err != nil || stored.SpeedLimit != 0 {
		t.Fatalf("record after clearing = %v, want 0", err)
	}
}

// A negative rate cannot be honoured, and an unknown id is not a successful
// change. Both are refused loudly rather than stored as something the engine
// will quietly ignore.
func TestSpeedLimitRefusesWhatItCannotHonour(t *testing.T) {
	f := newFixture(t)
	body := bytes.Repeat([]byte("x"), 4096)
	srv := ranged(t, body)

	payload, err := json.Marshal(AddRequest{URL: srv.URL + "/f.bin", Filename: "f.bin", Connections: 4})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec := call(t, f, http.MethodPost, "/api/downloads", testToken, string(payload))
	var created limitResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if rec = call(t, f, http.MethodPost, "/api/downloads/"+created.Download.ID+"/speed-limit",
		testToken, `{"speed_limit": -1}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("negative limit: status = %d, want 400", rec.Code)
	}
	if rec = call(t, f, http.MethodPost, "/api/downloads/nope/speed-limit",
		testToken, `{"speed_limit": 1024}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id: status = %d, want 404", rec.Code)
	}
	if rec = call(t, f, http.MethodPost, "/api/downloads/"+created.Download.ID+"/speed-limit",
		testToken, `not json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed body: status = %d, want 400", rec.Code)
	}

	// Nothing above may have changed the record.
	stored, err := f.store.Load(created.Download.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if stored.SpeedLimit != 0 {
		t.Fatalf("a rejected request changed speed_limit to %d, want it left at 0", stored.SpeedLimit)
	}

	// The same rule applies when the limit is chosen at add time.
	if rec = call(t, f, http.MethodPost, "/api/downloads", testToken,
		`{"url":"`+srv.URL+`/f.bin","filename":"n.bin","connections":4,"speed_limit":-10}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("add with a negative limit: status = %d, want 400", rec.Code)
	}
}

type orderedList struct {
	Downloads []struct {
		ID       string     `json:"id"`
		StartsAt *time.Time `json:"starts_at"`
	} `json:"downloads"`
}

func idsOf(rec *httptest.ResponseRecorder) []string {
	var out orderedList
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		return nil
	}
	got := make([]string, 0, len(out.Downloads))
	for _, d := range out.Downloads {
		got = append(got, d.ID)
	}
	return got
}

func equalStrings(got, want []string) bool {
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

// The order the window shows is the order the queue runs, so moving a row has
// to be visible in the very next list - not only in the next transfer.
func TestQueueOrderIsVisibleAndChangeableOverTheAPI(t *testing.T) {
	f := newFixture(t)
	body := bytes.Repeat([]byte("godl"), 4096)
	srv := ranged(t, body)

	var ids []string
	for _, name := range []string{"one.bin", "two.bin", "three.bin"} {
		// Three separate addresses: adding the same URL twice is answered as
		// a duplicate and hands back the record already in the list.
		_, added := add(t, f, srv.URL+"/"+name, name, 4)
		if added.Download.ID == "" {
			t.Fatalf("add %s returned no id", name)
		}
		ids = append(ids, added.Download.ID)
	}

	list := func() []string {
		t.Helper()
		rec := call(t, f, http.MethodGet, "/api/downloads", testToken, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("list: status = %d", rec.Code)
		}
		return idsOf(rec)
	}
	post := func(id, action string) *httptest.ResponseRecorder {
		t.Helper()
		return call(t, f, http.MethodPost, "/api/downloads/"+id+"/"+action, testToken, "")
	}

	if got := list(); !equalStrings(got, ids) {
		t.Fatalf("initial order = %v, want %v", got, ids)
	}

	// The newest download to the head, which is two steps.
	for i := 0; i < 2; i++ {
		if rec := post(ids[2], "move-up"); rec.Code != http.StatusOK {
			t.Fatalf("move-up %d: status = %d, body = %s", i, rec.Code, rec.Body.String())
		}
	}
	if got, want := list(), []string{ids[2], ids[0], ids[1]}; !equalStrings(got, want) {
		t.Fatalf("after two move-ups order = %v, want %v", got, want)
	}

	// One down from the head puts it back in the middle.
	if rec := post(ids[2], "move-down"); rec.Code != http.StatusOK {
		t.Fatalf("move-down: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got, want := list(), []string{ids[0], ids[2], ids[1]}; !equalStrings(got, want) {
		t.Fatalf("after move-down order = %v, want %v", got, want)
	}

	// Off both ends: a statement about the list's shape, not a server fault.
	if rec := post(ids[0], "move-up"); rec.Code != http.StatusConflict {
		t.Fatalf("move-up at the head: status = %d, want 409", rec.Code)
	}
	if rec := post(ids[1], "move-down"); rec.Code != http.StatusConflict {
		t.Fatalf("move-down at the foot: status = %d, want 409", rec.Code)
	}
	if rec := post("nope", "move-up"); rec.Code != http.StatusNotFound {
		t.Fatalf("move-up unknown: status = %d, want 404", rec.Code)
	}
}

// A schedule is a timestamp the server has to understand, and a download with
// no start left to postpone is refused before its payload is even read.
func TestScheduleTakesATimeAndRefusesWhatCannotBeHonoured(t *testing.T) {
	f := newFixture(t)
	body := bytes.Repeat([]byte("godl"), 4096)
	srv := ranged(t, body)

	_, added := add(t, f, srv.URL+"/file.bin", "later.bin", 4)
	id := added.Download.ID
	schedule := func(payload string) *httptest.ResponseRecorder {
		t.Helper()
		return call(t, f, http.MethodPost, "/api/downloads/"+id+"/schedule", testToken, payload)
	}

	when := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	rec := schedule(`{"starts_at":"` + when.Format(time.RFC3339) + `"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("schedule: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	stored, err := f.store.Load(id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if stored.StartsAt == nil || !stored.StartsAt.Equal(when) {
		t.Fatalf("starts_at = %v, want %v", stored.StartsAt, when)
	}

	// The list is what the window draws from, so the schedule has to be there.
	rec = call(t, f, http.MethodGet, "/api/downloads", testToken, "")
	var listed orderedList
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed.Downloads) != 1 || listed.Downloads[0].StartsAt == nil {
		t.Fatalf("list did not carry starts_at: %s", rec.Body.String())
	}

	// A time the server cannot parse is refused rather than stored as the
	// zero value, which would start the download at once and look like the
	// schedule had simply been ignored.
	if rec = schedule(`{"starts_at":"tomorrow"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unparseable time: status = %d, want 400", rec.Code)
	}
	if rec = schedule(`not json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed body: status = %d, want 400", rec.Code)
	}
	stored, err = f.store.Load(id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if stored.StartsAt == nil || !stored.StartsAt.Equal(when) {
		t.Fatalf("a rejected schedule left starts_at = %v, want %v", stored.StartsAt, when)
	}

	// Empty means "now", which is how the schedule is cleared.
	if rec = schedule(`{"starts_at":""}`); rec.Code != http.StatusOK {
		t.Fatalf("clear schedule: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	stored, err = f.store.Load(id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if stored.StartsAt != nil {
		t.Fatalf("starts_at after clearing = %v, want nil", stored.StartsAt)
	}

	// A finished download has no start left to postpone, and says so even
	// when the caller sent no body at all to reject instead.
	finished := &state.Download{
		ID:        state.NewID(),
		URL:       "http://example.com/already-here.zip",
		Dest:      filepath.Join(f.filesDir, "already-here.zip"),
		PartPath:  filepath.Join(f.filesDir, "already-here.zip.part"),
		TotalSize: 10,
		Segments:  state.PlanSegments(10, 1),
		Status:    state.StatusDone,
		CreatedAt: time.Now(),
	}
	if err := f.store.Save(finished); err != nil {
		t.Fatalf("save: %v", err)
	}
	rec = call(t, f, http.MethodPost, "/api/downloads/"+finished.ID+"/schedule", testToken, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("schedule a finished download: status = %d, want 409", rec.Code)
	}
}
