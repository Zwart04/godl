// Package host implements the native messaging host: the small process a
// browser launches to hand downloads to godl.
//
// The host is deliberately a relay and nothing more. It decodes the extension's
// JSON, calls the local manager's API over loopback, and answers the browser.
// All policy - queueing, probing, naming, segment planning - stays in the
// manager, so a bug here cannot corrupt a download.
//
// The wire format is our own JSON: {"cmd": ...}. IDM's proprietary "#"-delimited
// text framing is not reproduced anywhere in this package.
package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"godl/internal/config"
	"godl/internal/nmh"
	"godl/internal/queue"
)

// maxResponse caps how much of the manager's reply we will buffer.
const maxResponse = 8 << 20

// Request is one message from the extension.
type Request struct {
	Cmd         string `json:"cmd"`
	URL         string `json:"url,omitempty"`
	Filename    string `json:"filename,omitempty"`
	Connections int    `json:"connections,omitempty"`

	// The request context the page had. A file behind a login only transfers
	// if these travel with it.
	Cookie    string `json:"cookie,omitempty"`
	Referer   string `json:"referer,omitempty"`
	UserAgent string `json:"userAgent,omitempty"`
	TabURL    string `json:"tabUrl,omitempty"`
}

// Response is one message back to the extension.
type Response struct {
	OK        bool             `json:"ok"`
	Error     string           `json:"error,omitempty"`
	ID        string           `json:"id,omitempty"`
	Version   string           `json:"version,omitempty"`
	Downloads []queue.Snapshot `json:"downloads,omitempty"`
}

// Client reaches the local manager over loopback.
type Client struct {
	// BaseURL is the endpoint the manager recorded when it started, for
	// example http://127.0.0.1:58321. Empty means it has never run.
	BaseURL string
	Token   string
	// HTTP overrides the default client.
	HTTP *http.Client
	// SetupError, when set, is reported for every request instead of making
	// one. It means godl has never finished starting on this machine, and a
	// plain "401" would tell the user nothing useful.
	SetupError string
}

// Serve reads messages until the browser closes the pipe.
//
// io.EOF is the normal end: the extension went away, or the browser shut down.
// Anything else is a real fault and is returned so the process can exit
// non-zero, which is the only failure signal native messaging defines.
func Serve(ctx context.Context, in io.Reader, out io.Writer, c *Client) error {
	if c == nil {
		return errors.New("host: no client configured")
	}

	for {
		var req Request
		if err := nmh.ReadJSON(in, nmh.MaxIncoming, &req); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}

		resp := handle(ctx, c, req)
		if err := nmh.WriteJSON(out, nmh.MaxOutgoing, resp); err != nil {
			return err
		}
	}
}

// handle turns one request into one reply. Every path answers; none panics,
// because a silent host looks to the user exactly like a broken extension.
func handle(ctx context.Context, c *Client, req Request) Response {
	if c.SetupError != "" {
		return Response{Error: c.SetupError}
	}

	switch strings.ToLower(strings.TrimSpace(req.Cmd)) {
	case "ping":
		return Response{OK: true, Version: config.Version}

	case "add":
		return add(ctx, c, req)

	case "list":
		return list(ctx, c)

	case "":
		return Response{Error: "the extension sent a message with no cmd"}

	default:
		return Response{Error: fmt.Sprintf("unknown cmd %q", req.Cmd)}
	}
}

func add(ctx context.Context, c *Client, req Request) Response {
	if strings.TrimSpace(req.URL) == "" {
		return Response{Error: "the extension sent no URL"}
	}

	body := map[string]any{
		"url":         req.URL,
		"filename":    req.Filename,
		"connections": req.Connections,
		"cookie":      req.Cookie,
		"referer":     req.Referer,
		"user_agent":  req.UserAgent,
	}

	var out struct {
		OK        bool `json:"ok"`
		Duplicate bool `json:"duplicate"`
		Download  struct {
			ID string `json:"id"`
		} `json:"download"`
	}
	if err := c.roundtrip(ctx, http.MethodPost, "/api/downloads", body, &out); err != nil {
		return Response{Error: err.Error()}
	}
	return Response{OK: true, ID: out.Download.ID}
}

func list(ctx context.Context, c *Client) Response {
	var out struct {
		Downloads []queue.Snapshot `json:"downloads"`
	}
	if err := c.roundtrip(ctx, http.MethodGet, "/api/downloads", nil, &out); err != nil {
		return Response{Error: err.Error()}
	}
	return Response{OK: true, Downloads: out.Downloads}
}

// roundtrip performs one authenticated call and decodes the manager's reply.
// A non-2xx becomes an error carrying the manager's own message, so the user
// sees "the server did not report a size" rather than "status 501".
func (c *Client) roundtrip(ctx context.Context, method, path string, payload, out any) error {
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		return errors.New("godl is not running - open godl.exe once to set it up")
	}

	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.hc().Do(req)
	if err != nil {
		return fmt.Errorf("godl is not reachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return fmt.Errorf("reading the manager's reply: %w", err)
	}

	// Parsed even on failure: the manager explains itself in this field.
	var envelope struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &envelope)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if envelope.Error != "" {
			return errors.New(envelope.Error)
		}
		return fmt.Errorf("godl replied %s", resp.Status)
	}

	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("godl sent an unexpected reply: %w", err)
	}
	return nil
}

func (c *Client) hc() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	// Long enough to cover the manager's probe of a slow server. This is a
	// control call, not a transfer, so a bound still belongs here.
	return &http.Client{Timeout: 60 * time.Second}
}

// IsSetupProblem reports an error caused by godl never having started, which
// callers show verbatim instead of as a transport failure.
func IsSetupProblem(err error) bool {
	return err != nil && strings.Contains(err.Error(), "godl is not running")
}
