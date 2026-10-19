package engine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"

	"godl/internal/state"
)

// verifyOverlaps re-reads the seam between each pair of adjacent segments from
// the server, single-connection, and compares it against what the parallel
// transfer wrote to disk.
//
// This is the behaviour that made the IDMan.exe strings notable: IDM falls back
// to one connection when "newly received data in several connections did not
// match the same data downloaded earlier". A mismatch here means the server
// served inconsistent bytes per connection, so the assembled file cannot be
// trusted.
//
// An unverifiable seam is skipped, not treated as corruption: a download must
// not be discarded merely because a verification request failed.
func (r *Runner) verifyOverlaps(ctx context.Context, client *http.Client, d *state.Download) (string, bool) {
	if !d.Segmented || len(d.Segments) < 2 {
		return "", false
	}
	f, err := os.Open(d.PartPath)
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()

	for i := 1; i < len(d.Segments); i++ {
		if err := ctx.Err(); err != nil {
			return "", false
		}
		if !d.Segments[i-1].Done || !d.Segments[i].Done {
			continue
		}
		start := d.Segments[i].Start
		if start == 0 {
			continue
		}
		reqStart := start - overlapLen
		if reqStart < 0 {
			reqStart = 0
		}
		length := start - reqStart
		if length <= 0 {
			continue
		}

		have := make([]byte, length)
		if _, err := f.ReadAt(have, reqStart); err != nil {
			if err != io.EOF {
				continue
			}
			continue
		}

		want, err := r.fetchRange(ctx, client, d, reqStart, start-1)
		if err != nil {
			continue // could not verify; do not claim corruption
		}
		if len(want) != len(have) || !bytes.Equal(have, want) {
			return fmt.Sprintf("bytes at seam before segment %d did not match the server", i), true
		}
	}
	return "", false
}

// fetchRange performs one exact ranged GET with no overlap accounting.
func (r *Runner) fetchRange(ctx context.Context, client *http.Client, d *state.Download, start, end int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, end-start+1))
}
