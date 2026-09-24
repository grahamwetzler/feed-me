// Package fetch is the shared HTTP client (§3.1, Fetcher): per-host rate
// limiting, robots.txt, conditional GET, retries and a response size cap.
package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Fetcher fetches one URL. The interface leaves room for a headless-browser
// implementation later (§6.5).
type Fetcher interface {
	Fetch(ctx context.Context, req Request) (*Response, error)
}

// Request is a GET, made conditional when validators from an earlier fetch are set.
type Request struct {
	URL          string
	ETag         string
	LastModified string
}

// Response is a completed fetch. NotModified responses have no body.
type Response struct {
	URL          string // final URL after redirects
	Status       int
	Header       http.Header
	Body         []byte
	NotModified  bool
	ETag         string
	LastModified string
	FetchedAt    time.Time
}

// StatusError is a non-2xx, non-304 response that retries did not resolve.
type StatusError struct {
	URL    string
	Status int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("GET %s: HTTP %d %s", e.URL, e.Status, http.StatusText(e.Status))
}

// ErrDisallowed is returned when robots.txt forbids a URL.
var ErrDisallowed = errors.New("disallowed by robots.txt")

// Product is the User-Agent product token, which robots.txt groups match against.
const Product = "rss-er"

// Client is shared by all sites so per-host limits hold across them.
type Client struct {
	HTTP         *http.Client
	UserAgent    string
	MaxBodyBytes int64
	MaxAttempts  int
	Log          *slog.Logger

	// backoff returns the wait before retry n (1-based); replaceable in tests.
	backoff func(n int) time.Duration

	mu       sync.Mutex
	limiters map[string]*rate.Limiter
	robots   *robotsCache
}

// UserAgent builds the honest default User-Agent (§3.1).
func UserAgent(version, contactURL string) string {
	if contactURL == "" {
		return Product + "/" + version
	}
	return fmt.Sprintf("%s/%s (+%s)", Product, version, contactURL)
}

// NewClient returns a Client. A nil httpClient uses a fresh http.Client whose
// per-request timeouts come from each site's Options.
func NewClient(httpClient *http.Client, userAgent string, maxBodyBytes int64, log *slog.Logger) *Client {
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	c := &Client{
		HTTP:         httpClient,
		UserAgent:    userAgent,
		MaxBodyBytes: maxBodyBytes,
		MaxAttempts:  3,
		Log:          log,
		backoff:      defaultBackoff,
		limiters:     map[string]*rate.Limiter{},
	}
	c.robots = newRobotsCache(c)
	return c
}

func defaultBackoff(n int) time.Duration {
	base := time.Second << (n - 1) // 1s, 2s, 4s...
	return base/2 + rand.N(base)   // ±50% jitter
}

// Options are one site's fetch settings.
type Options struct {
	Rate          float64 // requests per second, per host
	Timeout       time.Duration
	Headers       map[string]string
	RespectRobots bool
}

// Site returns a Fetcher bound to one site's options.
func (c *Client) Site(o Options) Fetcher { return &siteFetcher{c: c, o: o} }

type siteFetcher struct {
	c *Client
	o Options
}

func (f *siteFetcher) Fetch(ctx context.Context, req Request) (*Response, error) {
	u, err := url.Parse(req.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("fetch %q: not an http(s) URL", req.URL)
	}
	if f.o.RespectRobots {
		ok, err := f.c.robots.allowed(ctx, u, f.o)
		if err != nil {
			return nil, fmt.Errorf("robots.txt for %s: %w", u.Host, err)
		}
		if !ok {
			return nil, fmt.Errorf("GET %s: %w", req.URL, ErrDisallowed)
		}
	}
	return f.c.do(ctx, req, f.o)
}

// limiter returns the host's limiter. When sites share a host, the slowest rate wins.
func (c *Client) limiter(host string, perSecond float64) *rate.Limiter {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.limiters[host]
	if !ok {
		l = rate.NewLimiter(rate.Limit(perSecond), 1)
		c.limiters[host] = l
	} else if rate.Limit(perSecond) < l.Limit() {
		l.SetLimit(rate.Limit(perSecond))
	}
	return l
}

// do performs a GET with rate limiting and retries on 429, 5xx and network errors.
func (c *Client) do(ctx context.Context, req Request, o Options) (*Response, error) {
	u, _ := url.Parse(req.URL)
	lim := c.limiter(u.Host, o.Rate)
	var lastErr error
	for attempt := 1; attempt <= c.MaxAttempts; attempt++ {
		if err := lim.Wait(ctx); err != nil {
			return nil, err
		}
		resp, retryAfter, err := c.once(ctx, req, o)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !retryable(err) || attempt == c.MaxAttempts {
			break
		}
		wait := c.backoff(attempt)
		if retryAfter > 0 {
			if retryAfter > 5*time.Minute {
				break // the server wants us gone for a while; don't hold the run open
			}
			wait = retryAfter
		}
		c.Log.Debug("retrying fetch", "url", req.URL, "attempt", attempt, "wait", wait, "err", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	return nil, lastErr
}

func retryable(err error) bool {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Status == http.StatusTooManyRequests || se.Status >= 500
	}
	var tb *tooBigError
	var re *redirectError
	return !errors.As(err, &tb) && !errors.As(err, &re) && !errors.Is(err, ErrDisallowed) &&
		!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

type tooBigError struct {
	url   string
	limit int64
}

func (e *tooBigError) Error() string {
	return fmt.Sprintf("GET %s: response exceeds %d bytes", e.url, e.limit)
}

// once makes a single request. It returns the server's Retry-After, if any.
func (c *Client) once(ctx context.Context, req Request, o Options) (*Response, time.Duration, error) {
	if o.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.Timeout)
		defer cancel()
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodGet, req.URL, nil)
	if err != nil {
		return nil, 0, err
	}
	for k, v := range o.Headers {
		hr.Header.Set(k, v)
	}
	hr.Header.Set("User-Agent", c.UserAgent)
	if req.ETag != "" {
		hr.Header.Set("If-None-Match", req.ETag)
	}
	if req.LastModified != "" {
		hr.Header.Set("If-Modified-Since", req.LastModified)
	}

	resp, err := c.client(o).Do(hr)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	out := &Response{
		URL:          resp.Request.URL.String(),
		Status:       resp.StatusCode,
		Header:       resp.Header,
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		FetchedAt:    time.Now(),
	}
	switch {
	case resp.StatusCode == http.StatusNotModified:
		out.NotModified = true
		// A 304 may omit validators; keep the ones we sent.
		if out.ETag == "" {
			out.ETag = req.ETag
		}
		if out.LastModified == "" {
			out.LastModified = req.LastModified
		}
		return out, 0, nil
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, parseRetryAfter(resp.Header.Get("Retry-After")), &StatusError{URL: req.URL, Status: resp.StatusCode}
	}

	limit := c.MaxBodyBytes
	if limit <= 0 {
		limit = 10 << 20
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, 0, fmt.Errorf("GET %s: reading body: %w", req.URL, err)
	}
	if int64(len(body)) > limit {
		return nil, 0, &tooBigError{url: req.URL, limit: limit}
	}
	out.Body = body
	return out, 0, nil
}

// maxRedirects matches net/http's default limit.
const maxRedirects = 10

// redirectError is a redirect we refuse to follow; retrying won't change it.
type redirectError struct{ msg string }

func (e *redirectError) Error() string { return e.msg }

// client returns c.HTTP with a redirect policy that applies robots.txt and the
// destination host's rate limit to every hop, not just the first request.
func (c *Client) client(o Options) *http.Client {
	hc := *c.HTTP
	prev := c.HTTP.CheckRedirect
	hc.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if prev != nil {
			if err := prev(r, via); err != nil {
				return err
			}
		}
		if len(via) >= maxRedirects {
			return &redirectError{fmt.Sprintf("stopped after %d redirects", maxRedirects)}
		}
		if r.URL.Scheme != "http" && r.URL.Scheme != "https" {
			return &redirectError{fmt.Sprintf("redirect to non-http(s) URL %s", r.URL)}
		}
		if o.RespectRobots {
			// net/http has already dropped Authorization, Cookie and the like
			// from r if the redirect leaves the original domain; the robots.txt
			// lookup must not send them there either.
			ro := o
			ro.Headers = map[string]string{}
			for k, v := range o.Headers {
				if r.Header.Get(k) != "" {
					ro.Headers[k] = v
				}
			}
			ok, err := c.robots.allowed(r.Context(), r.URL, ro)
			if err != nil {
				return fmt.Errorf("robots.txt for %s: %w", r.URL.Host, err)
			}
			if !ok {
				return fmt.Errorf("redirect to %s: %w", r.URL, ErrDisallowed)
			}
		}
		return c.limiter(r.URL.Host, o.Rate).Wait(r.Context())
	}
	return &hc
}

// parseRetryAfter reads delay-seconds or an HTTP date.
func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if s, err := strconv.Atoi(v); err == nil && s >= 0 {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
