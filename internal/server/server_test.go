package server

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rss-er/internal/config"
	"rss-er/internal/store"
)

type fixture struct {
	srv   *Server
	site  *config.Site
	store *store.Store
	h     http.Handler
}

// newFixture serves the shipped claude-blog config with one stored item.
func newFixture(t *testing.T, basePath string) *fixture {
	t.Helper()
	ctx := context.Background()
	g := &config.Global{PublicBaseURL: "https://rss.example.com", BasePath: basePath}
	g.Fetch.Rate.PerSecond = 1
	site, errs := config.LoadSite(filepath.Join("..", "..", "sites", "claude-blog.yaml"), g)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "rss-er.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	at := time.Date(2026, 9, 20, 16, 0, 0, 0, time.UTC)
	must(t, st.PutItem(ctx, &store.Item{
		SiteID: site.ID, GUID: "https://claude.com/blog/a", URL: "https://claude.com/blog/a", Canonical: "https://claude.com/blog/a",
		Title: "A post", Summary: "Summary", ContentHTML: "<p>Body</p>", Published: at, FirstSeen: at, LastFetched: at, ContentHash: "h",
	}))
	must(t, st.PutSiteState(ctx, site.ID, store.SiteState{LastRun: at, LastChange: at}))
	srv := New(g, []*config.Site{site}, st, "rss-er/test", slog.New(slog.DiscardHandler))
	return &fixture{srv: srv, site: site, store: st, h: srv.Handler()}
}

func (f *fixture) get(t *testing.T, path string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, req)
	return w
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestFeedsWithConditionalGET(t *testing.T) {
	f := newFixture(t, "/")
	if w := f.get(t, "/feeds/claude-blog.xml"); w.Code != http.StatusServiceUnavailable {
		t.Errorf("before the first render: %d, want 503", w.Code)
	}
	if w := f.get(t, "/feeds/nope.xml"); w.Code != http.StatusNotFound {
		t.Errorf("unknown feed: %d, want 404", w.Code)
	}
	must(t, f.srv.Refresh(context.Background(), f.site))

	for path, ctype := range map[string]string{
		"/feeds/claude-blog.xml":  "application/rss+xml; charset=utf-8",
		"/feeds/claude-blog.atom": "application/atom+xml; charset=utf-8",
	} {
		w := f.get(t, path)
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != ctype || !strings.Contains(w.Body.String(), "A post") {
			t.Fatalf("%s: %d %q\n%s", path, w.Code, w.Header().Get("Content-Type"), w.Body)
		}
		if w.Header().Get("Cache-Control") != CacheControl || w.Header().Get("Last-Modified") != "Sun, 20 Sep 2026 16:00:00 GMT" {
			t.Errorf("%s: headers %v", path, w.Header())
		}
		etag := w.Header().Get("ETag")
		if w := f.get(t, path, "If-None-Match", etag); w.Code != http.StatusNotModified {
			t.Errorf("%s: If-None-Match: %d, want 304", path, w.Code)
		}
		if w := f.get(t, path, "If-Modified-Since", "Sun, 20 Sep 2026 16:00:00 GMT"); w.Code != http.StatusNotModified {
			t.Errorf("%s: If-Modified-Since: %d, want 304", path, w.Code)
		}
		if w := f.get(t, path, "If-None-Match", `"stale"`); w.Code != http.StatusOK {
			t.Errorf("%s: stale ETag: %d, want 200", path, w.Code)
		}
	}
}

func TestRefreshKeepsLastGoodFeed(t *testing.T) {
	f := newFixture(t, "/")
	must(t, f.srv.Refresh(context.Background(), f.site))
	good := f.get(t, "/feeds/claude-blog.xml").Body.String()

	f.site.Schedule.MaxItems = 0 // renders an empty feed
	if err := f.srv.Refresh(context.Background(), f.site); err == nil || !strings.Contains(err.Error(), "no items stored") {
		t.Errorf("Refresh = %v, want an error", err)
	}
	if got := f.get(t, "/feeds/claude-blog.xml").Body.String(); got != good {
		t.Error("an empty render replaced the last good feed")
	}
}

func TestReadyAfterFirstSuccessfulRun(t *testing.T) {
	f := newFixture(t, "/")
	if w := f.get(t, "/healthz"); w.Code != http.StatusOK {
		t.Errorf("healthz: %d", w.Code)
	}
	if w := f.get(t, "/readyz"); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "claude-blog") {
		t.Errorf("readyz before a successful run: %d %s", w.Code, w.Body)
	}
	must(t, f.store.PutSiteState(context.Background(), f.site.ID, store.SiteState{LastSuccess: time.Now()}))
	if w := f.get(t, "/readyz"); w.Code != http.StatusOK {
		t.Errorf("readyz after a successful run: %d %s", w.Code, w.Body)
	}
}

func TestBasePathIndexAndOPML(t *testing.T) {
	f := newFixture(t, "/rss/")
	must(t, f.srv.Refresh(context.Background(), f.site))
	if w := f.get(t, "/rss/feeds/claude-blog.xml"); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), `<atom:link href="https://rss.example.com/rss/feeds/claude-blog.xml" rel="self"`) {
		t.Errorf("feed under base_path: %d", w.Code)
	}
	if w := f.get(t, "/feeds/claude-blog.xml"); w.Code != http.StatusNotFound {
		t.Errorf("feed outside base_path: %d, want 404", w.Code)
	}

	w := f.get(t, "/rss/feeds.opml")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(),
		`<outline type="rss" text="Claude Blog" title="Claude Blog" xmlUrl="https://rss.example.com/rss/feeds/claude-blog.xml" htmlUrl="https://claude.com/blog"></outline>`) {
		t.Errorf("opml: %d\n%s", w.Code, w.Body)
	}

	w = f.get(t, "/rss/")
	for _, want := range []string{`href="https://rss.example.com/rss/feeds/claude-blog.atom"`, `href="https://rss.example.com/rss/feeds.opml"`} {
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), want) {
			t.Errorf("index: %d, missing %s\n%s", w.Code, want, w.Body)
		}
	}
	if w := f.get(t, "/rss/nope"); w.Code != http.StatusNotFound {
		t.Errorf("unknown path: %d, want 404", w.Code)
	}
}
