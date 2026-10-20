package clipboard

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeReader stands in for the real clipboard, which belongs to the whole
// desktop: two applications cannot both hold it, so a test that read the real
// one would be racing every other program on the machine rather than testing
// this code.
type fakeReader struct {
	mu   sync.Mutex
	text string
	err  error
}

func (f *fakeReader) Text() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.text, f.err
}

func (f *fakeReader) set(text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.text = text
	f.err = nil
}

func (f *fakeReader) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func TestIsLinkTakesBareAddressesOnly(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"https://example.com/large.iso", true},
		{"http://example.com/a.zip", true},
		{"https://example.com", true},
		{"  https://example.com/a.zip  ", true},         // the text is still only a link
		{"See https://example.com/a.zip for it", false}, // prose is not an address
		{"https://a.zip\nhttps://b.zip", false},         // two of them is a list
		{"ftp://example.com/a.zip", false},
		{"file:///C:/a.zip", false},
		{"example.com/a.zip", false}, // no scheme
		{"https://", false},          // no host
		{"", false},
		{"   ", false},
		{"just some words", false},
		{"https://" + strings.Repeat("a", MaxLink) + ".com", false}, // a document, not a link
	} {
		if got := IsLink(tc.text); got != tc.want {
			t.Errorf("IsLink(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

// One copy is one offer. Offering again on every tick would be an alarm rather
// than a monitor, and the first thing anyone does with an alarm is turn it off.
func TestMonitorOffersEachCopyOnce(t *testing.T) {
	r := &fakeReader{}
	m := New(r, 5*time.Millisecond)
	defer m.Stop()

	var mu sync.Mutex
	var got []string
	m.Start(func(link string) {
		mu.Lock()
		got = append(got, link)
		mu.Unlock()
	})

	r.set("https://example.com/a.zip")
	time.Sleep(120 * time.Millisecond) // many ticks with the same thing copied

	r.set("https://example.com/b.zip")
	time.Sleep(60 * time.Millisecond)

	m.Stop()

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("offered %d times: %v, want exactly 2 - one per copy", len(got), got)
	}
	if got[0] != "https://example.com/a.zip" || got[1] != "https://example.com/b.zip" {
		t.Errorf("offered %v, want them in the order they were copied", got)
	}
}

func TestMonitorIgnoresEverythingThatIsNotALink(t *testing.T) {
	r := &fakeReader{}
	m := New(r, 5*time.Millisecond)
	defer m.Stop()

	var mu sync.Mutex
	count := 0
	m.Start(func(string) { mu.Lock(); count++; mu.Unlock() })

	for _, text := range []string{
		"",
		"just some words",
		"See https://example.com/a.zip",
		"file:///C:/a.zip",
		"  ",
	} {
		r.set(text)
		time.Sleep(40 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if count != 0 {
		t.Fatalf("offered %d times for text that is not a link", count)
	}
}

// Another application holding the clipboard is ordinary and passes in a
// moment. It must neither end the watch nor be offered, and it must not be
// remembered as having been seen - or the link would be lost once it could
// finally be read.
func TestMonitorSkipsAClipboardItCouldNotRead(t *testing.T) {
	r := &fakeReader{}
	m := New(r, 5*time.Millisecond)
	defer m.Stop()

	var mu sync.Mutex
	count := 0
	m.Start(func(string) { mu.Lock(); count++; mu.Unlock() })

	r.text = "https://example.com/a.zip"
	r.fail(ErrHeld)
	time.Sleep(40 * time.Millisecond)

	mu.Lock()
	held := count
	mu.Unlock()
	if held != 0 {
		t.Fatalf("offered %d times while the clipboard was held", held)
	}

	r.fail(nil)
	time.Sleep(80 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if count != 1 {
		t.Fatalf("offered %d times after the clipboard was free, want 1 - a held copy is skipped, not lost", count)
	}
}

func TestMonitorStopsAndStaysStopped(t *testing.T) {
	r := &fakeReader{}
	m := New(r, 5*time.Millisecond)
	m.Start(func(string) {})

	// Twice must be safe: a shutdown path that also runs on a failed start
	// would otherwise panic on the second close.
	m.Stop()
	m.Stop()

	if m.Running() {
		t.Fatal("still running after Stop")
	}
}

// Stop from several goroutines at once, which is what a shutdown does when the
// window closes while the process is already leaving.
func TestMonitorStopIsSafeFromManyGoroutines(t *testing.T) {
	r := &fakeReader{}
	m := New(r, 5*time.Millisecond)
	m.Start(func(string) {})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); m.Stop() }()
	}
	wg.Wait()

	if m.Running() {
		t.Fatal("still running after concurrent Stop")
	}
}

// Starting twice must not start two goroutines both polling and both offering,
// which would double every capture.
func TestMonitorStartsOnlyOnce(t *testing.T) {
	r := &fakeReader{}
	m := New(r, 5*time.Millisecond)
	defer m.Stop()

	var mu sync.Mutex
	count := 0
	m.Start(func(string) { mu.Lock(); count++; mu.Unlock() })
	m.Start(func(string) { mu.Lock(); count++; mu.Unlock() })

	r.set("https://example.com/a.zip")
	time.Sleep(60 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if count != 1 {
		t.Fatalf("offered %d times, want 1 - a second Start must not double the watch", count)
	}
}
