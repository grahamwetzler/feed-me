package fetch

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(srv *httptest.Server) *Client {
	c := NewClient(srv.Client(), "rss-er/test (+https://example.com)", 1<<10, nil)
	c.backoff = func(int) time.Duration { return time.Millisecond }
	return c
}

var fast = Options{Rate: 1000, Timeout: 5 * time.Second, RespectRobots: true}

func TestFetchSetsUserAgentAndHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write([]byte(r.UserAgent() + "|" + r.Header.Get("X-Extra")))
	}))
	defer srv.Close()

	o := fast
	o.Headers = map[string]string{"X-Extra": "yes", "User-Agent": "spoofed"}
	resp, err := newTestClient(srv).Site(o).Fetch(context.Background(), Request{URL: srv.URL + "/a"})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(resp.Body); got != "rss-er/test (+https://example.com)|yes" {
		t.Errorf("body = %q", got)
	}
	if resp.ETag != `"v1"` {
		t.Errorf("etag = %q", resp.ETag)
	}
}

func TestConditionalGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"v1"` && r.Header.Get("If-Modified-Since") == "Mon, 21 Sep 2026 15:12:00 GMT" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write([]byte("full"))
	}))
	defer srv.Close()

	o := fast
	o.RespectRobots = false
	resp, err := newTestClient(srv).Site(o).Fetch(context.Background(), Request{
		URL: srv.URL + "/a", ETag: `"v1"`, LastModified: "Mon, 21 Sep 2026 15:12:00 GMT",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.NotModified || resp.Body != nil || resp.ETag != `"v1"` {
		t.Errorf("got %+v", resp)
	}
}

func TestRetriesHonorRetryAfter(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	o := fast
	o.RespectRobots = false
	resp, err := newTestClient(srv).Site(o).Fetch(context.Background(), Request{URL: srv.URL})
	if err != nil || string(resp.Body) != "ok" || calls.Load() != 3 {
		t.Fatalf("resp=%v err=%v calls=%d", resp, err, calls.Load())
	}
}

func TestNoRetryOn404AndGiveUpAfterMaxAttempts(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	o := fast
	o.RespectRobots = false
	f := newTestClient(srv).Site(o)

	_, err := f.Fetch(context.Background(), Request{URL: srv.URL + "/missing"})
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 404 || calls.Load() != 1 {
		t.Errorf("404: err=%v calls=%d", err, calls.Load())
	}

	calls.Store(0)
	_, err = f.Fetch(context.Background(), Request{URL: srv.URL + "/flaky"})
	if !errors.As(err, &se) || se.Status != 502 || calls.Load() != 3 {
		t.Errorf("502: err=%v calls=%d", err, calls.Load())
	}
}

func TestBodySizeCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 2<<10)))
	}))
	defer srv.Close()
	o := fast
	o.RespectRobots = false
	_, err := newTestClient(srv).Site(o).Fetch(context.Background(), Request{URL: srv.URL})
	if err == nil || !strings.Contains(err.Error(), "exceeds 1024 bytes") {
		t.Errorf("err = %v", err)
	}
}

func TestRobots(t *testing.T) {
	var robotsCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			robotsCalls.Add(1)
			_, _ = w.Write([]byte("User-agent: *\nDisallow: /*.json$\n\nUser-agent: rss-er\nDisallow: /private/\n"))
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	f := newTestClient(srv).Site(fast)
	ctx := context.Background()
	if _, err := f.Fetch(ctx, Request{URL: srv.URL + "/posts/a"}); err != nil {
		t.Errorf("allowed path: %v", err)
	}
	if _, err := f.Fetch(ctx, Request{URL: srv.URL + "/private/x"}); !errors.Is(err, ErrDisallowed) {
		t.Errorf("rss-er group should disallow /private/: %v", err)
	}
	// Our own group applies, not *, so *.json is allowed for rss-er.
	if _, err := f.Fetch(ctx, Request{URL: srv.URL + "/data.json"}); err != nil {
		t.Errorf("/data.json: %v", err)
	}
	if n := robotsCalls.Load(); n != 1 {
		t.Errorf("robots.txt fetched %d times, want 1 (cached)", n)
	}

	o := fast
	o.RespectRobots = false
	if _, err := newTestClient(srv).Site(o).Fetch(ctx, Request{URL: srv.URL + "/private/x"}); err != nil {
		t.Errorf("respect_robots: false should skip the check: %v", err)
	}
}

func TestRobots5xxDisallows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	if _, err := newTestClient(srv).Site(fast).Fetch(context.Background(), Request{URL: srv.URL + "/a"}); !errors.Is(err, ErrDisallowed) {
		t.Errorf("err = %v, want ErrDisallowed", err)
	}
}

func TestRedirectsAreCheckedAgainstRobots(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			http.Redirect(w, r, "/robots-real.txt", http.StatusMovedPermanently) // must not recurse
		case "/robots-real.txt":
			_, _ = w.Write([]byte("User-agent: *\nDisallow: /private/\n"))
		case "/public":
			http.Redirect(w, r, "/private/x", http.StatusFound)
		default:
			hits.Add(1)
			_, _ = w.Write([]byte("secret"))
		}
	}))
	defer srv.Close()
	_, err := newTestClient(srv).Site(fast).Fetch(context.Background(), Request{URL: srv.URL + "/public"})
	if !errors.Is(err, ErrDisallowed) {
		t.Errorf("err = %v, want ErrDisallowed", err)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("disallowed redirect target fetched %d times", n)
	}
}

func TestRedirectsWaitForDestinationHost(t *testing.T) {
	dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer dst.Close()
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, dst.URL+"/a", http.StatusFound)
	}))
	defer src.Close()

	o := fast
	o.RespectRobots = false
	o.Rate = 5 // one request per 200ms per host
	c := newTestClient(src)
	dstHost := strings.TrimPrefix(dst.URL, "http://")
	c.limiter(dstHost, o.Rate).Allow() // the destination's token is already spent

	start := time.Now()
	if _, err := c.Site(o).Fetch(context.Background(), Request{URL: src.URL + "/r"}); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 150*time.Millisecond {
		t.Errorf("redirect to a rate-limited host took %v; want it to wait for a token", d)
	}
}

func TestRedirectToAnotherDomainDropsCredentials(t *testing.T) {
	type seen struct{ host, path, auth, cookie, extra string }
	var mu sync.Mutex
	var reqs []seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, seen{r.Host, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Cookie"), r.Header.Get("X-Extra")})
		mu.Unlock()
		switch {
		case r.URL.Path == "/robots.txt":
			http.NotFound(w, r)
		case strings.HasPrefix(r.Host, "src.test"):
			http.Redirect(w, r, "http://dst.test/a", http.StatusFound)
		default:
			_, _ = w.Write([]byte("ok"))
		}
	}))
	defer srv.Close()
	// Every hostname dials the one test server, so the redirect really
	// crosses domains as far as net/http is concerned.
	addr := srv.Listener.Addr().String()
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}}
	c := NewClient(hc, "ua", 1<<10, nil)
	o := fast
	o.Headers = map[string]string{"Authorization": "Bearer secret", "Cookie": "s=1", "X-Extra": "yes"}
	if _, err := c.Site(o).Fetch(context.Background(), Request{URL: "http://src.test/r"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	var dstRobots bool
	for _, r := range reqs {
		if !strings.HasPrefix(r.host, "dst.test") {
			continue
		}
		dstRobots = dstRobots || r.path == "/robots.txt"
		if r.auth != "" || r.cookie != "" {
			t.Errorf("%s%s received credentials: %+v", r.host, r.path, r)
		}
		if r.extra != "yes" {
			t.Errorf("%s%s lost the non-sensitive header: %+v", r.host, r.path, r)
		}
	}
	if !dstRobots {
		t.Errorf("destination robots.txt never checked: %+v", reqs)
	}
}

func TestRobotsCacheIsPerHeaderSet(t *testing.T) {
	var robotsCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			robotsCalls.Add(1)
			if r.Header.Get("Authorization") == "" {
				w.WriteHeader(http.StatusUnauthorized) // anonymous: 4xx, read as allow-all
				return
			}
			_, _ = w.Write([]byte("User-agent: *\nDisallow: /private/\n"))
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	c := newTestClient(srv)
	ctx := context.Background()
	if _, err := c.Site(fast).Fetch(ctx, Request{URL: srv.URL + "/private/x"}); err != nil {
		t.Fatalf("anonymous: %v", err)
	}
	authed := fast
	authed.Headers = map[string]string{"Authorization": "Bearer t"}
	if _, err := c.Site(authed).Fetch(ctx, Request{URL: srv.URL + "/private/x"}); !errors.Is(err, ErrDisallowed) {
		t.Errorf("authenticated lookup reused the anonymous robots.txt: %v", err)
	}
	if n := robotsCalls.Load(); n != 2 {
		t.Errorf("robots.txt fetched %d times, want 2 (one per header set)", n)
	}
}

func TestRobotsCacheDropsExpiredEntries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	c := newTestClient(srv)
	now := time.Now()
	c.robots.now = func() time.Time { return now }
	ctx := context.Background()
	for i := range 5 {
		o := fast
		o.Headers = map[string]string{"Authorization": "Bearer " + strconv.Itoa(i)}
		if _, err := c.Site(o).Fetch(ctx, Request{URL: srv.URL + "/x"}); err != nil {
			t.Fatal(err)
		}
		now = now.Add(robotsTTL) // each token's entry expires before the next is used
	}
	c.robots.mu.Lock()
	n := len(c.robots.m)
	c.robots.mu.Unlock()
	if n != 1 {
		t.Errorf("robots cache holds %d entries, want 1 (expired ones dropped)", n)
	}
}

func TestRateLimitIsPerHostAndSlowestWins(t *testing.T) {
	c := NewClient(nil, "ua", 0, nil)
	l := c.limiter("a.example", 10)
	c.limiter("a.example", 2)
	c.limiter("a.example", 5)
	if l.Limit() != 2 {
		t.Errorf("limit = %v, want 2", l.Limit())
	}
	if c.limiter("b.example", 1) == l {
		t.Error("hosts share a limiter")
	}
}

func TestParseRetryAfter(t *testing.T) {
	if d := parseRetryAfter("120"); d != 2*time.Minute {
		t.Errorf("seconds: %v", d)
	}
	future := time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)
	if d := parseRetryAfter(future); d < 59*time.Minute {
		t.Errorf("date: %v", d)
	}
	if d := parseRetryAfter("soon"); d != 0 {
		t.Errorf("junk: %v", d)
	}
}

func TestUserAgent(t *testing.T) {
	if got := UserAgent("1.2.3", "https://example.com/bot"); got != "rss-er/1.2.3 (+https://example.com/bot)" {
		t.Error(got)
	}
	if got := UserAgent("dev", ""); got != "rss-er/dev" {
		t.Error(got)
	}
}
