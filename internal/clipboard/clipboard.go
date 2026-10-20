// Package clipboard watches the Windows clipboard for a link worth offering
// as a download.
//
// A download manager that cannot pick up what you just copied is asking you to
// retype an address you already have. The clipboard is read by asking Windows
// for it, on a timer, from the process that is already running: no helper
// process, no injection, and nothing that holds the clipboard longer than the
// read itself.
//
// Windows by construction, but the polling loop is not: it reads through a
// Reader, so it can be tested without touching the real clipboard, which is a
// shared resource no test may assume it owns.
package clipboard

import (
	"net/url"
	"strings"
	"sync"
	"time"
)

// Reader answers what is on the clipboard right now.
//
// It is an interface because the clipboard belongs to the whole machine: two
// applications cannot both hold it, so a test that read the real one would be
// racing every other program on the desktop rather than testing this code.
type Reader interface {
	// Text returns the clipboard's text. An empty string means there is
	// nothing to read, which is the ordinary case and not a fault.
	Text() (string, error)
}

// MaxLink is the longest clipboard text still offered as a link. Anything
// longer is a document that happens to begin with an address, not an address.
const MaxLink = 2048

// IsLink reports whether clipboard text is worth offering as a download.
//
// It is deliberately strict. The clipboard holds every fragment of text people
// copy - chat messages, log lines, half a sentence with a link in the middle -
// and a monitor that offered to download those would be switched off within a
// day. A link has to be the whole of what was copied, and it has to be a plain
// http or https address with a host.
func IsLink(text string) bool {
	s := strings.TrimSpace(text)
	if s == "" || len(s) > MaxLink {
		return false
	}
	// A URL carries no whitespace. Anything else with a link somewhere inside
	// it is prose, and prose is not an address to fetch.
	if strings.ContainsAny(s, " \t\r\n") {
		return false
	}
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return u.Host != ""
}

// Monitor polls a Reader on a timer and reports each new link it sees.
type Monitor struct {
	reader Reader
	every  time.Duration

	mu      sync.Mutex
	last    string
	running bool
	done    chan struct{}
}

// New returns a monitor reading from r roughly every d.
func New(r Reader, d time.Duration) *Monitor {
	if d <= 0 {
		d = time.Second
	}
	return &Monitor{reader: r, every: d}
}

// Start begins watching, calling offer with each new link.
//
// offer runs on the monitor's own goroutine, and only with text IsLink has
// already accepted. It is never called twice for one copy: the monitor
// remembers what it last saw, so the next offer is for the next thing copied
// rather than for the same thing on the next tick.
func (m *Monitor) Start(offer func(string)) {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return
	}
	m.running = true
	m.done = make(chan struct{})
	done := m.done
	m.mu.Unlock()

	go func() {
		t := time.NewTicker(m.every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				m.tick(offer)
			}
		}
	}()
}

// Stop ends the watch. It is safe to call more than once, and safe to call
// while an offer is being handled.
func (m *Monitor) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.running {
		return
	}
	m.running = false
	close(m.done)
}

// Running reports whether the watch is currently going.
func (m *Monitor) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

func (m *Monitor) tick(offer func(string)) {
	text, err := m.reader.Text()
	if err != nil || !IsLink(text) {
		// Another application holding the clipboard is ordinary and passes in
		// a moment, so the next tick reads it. Nothing here is worth failing
		// over: missing a copy is recovered by copying it again, and this
		// runs forever.
		return
	}
	text = strings.TrimSpace(text)

	m.mu.Lock()
	if text == m.last {
		m.mu.Unlock()
		return
	}
	m.last = text
	m.mu.Unlock()

	offer(text)
}
