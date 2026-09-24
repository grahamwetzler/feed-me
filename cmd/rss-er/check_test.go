package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"

	"rss-er/internal/fetch/fetchtest"
)

var update = flag.Bool("update", false, "rewrite golden files")

const root = "../.."

// fixtures maps every saved page to its live URL.
func fixtures(t *testing.T) *fetchtest.Transport {
	t.Helper()
	files := map[string]string{
		"https://claude.com/sitemap.xml":   root + "/testdata/claude-blog/sitemap.xml",
		"https://claude.com/robots.txt":    root + "/testdata/claude-blog/robots.txt",
		"https://select.dev/posts/rss.xml": root + "/testdata/select-dev/rss.xml",
		"https://select.dev/robots.txt":    root + "/testdata/select-dev/robots.txt",
	}
	for site, host := range map[string]string{"claude-blog": "https://claude.com/blog/", "select-dev": "https://select.dev/posts/"} {
		posts, _ := filepath.Glob(filepath.Join(root, "testdata", site, "posts", "*.html"))
		for _, p := range posts {
			files[host+strings.TrimSuffix(filepath.Base(p), ".html")] = p
		}
	}
	return &fetchtest.Transport{Files: files}
}

// digest is a golden-friendly view of a check result: content is reduced to
// counts and a hash so the golden file stays readable.
type digest struct {
	URL               string            `json:"url"`
	Canonical         string            `json:"canonical"`
	Title             string            `json:"title"`
	Summary           string            `json:"summary,omitempty"`
	Author            string            `json:"author,omitempty"`
	Image             string            `json:"image,omitempty"`
	Categories        []string          `json:"categories,omitempty"`
	Published         string            `json:"published"`
	PublishedDateOnly bool              `json:"published_date_only,omitempty"`
	Updated           string            `json:"updated,omitempty"`
	ContentBlocks     int               `json:"content_blocks"`
	ContentChars      int               `json:"content_chars"`
	ContentElements   map[string]int    `json:"content_elements"`
	ContentStart      string            `json:"content_start"`
	ContentSHA256     string            `json:"content_sha256"`
	Sources           map[string]string `json:"sources"`
	Warnings          []string          `json:"warnings,omitempty"`
	Error             string            `json:"error,omitempty"`
}

func runCheck(t *testing.T, args ...string) []digest {
	t.Helper()
	transport, rateOverride = fixtures(t), 1000
	t.Cleanup(func() { transport, rateOverride = nil, 0 })
	t.Setenv("RSS_ER_PUBLIC_BASE_URL", "")

	var stdout, stderr bytes.Buffer
	code := run(append([]string{"check", "--config", root + "/rss-er.yaml", "--json"}, args...), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d\nstderr: %s\nstdout: %s", code, stderr.String(), stdout.String())
	}
	var raw []struct {
		URL, Canonical, Title, Summary, Author, Image, Content, Error string
		Categories                                                    []string
		Published, Updated                                            time.Time
		PublishedDateOnly                                             bool `json:"published_date_only"`
		ContentMatches                                                int  `json:"content_matches"`
		Sources                                                       map[string]string
		Warnings                                                      []string
	}
	if err := json.Unmarshal(stdout.Bytes(), &raw); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, stdout.String())
	}
	var out []digest
	for _, r := range raw {
		doc, err := goquery.NewDocumentFromReader(strings.NewReader(r.Content))
		if err != nil {
			t.Fatal(err)
		}
		text := strings.Join(strings.Fields(doc.Text()), " ")
		elems := map[string]int{}
		for _, tag := range []string{"p", "h2", "h3", "img", "pre", "table", "iframe", "figure", "ul", "ol"} {
			if n := doc.Find(tag).Length(); n > 0 {
				elems[tag] = n
			}
		}
		sum := sha256.Sum256([]byte(r.Content))
		d := digest{
			URL: r.URL, Canonical: r.Canonical, Title: r.Title, Summary: r.Summary, Author: r.Author,
			Image: r.Image, Categories: r.Categories, Published: fmtTime(r.Published),
			PublishedDateOnly: r.PublishedDateOnly, Updated: fmtTime(r.Updated),
			ContentBlocks: r.ContentMatches, ContentChars: len([]rune(text)), ContentElements: elems,
			ContentStart: truncate(text, 80), ContentSHA256: hex.EncodeToString(sum[:8]),
			Sources: r.Sources, Warnings: r.Warnings, Error: r.Error,
		}
		out = append(out, d)
	}
	return out
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

func golden(t *testing.T, name string, got []digest) {
	t.Helper()
	path := filepath.Join(root, "testdata", name)
	data, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if *update {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./cmd/rss-er -update to create it)", err)
	}
	if !bytes.Equal(want, data) {
		t.Errorf("%s differs from golden output; run go test ./cmd/rss-er -update and review the diff.\ngot:\n%s", path, data)
	}
}

// TestCheckClaudeBlog is the M1 acceptance check: correct title, date,
// summary and body for the saved Claude blog posts.
func TestCheckClaudeBlog(t *testing.T) {
	slugs := []string{
		"t-rowe-price-brings-more-of-claude-to-its-investment-process",                              // plain text
		"how-anthropics-business-development-team-uses-claude-to-run-inbound-and-outbound-at-scale", // images
		"claude-and-slack",               // video embeds
		"introduction-to-agentic-coding", // code blocks
		"prompt-caching",                 // tables; hidden CMS slot carries the class itself
		"getting-started-with-loops",     // body split across two rich-text blocks
	}
	var args []string
	for _, s := range slugs {
		args = append(args, "--url", "https://claude.com/blog/"+s)
	}
	got := runCheck(t, append(args, "--site", "claude-blog")...)
	if len(got) != len(slugs) {
		t.Fatalf("got %d results", len(got))
	}
	byURL := map[string]digest{}
	for _, d := range got {
		byURL[strings.TrimPrefix(d.URL, "https://claude.com/blog/")] = d
		if d.Error != "" || d.Title == "" || d.Summary == "" || d.Published == "" || !d.PublishedDateOnly || d.ContentChars < 500 {
			t.Errorf("%s: incomplete: %+v", d.URL, d)
		}
		if d.Canonical != d.URL {
			t.Errorf("%s: canonical %s", d.URL, d.Canonical)
		}
		if strings.Contains(d.Summary, "&#39;") {
			t.Errorf("%s: summary has an undecoded entity: %q", d.URL, d.Summary)
		}
	}

	loops := byURL["getting-started-with-loops"]
	if loops.Title != "Loop engineering: Getting started with loops" ||
		loops.Published != "2026-06-30T00:00:00-07:00" || loops.Updated != "2026-08-20T00:00:00-07:00" {
		t.Errorf("loops: %+v", loops)
	}
	if loops.ContentBlocks != 2 || !strings.HasPrefix(loops.ContentStart, "Getting started with loops") {
		t.Errorf("loops body should join both rich-text blocks: %d blocks, starts %q", loops.ContentBlocks, loops.ContentStart)
	}
	if pc := byURL["prompt-caching"]; pc.ContentBlocks != 1 || pc.ContentElements["table"] != 2 {
		t.Errorf("prompt-caching: hidden slot not excluded or tables lost: %+v", pc)
	}
	if ac := byURL["introduction-to-agentic-coding"]; ac.ContentElements["pre"] == 0 {
		t.Errorf("agentic coding: no code blocks: %+v", ac.ContentElements)
	}
	golden(t, "claude-blog/check.golden.json", got)
}

// TestCheckSelectDev covers the feed-discovery path: listing dates with exact
// times, meta-tag titles free of stega characters, and per-post authors.
func TestCheckSelectDev(t *testing.T) {
	got := runCheck(t, "--site", "select-dev", "--limit", "3")
	want := []struct{ slug, title, published, author string }{
		{"databricks-liquid-clustering-simplified", "Databricks Liquid Clustering Simplified", "2026-09-21T15:12:00Z", "Niall Woodward"},
		{"what-is-databricks-genie-and-should-you-be-using-it-webinar-recap", "What Is Databricks Genie, and Should You Be Using It? (Webinar Recap)", "2026-09-16T18:55:00Z", "Olivier Soucy"},
		{"snowflake-dcm-projects-infrastructure-as-code-in-plain-sql", "Snowflake DCM Projects: Infrastructure as Code, in Plain SQL", "2026-09-15T15:40:00Z", "Jeff Skoldberg"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d results", len(got))
	}
	for i, w := range want {
		d := got[i]
		if d.URL != "https://select.dev/posts/"+w.slug {
			t.Errorf("[%d] url %s", i, d.URL)
			continue
		}
		if d.Title != w.title || d.Published != w.published || d.PublishedDateOnly || d.Author != w.author {
			t.Errorf("%s: title=%q published=%s dateOnly=%v author=%q", w.slug, d.Title, d.Published, d.PublishedDateOnly, d.Author)
		}
		if d.Sources["published"] != "listing:published" || d.ContentChars < 5000 {
			t.Errorf("%s: sources=%v elements=%v", w.slug, d.Sources, d.ContentElements)
		}
	}
	golden(t, "select-dev/check.golden.json", got)
}
