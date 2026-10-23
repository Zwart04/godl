package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"godl/internal/update"
)

func updateFixture(t *testing.T, current string, release string) (*fixture, *atomic.Int32) {
	t.Helper()
	f := newFixture(t)
	var calls atomic.Int32
	releaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(release))
	}))
	t.Cleanup(releaseServer.Close)

	checker := update.New(current)
	checker.Endpoint = releaseServer.URL
	f.server.Updates = checker
	return f, &calls
}

func TestUpdateCheckReportsANewerRelease(t *testing.T) {
	f, calls := updateFixture(t, "0.3.6", `{
		"tag_name": "v0.3.7",
		"name": "godl 0.3.7",
		"html_url": "https://github.com/Zwart04/godl/releases/tag/v0.3.7",
		"published_at": "2026-10-23T00:00:00Z"
	}`)

	rec := call(t, f, http.MethodPost, "/api/update/check", testToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("update check = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got update.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode update status: %v", err)
	}
	if !got.Enabled || !got.Available || got.Latest != "0.3.7" || got.Installed != "0.3.6" {
		t.Fatalf("unexpected update status: %+v", got)
	}
	if got.URL != "https://github.com/Zwart04/godl/releases/tag/v0.3.7" {
		t.Fatalf("release URL = %q", got.URL)
	}
	if calls.Load() != 1 {
		t.Fatalf("GitHub was contacted %d times, want 1", calls.Load())
	}

	// The page reads the cached result after a restart of the view; it must
	// not spend another request just to redraw the same sentence.
	rec = call(t, f, http.MethodGet, "/api/update", testToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("cached update = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("reading the status contacted GitHub again: %d calls", calls.Load())
	}
}

func TestUpdateCheckKeepsAFailureSoft(t *testing.T) {
	f, _ := updateFixture(t, "0.3.6", `{"tag_name":"not-a-release"}`)

	rec := call(t, f, http.MethodPost, "/api/update/check", testToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("failed update check = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got update.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode update status: %v", err)
	}
	if !got.Enabled || got.Available || got.Error == "" {
		t.Fatalf("unexpected failure status: %+v", got)
	}
}

func TestUpdateCheckCanBeDisabled(t *testing.T) {
	f := newFixture(t)
	f.server.Updates = nil

	rec := call(t, f, http.MethodPost, "/api/update/check", testToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("disabled update check = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got update.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode update status: %v", err)
	}
	if got.Enabled || got.Installed != Version || got.Available {
		t.Fatalf("unexpected disabled status: %+v", got)
	}
}
