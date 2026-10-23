// Package update checks the project's public release channel. It is the only
// place that talks to GitHub for version information; the UI asks the local
// control API, which only relays the version comparison.
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultEndpoint is GitHub's latest-release endpoint. It is public, unauthenticated,
// and rate limited without an access token. The app only asks this at start-up
// (and after an explicit user check), so a single local run costs one request.
const DefaultEndpoint = "https://api.github.com/repos/Zwart04/godl/releases/latest"

// ReleasePage is the human-facing URL opened when the app asks the browser to
// view the available release.
const ReleasePage = "https://github.com/Zwart04/godl/releases/latest"

// Status is the value the UI reads back from an update check. Every field is
// plain data and never carries secrets.
type Status struct {
	Enabled     bool   `json:"enabled"`
	Installed   string `json:"current"`
	Latest      string `json:"latest,omitempty"`
	Available   bool   `json:"available"`
	URL         string `json:"url,omitempty"`
	Name        string `json:"name,omitempty"`
	PublishedAt string `json:"published_at,omitempty"`
	CheckedAt   string `json:"checked_at,omitempty"`
	Error       string `json:"error,omitempty"`
}

// Checker asks GitHub once per check and keeps the result quiet until the caller
// asks for it. It coalesces concurrent checks so a page reload cannot slice the
// hourly GitHub quota while the user keeps tapping.
type Checker struct {
	Current  string
	Endpoint string
	Client   *http.Client

	mu      sync.Mutex
	status  Status
	running bool
	done    chan struct{}
}

// New builds a checker for the currently installed version. Disable it by
// handing nil to the Server.Updates field.
func New(current string) *Checker {
	return &Checker{
		Current:  current,
		Endpoint: DefaultEndpoint,
		Client:   &http.Client{Timeout: 8 * time.Second},
		status: Status{
			Enabled:   true,
			Installed: current,
		},
	}
}

// Status returns the last result, never triggering a new network request.
func (c *Checker) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.status
	s.Enabled = true
	s.Installed = c.Current
	return s
}

// Check performs one release check, coalescing concurrent callers and honoring
// a caller-supplied deadline. It is safe to call again after a failure.
func (c *Checker) Check(ctx context.Context) (Status, error) {
	c.mu.Lock()
	if c.running {
		done := c.done
		c.mu.Unlock()
		select {
		case <-done:
			return c.Status(), nil
		case <-ctx.Done():
			return c.Status(), ctx.Err()
		}
	}
	c.running = true
	c.done = make(chan struct{})
	c.mu.Unlock()

	result, err := c.fetch(ctx)

	c.mu.Lock()
	if err == nil {
		result.Enabled = true
		result.Installed = c.Current
		result.CheckedAt = time.Now().UTC().Format(time.RFC3339)
		c.status = result
	} else {
		c.status.Error = friendlyError(err)
	}
	c.running = false
	close(c.done)
	c.mu.Unlock()

	return c.Status(), err
}

func (c *Checker) fetch(ctx context.Context) (Status, error) {
	endpoint := c.Endpoint
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Status{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", userAgent(c.Current))

	res, err := client.Do(req)
	if err != nil {
		return Status{}, err
	}
	defer func() { _ = res.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return Status{}, err
	}
	if res.StatusCode != http.StatusOK {
		return Status{}, fmt.Errorf("GitHub returned %s", res.Status)
	}

	var release struct {
		TagName     string `json:"tag_name"`
		Name        string `json:"name"`
		HTMLURL     string `json:"html_url"`
		PublishedAt string `json:"published_at"`
	}
	if err := json.Unmarshal(body, &release); err != nil {
		return Status{}, err
	}
	if release.TagName == "" {
		return Status{}, fmt.Errorf("no release tag in GitHub response")
	}

	latest := strings.TrimSpace(strings.TrimPrefix(release.TagName, "v"))
	if !isVersion(latest) {
		return Status{}, fmt.Errorf("unsupported release tag %q", release.TagName)
	}

	link := cleanReleaseLink(release.HTMLURL)

	return Status{
		Enabled:     true,
		Installed:   c.Current,
		Latest:      latest,
		Available:   Compare(latest, c.Current) > 0,
		URL:         link,
		Name:        strings.TrimSpace(release.Name),
		PublishedAt: strings.TrimSpace(release.PublishedAt),
	}, nil
}

// Compare reports the semantic ordering of two plain-version strings.
//
//	Compare("0.3.7", "0.3.6") == 1
func Compare(a, b string) int {
	pa, preA, okA := parseVersion(a)
	pb, preB, okB := parseVersion(b)
	if !okA || !okB {
		return 0
	}
	for i := 0; i < 3; i++ {
		if pa[i] < pb[i] {
			return -1
		}
		if pa[i] > pb[i] {
			return 1
		}
	}
	return comparePrerelease(preA, preB)
}

func parseVersion(s string) ([3]int, []string, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	s = strings.TrimPrefix(s, "V")
	if s == "" {
		return [3]int{}, nil, false
	}
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	pre := []string{}
	if i := strings.IndexByte(s, '-'); i >= 0 {
		pre = strings.Split(s[i+1:], ".")
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) < 1 || len(parts) > 3 {
		return [3]int{}, nil, false
	}
	nums := [3]int{}
	for i := 0; i < len(parts); i++ {
		n, err := strconv.Atoi(parts[i])
		if err != nil || n < 0 || len(parts[i]) > 9 {
			return [3]int{}, nil, false
		}
		nums[i] = n
	}
	for i := len(parts); i < 3; i++ {
		nums[i] = 0
	}
	return nums, pre, true
}

func comparePrerelease(a, b []string) int {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}
	if len(a) == 0 {
		return 1
	}
	if len(b) == 0 {
		return -1
	}
	for i := 0; i < len(a) && i < len(b); i++ {
		na, aNum := toInt(a[i])
		nb, bNum := toInt(b[i])
		if aNum {
			if bNum {
				if na < nb {
					return -1
				}
				if na > nb {
					return 1
				}
				continue
			}
			return -1
		}
		if bNum {
			return 1
		}
		if strings.ToLower(a[i]) < strings.ToLower(b[i]) {
			return -1
		}
		if strings.ToLower(a[i]) > strings.ToLower(b[i]) {
			return 1
		}
	}
	if len(a) < len(b) {
		return -1
	}
	if len(a) > len(b) {
		return 1
	}
	return 0
}

func toInt(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

func userAgent(current string) string {
	return "godl/" + current + " (+https://github.com/Zwart04/godl)"
}

func friendlyError(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case strings.Contains(strings.ToLower(err.Error()), "timeout"):
		return "The update check timed out."
	case strings.Contains(strings.ToLower(err.Error()), "http:") ||
		strings.Contains(strings.ToLower(err.Error()), "putting"):
		return "The update check could not be reached."
	default:
		return "Could not check for updates right now."
	}
}

func isVersion(s string) bool {
	_, _, ok := parseVersion(s)
	return ok
}

func cleanReleaseLink(raw string) string {
	link := strings.TrimSpace(raw)
	if link == "" {
		return ReleasePage
	}
	u, err := url.Parse(link)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ReleasePage
	}
	if u.Scheme != "https" || !strings.EqualFold(u.Host, "github.com") {
		return ReleasePage
	}
	if !strings.HasPrefix(u.Path, "/Zwart04/godl/releases/") {
		return ReleasePage
	}
	return link
}
