package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/PuerkitoBio/goquery"

	"feed-me/internal/config"
	"feed-me/internal/discovery"
	"feed-me/internal/extract"
	"feed-me/internal/fetch"
)

// urlList collects a repeatable --url flag.
type urlList []string

func (u *urlList) String() string     { return strings.Join(*u, ",") }
func (u *urlList) Set(v string) error { *u = append(*u, v); return nil }

// checkResult pairs an extraction with the error that prevented it, if any.
type checkResult struct {
	*extract.Result
	Error string `json:"error,omitempty"`
}

// cmdCheck dry-runs extraction: with --url it checks those pages, otherwise it
// runs discovery and checks the first --limit candidates. Nothing is stored.
func cmdCheck(args []string, stdout, stderr io.Writer) int {
	fs, cfgPath := newFlags("check", stderr)
	siteID := fs.String("site", "", "site id (required)")
	var urls urlList
	fs.Var(&urls, "url", "article URL to check (repeatable); default: the first --limit discovered URLs")
	limit := fs.Int("limit", 5, "how many discovered URLs to check when --url is not given")
	asJSON := fs.Bool("json", false, "print results as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *siteID == "" {
		fmt.Fprintln(stderr, "feed-me check: --site is required")
		return 2
	}
	if *limit < 1 {
		fmt.Fprintln(stderr, "feed-me check: --limit must be at least 1")
		return 2
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return reportErrors(err, stderr)
	}
	site := cfg.Site(*siteID)
	if site == nil {
		fmt.Fprintf(stderr, "feed-me check: no site %q in %s\n", *siteID, cfg.Global.SitesDir)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	log := newLogger(cfg.Global.Log, stderr)
	f := siteFetcher(newClient(&cfg.Global, log), site)

	type target struct {
		url   string
		hints map[string]string
	}
	var targets []target
	if len(urls) > 0 {
		for _, u := range urls {
			targets = append(targets, target{url: u})
		}
	} else {
		cands, err := discovery.Discover(ctx, f, site)
		if err != nil {
			fmt.Fprintf(stderr, "feed-me check: %v\n", err)
			return 1
		}
		fmt.Fprintf(stderr, "discovered %d URLs; checking the first %d\n", len(cands), min(*limit, len(cands)))
		for _, c := range cands[:min(*limit, len(cands))] {
			targets = append(targets, target{url: c.URL, hints: c.Hints})
		}
	}

	var results []checkResult
	for _, t := range targets {
		res, err := checkOne(ctx, f, site, t.url, t.hints)
		cr := checkResult{Result: res}
		if err != nil {
			cr = checkResult{Result: &extract.Result{URL: t.url}, Error: err.Error()}
		}
		results = append(results, cr)
		if !*asJSON {
			printResult(stdout, site, cr)
		}
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(results); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	} else {
		printHealth(stdout, results)
	}
	for _, r := range results {
		if r.Error != "" || r.Content == "" || r.Title == "" {
			return 1
		}
	}
	return 0
}

func checkOne(ctx context.Context, f fetch.Fetcher, site *config.Site, u string, hints map[string]string) (*extract.Result, error) {
	resp, err := f.Fetch(ctx, fetch.Request{URL: u})
	if err != nil {
		return nil, err
	}
	return extract.Extract(site, resp.URL, resp.Body, hints)
}

func printResult(w io.Writer, site *config.Site, r checkResult) {
	fmt.Fprintf(w, "\n%s\n", r.URL)
	if r.Error != "" {
		fmt.Fprintf(w, "  ERROR  %s\n", r.Error)
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	row := func(field, value string) {
		src := r.Sources[field]
		if value == "" {
			value, src = "—", ""
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", field, truncate(value, 90), src)
	}
	row("canonical", r.Canonical)
	row("title", r.Title)
	row("summary", r.Summary)
	row("author", orDefault(r.Author, site.Channel.Author, "(channel default)"))
	row("image", r.Image)
	row("categories", strings.Join(r.Categories, ", "))
	row("published", formatDate(r.Published, r.PublishedDateOnly, site))
	row("updated", formatDate(r.Updated, r.UpdatedDateOnly, site))
	row("content", contentStats(r.Content, r.ContentMatches))
	tw.Flush()
	for _, warn := range r.Warnings {
		fmt.Fprintf(w, "  ! %s\n", warn)
	}
}

// printHealth summarizes how many pages yielded each key field (§8, extraction health).
func printHealth(w io.Writer, results []checkResult) {
	n := len(results)
	count := func(ok func(checkResult) bool) int {
		c := 0
		for _, r := range results {
			if r.Error == "" && ok(r) {
				c++
			}
		}
		return c
	}
	fmt.Fprintf(w, "\n%d page(s): fetched %d, title %d, published %d, content %d\n", n,
		count(func(checkResult) bool { return true }),
		count(func(r checkResult) bool { return r.Title != "" }),
		count(func(r checkResult) bool { return !r.Published.IsZero() }),
		count(func(r checkResult) bool { return r.Content != "" }))
}

func formatDate(t time.Time, dateOnly bool, site *config.Site) string {
	if t.IsZero() {
		return ""
	}
	if dateOnly {
		return t.Format("2006-01-02") + " (date only, " + site.Schedule.Timezone + ")"
	}
	return t.Format(time.RFC3339)
}

func contentStats(h string, blocks int) string {
	if h == "" {
		return ""
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(h))
	if err != nil {
		return fmt.Sprintf("%d bytes (unparseable: %v)", len(h), err)
	}
	text := strings.Join(strings.Fields(doc.Text()), " ")
	parts := []string{fmt.Sprintf("%d block(s)", blocks), fmt.Sprintf("%d chars", utf8.RuneCountInString(text))}
	for _, c := range []struct{ sel, name string }{
		{"img", "images"}, {"pre", "code blocks"}, {"table", "tables"}, {"iframe, video", "embeds"},
	} {
		if n := doc.Find(c.sel).Length(); n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, c.name))
		}
	}
	return strings.Join(parts, ", ") + ": " + truncate(text, 60)
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

func orDefault(v, def, note string) string {
	if v != "" || def == "" {
		return v
	}
	return def + " " + note
}
