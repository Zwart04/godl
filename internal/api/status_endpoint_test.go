package api

import (
	"net/http"
	"testing"
)

// Double-clicking a row asks what one download is doing, and the answer is
// that download's own window: its URL, its rate, the time left, and what each
// connection is carrying. The API's whole job here is to carry the question
// with the right id, and to be honest about the case where there is nobody at
// home to open it.

func TestStatusEndpointOpensTheRightDialog(t *testing.T) {
	f := newFixture(t)
	src := ranged(t, make([]byte, 4096))

	rec, out := add(t, f, src.URL+"/file.bin", "file.bin", 4)
	if rec.Code != http.StatusCreated {
		t.Fatalf("add = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	if out.Download.ID == "" {
		t.Fatal("the download came back with no id, so the window has nothing to open for")
	}

	var opened []string
	f.server.OpenStatus = func(id string) { opened = append(opened, id) }

	rec = call(t, f, http.MethodPost, "/api/downloads/"+out.Download.ID+"/status", testToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(opened) != 1 || opened[0] != out.Download.ID {
		t.Fatalf("opened %v, want the double-clicked download %q", opened, out.Download.ID)
	}
}

// A stale tab cannot ask for a window about nothing: the record is looked up
// before anybody is told to make one, which is what keeps a dialog from
// opening for a download that has already been removed.
func TestStatusEndpointRefusesADownloadThatIsGone(t *testing.T) {
	f := newFixture(t)

	var opened []string
	f.server.OpenStatus = func(id string) { opened = append(opened, id) }

	rec := call(t, f, http.MethodPost, "/api/downloads/not-a-download/status", testToken, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if len(opened) != 0 {
		t.Fatalf("a dialog was opened for %v, want none", opened)
	}
}

// With no window to show - the headless manager, running as a service with
// nobody in front of it - the request is answered 204 rather than failing.
// The request was honest; there is simply nowhere to put the answer.
func TestStatusEndpointWithoutAWindowSaysSoQuietly(t *testing.T) {
	f := newFixture(t)
	src := ranged(t, make([]byte, 4096))

	rec, out := add(t, f, src.URL+"/file.bin", "file.bin", 4)
	if rec.Code != http.StatusCreated {
		t.Fatalf("add = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	f.server.OpenStatus = nil

	rec = call(t, f, http.MethodPost, "/api/downloads/"+out.Download.ID+"/status", testToken, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", rec.Code, rec.Body.String())
	}
}
