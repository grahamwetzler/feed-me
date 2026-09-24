package pipeline

import (
	"bytes"
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rss-er/internal/config"
	"rss-er/internal/extract"
	"rss-er/internal/feed"
	"rss-er/internal/fetch"
	"rss-er/internal/fetch/fetchtest"
	"rss-er/internal/store"
)

const root = "../.."

var update = flag.Bool("update", false, "rewrite golden files")

// golden compares a rendered feed with testdata/<name>; -update rewrites it.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join(root, "testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/pipeline -update to create it)", err)
	}
	if !bytes.Equal(want, got) {
		t.Errorf("%s differs from the rendered feed; run go test ./internal/pipeline -update and review the diff", path)
	}
}

var la, _ = time.LoadLocation("America/Los_Angeles")

func newRunner(t *testing.T, now *time.Time) *Runner {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "rss-er.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Runner{Store: st, Log: discard(), Now: func() time.Time { return *now }}
}

func fetcherFor(files map[string]string) (fetch.Fetcher, *fetchtest.Transport) {
	tr := &fetchtest.Transport{Files: files}
	c := fetch.NewClient(&http.Client{Transport: tr}, "rss-er/test", 16<<20, nil)
	return c.Site(fetch.Options{Rate: 1000, RespectRobots: true}), tr
}

func postFiles(site, host string) map[string]string {
	files := map[string]string{}
	posts, _ := filepath.Glob(filepath.Join(root, "testdata", site, "posts", "*.html"))
	for _, p := range posts {
		files[host+strings.TrimSuffix(filepath.Base(p), ".html")] = p
	}
	return files
}

// loadSite loads a shipped site config, applying edits to its YAML first.
func loadSite(t *testing.T, id string, edit func(string) string) (*config.Global, *config.Site) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "sites", id+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), id+".yaml")
	if err := os.WriteFile(path, []byte(edit(string(data))), 0o644); err != nil {
		t.Fatal(err)
	}
	g := &config.Global{PublicBaseURL: "https://rss.example.com", BasePath: "/"}
	g.Fetch.Rate.PerSecond = 1000
	site, errs := config.LoadSite(path, g)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	return g, site
}

func TestSelectDevBackfillAndSteadyState(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 24, 17, 0, 0, 0, time.UTC)
	r := newRunner(t, &now)
	g, site := loadSite(t, "select-dev", func(s string) string {
		return strings.Replace(s, "max_items: 50", "max_items: 4", 1) // the four saved posts are the newest four
	})
	files := postFiles("select-dev", "https://select.dev/posts/")
	files["https://select.dev/posts/rss.xml"] = root + "/testdata/select-dev/rss.xml"
	files["https://select.dev/robots.txt"] = root + "/testdata/select-dev/robots.txt"
	f, tr := fetcherFor(files)

	st, err := r.Run(ctx, site, f)
	if err != nil {
		t.Fatal(err)
	}
	if st.Discovered != 103 || st.Fetched != 4 || st.New != 4 || st.Errors != 0 {
		t.Fatalf("first run: %+v", st)
	}

	data, n, err := RenderRSS(ctx, r.Store, g, site, "rss-er/test")
	if err != nil || n != 4 {
		t.Fatalf("render: n=%d err=%v", n, err)
	}
	if probs := feed.Check(data); len(probs) > 0 {
		t.Errorf("check: %v", probs)
	}
	golden(t, "select-dev/feed.golden.xml", data)
	s := string(data)
	for _, want := range []string{
		"<title>Databricks Liquid Clustering Simplified</title>",
		"<pubDate>Mon, 21 Sep 2026 15:12:00 +0000</pubDate>",
		"<dc:creator>Niall Woodward</dc:creator>",
		"<pre><code>-- DBR 18.1 and above", // line-number spans excluded
		`<atom:link href="https://rss.example.com/feeds/select-dev.xml" rel="self"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("feed missing %q", want)
		}
	}
	if strings.ContainsAny(s, "\u200b\u200c\ufeff") || strings.Contains(s, "class=") || strings.Contains(s, "style=") {
		t.Error("feed contains invisible characters or unsanitized attributes")
	}

	// An hour later: the 99 older feed entries still aren't worth fetching, and
	// the four recent items are re-checked and found unchanged.
	now = now.Add(time.Hour)
	before := len(tr.Requests)
	st, err = r.Run(ctx, site, f)
	if err != nil {
		t.Fatal(err)
	}
	if st.New != 0 || st.Fetched != 4 || st.Unchanged != 4 || st.Updated != 0 {
		t.Errorf("second run: %+v", st)
	}
	if got := len(tr.Requests) - before; got != 5 { // feed + 4 posts (robots.txt is cached)
		t.Errorf("second run made %d requests", got)
	}
	state, _ := r.Store.SiteState(ctx, site.ID)
	if !state.LastChange.Equal(now.Add(-time.Hour)) {
		t.Errorf("lastBuildDate should stay at the first run: %v", state.LastChange)
	}
}

// claudeLinks swaps the sitemap for the saved posts, in a fixed order.
func claudeLinks(s string) string {
	i, j := strings.Index(s, "discovery:"), strings.Index(s, "item:")
	return s[:i] + `discovery:
  - type: links
    urls:
      - https://claude.com/blog/t-rowe-price-brings-more-of-claude-to-its-investment-process
      - https://claude.com/blog/getting-started-with-loops
      - https://claude.com/blog/prompt-caching
      - https://claude.com/blog/claude-and-slack

` + s[j:]
}

func TestClaudeBlogTimestampsAndEdits(t *testing.T) {
	ctx := context.Background()
	// First seen on the day T. Rowe Price's post went up (Sep 10), mid-morning.
	now := time.Date(2026, 9, 10, 9, 30, 0, 0, la)
	r := newRunner(t, &now)
	g, site := loadSite(t, "claude-blog", claudeLinks)
	files := postFiles("claude-blog", "https://claude.com/blog/")
	files["https://claude.com/robots.txt"] = root + "/testdata/claude-blog/robots.txt"
	f, _ := fetcherFor(files)

	if st, err := r.Run(ctx, site, f); err != nil || st.New != 4 {
		t.Fatalf("run: %+v %v", st, err)
	}
	get := func(slug string) *store.Item {
		it, err := r.Store.ItemByURL(ctx, site.ID, "https://claude.com/blog/"+slug)
		if err != nil || it == nil {
			t.Fatalf("%s: %v", slug, err)
		}
		return it
	}
	// Same-day date-only post: the first-seen time.
	if tr := get("t-rowe-price-brings-more-of-claude-to-its-investment-process"); !tr.Published.Equal(now) {
		t.Errorf("same-day post published %v, want first-seen %v", tr.Published.In(la), now)
	}
	// Older date-only post: local midnight plus a sub-second offset by discovery order.
	loops := get("getting-started-with-loops")
	if want := time.Date(2026, 6, 30, 0, 0, 0, 998e6, la); !loops.Published.Equal(want) {
		t.Errorf("loops published %v, want %v", loops.Published.In(la), want)
	}
	if want := time.Date(2026, 8, 20, 0, 0, 0, 0, la); !loops.Updated.Equal(want) {
		t.Errorf("loops updated %v, want dateModified %v", loops.Updated.In(la), want)
	}
	if loops.GUID != "https://claude.com/blog/getting-started-with-loops" || loops.Author != "" {
		t.Errorf("loops guid=%q author=%q", loops.GUID, loops.Author)
	}
	if slack := get("claude-and-slack"); !strings.Contains(slack.ContentHTML, `<a href="https://www.youtube.com/watch?v=tI1uzSJuSxc">▶ Watch video: Claude in Slack</a>`) {
		t.Error("YouTube embed not replaced with a link")
	}

	// Edit a post a few days later: content changes, published stays frozen.
	edited := filepath.Join(t.TempDir(), "tr.html")
	orig, _ := os.ReadFile(files["https://claude.com/blog/t-rowe-price-brings-more-of-claude-to-its-investment-process"])
	if err := os.WriteFile(edited, []byte(strings.Replace(string(orig), "T. Rowe Price", "T. Rowe Price Group", -1)), 0o644); err != nil {
		t.Fatal(err)
	}
	files["https://claude.com/blog/t-rowe-price-brings-more-of-claude-to-its-investment-process"] = edited
	first := now
	now = now.Add(72 * time.Hour)
	st, err := r.Run(ctx, site, f)
	if err != nil {
		t.Fatal(err)
	}
	// Only posts inside the 14-day refresh window are refetched: T. Rowe Price (Sep 10).
	if st.Fetched != 1 || st.Updated != 1 || st.New != 0 {
		t.Errorf("edit run: %+v", st)
	}
	tr := get("t-rowe-price-brings-more-of-claude-to-its-investment-process")
	if !tr.Published.Equal(first) || !tr.Updated.Equal(now) {
		t.Errorf("after edit: published %v (want frozen %v), updated %v (want %v)", tr.Published, first, tr.Updated, now)
	}

	// Rendered order: newest first, and the feed passes the local checks.
	data, n, err := RenderRSS(ctx, r.Store, g, site, "rss-er/test")
	if err != nil || n != 4 {
		t.Fatalf("render: %d %v", n, err)
	}
	if probs := feed.Check(data); len(probs) > 0 {
		t.Errorf("check: %v", probs)
	}
	golden(t, "claude-blog/feed.golden.xml", data)
	s := string(data)
	if a, b := strings.Index(s, "t-rowe-price"), strings.Index(s, "getting-started-with-loops"); a > b {
		t.Error("items not newest first")
	}
	if !strings.Contains(s, "<dc:creator>Anthropic</dc:creator>") {
		t.Error("channel default author not applied")
	}
}

func TestMissingItemsAreKept(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, la)
	r := newRunner(t, &now)
	_, site := loadSite(t, "claude-blog", claudeLinks)
	files := postFiles("claude-blog", "https://claude.com/blog/")
	f, _ := fetcherFor(files)
	if _, err := r.Run(ctx, site, f); err != nil {
		t.Fatal(err)
	}
	site.Discovery[0].URLs = site.Discovery[0].URLs[:3]
	now = now.Add(time.Hour)
	if _, err := r.Run(ctx, site, f); err != nil {
		t.Fatal(err)
	}
	gone, _ := r.Store.ItemByURL(ctx, site.ID, "https://claude.com/blog/claude-and-slack")
	if gone == nil || !gone.MissingSince.Equal(now) {
		t.Fatalf("missing item: %+v", gone)
	}
	if items, _ := r.Store.Recent(ctx, site.ID, 50); len(items) != 4 {
		t.Errorf("feed lost a missing item: %d items", len(items))
	}
}

func TestDiscoveryFailureKeepsItems(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	r := newRunner(t, &now)
	_, site := loadSite(t, "select-dev", func(s string) string { return s })
	f, _ := fetcherFor(map[string]string{}) // the feed 404s
	if _, err := r.Run(ctx, site, f); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err = %v", err)
	}
	state, _ := r.Store.SiteState(ctx, site.ID)
	if state.LastError == "" || !state.LastSuccess.IsZero() {
		t.Errorf("state: %+v", state)
	}
}

func TestPublishedAt(t *testing.T) {
	firstSeen := time.Date(2026, 9, 17, 14, 5, 0, 0, la)
	day := time.Date(2026, 9, 17, 0, 0, 0, 0, la)
	tests := []struct {
		name  string
		res   extract.Result
		order int
		want  time.Time
		src   string
	}{
		{"no date", extract.Result{}, 0, firstSeen, "first_seen"},
		{"exact", extract.Result{Published: day.Add(3 * time.Hour)}, 0, day.Add(3 * time.Hour), "s"},
		{"date only, same day", extract.Result{Published: day, PublishedDateOnly: true}, 0, firstSeen, "s (date) + first_seen time"},
		{"date only, earlier day", extract.Result{Published: day.AddDate(0, 0, -1), PublishedDateOnly: true}, 5, day.AddDate(0, 0, -1).Add(994 * time.Millisecond), "s"},
		{"order past 999 clamps", extract.Result{Published: day.AddDate(0, 0, -1), PublishedDateOnly: true}, 5000, day.AddDate(0, 0, -1), "s"},
	}
	for _, tt := range tests {
		tt.res.Sources = map[string]string{"published": "s"}
		got, src := publishedAt(&tt.res, firstSeen, tt.order, la)
		if !got.Equal(tt.want) || src != tt.src {
			t.Errorf("%s: got %v %q, want %v %q", tt.name, got, src, tt.want, tt.src)
		}
	}
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }
