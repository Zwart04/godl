package host

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"godl/internal/config"
	"godl/internal/nmh"
)

// encodeFrames turns messages into the byte stream the browser would write.
func encodeFrames(t *testing.T, msgs ...any) *bytes.Buffer {
	t.Helper()
	buf := new(bytes.Buffer)
	for _, m := range msgs {
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := nmh.Write(buf, raw, nmh.MaxIncoming); err != nil {
			t.Fatalf("frame: %v", err)
		}
	}
	return buf
}

// decodeFrames reads every reply the host produced.
func decodeFrames(t *testing.T, buf *bytes.Buffer) []Response {
	t.Helper()
	var out []Response
	for buf.Len() > 0 {
		var r Response
		if err := nmh.ReadJSON(buf, nmh.MaxIncoming, &r); err != nil {
			t.Fatalf("decode reply: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// run drives Serve to completion against an in-memory stream. A buffer reaches
// EOF once drained, which is exactly what a closed native messaging pipe does.
func run(t *testing.T, c *Client, msgs ...any) []Response {
	t.Helper()
	in := encodeFrames(t, msgs...)
	out := new(bytes.Buffer)
	if err := Serve(context.Background(), in, out, c); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	return decodeFrames(t, out)
}

func TestPingAnswersWithoutContactingTheManager(t *testing.T) {
	replies := run(t, &Client{}, Request{Cmd: "ping"})
	if len(replies) != 1 {
		t.Fatalf("got %d replies, want 1", len(replies))
	}
	if !replies[0].OK {
		t.Error("ping reported failure")
	}
	if replies[0].Version != config.Version {
		t.Errorf("version = %q, want %q", replies[0].Version, config.Version)
	}
}

func TestCommandHandlingIsForgiving(t *testing.T) {
	cases := []struct {
		name    string
		req     Request
		wantErr string
	}{
		{"missing cmd", Request{}, "no cmd"},
		{"unknown cmd", Request{Cmd: "format-drive"}, "unknown cmd"},
		{"add without url", Request{Cmd: "add"}, "no URL"},
		{"cmd with odd case", Request{Cmd: "  PiNg  "}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			replies := run(t, &Client{}, tc.req)
			if len(replies) != 1 {
				t.Fatalf("got %d replies, want 1", len(replies))
			}
			if tc.wantErr == "" {
				if !replies[0].OK {
					t.Fatalf("reply = %+v, want OK", replies[0])
				}
				return
			}
			if replies[0].OK {
				t.Fatalf("reply = %+v, want failure", replies[0])
			}
			if !strings.Contains(replies[0].Error, tc.wantErr) {
				t.Errorf("error = %q, want it to mention %q", replies[0].Error, tc.wantErr)
			}
		})
	}
}

func TestAddForwardsThePageContextToTheManager(t *testing.T) {
	type seen struct {
		auth    string
		body    map[string]any
		content string
	}
	var got seen

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.auth = r.Header.Get("Authorization")
		got.content = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got.body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true,"download":{"id":"dl-1"}}`)
	}))
	t.Cleanup(srv.Close)

	replies := run(t, &Client{BaseURL: srv.URL, Token: "tok-123"}, Request{
		Cmd:         "add",
		URL:         "https://example.com/report.pdf",
		Filename:    "report.pdf",
		Connections: 6,
		Cookie:      "session=abc; theme=dark",
		Referer:     "https://example.com/dashboard",
		UserAgent:   "Mozilla/5.0 (test)",
		TabURL:      "https://example.com/view/1",
	})

	if len(replies) != 1 || !replies[0].OK {
		t.Fatalf("replies = %+v, want one OK reply", replies)
	}
	if replies[0].ID != "dl-1" {
		t.Errorf("id = %q, want dl-1", replies[0].ID)
	}
	if got.auth != "Bearer tok-123" {
		t.Errorf("Authorization = %q, want the bearer token", got.auth)
	}
	if !strings.Contains(got.content, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", got.content)
	}

	// Everything a session-gated download depends on has to survive the hop.
	for field, want := range map[string]string{
		"url":        "https://example.com/report.pdf",
		"filename":   "report.pdf",
		"cookie":     "session=abc; theme=dark",
		"referer":    "https://example.com/dashboard",
		"user_agent": "Mozilla/5.0 (test)",
	} {
		if got.body[field] != want {
			t.Errorf("body[%q] = %v, want %q", field, got.body[field], want)
		}
	}
	if got.body["connections"].(float64) != 6 {
		t.Errorf("connections = %v, want 6", got.body["connections"])
	}
	// tabUrl is context for the extension, not something the manager stores.
	if _, leaked := got.body["tabUrl"]; leaked {
		t.Error("tabUrl was forwarded to the manager although it is not part of the API")
	}
}

func TestListHandsBackTheQueue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/downloads" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true,"downloads":[
			{"id":"a","dest":"C:\\\\files\\\\a.zip","status":"running"},
			{"id":"b","dest":"C:\\\\files\\\\b.zip","status":"done"}
		]}`)
	}))
	t.Cleanup(srv.Close)

	replies := run(t, &Client{BaseURL: srv.URL, Token: "t"}, Request{Cmd: "list"})
	if len(replies) != 1 || !replies[0].OK {
		t.Fatalf("replies = %+v", replies)
	}
	if len(replies[0].Downloads) != 2 {
		t.Fatalf("got %d downloads, want 2", len(replies[0].Downloads))
	}
	if replies[0].Downloads[0].ID != "a" {
		t.Errorf("first download = %q, want a", replies[0].Downloads[0].ID)
	}
}

func TestUnreachableManagerExplainsItself(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now

	replies := run(t, &Client{BaseURL: url, Token: "t"}, Request{Cmd: "list"})
	if replies[0].OK {
		t.Fatal("wanted a failure")
	}
	if !strings.Contains(replies[0].Error, "not reachable") {
		t.Errorf("error = %q, want it to say the manager is not reachable", replies[0].Error)
	}
}

// The browser keeps the native host alive across a manager restart. The
// address read at launch then points at a process that is gone, and a handover
// would fail even though a healthy manager is listening on a new port.
func TestClientFollowsTheManagerToANewPortAfterARestart(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	old := dead.URL
	dead.Close()

	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true,"downloads":[{"id":"after-restart","status":"done"}]}`)
	}))
	t.Cleanup(live.Close)

	c := &Client{
		BaseURL:  old,
		Token:    "t",
		Endpoint: func() string { return live.URL },
	}
	replies := run(t, c, Request{Cmd: "list"})
	if len(replies) != 1 || !replies[0].OK {
		t.Fatalf("replies = %+v, want the restarted manager's answer", replies)
	}
	if len(replies[0].Downloads) != 1 || replies[0].Downloads[0].ID != "after-restart" {
		t.Fatalf("downloads = %+v, want the one the new manager holds", replies[0].Downloads)
	}
	if c.BaseURL != live.URL {
		t.Errorf("BaseURL = %q, want the new endpoint %q", c.BaseURL, live.URL)
	}
}

func TestNeverConfiguredClientSaysHowToFixIt(t *testing.T) {
	replies := run(t, &Client{}, Request{Cmd: "list"})
	if replies[0].OK {
		t.Fatal("wanted a failure")
	}
	if !strings.Contains(replies[0].Error, "godl.exe") {
		t.Errorf("error = %q, want it to name the program to open", replies[0].Error)
	}
}

func TestSetupErrorShortCircuitsEveryRequest(t *testing.T) {
	// The manager's own words matter more than a transport error here, so no
	// request should be made at all.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the manager was contacted despite a setup failure")
	}))
	t.Cleanup(srv.Close)

	replies := run(t, &Client{BaseURL: srv.URL, Token: "t", SetupError: "godl has not finished setting up"}, Request{Cmd: "add", URL: "http://x/y"})
	if replies[0].OK || !strings.Contains(replies[0].Error, "not finished setting up") {
		t.Errorf("reply = %+v, want the setup error", replies[0])
	}
}

func TestServeStopsCleanlyWhenTheBrowserHangsUp(t *testing.T) {
	out := new(bytes.Buffer)
	if err := Serve(context.Background(), bytes.NewReader(nil), out, &Client{}); err != nil {
		t.Fatalf("empty stream: err = %v, want nil", err)
	}
	if out.Len() != 0 {
		t.Errorf("wrote %d bytes with nothing to answer", out.Len())
	}
}

func TestServeFailsOnATruncatedMessage(t *testing.T) {
	full := encodeFrames(t, Request{Cmd: "ping"})
	cut := full.Bytes()[:2]

	err := Serve(context.Background(), bytes.NewReader(cut), new(bytes.Buffer), &Client{})
	if err == nil {
		t.Fatal("a half-written message reported success")
	}
}

func TestServeReportsAWriteFailure(t *testing.T) {
	// If the pipe is gone we must not pretend the exchange succeeded.
	in := encodeFrames(t, Request{Cmd: "ping"})
	if err := Serve(context.Background(), in, failingWriter{}, &Client{}); err == nil {
		t.Fatal("a closed output reported success")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestManagerReplyErrorsReachTheExtension(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		io.WriteString(w, `{"ok":false,"error":"streaming downloads are not supported yet"}`)
	}))
	t.Cleanup(srv.Close)

	replies := run(t, &Client{BaseURL: srv.URL, Token: "t"}, Request{Cmd: "add", URL: "http://x/y"})
	if replies[0].OK {
		t.Fatal("wanted a failure")
	}
	// The manager's sentence, not a status code: this is what the user reads.
	if replies[0].Error != "streaming downloads are not supported yet" {
		t.Errorf("error = %q, want the manager's own message", replies[0].Error)
	}
}

func TestControlClientHasABoundButNotAShortOne(t *testing.T) {
	// The default client must outlast the manager's own probe timeout, or the
	// host would give up while the server is still legitimately working.
	hc := (&Client{}).hc()
	if hc.Timeout < 30*time.Second {
		t.Errorf("timeout = %v, want at least 30s", hc.Timeout)
	}
}
