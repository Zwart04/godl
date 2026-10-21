package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A download the browser hands over is a question until somebody answers it.
// Before the wizard existed it was an order: a click in the browser produced a
// finished file somewhere the person was not looking, and nothing on screen in
// between - the browser had already erased its own row by then.

func captureList(t *testing.T, f *fixture) []Capture {
	t.Helper()
	rec := call(t, f, http.MethodGet, "/api/captures", testToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/captures = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Captures []Capture `json:"captures"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode captures: %v", err)
	}
	return out.Captures
}

func postCapture(t *testing.T, f *fixture, body string) *httptest.ResponseRecorder {
	t.Helper()
	return call(t, f, http.MethodPost, "/api/captures", testToken, body)
}

func captureID(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var made struct {
		Capture struct {
			ID string `json:"id"`
		} `json:"capture"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &made); err != nil {
		t.Fatalf("decode capture: %v", err)
	}
	if made.Capture.ID == "" {
		t.Fatal("the capture came back with no id, so the window has nothing to answer with")
	}
	return made.Capture.ID
}

// A handover has to reach the person, not just the queue. The window may be
// hidden in the tray or sitting behind the browser that sent the link, so the
// server asks the window to come forward as soon as a capture is waiting.
func TestCaptureAsksTheWindowToComeForward(t *testing.T) {
	f := newFixture(t)

	var calls int32
	f.server.OnCapture = func() { atomic.AddInt32(&calls, 1) }

	srv := ranged(t, make([]byte, 4096))
	rec := postCapture(t, f, `{"url":"`+srv.URL+`/file.bin","filename":"file.bin"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/captures = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("the window was asked to come forward %d times, want 1", got)
	}

	// An address godl cannot download is not a question, so it must not pull
	// the window away from whatever the person was doing.
	bad := postCapture(t, f, `{"url":"file:///C:/Windows/notepad.exe"}`)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("refused capture = %d, want 400: %s", bad.Code, bad.Body.String())
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("a refused address still asked the window to come forward (%d calls)", got)
	}
}

func TestCaptureHoldsTheDownloadUntilItIsAnswered(t *testing.T) {
	f := newFixture(t)
	srv := ranged(t, make([]byte, 4096))

	rec := postCapture(t, f, `{"url":"`+srv.URL+`/file.bin","filename":"file.bin","connections":4}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/captures = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	id := captureID(t, rec)

	// The pause is the feature. Anything transferred before a choice is made
	// means the dialog is a report rather than a question.
	if got := len(f.server.Queue.Snapshot()); got != 0 {
		t.Fatalf("the queue holds %d downloads before anyone chose, want 0", got)
	}
	if got := len(captureList(t, f)); got != 1 {
		t.Fatalf("GET /api/captures = %d waiting, want 1", got)
	}

	acc := call(t, f, http.MethodPost, "/api/captures/"+id+"/accept", testToken, "")
	if acc.Code != http.StatusCreated {
		t.Fatalf("accept = %d, want 201: %s", acc.Code, acc.Body.String())
	}
	var started addResponse
	if err := json.Unmarshal(acc.Body.Bytes(), &started); err != nil {
		t.Fatalf("decode accept: %v", err)
	}
	if started.Download.ID == "" {
		t.Fatal("accept started nothing")
	}
	if got := len(captureList(t, f)); got != 0 {
		t.Fatalf("%d captures still waiting after one was accepted, want 0", got)
	}
}

// The window folds itself into a small dialog for a handover. It needs to know
// whether answering one leaves another question waiting, so it stays small for
// the next sheet instead of unfolding and folding again.
func TestCaptureAnsweredReportsHowManyAreStillWaiting(t *testing.T) {
	f := newFixture(t)
	answers := make(chan int, 4)
	f.server.OnCaptureAnswered = func(remaining int) { answers <- remaining }

	srv := ranged(t, make([]byte, 4096))
	firstRec := postCapture(t, f, `{"url":"`+srv.URL+`/one.bin","filename":"one.bin"}`)
	first := captureID(t, firstRec)
	secondRec := postCapture(t, f, `{"url":"`+srv.URL+`/two.bin","filename":"two.bin"}`)
	second := captureID(t, secondRec)

	if acc := call(t, f, http.MethodPost, "/api/captures/"+first+"/accept", testToken, ""); acc.Code != http.StatusCreated {
		t.Fatalf("accept = %d: %s", acc.Code, acc.Body.String())
	}
	if got := <-answers; got != 1 {
		t.Fatalf("after one answer, remaining = %d, want 1", got)
	}

	if ign := call(t, f, http.MethodPost, "/api/captures/"+second+"/ignore", testToken, ""); ign.Code != http.StatusOK {
		t.Fatalf("ignore = %d: %s", ign.Code, ign.Body.String())
	}
	if got := <-answers; got != 0 {
		t.Fatalf("after the last answer, remaining = %d, want 0", got)
	}
}

func TestCaptureAcceptAppliesWhatTheWindowChanged(t *testing.T) {
	f := newFixture(t)
	srv := ranged(t, make([]byte, 4096))

	rec := postCapture(t, f, `{"url":"`+srv.URL+`/file.bin","filename":"file.bin"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/captures = %d, want 201", rec.Code)
	}
	id := captureID(t, rec)

	acc := call(t, f, http.MethodPost, "/api/captures/"+id+"/accept", testToken,
		`{"filename":"renamed.bin","connections":2}`)
	if acc.Code != http.StatusCreated {
		t.Fatalf("accept = %d, want 201: %s", acc.Code, acc.Body.String())
	}
	var started addResponse
	_ = json.Unmarshal(acc.Body.Bytes(), &started)
	if !strings.HasSuffix(started.Download.Dest, "renamed.bin") {
		t.Fatalf("dest = %q, want the name the person typed", started.Download.Dest)
	}
}

// The window polls for captures every fraction of a second, and it is a page
// a browser is drawing. A session cookie has no business being handed to it.
func TestCaptureKeepsItsCredentialsOffTheWire(t *testing.T) {
	f := newFixture(t)
	srv := ranged(t, make([]byte, 1024))

	rec := postCapture(t, f, `{"url":"`+srv.URL+`/file.bin","filename":"file.bin",`+
		`"cookie":"session=topsecret","referer":"`+srv.URL+`/page","user_agent":"TestUA/1"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/captures = %d, want 201", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "topsecret") {
		t.Fatalf("the capture reply carries the session cookie: %s", rec.Body.String())
	}

	list := call(t, f, http.MethodGet, "/api/captures", testToken, "")
	if strings.Contains(list.Body.String(), "topsecret") || strings.Contains(list.Body.String(), "TestUA") {
		t.Fatalf("the capture list carries the credentials: %s", list.Body.String())
	}
}

func TestCaptureLearnsItsSizeWithoutHoldingUpTheDialog(t *testing.T) {
	f := newFixture(t)
	srv := ranged(t, make([]byte, 4096))

	rec := postCapture(t, f, `{"url":"`+srv.URL+`/file.bin","filename":"file.bin"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/captures = %d, want 201", rec.Code)
	}

	// Asked for after the sheet is already up, so a slow server costs the
	// size line and never the dialog itself.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if caps := captureList(t, f); len(caps) == 1 && caps[0].Size == 4096 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the capture never learned its size; last seen %+v", captureList(t, f))
}

func TestCaptureCanBePutDown(t *testing.T) {
	f := newFixture(t)
	srv := ranged(t, make([]byte, 1024))

	rec := postCapture(t, f, `{"url":"`+srv.URL+`/file.bin","filename":"file.bin"}`)
	id := captureID(t, rec)

	ignore := call(t, f, http.MethodPost, "/api/captures/"+id+"/ignore", testToken, "")
	if ignore.Code != http.StatusOK {
		t.Fatalf("ignore = %d, want 200", ignore.Code)
	}
	if got := len(captureList(t, f)); got != 0 {
		t.Fatalf("%d captures still waiting after being put down, want 0", got)
	}
	if got := len(f.server.Queue.Snapshot()); got != 0 {
		t.Fatalf("putting a capture down started %d downloads, want 0", got)
	}

	// Clearing something that is no longer there is not an error: the window
	// is tidying up what it was showing, and a 404 here would put a
	// complaint in the console every time someone pressed Cancel.
	again := call(t, f, http.MethodPost, "/api/captures/never-existed/ignore", testToken, "")
	if again.Code != http.StatusOK {
		t.Fatalf("ignoring a capture that is gone = %d, want 200", again.Code)
	}
}

func TestCaptureAcceptingSomethingGoneIsRefused(t *testing.T) {
	f := newFixture(t)
	acc := call(t, f, http.MethodPost, "/api/captures/never-existed/accept", testToken, "")
	if acc.Code != http.StatusNotFound {
		t.Fatalf("accept = %d, want 404: %s", acc.Code, acc.Body.String())
	}
}

// With no window there is nobody to put a dialog in front of. Holding the
// download would mean a link the person never saw simply never arrives.
func TestCaptureStartsAtOnceWhenThereIsNoWindow(t *testing.T) {
	f := newFixture(t)
	f.server.AutoAccept = true
	srv := ranged(t, make([]byte, 4096))

	rec := postCapture(t, f, `{"url":"`+srv.URL+`/file.bin","filename":"file.bin"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/captures = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	var started addResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if started.Download.ID == "" {
		t.Fatal("a headless manager held the download for an answer nobody can give")
	}
	if got := len(captureList(t, f)); got != 0 {
		t.Fatalf("%d captures waiting in a headless manager, want 0", got)
	}
}

func TestCaptureRefusesAnAddressItCannotDownload(t *testing.T) {
	f := newFixture(t)
	rec := postCapture(t, f, `{"url":"file:///C:/Windows/notepad.exe","filename":"notepad.exe"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /api/captures = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// Clicking the same link twice is the same question twice. Only the latest is
// one the person is looking at, and a dialog for the earlier one would be a
// dialog nobody meant to open.
func TestCaptureRepeatsReplaceRatherThanQueue(t *testing.T) {
	f := newFixture(t)
	srv := ranged(t, make([]byte, 1024))

	first := postCapture(t, f, `{"url":"`+srv.URL+`/file.bin","filename":"file.bin"}`)
	if first.Code != http.StatusCreated {
		t.Fatalf("first capture = %d, want 201", first.Code)
	}
	second := postCapture(t, f, `{"url":"`+srv.URL+`/file.bin","filename":"file.bin"}`)
	if second.Code != http.StatusCreated {
		t.Fatalf("second capture = %d, want 201", second.Code)
	}

	caps := captureList(t, f)
	if len(caps) != 1 {
		t.Fatalf("%d captures waiting after the same link twice, want 1", len(caps))
	}
	if caps[0].ID != captureID(t, second) {
		t.Fatal("the older capture survived; only the newest click is one the person is answering")
	}
}
