package discovery

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andybalholm/cascadia"

	"feed-me/internal/config"
	"feed-me/internal/fetch"
	"feed-me/internal/fetch/fetchtest"
)

func fetcher(files map[string]string) (fetch.Fetcher, *fetchtest.Transport) {
	tr := &fetchtest.Transport{Files: files}
	c := fetch.NewClient(&http.Client{Transport: tr}, "feed-me/test", 16<<20, nil)
	return c.Site(fetch.Options{Rate: 1000, RespectRobots: true}), tr
}

func loadSite(t *testing.T, id string) *config.Site {
	t.Helper()
	cfg, err := config.Load("../../feed-me.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Site(id)
}

func TestSitemapClaudeBlog(t *testing.T) {
	site := loadSite(t, "claude-blog")
	f, _ := fetcher(map[string]string{
		"https://claude.com/sitemap.xml": "../../testdata/claude-blog/sitemap.xml",
		"https://claude.com/robots.txt":  "../../testdata/claude-blog/robots.txt",
	})
	cands, err := Discover(context.Background(), f, site)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 243 {
		t.Errorf("got %d candidates, want 243", len(cands))
	}
	for i, c := range cands {
		if !strings.HasPrefix(c.URL, "https://claude.com/blog/") || strings.Count(c.URL, "/") != 4 {
			t.Errorf("unexpected URL %s", c.URL)
		}
		if c.Order != i || c.Hints["lastmod"] == "" {
			t.Errorf("%s: order=%d hints=%v", c.URL, c.Order, c.Hints)
		}
	}
}

func TestFeedSelectDev(t *testing.T) {
	site := loadSite(t, "select-dev")
	f, _ := fetcher(map[string]string{
		"https://select.dev/posts/rss.xml": "../../testdata/select-dev/rss.xml",
		"https://select.dev/robots.txt":    "../../testdata/select-dev/robots.txt",
	})
	cands, err := Discover(context.Background(), f, site)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 103 {
		t.Fatalf("got %d candidates, want 103", len(cands))
	}
	c := cands[0]
	if c.URL != "https://select.dev/posts/databricks-liquid-clustering-simplified" {
		t.Errorf("first URL %s", c.URL)
	}
	if c.Hints["title"] != "Databricks Liquid Clustering Simplified" {
		t.Errorf("title hint not cleaned of stega characters: %q (%d bytes)", c.Hints["title"], len(c.Hints["title"]))
	}
	if c.Hints["published"] != "Mon, 21 Sep 2026 15:12:00 GMT" {
		t.Errorf("published hint %q", c.Hints["published"])
	}
	for _, c := range cands {
		for k, v := range c.Hints {
			if strings.ContainsAny(v, "\u200b\u200c\ufeff") {
				t.Errorf("%s hint %s still has invisible characters", c.URL, k)
			}
		}
	}
}

func TestAtomFeedAndDedupe(t *testing.T) {
	dir := t.TempDir()
	atom := `<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <entry>
    <title>First</title>
    <link rel="alternate" href="/posts/one#top"/>
    <published>2026-09-01T10:00:00Z</published>
    <updated>2026-09-02T10:00:00Z</updated>
    <author><name>Ada</name></author>
    <category term="go"/><category term="rss"/>
  </entry>
  <entry><title>Second</title><link href="https://example.com/posts/two"/></entry>
</feed>`
	links := `id: ex
version: 1
channel: {title: T, link: https://example.com, description: D}
discovery:
  - {type: feed, url: https://example.com/atom.xml}
  - {type: links, urls: ['https://example.com/posts/one', 'https://example.com/posts/three']}
item: {title: [listing:title], content: {selector: article}}
`
	writeFile(t, filepath.Join(dir, "atom.xml"), atom)
	writeFile(t, filepath.Join(dir, "sites", "ex.yaml"), links)
	writeFile(t, filepath.Join(dir, "feed-me.yaml"), "public_base_url: https://rss.example.com\n")
	cfg, err := config.Load(filepath.Join(dir, "feed-me.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	f, _ := fetcher(map[string]string{"https://example.com/atom.xml": filepath.Join(dir, "atom.xml")})
	cands, err := Discover(context.Background(), f, cfg.Site("ex"))
	if err != nil {
		t.Fatal(err)
	}
	var urls []string
	for _, c := range cands {
		urls = append(urls, c.URL)
	}
	want := "https://example.com/posts/one https://example.com/posts/two https://example.com/posts/three"
	if strings.Join(urls, " ") != want {
		t.Errorf("urls = %v", urls)
	}
	h := cands[0].Hints
	if h["published"] != "2026-09-01T10:00:00Z" || h["updated"] != "2026-09-02T10:00:00Z" || h["author"] != "Ada" || h["category"] != "go\nrss" {
		t.Errorf("hints = %v", h)
	}
}

func TestGzippedSitemapIndex(t *testing.T) {
	dir := t.TempDir()
	index := `<?xml version="1.0"?><sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
<sitemap><loc>https://example.com/s1.xml.gz</loc></sitemap></sitemapindex>`
	child := `<?xml version="1.0"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
<url><loc>https://example.com/a</loc><lastmod>2026-09-22</lastmod></url><url><loc> https://example.com/b </loc></url></urlset>`
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte(child))
	_ = zw.Close()
	writeFile(t, filepath.Join(dir, "index.xml"), index)
	writeFile(t, filepath.Join(dir, "s1.xml.gz"), gz.String())

	f, _ := fetcher(map[string]string{
		"https://example.com/sitemap.xml": filepath.Join(dir, "index.xml"),
		"https://example.com/s1.xml.gz":   filepath.Join(dir, "s1.xml.gz"),
	})
	cands, err := fromSitemap(context.Background(), f, "https://example.com/sitemap.xml")
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 2 || cands[0].Hints["lastmod"] != "2026-09-22" || cleanURL(cands[1].URL) != "https://example.com/b" {
		t.Errorf("got %+v", cands)
	}
}

func TestIndexDiscovery(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "p1.html"), `<html><body>
<div class="card"><a href="/posts/a">Post <b>A</b></a></div>
<div class="card"><a href="https://example.com/posts/b">Post B</a></div>
<a class="next" href="?page=2">Next</a></body></html>`)
	writeFile(t, filepath.Join(dir, "p2.html"), `<html><head><base href="https://example.com/blog/"></head><body>
<div class="card"><a href="c">Post C</a></div><a class="next" href="https://example.com/blog?page=1">Back to 1</a></body></html>`)
	d := config.Discovery{Type: "index", URL: "https://example.com/blog?page=1", MaxPages: 5}
	var err error
	d.LinkSelector.Raw, d.NextSelector.Raw = ".card", "a.next"
	d.LinkSelector.Matcher, err = cascadia.Compile(".card")
	if err != nil {
		t.Fatal(err)
	}
	d.NextSelector.Matcher, _ = cascadia.Compile("a.next")

	f, tr := fetcher(map[string]string{
		"https://example.com/blog?page=1": filepath.Join(dir, "p1.html"),
		"https://example.com/blog?page=2": filepath.Join(dir, "p2.html"),
	})
	cands, err := fromIndex(context.Background(), f, &d)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range cands {
		got = append(got, c.URL+"="+c.Hints["title"])
	}
	want := "https://example.com/posts/a=Post A|https://example.com/posts/b=Post B|https://example.com/blog/c=Post C"
	if strings.Join(got, "|") != want {
		t.Errorf("got %v", got)
	}
	if tr.Count("https://example.com/blog?page=1") != 1 {
		t.Error("pagination loop was not stopped")
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParseFeedEdgeCases(t *testing.T) {
	base, _ := url.Parse("https://example.com/feeds/atom.xml")
	atomDoc := `<feed xmlns="http://www.w3.org/2005/Atom" xml:base="https://example.com/posts/">
  <entry><title>No link</title></entry>
  <entry><title>Only related</title><link rel="related" href="x"/></entry>
  <entry><title>Blank first</title><link href=" "/><link rel="alternate" href="zero"/></entry>
  <entry><title type="html">&lt;b&gt;Bold&lt;/b&gt; &amp;amp; more</title><link href="one"/>
    <summary type="xhtml"><div xmlns="http://www.w3.org/1999/xhtml">Nested <em>text</em> here</div></summary></entry>
  <entry xml:base="2026/"><title>Entry base</title><link href="two"/></entry>
  <entry xml:base="2026/"><title>Link base</title><link xml:base="/other/" href="three"/></entry>
</feed>`
	cands, err := parseFeed([]byte(atomDoc), base)
	if err != nil {
		t.Fatal(err)
	}
	var urls []string
	for _, c := range cands {
		urls = append(urls, c.URL)
	}
	want := "https://example.com/posts/zero https://example.com/posts/one https://example.com/posts/2026/two https://example.com/other/three"
	if got := strings.Join(urls, " "); got != want {
		t.Errorf("atom urls = %s\nwant %s", got, want)
	}
	if len(cands) > 1 {
		if h := cands[1].Hints; h["title"] != "Bold & more" || h["description"] != "Nested text here" {
			t.Errorf("atom text hints = %q", h)
		}
	}

	rss := `<rss version="2.0"><channel>
  <item><title>No link</title><guid isPermaLink="false">abc</guid></item>
  <item><title>Blank</title><link> </link></item>
  <item><title>Good</title><link>https://example.com/posts/good</link></item>
</channel></rss>`
	cands, err = parseFeed([]byte(rss), base)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].URL != "https://example.com/posts/good" {
		t.Errorf("rss candidates = %+v", cands)
	}
}
