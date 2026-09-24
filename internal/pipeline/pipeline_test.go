package pipeline

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rss-er/internal/config"
	"rss-er/internal/discovery"
	"rss-er/internal/extract"
	"rss-er/internal/feed"
	"rss-er/internal/fetch"
	"rss-er/internal/fetch/fetchtest"
	"rss-er/internal/store"
)

const root = "../.."

var update = flag.Bool("update", false, "rewrite golden files")

// checkAtom renders the site's Atom feed, checks it and compares it with a golden file.
func checkAtom(t *testing.T, ctx context.Context, st *store.Store, g *config.Global, site *config.Site, name string) {
	t.Helper()
	data, _, err := Render(ctx, st, g, site, "rss-er/test", Atom)
	if err != nil {
		t.Fatal(err)
	}
	if probs := feed.CheckAtom(data); len(probs) > 0 {
		t.Errorf("atom check: %v", probs)
	}
	golden(t, name, data)
}

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
	return loadSiteAs(t, id, id, edit)
}

// loadSiteAs loads a shipped site config under another site id.
func loadSiteAs(t *testing.T, id, newID string, edit func(string) string) (*config.Global, *config.Site) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "sites", id+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), "id: "+id, "id: "+newID, 1))
	path := filepath.Join(t.TempDir(), newID+".yaml")
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

	data, n, err := Render(ctx, r.Store, g, site, "rss-er/test", RSS)
	if err != nil || n != 4 {
		t.Fatalf("render: n=%d err=%v", n, err)
	}
	if probs := feed.Check(data); len(probs) > 0 {
		t.Errorf("check: %v", probs)
	}
	golden(t, "select-dev/feed.golden.xml", data)
	checkAtom(t, ctx, r.Store, g, site, "select-dev/feed.golden.atom")
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
	data, n, err := Render(ctx, r.Store, g, site, "rss-er/test", RSS)
	if err != nil || n != 4 {
		t.Fatalf("render: %d %v", n, err)
	}
	if probs := feed.Check(data); len(probs) > 0 {
		t.Errorf("check: %v", probs)
	}
	golden(t, "claude-blog/feed.golden.xml", data)
	checkAtom(t, ctx, r.Store, g, site, "claude-blog/feed.golden.atom")
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

const trowe = "https://claude.com/blog/t-rowe-price-brings-more-of-claude-to-its-investment-process"

// editedCopy writes a copy of a fixture with every old replaced by new.
func editedCopy(t *testing.T, path, old, new string) string {
	t.Helper()
	orig, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(orig), old) {
		t.Fatalf("%s has no %q", path, old)
	}
	out := filepath.Join(t.TempDir(), filepath.Base(path))
	if err := os.WriteFile(out, []byte(strings.ReplaceAll(string(orig), old, new)), 0o644); err != nil {
		t.Fatal(err)
	}
	return out
}

// A failed extraction must not save the response's validators: once the
// selector is fixed, the next run has to fetch in full, not get a 304.
func TestFailedExtractionIsRetriedInFull(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 11, 9, 0, 0, 0, la)
	r := newRunner(t, &now)
	_, site := loadSite(t, "claude-blog", claudeLinks)
	_, broken := loadSite(t, "claude-blog", func(s string) string {
		return strings.Replace(claudeLinks(s), "blog_post_content_wrap", "no_such_wrap", 1)
	})
	files := postFiles("claude-blog", "https://claude.com/blog/")
	f, tr := fetcherFor(files)
	tr.ETags = true
	if st, err := r.Run(ctx, site, f); err != nil || st.New != 4 {
		t.Fatalf("first run: %+v %v", st, err)
	}

	files[trowe] = editedCopy(t, files[trowe], "T. Rowe Price", "T. Rowe Price Group")
	now = now.Add(time.Hour)
	if st, _ := r.Run(ctx, broken, f); st.Errors != 1 {
		t.Fatalf("broken run: %+v", st)
	}
	now = now.Add(time.Hour)
	st, err := r.Run(ctx, site, f)
	if err != nil || st.Updated != 1 || st.NotModified != 0 {
		t.Fatalf("fixed run: %+v %v", st, err)
	}
	if it, _ := r.Store.ItemByURL(ctx, site.ID, trowe); !strings.Contains(it.ContentHTML, "T. Rowe Price Group") {
		t.Error("edit never stored")
	}
}

// Two sites fetching one URL keep separate validators, so one site's
// refresh can't turn the other's into a 304 for content it never stored.
func TestValidatorsArePerSite(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 11, 9, 0, 0, 0, la)
	r := newRunner(t, &now)
	_, a := loadSite(t, "claude-blog", claudeLinks)
	_, b := loadSiteAs(t, "claude-blog", "claude-copy", claudeLinks)
	files := postFiles("claude-blog", "https://claude.com/blog/")
	f, tr := fetcherFor(files)
	tr.ETags = true
	for _, s := range []*config.Site{a, b} {
		if st, err := r.Run(ctx, s, f); err != nil || st.New != 4 {
			t.Fatalf("%s: %+v %v", s.ID, st, err)
		}
	}
	files[trowe] = editedCopy(t, files[trowe], "T. Rowe Price", "T. Rowe Price Group")
	now = now.Add(time.Hour)
	for _, s := range []*config.Site{a, b} {
		if st, err := r.Run(ctx, s, f); err != nil || st.Updated != 1 {
			t.Errorf("%s: %+v %v", s.ID, st, err)
		}
	}
}

// Fields outside the content hash still count: here the article turns up
// under a new URL (a trailing slash) with the same canonical, so it is found
// by GUID and must move to the new URL rather than look new every run.
func TestMovedURLIsStored(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 11, 9, 0, 0, 0, la)
	r := newRunner(t, &now)
	_, site := loadSite(t, "claude-blog", claudeLinks)
	files := postFiles("claude-blog", "https://claude.com/blog/")
	f, _ := fetcherFor(files)
	if _, err := r.Run(ctx, site, f); err != nil {
		t.Fatal(err)
	}
	before, _ := r.Store.ItemByURL(ctx, site.ID, trowe)

	files[trowe+"/"] = files[trowe]
	site.Discovery[0].URLs[0] = trowe + "/"
	for i := 0; i < 2; i++ {
		now = now.Add(time.Hour)
		st, err := r.Run(ctx, site, f)
		if err != nil || st.New != 0 {
			t.Fatalf("run %d: %+v %v", i, st, err)
		}
	}
	it, _ := r.Store.ItemByURL(ctx, site.ID, trowe+"/")
	if it == nil || it.GUID != before.GUID || !it.Updated.Equal(before.Updated) {
		t.Errorf("moved item: %+v", it)
	}
}

// A new date-only candidate dated today ranks where publishedAt will put it
// (now), not at midnight behind items already stored today.
func TestNewestOnlyRanksDateOnlyHintsLikePublishedAt(t *testing.T) {
	now := time.Date(2026, 9, 24, 15, 0, 0, 0, la)
	known := map[string]store.Known{"https://e.com/a": {Published: time.Date(2026, 9, 24, 9, 0, 0, 0, la)}}
	fresh := []discovery.Candidate{{URL: "https://e.com/b", Hints: map[string]string{"published": "2026-09-24"}}}
	if got := newestOnly(fresh, known, 1, now, la); len(got) != 1 {
		t.Errorf("new post dated today was not fetched: %+v", got)
	}
	// An older date-only candidate still loses to a newer stored item.
	fresh[0].Hints["published"] = "2026-09-23"
	if got := newestOnly(fresh, known, 1, now, la); len(got) != 0 {
		t.Errorf("older post fetched: %+v", got)
	}
}

// When discovery's hints change (the feed corrects a date), the page is
// fetched in full even though it would answer 304, so the change lands.
func TestChangedListingHintsBypass304(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 24, 17, 0, 0, 0, time.UTC)
	r := newRunner(t, &now)
	_, site := loadSite(t, "select-dev", func(s string) string {
		return strings.Replace(s, "max_items: 50", "max_items: 4", 1)
	})
	files := postFiles("select-dev", "https://select.dev/posts/")
	files["https://select.dev/posts/rss.xml"] = root + "/testdata/select-dev/rss.xml"
	f, tr := fetcherFor(files)
	tr.ETags = true
	if st, err := r.Run(ctx, site, f); err != nil || st.New != 4 {
		t.Fatalf("first run: %+v %v", st, err)
	}

	now = now.Add(time.Hour)
	if st, _ := r.Run(ctx, site, f); st.NotModified != 4 {
		t.Fatalf("steady state should be all 304s: %+v", st)
	}

	files["https://select.dev/posts/rss.xml"] = editedCopy(t, files["https://select.dev/posts/rss.xml"],
		"Mon, 21 Sep 2026 15:12:00 GMT", "Tue, 22 Sep 2026 15:12:00 GMT")
	now = now.Add(time.Hour)
	st, err := r.Run(ctx, site, f)
	if err != nil || st.NotModified != 3 || st.Updated != 1 {
		t.Fatalf("hint change run: %+v %v", st, err)
	}
	it, _ := r.Store.ItemByURL(ctx, site.ID, "https://select.dev/posts/databricks-liquid-clustering-simplified")
	if want := time.Date(2026, 9, 22, 15, 12, 0, 0, time.UTC); !it.Published.Equal(want) {
		t.Errorf("published = %v, want the corrected %v", it.Published, want)
	}
}

// Hints no listing: source reads (here a sitemap lastmod) don't change the
// item, so they must not turn 304s into full fetches.
func TestUnusedHintsKeepConditionalGET(t *testing.T) {
	_, site := loadSite(t, "claude-blog", func(s string) string { return s })
	a := hintsHash(site, map[string]string{"lastmod": "2026-09-24T10:00:00Z"})
	if b := hintsHash(site, map[string]string{"lastmod": "2026-09-24T11:00:00Z"}); a != b {
		t.Error("claude-blog reads no listing hints, but a lastmod change altered the hash")
	}
	_, sd := loadSite(t, "select-dev", func(s string) string { return s })
	x := hintsHash(sd, map[string]string{"published": "Mon, 21 Sep 2026 15:12:00 GMT", "lastmod": "1"})
	if y := hintsHash(sd, map[string]string{"published": "Mon, 21 Sep 2026 15:12:00 GMT", "lastmod": "2"}); x != y {
		t.Error("select-dev: lastmod is not a listing source but changed the hash")
	}
	if y := hintsHash(sd, map[string]string{"published": "Tue, 22 Sep 2026 15:12:00 GMT"}); x == y {
		t.Error("select-dev: a changed listing:published hint must change the hash")
	}
}

// A page that gains a publish date after its first store keeps its frozen
// published time but records the source date, so later moves are detected.
func TestGainedSourceDateIsStored(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 11, 9, 0, 0, 0, la)
	r := newRunner(t, &now)
	_, undated := loadSite(t, "claude-blog", func(s string) string {
		s = claudeLinks(s)
		i, j := strings.Index(s, "  published:"), strings.Index(s, "  updated:")
		return s[:i] + "  published:\n    sources: [meta:no-such-date]\n" + s[j:]
	})
	_, site := loadSite(t, "claude-blog", claudeLinks)
	files := postFiles("claude-blog", "https://claude.com/blog/")
	f, _ := fetcherFor(files)
	if _, err := r.Run(ctx, undated, f); err != nil {
		t.Fatal(err)
	}
	before, _ := r.Store.ItemByURL(ctx, site.ID, trowe)
	if before.SourcePublished != "" {
		t.Fatalf("setup: source date %q", before.SourcePublished)
	}
	now = now.Add(time.Hour)
	if _, err := r.Run(ctx, site, f); err != nil {
		t.Fatal(err)
	}
	after, _ := r.Store.ItemByURL(ctx, site.ID, trowe)
	if after.SourcePublished != "2026-09-10" || !after.Published.Equal(before.Published) {
		t.Errorf("source date %q (want 2026-09-10), published %v (want frozen %v)", after.SourcePublished, after.Published, before.Published)
	}
}

func TestLostSourceDateIsKept(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 11, 9, 0, 0, 0, la)
	r := newRunner(t, &now)
	_, undated := loadSite(t, "claude-blog", func(s string) string {
		s = claudeLinks(s)
		i, j := strings.Index(s, "  published:"), strings.Index(s, "  updated:")
		return s[:i] + "  published:\n    sources: [meta:no-such-date]\n" + s[j:]
	})
	_, site := loadSite(t, "claude-blog", claudeLinks)
	files := postFiles("claude-blog", "https://claude.com/blog/")
	f, _ := fetcherFor(files)
	run := func(s *config.Site) *store.Item {
		t.Helper()
		if _, err := r.Run(ctx, s, f); err != nil {
			t.Fatal(err)
		}
		it, _ := r.Store.ItemByURL(ctx, site.ID, trowe)
		return it
	}
	before := run(site)
	now = now.Add(time.Hour)
	if got := run(undated); got.SourcePublished != "2026-09-10" {
		t.Fatalf("undated run cleared the source date: %q", got.SourcePublished)
	}
	now = now.Add(time.Hour)
	files[trowe] = editedCopy(t, files[trowe], "Sep 10, 2026", "Sep 8, 2026")
	f, _ = fetcherFor(files)
	after := run(site)
	if after.SourcePublished != "2026-09-08" || after.Published.Equal(before.Published) {
		t.Errorf("source date %q (want 2026-09-08), published %v (want recomputed from %v)", after.SourcePublished, after.Published, before.Published)
	}
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// stopAfterFirst closes stop once the first page fetch returns.
type stopAfterFirst struct {
	fetch.Fetcher
	stop    chan struct{}
	fetches int
}

func (s *stopAfterFirst) Fetch(ctx context.Context, req fetch.Request) (*fetch.Response, error) {
	resp, err := s.Fetcher.Fetch(ctx, req)
	if s.fetches++; s.fetches == 1 {
		close(s.stop)
	}
	return resp, err
}

func TestStopEndsRunAfterPageInFlight(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 10, 9, 30, 0, 0, la)
	r := newRunner(t, &now)
	_, site := loadSite(t, "claude-blog", claudeLinks)
	f, _ := fetcherFor(postFiles("claude-blog", "https://claude.com/blog/"))
	sf := &stopAfterFirst{Fetcher: f, stop: make(chan struct{})}
	r.Stop = sf.stop

	st, err := r.Run(ctx, site, sf)
	if !errors.Is(err, ErrStopped) || st.New != 1 || sf.fetches != 1 {
		t.Errorf("stopped run: %+v, %d fetches, err %v; want the one page in flight stored", st, sf.fetches, err)
	}
	state, _ := r.Store.SiteState(ctx, site.ID)
	if !state.LastRun.IsZero() {
		t.Errorf("a stopped run recorded state %+v; it should run again at the next start", state)
	}
}
