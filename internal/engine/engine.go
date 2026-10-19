// Package engine performs the actual transfer.
//
// Two behaviours here are taken directly from the IDMan.exe analysis: the four
// Range request shapes, and the cross-segment integrity check that degrades a
// corrupt parallel download back to a single connection.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"godl/internal/state"
)

const (
	// chunkSize is how much we read per Read call.
	chunkSize = 256 * 1024
	// overlapLen is how many bytes each segment re-reads from the segment
	// before it. Those bytes are compared after the transfer; a mismatch means
	// the server sent inconsistent data on different connections.
	overlapLen = 64
	// userAgent is deliberately identifiable; pretending to be a browser gets
	// some servers to reject multi-connection downloads outright.
	userAgent = "godl/1.0"
)

var (
	// ErrServerRejectedRange means the server answered 200 to a Range request,
	// so it cannot serve a partial file.
	ErrServerRejectedRange = errors.New("server does not support range requests")
	// ErrValidatorChanged means the remote identity moved between probes, so
	// the bytes we already hold belong to a different file.
	ErrValidatorChanged = errors.New("remote file changed since the download was created")
)

// ProbeResult is what we learned about the remote file before planning.
type ProbeResult struct {
	TotalSize    int64
	Segmented    bool
	ETag         string
	LastModified string
	ContentType  string
	Name         string
}

// Hints reproduce the request a browser made when it offered the link. A file
// behind a login only transfers if these travel with it.
type Hints struct {
	Referer   string
	Cookie    string
	UserAgent string
}

// apply puts the hints on a request, leaving unset ones alone so the default
// user agent survives.
func (h Hints) apply(req *http.Request) {
	if h.Cookie != "" {
		req.Header.Set("Cookie", h.Cookie)
	}
	if h.Referer != "" {
		req.Header.Set("Referer", h.Referer)
	}
	ua := h.UserAgent
	if ua == "" {
		ua = userAgent
	}
	req.Header.Set("User-Agent", ua)
}

// Probe inspects the remote file with a one-byte ranged GET, the cheapest
// request that still proves whether Accept-Ranges is honoured.
func Probe(ctx context.Context, client *http.Client, url string, hints ...Hints) (*ProbeResult, error) {
	var h Hints
	if len(hints) > 0 {
		h = hints[0]
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", "bytes=0-0")
	h.apply(req)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.CopyN(io.Discard, resp.Body, 1)

	res := &ProbeResult{
		TotalSize:    resp.ContentLength,
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		ContentType:  resp.Header.Get("Content-Type"),
		Name:         fileName(resp),
	}

	switch resp.StatusCode {
	case http.StatusPartialContent:
		total, err := parseContentRangeTotal(resp.Header.Get("Content-Range"))
		if err != nil {
			return nil, err
		}
		res.TotalSize = total
		res.Segmented = true
		return res, nil

	case http.StatusOK:
		// The server ignored Range entirely. ContentLength is then the whole
		// file, and -1 when chunked; -1 must not be planned against.
		res.Segmented = false
		return res, nil

	default:
		return nil, fmt.Errorf("probe: unexpected status %s", resp.Status)
	}
}

// parseContentRangeTotal reads "bytes <start>-<end>/<total>". A "*" total means
// the server will not say; we report that rather than guessing 0.
func parseContentRangeTotal(h string) (int64, error) {
	h = strings.TrimSpace(h)
	if !strings.HasPrefix(strings.ToLower(h), "bytes") {
		return 0, fmt.Errorf("malformed Content-Range %q", h)
	}
	slash := strings.LastIndex(h, "/")
	if slash < 0 {
		return 0, fmt.Errorf("malformed Content-Range %q", h)
	}
	total := strings.TrimSpace(h[slash+1:])
	if total == "*" {
		return 0, errors.New("server withheld total size in Content-Range")
	}
	n, err := strconv.ParseInt(total, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("malformed Content-Range total %q", total)
	}
	return n, nil
}

// fileName prefers RFC 5987 filename*, then the plain filename, and returns an
// empty string when the server proposed nothing. We never invent a name.
func fileName(resp *http.Response) string {
	cd := resp.Header.Get("Content-Disposition")
	if cd == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(cd)
	if err != nil {
		return ""
	}
	if n := params["filename*"]; n != "" {
		return strings.TrimPrefix(n, "UTF-8''")
	}
	return params["filename"]
}

// Options tune one run.
type Options struct {
	// Connections is the desired parallelism, clamped to 1 when the server
	// rejected ranges or a previous run already fell back.
	Connections int
	// HTTPClient overrides the runner's client.
	HTTPClient *http.Client
	// Limit caps this transfer as a whole. Nil means no ceiling. It is read
	// on every wait, so it may be moved while the transfer is running.
	Limit *Limit
	// Progress, when set, is kept up to date while the transfer runs so a
	// watcher can read it from another goroutine.
	Progress *Progress
}

// Report summarises a finished run.
type Report struct {
	FallbackUsed bool
	FallbackWhy  string
	Duration     time.Duration
	Bytes        int64
}

// Runner executes a prepared download.
type Runner struct {
	Client *http.Client
}

func (r *Runner) client(opt Options) *http.Client {
	if opt.HTTPClient != nil {
		return opt.HTTPClient
	}
	if r.Client != nil {
		return r.Client
	}
	return &http.Client{}
}

// Run transfers every outstanding segment and finalises the file.
func (r *Runner) Run(ctx context.Context, d *state.Download, opt Options) (*Report, error) {
	client := r.client(opt)

	n := opt.Connections
	if !d.Segmented || n < 1 || d.SingleConnectionFallback {
		n = 1
	}

	if err := ensurePartFile(d); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(d.PartPath, os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	prog := opt.Progress
	if prog == nil {
		prog = NewProgress(d.TotalSize)
	}
	// Bytes already on disk count towards the progress, otherwise a resumed
	// download would appear to start over.
	prog.Seed(d.Completed())

	lim := newLimiter(opt.Limit)
	start := time.Now()

	if err := r.runSegments(ctx, client, d, f, lim, prog, n); err != nil {
		d.Status = state.StatusFailed
		d.Error = err.Error()
		return nil, err
	}

	if why, bad := r.verifyOverlaps(ctx, client, d); bad && n > 1 {
		// The server sent different bytes for the same offset on different
		// connections. Discard the parallel result and redo it on one
		// connection, which is what IDM does in this situation.
		d.SingleConnectionFallback = true
		d.FallbackReason = why
		resetSegments(d)
		prog.Seed(d.Completed())
		if err := r.runSegments(ctx, client, d, f, lim, prog, 1); err != nil {
			d.Status = state.StatusFailed
			d.Error = err.Error()
			return nil, err
		}
		if _, bad2 := r.verifyOverlaps(ctx, client, d); bad2 {
			err := errors.New("data mismatch persists on a single connection")
			d.Status = state.StatusFailed
			d.Error = err.Error()
			return nil, err
		}
	}

	if err := f.Sync(); err != nil {
		return nil, err
	}
	// The part file is pre-sized, so a short final size means the server
	// under-delivered. Say so rather than ship a truncated file.
	if fi, err := f.Stat(); err == nil && fi.Size() != d.TotalSize {
		err := fmt.Errorf("size mismatch: have %d bytes, expected %d", fi.Size(), d.TotalSize)
		d.Status = state.StatusFailed
		d.Error = err.Error()
		return nil, err
	}

	// The handle must be released before publishing: on Windows a rename fails
	// while any handle to the source path is still open.
	if err := f.Close(); err != nil {
		return nil, err
	}

	d.Status = state.StatusDone

	// Publish last: the part file replaces the destination only after the size
	// check passed, so an interrupted or truncated transfer can never appear as
	// a finished file.
	if err := os.Remove(d.Dest); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.Rename(d.PartPath, d.Dest); err != nil {
		return nil, err
	}

	return &Report{
		FallbackUsed: d.SingleConnectionFallback,
		FallbackWhy:  d.FallbackReason,
		Duration:     time.Since(start),
		Bytes:        d.Completed(),
	}, nil
}

func ensurePartFile(d *state.Download) error {
	if d.PartPath == "" {
		d.PartPath = d.Dest + ".part"
	}
	f, err := os.OpenFile(d.PartPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	// Only size it up; shrinking would destroy bytes we already verified.
	if fi.Size() < d.TotalSize {
		return f.Truncate(d.TotalSize)
	}
	return nil
}

func (r *Runner) runSegments(ctx context.Context, client *http.Client, d *state.Download, f *os.File, lim *limiter, prog *Progress, n int) error {
	pending := make([]*state.Segment, 0, len(d.Segments))
	for i := range d.Segments {
		if !d.Segments[i].Done {
			pending = append(pending, &d.Segments[i])
		}
	}
	if len(pending) == 0 {
		return nil
	}
	if n > len(pending) {
		n = len(pending)
	}

	jobs := make(chan *state.Segment)
	results := make(chan error, len(pending))

	for w := 0; w < n; w++ {
		go func() {
			for seg := range jobs {
				results <- r.fetchSegment(ctx, client, d, f, lim, prog, seg)
			}
		}()
	}
	for _, seg := range pending {
		select {
		case <-ctx.Done():
			close(jobs)
			return ctx.Err()
		case jobs <- seg:
		}
	}
	close(jobs)

	var firstErr error
	for range pending {
		if err := <-results; err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// fetchSegment pulls one range. For segments after the first, the request
// starts overlapLen bytes early so those bytes can be cross-checked later;
// only the owned range is written to disk.
func (r *Runner) fetchSegment(ctx context.Context, client *http.Client, d *state.Download, f *os.File, lim *limiter, prog *Progress, seg *state.Segment) error {
	reqStart := seg.Start
	if seg.Index > 0 && seg.Start > overlapLen {
		reqStart = seg.Start - overlapLen
	}

	byteStart := reqStart + seg.Written
	if byteStart > seg.End {
		seg.Done = true
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.URL, nil)
	if err != nil {
		return err
	}
	Hints{Cookie: d.Cookie, Referer: d.Referer, UserAgent: d.UserAgent}.apply(req)
	if d.Segmented {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", byteStart, seg.End))
		if d.ETag != "" {
			req.Header.Set("If-Range", d.ETag)
		} else if d.LastModified != "" {
			req.Header.Set("If-Range", d.LastModified)
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if err := checkResumeStatus(d, resp); err != nil {
		return err
	}

	// Prefix bytes belonging to the previous segment must not be written.
	prefix := int64(0)
	if reqStart < seg.Start {
		prefix = seg.Start - reqStart
	}
	discard := prefix

	buf := make([]byte, chunkSize)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		nr, er := resp.Body.Read(buf)
		if nr > 0 {
			chunk := buf[:nr]
			if discard > 0 {
				skip := int64(nr)
				if skip > discard {
					skip = discard
				}
				chunk = chunk[skip:]
				discard -= skip
			}
			if len(chunk) > 0 {
				if err := lim.Wait(ctx, int64(len(chunk))); err != nil {
					return err
				}
				offset := seg.Start + seg.Written
				nw, ew := f.WriteAt(chunk, offset)
				if ew != nil {
					return ew
				}
				seg.Written += int64(nw)
				prog.Add(int64(nw))
				if seg.Written >= seg.Len() {
					seg.Done = true
					return nil
				}
			}
		}
		if er != nil {
			if errors.Is(er, io.EOF) {
				if seg.Written < seg.Len() {
					// Server closed early. Leave the segment resumable.
					return fmt.Errorf("segment %d truncated: %d of %d bytes", seg.Index, seg.Written, seg.Len())
				}
				seg.Done = true
				return nil
			}
			return er
		}
	}
}

// checkResumeStatus turns a resume rejection into a typed error rather than
// silently accepting a full-body 200 as though it were a partial one.
func checkResumeStatus(d *state.Download, resp *http.Response) error {
	if !d.Segmented {
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("unexpected status %s", resp.Status)
		}
		return nil
	}
	switch resp.StatusCode {
	case http.StatusPartialContent:
		return nil
	case http.StatusRequestedRangeNotSatisfiable:
		return errors.New("server refused the requested range")
	case http.StatusOK:
		return ErrServerRejectedRange
	default:
		return fmt.Errorf("unexpected status %s", resp.Status)
	}
}

func resetSegments(d *state.Download) {
	for i := range d.Segments {
		d.Segments[i].Written = 0
		d.Segments[i].Done = false
	}
}
