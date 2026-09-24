package fetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/temoto/robotstxt"
)

const (
	robotsTTL      = 24 * time.Hour
	robotsErrorTTL = 10 * time.Minute // for 5xx answers, which disallow everything
)

type robotsEntry struct {
	data    *robotstxt.RobotsData
	expires time.Time
}

// robotsCache holds one parsed robots.txt per scheme+host.
type robotsCache struct {
	c   *Client
	mu  sync.Mutex
	m   map[string]robotsEntry
	now func() time.Time
}

func newRobotsCache(c *Client) *robotsCache {
	return &robotsCache{c: c, m: map[string]robotsEntry{}, now: time.Now}
}

func (r *robotsCache) allowed(ctx context.Context, u *url.URL, o Options) (bool, error) {
	if u.Path == "/robots.txt" {
		return true, nil
	}
	data, err := r.get(ctx, u, o)
	if err != nil {
		return false, err
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	// TestAgent, not Group.Test: only it honors the disallow-all answer for 5xx.
	return data.TestAgent(path, Product), nil
}

func (r *robotsCache) get(ctx context.Context, u *url.URL, o Options) (*robotstxt.RobotsData, error) {
	origin := u.Scheme + "://" + u.Host
	// The answer can depend on the headers sent (credentials, say), so
	// lookups with different headers don't share a cache entry.
	key := origin + "\x00" + headersKey(o.Headers)
	r.mu.Lock()
	e, ok := r.m[key]
	r.mu.Unlock()
	if ok && r.now().Before(e.expires) {
		return e.data, nil
	}

	// Fetch through the rate limiter, but never check robots for robots.txt
	// itself, including where it redirects: that would recurse into this lookup.
	o.RespectRobots = false
	resp, err := r.c.do(ctx, Request{URL: origin + "/robots.txt"}, o)
	status, body := 200, []byte(nil)
	var se *StatusError
	switch {
	case errors.As(err, &se):
		status = se.Status
	case err != nil:
		return nil, err
	default:
		body = resp.Body
	}
	// robotstxt applies the usual rules: 4xx allows everything, 5xx disallows everything.
	data, err := robotstxt.FromStatusAndBytes(status, body)
	if err != nil {
		data, _ = robotstxt.FromStatusAndBytes(200, nil) // unparseable: treat as absent
	}
	ttl := robotsTTL
	if status >= 500 {
		ttl = robotsErrorTTL
	}
	r.mu.Lock()
	r.m[key] = robotsEntry{data: data, expires: r.now().Add(ttl)}
	r.mu.Unlock()
	return data, nil
}

// headersKey is a stable, compact identity for a header set.
func headersKey(h map[string]string) string {
	if len(h) == 0 {
		return ""
	}
	sum := sha256.New()
	for _, k := range slices.Sorted(maps.Keys(h)) {
		fmt.Fprintf(sum, "%s\x00%s\x00", http.CanonicalHeaderKey(k), h[k])
	}
	return hex.EncodeToString(sum.Sum(nil)[:8])
}
