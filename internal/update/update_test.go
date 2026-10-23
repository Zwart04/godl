package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.3.6", "0.3.6", 0},
		{"0.3.7", "0.3.6", 1},
		{"0.4.0", "0.3.99", 1},
		{"1.0.0", "0.99.99", 1},
		{"0.2.9", "0.3.0", -1},
		{"v0.3.6", "0.3.6", 0},
		{"0.3.6-beta", "0.3.6", -1},
		{"0.3.6", "0.3.6-rc.1", 1},
		{"", "0.3.6", 0},
		{"0.3", "0.3.0", 0},
	}
	for _, tc := range cases {
		t.Run(tc.a+" vs "+tc.b, func(t *testing.T) {
			if got := Compare(tc.a, tc.b); got != tc.want {
				t.Errorf("Compare(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestChecker(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tag_name":"v9.9.9","name":"godl 9.9.9","html_url":"https://github.com/Zwart04/godl/releases/tag/v9.9.9","published_at":"2026-01-01T00:00:00Z"}`))
	}))
	defer server.Close()

	ch := New("0.3.6")
	ch.Endpoint = server.URL

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	got, err := ch.Check(ctx)
	if err != nil {
		t.Fatalf("check error: %v", err)
	}
	if !got.Enabled || got.Installed != "0.3.6" {
		t.Fatalf("status = %+v", got)
	}
	if got.Latest != "9.9.9" || got.Available != true || got.URL != "https://github.com/Zwart04/godl/releases/tag/v9.9.9" {
		t.Fatalf("unexpected status: %+v", got)
	}
}

func TestCheckerReportsLatestAsUpToDate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tag_name":"v0.3.6"}`))
	}))
	defer server.Close()

	ch := New("0.3.6")
	ch.Endpoint = server.URL
	got, err := ch.Check(context.Background())
	if err != nil {
		t.Fatalf("check error: %v", err)
	}
	if got.Error != "" || got.Available || got.Latest != "0.3.6" {
		t.Fatalf("unexpected status: %+v", got)
	}
}

func TestCheckerRejectsNonVersionTag(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tag_name":"nightly-2026-01-01"}`))
	}))
	defer server.Close()

	ch := New("0.3.6")
	ch.Endpoint = server.URL
	if _, err := ch.Check(context.Background()); err == nil {
		t.Fatal("expected an error for an unversioned tag")
	}
}

func TestCheckerCoalescing(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tag_name":"v0.3.7"}`))
	}))
	defer server.Close()

	ch := New("0.3.6")
	ch.Endpoint = server.URL

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wg := new(sync.WaitGroup)
	wg.Add(5)
	for i := 0; i < 5; i++ {
		go func() {
			defer wg.Done()
			if _, err := ch.Check(ctx); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Errorf("GitHub was contacted %d times, want 1", got)
	}
}

func TestCheckerSideChannelDoesNotLeak(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tag_name":"v0.3.7","html_url":"https://github.com/Zwart04/godl/releases/tag/v0.3.7"}`))
	}))
	defer server.Close()

	ch := New("0.3.6")
	ch.Endpoint = server.URL
	if _, err := ch.Check(context.Background()); err != nil {
		t.Fatalf("check error: %v", err)
	}

	got := ch.Status()
	if got.Error != "" || got.Installed != "0.3.6" {
		t.Fatalf("status leaked internals: %+v", got)
	}
}
