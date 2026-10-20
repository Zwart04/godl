package status

import (
	"testing"
	"time"

	"godl/internal/engine"
	"godl/internal/queue"
	"godl/internal/state"
)

// The dialog's whole job is reporting numbers correctly while they move.
// Every one of these is a line somebody reads to decide whether to keep
// waiting, so each is checked against the house style the list already uses:
// two windows showing the same bytes must not show two different digits.
func TestFormatSizeMatchesTheRestOfGodl(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "—"},
		{-1, "—"},
		{1, "1 B"},
		{512, "512 B"},
		{1024, "1.0 KB"},
		{12800, "12.5 KB"},
		{100 * 1024 * 1024, "100 MB"},
		// Past one unit the count steps up rather than growing digits, which
		// is what keeps a title bar and a list column readable at any size.
		{17745 * 1024 * 1024, "17.3 GB"},
		{1536 * 1024 * 1024, "1.5 GB"},
	}
	for _, c := range cases {
		if got := FormatSize(c.in); got != c.want {
			t.Errorf("FormatSize(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatRateSaysZeroRatherThanUnknown(t *testing.T) {
	// Nothing moving is a fact about a transfer, not an absence of one: a
	// stalled file that reported "—" would hide the exact thing worth seeing.
	if got := FormatRate(0); got != "0 B/s" {
		t.Errorf("FormatRate(0) = %q, want %q", got, "0 B/s")
	}
	if got := FormatRate(-5); got != "0 B/s" {
		t.Errorf("FormatRate(-5) = %q, want %q", got, "0 B/s")
	}
	if got := FormatRate(500); got != "500 B/s" {
		t.Errorf("FormatRate(500) = %q, want %q", got, "500 B/s")
	}
	if got := FormatRate(12.5 * 1024 * 1024); got != "12.5 MB/s" {
		t.Errorf("FormatRate(12.5MB) = %q, want %q", got, "12.5 MB/s")
	}
}

func TestFormatDurationUsesTheListsWords(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0s"},
		{-3, "0s"},
		{6, "6s"},
		{65, "1m 05s"},
		{60 * 15, "15m 00s"},
		{3840, "1h 04m"},
		{7200, "2h 00m"},
	}
	for _, c := range cases {
		if got := FormatDuration(c.in); got != c.want {
			t.Errorf("FormatDuration(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatETAAbsentWhenItCannotBeKnown(t *testing.T) {
	// An ETA exists so somebody can decide whether to wait. Any of these
	// three answered with a number would be a decision made from a fiction.
	if got := FormatETA(0, 0, 1000); got != "—" {
		t.Errorf("unknown size: ETA = %q, want —", got)
	}
	if got := FormatETA(0, 1000, 0); got != "—" {
		t.Errorf("stalled: ETA = %q, want —", got)
	}
	if got := FormatETA(1000, 1000, 1000); got != "—" {
		t.Errorf("finished: ETA = %q, want —", got)
	}
	if got := FormatETA(0, 1000, 100); got != "10s" {
		t.Errorf("ETA = %q, want 10s", got)
	}
	if got := FormatETA(900, 1000, 100); got != "1s" {
		t.Errorf("ETA = %q, want 1s", got)
	}
}

// Bytes and percentage travel in one string, so they are one measurement and
// cannot be read from two different moments.
func TestDownloadedTextCarriesBytesAndShareTogether(t *testing.T) {
	if got := DownloadedText(400, 1000); got != "400 B (40.0%)" {
		t.Errorf("DownloadedText = %q, want %q", got, "400 B (40.0%)")
	}
	if got := DownloadedText(0, 1000); got != "— (0.0%)" {
		t.Errorf("DownloadedText = %q, want %q", got, "— (0.0%)")
	}
	if got := DownloadedText(1200, 1000); got != "1.2 KB (100.0%)" {
		t.Errorf("DownloadedText over 100 = %q, want the bytes and the clamp", got)
	}
	// No size: the bytes are all there is to say, and a percentage of an
	// unknown total would be arithmetic on nothing.
	if got := DownloadedText(400, 0); got != "400 B" {
		t.Errorf("DownloadedText without a size = %q, want the bytes alone", got)
	}
}

// A bar that ran off the end of its groove reads as broken, not finished.
func TestPercentStaysOnItsTrack(t *testing.T) {
	mk := func(p float64) queue.Snapshot {
		return queue.Snapshot{Progress: engine.Snapshot{Percent: p}}
	}
	if got := Percent(mk(-4)); got != 0 {
		t.Errorf("Percent(-4) = %v, want 0", got)
	}
	if got := Percent(mk(42.5)); got != 42.5 {
		t.Errorf("Percent(42.5) = %v, want 42.5", got)
	}
	if got := Percent(mk(100.4)); got != 100 {
		t.Errorf("Percent(100.4) = %v, want 100", got)
	}
}

func TestStatusTextSaysWhatTheTransferIsDoing(t *testing.T) {
	cases := []struct {
		name string
		snap queue.Snapshot
		want string
	}{
		{"running with bytes", queue.Snapshot{
			Status:   state.StatusRunning,
			Progress: engine.Snapshot{Done: 10},
		}, "Receiving data…"},
		// Before the first byte arrives the transfer is asking, not receiving
		// - the only moment when anything is being requested of the server.
		{"running, nothing yet", queue.Snapshot{Status: state.StatusRunning}, "Connecting…"},
		{"paused", queue.Snapshot{Status: state.StatusPaused}, "Paused"},
		{"queued", queue.Snapshot{Status: state.StatusQueued}, "Waiting for a free slot"},
		{"scheduled", queue.Snapshot{
			Status:   state.StatusQueued,
			StartsAt: func() *time.Time { t := time.Unix(0, 0); return &t }(),
		}, "Waiting for its schedule"},
		{"done", queue.Snapshot{Status: state.StatusDone}, "Complete"},
		{"cancelled", queue.Snapshot{Status: state.StatusCancelled}, "Cancelled"},
		// A failure is its reason. "Failed" would make the person go and ask
		// the same question again somewhere else.
		{"failed with a reason", queue.Snapshot{
			Status: state.StatusFailed,
			Error:  "server refused the requested range",
		}, "server refused the requested range"},
		{"failed without one", queue.Snapshot{Status: state.StatusFailed}, "Failed"},
	}
	for _, c := range cases {
		if got := StatusText(c.snap); got != c.want {
			t.Errorf("%s: StatusText = %q, want %q", c.name, got, c.want)
		}
	}
}

// The transfer's own probe decided this before the first byte. Answering Yes
// for a server that refused ranges would promise a resume that would break.
func TestResumeTextFollowsTheProbesAnswer(t *testing.T) {
	if got := ResumeText(queue.Snapshot{Segmented: true}); got != "Yes" {
		t.Errorf("Segmented: ResumeText = %q, want Yes", got)
	}
	if got := ResumeText(queue.Snapshot{Segmented: false}); got != "No" {
		t.Errorf("not segmented: ResumeText = %q, want No", got)
	}
}

func TestConnectionInfoReadsThePhaseNotTheByteCount(t *testing.T) {
	cases := []struct {
		seg  engine.SegState
		want string
	}{
		// A connection waiting on its response has written nothing, exactly
		// like one nobody has tried yet. Only the phase tells them apart.
		{engine.SegState{Phase: engine.PhaseGet}, "Send GET…"},
		{engine.SegState{Phase: engine.PhaseReceiving, Written: 10}, "Receiving data…"},
		{engine.SegState{Phase: engine.PhaseQueued}, "Queued"},
		{engine.SegState{Done: true, Written: 10, Phase: engine.PhaseReceiving}, "Complete"},
	}
	for _, c := range cases {
		if got := ConnectionInfo(c.seg); got != c.want {
			t.Errorf("ConnectionInfo(%+v) = %q, want %q", c.seg, got, c.want)
		}
	}
}

// The title is worn while the window is among others: the percentage first,
// then the name - never a path, and never an address.
func TestTitleBarLeadsWithThePercentageAndTheName(t *testing.T) {
	got := TitleBar(queue.Snapshot{
		Dest:     `C:\Users\z\Downloads\godl\Programs\debian.iso`,
		Progress: engine.Snapshot{Percent: 24.4},
	})
	if want := "24% godl — debian.iso"; got != want {
		t.Errorf("TitleBar = %q, want %q", got, want)
	}

	if got, want := TitleBar(queue.Snapshot{Progress: engine.Snapshot{Percent: 99.6}}), "100% godl — godl"; got != want {
		t.Errorf("TitleBar without a name = %q, want %q", got, want)
	}
}

// The strip claims to be a picture of the file, so the blocks sit where the
// byte ranges sit rather than being cut into equal shares.
func TestStripRectsPlaceEachConnectionWhereItsBytesAre(t *testing.T) {
	segs := []engine.SegState{
		{Index: 0, Start: 0, End: 499, Length: 500, Written: 250},
		{Index: 1, Start: 500, End: 999, Length: 500, Written: 500, Done: true},
	}
	blocks := StripRects(segs, 1000, 100)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d, want 2", len(blocks))
	}
	if blocks[0].X != 0 || blocks[0].W != 50 {
		t.Errorf("first block = %+v, want X=0 W=50", blocks[0])
	}
	if blocks[0].Filled != 25 {
		t.Errorf("first block filled = %d, want half of 50", blocks[0].Filled)
	}
	if blocks[1].X != 50 || blocks[1].W != 50 {
		t.Errorf("second block = %+v, want X=50 W=50", blocks[1])
	}
	// A finished range is full however the last byte arrived.
	if blocks[1].Filled != 50 {
		t.Errorf("second block filled = %d, want the whole block", blocks[1].Filled)
	}
}

func TestStripRectsWithoutASizeEvenlyDividesThePlan(t *testing.T) {
	segs := []engine.SegState{{Index: 0}, {Index: 1}, {Index: 2}}
	blocks := StripRects(segs, 0, 90)
	if len(blocks) != 3 {
		t.Fatalf("blocks = %d, want 3", len(blocks))
	}
	for i, b := range blocks {
		if b.W != 30 {
			t.Errorf("block %d width = %d, want 30", i, b.W)
		}
	}

	// Nothing to draw rather than a guess: with no plan the caller shows "—".
	if got := StripRects(nil, 1000, 100); got != nil {
		t.Errorf("StripRects without a plan = %v, want nil", got)
	}
	if got := StripRects(segs, 1000, 0); got != nil {
		t.Errorf("StripRects with no width = %v, want nil", got)
	}
}

// A block must never run off the end of the strip, whatever the server claims
// about its own size: painting past the track would bleed into whatever is
// beside it.
func TestStripRectsStayOnTheTrack(t *testing.T) {
	segs := []engine.SegState{
		// A server that reported a size smaller than its own ranges.
		{Index: 0, Start: 0, End: 9999, Length: 10000, Written: 10000, Done: true},
	}
	blocks := StripRects(segs, 100, 40)
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d, want 1", len(blocks))
	}
	if b := blocks[0]; b.X < 0 || b.W < 0 || b.X+b.W > 40 {
		t.Errorf("block = %+v, want to stay within [0,40]", b)
	}
}

func TestBaseNameIsTheFileNotThePath(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`C:\Downloads\file.iso`, "file.iso"},
		{"https://host/a/b/c.zip", "c.zip"},
		{"plain.zip", "plain.zip"},
		{"", ""},
	} {
		if got := baseName(c.in); got != c.want {
			t.Errorf("baseName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
