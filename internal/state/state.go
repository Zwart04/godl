// Package state holds the on-disk representation of a download.
//
// The design mirrors what the REA analysis of IDMan.exe exposed: segments are
// tracked explicitly, the file parts live apart from the final artifact, and
// the validator identity (ETag / Last-Modified) is retained so a resumed
// transfer can prove the server still has the same bytes.
package state

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Status string

const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusPaused    Status = "paused"
	StatusDone      Status = "done"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

// Segment describes one byte range of the target file. End is inclusive, the
// same convention the HTTP Range header uses, so a segment maps 1:1 onto a
// "Range: bytes=start-end" request without translation.
type Segment struct {
	Index   int   `json:"index"`
	Start   int64 `json:"start"`
	End     int64 `json:"end"`
	Written int64 `json:"written"`
	Done    bool  `json:"done"`
}

// Len is the total number of bytes this segment owns.
func (s Segment) Len() int64 { return s.End - s.Start + 1 }

// Remaining is how many bytes are still outstanding in this segment.
func (s Segment) Remaining() int64 { return s.Len() - s.Written }

// Download is the complete persisted record for one file.
type Download struct {
	ID       string `json:"id"`
	URL      string `json:"url"`
	Dest     string `json:"dest"`
	PartPath string `json:"part_path"`

	TotalSize int64 `json:"total_size"`
	// Segmented reports whether the server advertised Accept-Ranges. When false
	// we still allow one connection, but never plan more than one.
	Segmented bool `json:"segmented"`

	// Validator identity, sent back as If-Range on resume. Empty means the
	// server gave us nothing to compare, which we record honestly.
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`

	// Request context the browser had when it offered the link. Servers that
	// gate a file behind a session accept the transfer only if it reproduces
	// those headers, so they are stored with the record.
	Cookie    string `json:"cookie,omitempty"`
	Referer   string `json:"referer,omitempty"`
	UserAgent string `json:"user_agent,omitempty"`

	// Filename the server proposed, kept so we never guess blindly.
	SuggestedName string `json:"suggested_name,omitempty"`

	// SpeedLimit is this download's own ceiling in bytes per second. Zero
	// means no ceiling. It is stored rather than applied once because the
	// transfer it belongs to may outlive the window where it was set.
	SpeedLimit int64 `json:"speed_limit,omitempty"`

	Segments []Segment `json:"segments"`
	Status   Status    `json:"status"`
	Error    string    `json:"error,omitempty"`

	// SingleConnectionFallback records that an integrity mismatch forced us to
	// abandon parallel segments. Kept so the user can see why a fast download
	// became a slow one.
	SingleConnectionFallback bool   `json:"single_connection_fallback,omitempty"`
	FallbackReason           string `json:"fallback_reason,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Progress returns bytes completed out of the total.
func (d *Download) Completed() int64 {
	var n int64
	for _, s := range d.Segments {
		n += s.Written
	}
	return n
}

// NewID creates a short random identifier.
func NewID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// PlanSegments splits size into n ranges. Remainder bytes are distributed from
// the front so no segment ends up empty, which would make a Range request
// degenerate.
func PlanSegments(size int64, n int) []Segment {
	if n < 1 {
		n = 1
	}
	if size <= 0 {
		return []Segment{{Index: 0, Start: 0, End: 0}}
	}
	if int64(n) > size {
		n = int(size)
	}
	base := size / int64(n)
	rem := size % int64(n)

	segs := make([]Segment, 0, n)
	var start int64
	for i := 0; i < n; i++ {
		length := base
		if int64(i) < rem {
			length++
		}
		end := start + length - 1
		segs = append(segs, Segment{Index: i, Start: start, End: end})
		start = end + 1
	}
	return segs
}

// Store persists a download record atomically.
type Store struct{ Dir string }

func (s Store) path(id string) string { return filepath.Join(s.Dir, id+".json") }

func (s Store) Save(d *Download) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	d.UpdatedAt = time.Now()
	raw, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	target := s.path(d.ID)
	// The temp name is unique per write. A fixed one would let two saves of the
	// same record truncate each other halfway through.
	tmp := fmt.Sprintf("%s.%d.tmp", target, time.Now().UnixNano())
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := renameWithRetry(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// renameWithRetry swaps the new file in, tolerating a reader that still holds
// the destination. Go opens files without FILE_SHARE_DELETE, so Windows refuses
// an overwrite while that handle lives. Readers release it before they finish
// parsing, so the window is microseconds and a short backoff closes it. Without
// this the failed write would be indistinguishable from a successful one to
// callers that ignore the error, leaving stale state on disk.
func renameWithRetry(from, to string) error {
	var err error
	for attempt := 0; attempt < 8; attempt++ {
		if err = os.Rename(from, to); err == nil {
			return nil
		}
		time.Sleep(time.Duration(attempt+1) * 2 * time.Millisecond)
	}
	return err
}

func (s Store) Load(id string) (*Download, error) {
	raw, err := os.ReadFile(s.path(id))
	if err != nil {
		return nil, err
	}
	var d Download
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("corrupt state for %s: %w", id, err)
	}
	return &d, nil
}

func (s Store) List() ([]*Download, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Download
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		d, err := s.Load(e.Name()[:len(e.Name())-len(".json")])
		if err != nil {
			continue // a corrupt record must not hide the healthy ones
		}
		out = append(out, d)
	}
	return out, nil
}

func (s Store) Delete(id string) error {
	err := os.Remove(s.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
