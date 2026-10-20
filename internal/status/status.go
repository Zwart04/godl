// Package status shows the transfer dialog: the window that opens while a
// download runs, and says what it is doing while it runs.
//
// A manager that only lists what it has already done is half the thing. The
// person who clicked the link cannot tell whether anything is happening, how
// fast, or how long is left, without finding the window, reading one row among
// several and doing the arithmetic themselves - and for the whole of that
// time the download looks, from the chair in front of it, like nothing. The
// transfer window is where a download manager explains itself: one file at a
// time, in the terms people actually ask in - what is left, how fast is it
// going, can I stop it, does it resume.
//
// It is a window of its own rather than a sheet inside the manager, because
// the manager's own window is a list of everything and is usually sitting
// behind the browser. A progress report nobody can see is not a progress
// report.
//
// Every number here comes from the queue's snapshot - the same one the web
// table and the browser host read, so three views of one transfer cannot
// disagree about it. The per-connection rows are part of that snapshot now:
// the engine has always tracked what each range wrote, and the snapshot used
// to drop it on the way out, which meant the table in this dialog would have
// been a picture with nothing behind it. It is fixed at the source, in
// engine.Progress, rather than reconstructed here from a second account.
//
// The window itself is Windows-only and lives in status_windows.go; the parts
// that turn bytes into words are here, where they can be tested without a
// desktop.
package status

import (
	"fmt"
	"strings"

	"godl/internal/engine"
	"godl/internal/queue"
	"godl/internal/state"
)

// FormatSize reads a byte count the way the rest of godl reads one, so a size
// on this dialog and the same size in the list cannot disagree by a digit.
// Zero and below are unknown, not empty: a file with no size reported yet has
// not failed, it has simply not been asked.
func FormatSize(n int64) string {
	if n <= 0 {
		return "—"
	}
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	v := float64(n)
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	decimals := 0
	if i > 0 && v < 100 {
		decimals = 1
	}
	return fmt.Sprintf("%.*f %s", decimals, v, units[i])
}

// FormatRate reads a transfer rate the same way. Zero is a real answer here -
// nothing is moving - so it is printed as zero rather than as unknown; a
// dialog that said "—" while a file was stalled would be hiding the one thing
// worth knowing.
func FormatRate(bps float64) string {
	if bps <= 0 {
		return "0 B/s"
	}
	return FormatSize(int64(bps+0.5)) + "/s"
}

// FormatDuration is godl's own reading of a span of time - "6s", "1m 05s",
// "1h 04m" - the same words the list uses, so two windows do not teach two
// vocabularies for one idea.
func FormatDuration(secs int64) string {
	if secs < 0 {
		secs = 0
	}
	if secs < 60 {
		return fmt.Sprintf("%ds", secs)
	}
	m := secs / 60
	if m < 60 {
		return fmt.Sprintf("%dm %02ds", m, secs%60)
	}
	return fmt.Sprintf("%dh %02dm", m/60, m%60)
}

// FormatETA is how long at this rate the file has left.
//
// There is no answer while nothing is moving, or while the size is not known,
// and an ellipsis is the honest one for both: this field exists so somebody
// can decide whether to wait, and a number that is wrong is worse for that
// than no number at all.
func FormatETA(done, total int64, bps float64) string {
	if total <= 0 || bps <= 0 || done >= total {
		return "—"
	}
	secs := int64(float64(total-done)/bps + 0.5)
	return FormatDuration(secs)
}

// DownloadedText is bytes done and how far that is through the file, in one
// string so the two can never be read from different moments: a row showing
// "12.4 MB (40.0%)" is one measurement, not two.
func DownloadedText(done, total int64) string {
	if total <= 0 {
		return FormatSize(done)
	}
	p := float64(done) / float64(total) * 100
	if p > 100 {
		p = 100
	}
	return fmt.Sprintf("%s (%.1f%%)", FormatSize(done), p)
}

// Percent is the transfer's own percentage, clamped to its track.
//
// The clamp matters because a record can read past 100 for an instant while
// the last range closes, and a bar that ran off the end of its groove looks
// broken rather than finished.
func Percent(s queue.Snapshot) float64 {
	p := s.Progress.Percent
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}

// StatusText is what the Status line says, in the words the transfer itself is
// in. A failure speaks in the engine's own words rather than a label: the
// reason is the entire content of a failed transfer, and a dialog that said
// only "failed" would have made the person ask the rest of the question again
// somewhere else.
func StatusText(s queue.Snapshot) string {
	switch s.Status {
	case state.StatusDone:
		return "Complete"
	case state.StatusCancelled:
		return "Cancelled"
	case state.StatusPaused:
		return "Paused"
	case state.StatusFailed:
		if s.Error != "" {
			return s.Error
		}
		return "Failed"
	case state.StatusQueued:
		if s.StartsAt != nil {
			return "Waiting for its schedule"
		}
		return "Waiting for a free slot"
	case state.StatusRunning:
		if s.Progress.Done == 0 {
			return "Connecting…"
		}
		return "Receiving data…"
	default:
		return string(s.Status)
	}
}

// ResumeText answers the dialog's "Resume capability" line.
//
// The transfer's own probe decided this before the first byte was asked for.
// A server that refused ranges cannot be resumed, and answering Yes to it
// would be a promise the next start of that same download would break.
func ResumeText(s queue.Snapshot) string {
	if s.Segmented {
		return "Yes"
	}
	return "No"
}

// ConnectionInfo is the Info column of the connection table: what one
// connection is doing, rather than only what it has managed to carry.
//
// The distinction the phases carry is the useful one - a connection waiting on
// its response has written exactly as many bytes as one nobody has tried yet,
// and only the phase can tell them apart.
func ConnectionInfo(seg engine.SegState) string {
	switch {
	case seg.Done:
		return "Complete"
	case seg.Phase == engine.PhaseGet:
		return "Send GET…"
	case seg.Phase == engine.PhaseReceiving:
		return "Receiving data…"
	default:
		return "Queued"
	}
}

// TitleBar is what the window calls itself: the percentage first, because that
// is what somebody glancing at the taskbar wants, then the file's name.
//
// Only the name goes in. A window title is a label worn while the window is
// among others, and an address of a hundred characters would wear it past the
// point of being read.
func TitleBar(s queue.Snapshot) string {
	name := baseName(s.Dest)
	if name == "" {
		name = "godl"
	}
	p := int(Percent(s) + 0.5)
	return fmt.Sprintf("%d%% godl — %s", p, name)
}

// StripBlock is one connection's place along the map under the progress bar:
// where its range sits in the file, and how much of that range it has filled.
type StripBlock struct {
	// X and W are pixels from the left edge of the strip.
	X, W int
	// Filled is how many of those W pixels have actually arrived.
	Filled int
}

// StripRects places one block per connection along a strip of the given width.
//
// The blocks sit where those byte ranges sit in the file, rather than being
// divided evenly, because the strip's entire claim is that it is a picture of
// the file: a connection holding the last quarter should sit over the last
// quarter, and a person who watches two of them meet knows the transfer is
// nearly joined up.
//
// With no size to divide by there is no picture to draw - the caller is
// showing "—" instead - so the answer is empty rather than a guess.
func StripRects(segs []engine.SegState, total int64, width int) []StripBlock {
	if len(segs) == 0 || width <= 0 {
		return nil
	}
	if total <= 0 {
		// Unknown size: an even split is still the truth about how the work
		// was planned, which is the only claim left to make.
		out := make([]StripBlock, len(segs))
		w := width / len(segs)
		for i := range out {
			out[i] = StripBlock{X: i * w, W: w}
		}
		return out
	}

	out := make([]StripBlock, len(segs))
	for i, s := range segs {
		start := int(float64(s.Start) / float64(total) * float64(width))
		end := int(float64(s.End+1) / float64(total) * float64(width))
		if start < 0 {
			start = 0
		}
		if end > width {
			end = width
		}
		if end < start {
			end = start
		}
		block := StripBlock{X: start, W: end - start}
		if s.Length > 0 && block.W > 0 {
			ratio := float64(s.Written) / float64(s.Length)
			if ratio < 0 {
				ratio = 0
			}
			if ratio > 1 {
				ratio = 1
			}
			block.Filled = int(ratio*float64(block.W) + 0.5)
		}
		if s.Done {
			block.Filled = block.W
		}
		out[i] = block
	}
	return out
}

// baseName is the file's name out of a path. Both separators are treated as
// one: the manager runs on Windows, but the destination it is reading may have
// arrived from anything, and a title that kept a full path would be the title
// of a filesystem rather than of a file.
func baseName(dest string) string {
	if i := strings.LastIndexAny(dest, `/\`); i >= 0 {
		return dest[i+1:]
	}
	return dest
}
