package extract

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html"

	"feed-me/internal/config"
	"feed-me/internal/normalize"
)

// Result is everything extracted from one article. Content is the raw inner
// HTML of the body; the normalizer cleans it (§6).
type Result struct {
	URL        string    `json:"url"`
	Base       string    `json:"-"` // resolves the body's relative URLs (page URL or <base href>)
	Canonical  string    `json:"canonical"`
	Title      string    `json:"title"`
	Summary    string    `json:"summary,omitempty"`
	Author     string    `json:"author,omitempty"`
	Image      string    `json:"image,omitempty"`
	Categories []string  `json:"categories,omitempty"`
	Published  time.Time `json:"published,omitzero"`
	Updated    time.Time `json:"updated,omitzero"`
	// PublishedDateOnly means the source had no time of day (§5.2).
	PublishedDateOnly bool   `json:"published_date_only,omitempty"`
	UpdatedDateOnly   bool   `json:"updated_date_only,omitempty"`
	Content           string `json:"content"`
	ContentMatches    int    `json:"content_matches"`

	// Sources records which source won for each field, for `feed-me check`.
	Sources  map[string]string `json:"sources"`
	Warnings []string          `json:"warnings,omitempty"`
}

// Extract resolves every configured field of one fetched article.
func Extract(site *config.Site, pageURL string, body []byte, hints map[string]string) (*Result, error) {
	p, err := NewPage(pageURL, body, hints)
	if err != nil {
		return nil, err
	}
	it := &site.Item
	r := &Result{URL: pageURL, Base: p.Base.String(), Sources: map[string]string{}, Warnings: p.warnings}

	scalar := func(field string, srcs []config.Source, isURL bool) string {
		res := p.resolve(srcs, isURL)
		if len(res.values) == 0 {
			return ""
		}
		r.Sources[field] = res.source
		return res.values[0]
	}

	r.Canonical = scalar("canonical", it.Canonical, true)
	if r.Canonical == "" {
		r.Canonical = pageURL
		r.Sources["canonical"] = "request"
	}
	if hostOf(r.Canonical) != hostOf(pageURL) {
		r.Warnings = append(r.Warnings, fmt.Sprintf("canonical URL %s is on a different host than %s", r.Canonical, pageURL))
	}

	r.Title = scalar("title", it.Title, false)
	r.Summary = scalar("summary", it.Summary, false)
	// Every value of the winning source is an author: a JSON-LD author array,
	// or a CSS selector matching repeated elements. A meta source gives only
	// the first tag with its name.
	if res := p.resolve(it.Author, false); len(res.values) > 0 {
		r.Author, r.Sources["author"] = joinNames(dedupe(res.values)), res.source
	}
	r.Image = scalar("image", it.Image, true)
	if res := p.resolve(it.Categories, false); len(res.values) > 0 {
		r.Categories, r.Sources["categories"] = dedupe(res.values), res.source
	}

	loc := site.Schedule.Location
	if d, src, ok := resolveDate(p, &it.Published, loc, &r.Warnings, "published"); ok {
		r.Published, r.PublishedDateOnly, r.Sources["published"] = d.Time, d.DateOnly, src
	}
	if d, src, ok := resolveDate(p, &it.Updated, loc, &r.Warnings, "updated"); ok {
		r.Updated, r.UpdatedDateOnly, r.Sources["updated"] = d.Time, d.DateOnly, src
	}

	r.Content, r.ContentMatches = content(p.Doc, &it.Content)
	if r.ContentMatches > 0 {
		r.Sources["content"] = it.Content.Selector.Raw
	}

	if r.Title == "" {
		r.Warnings = append(r.Warnings, "no title found")
	}
	if r.Published.IsZero() {
		r.Warnings = append(r.Warnings, "no published date found; the first-seen time will be used")
	}
	if strings.TrimSpace(r.Content) == "" {
		r.Warnings = append(r.Warnings, fmt.Sprintf("content selector %q matched nothing with text", it.Content.Selector.Raw))
	}
	return r, nil
}

// resolveDate tries each source in order and takes the first value that parses.
// Values that fail to parse are reported, not fatal.
func resolveDate(p *Page, f *config.DateField, loc *time.Location, warnings *[]string, field string) (normalize.Date, string, bool) {
	layouts := make([]string, len(f.Layouts))
	for i, l := range f.Layouts {
		layouts[i] = l.Raw
	}
	for i := range f.Sources {
		src := &f.Sources[i]
		for _, v := range p.allValues(src) {
			if v = normalize.CleanText(v); v == "" {
				continue
			}
			d, err := normalize.ParseDate(v, layouts, loc)
			if err != nil {
				*warnings = append(*warnings, fmt.Sprintf("%s from %s: %v", field, src.Raw, err))
				continue
			}
			return d, src.Raw, true
		}
	}
	return normalize.Date{}, "", false
}

// content returns the inner HTML of every selector match in document order,
// after removing excluded nodes. A match nested inside another match is
// skipped, since its HTML is already included.
func content(doc *goquery.Document, c *config.Content) (string, int) {
	// Pick the outermost matches before exclusions detach anything: a removed
	// subtree that also matches would otherwise look like a separate match.
	var outer []*html.Node
	for _, n := range doc.FindMatcher(c.Selector.Matcher).Nodes {
		if len(outer) == 0 || !contains(outer[len(outer)-1], n) {
			outer = append(outer, n)
		}
	}
	var parts []string
	for _, n := range outer {
		s := goquery.NewDocumentFromNode(n).Selection
		for _, ex := range c.Exclude {
			s.FindMatcher(ex.Matcher).Remove()
		}
		h, err := s.Html()
		empty := strings.TrimSpace(s.Text()) == "" && s.Find("img, video, iframe").Length() == 0
		if err != nil || empty {
			continue
		}
		parts = append(parts, strings.TrimSpace(h))
	}
	return strings.Join(parts, "\n"), len(parts)
}

func contains(ancestor, n *html.Node) bool {
	for p := n.Parent; p != nil; p = p.Parent {
		if p == ancestor {
			return true
		}
	}
	return false
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// joinNames writes a list of names as prose: "A", "A and B", "A, B, and C".
func joinNames(names []string) string {
	switch len(names) {
	case 1:
		return names[0]
	case 2:
		return names[0] + " and " + names[1]
	}
	return strings.Join(names[:len(names)-1], ", ") + ", and " + names[len(names)-1]
}

func dedupe(vs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range vs {
		if k := strings.ToLower(v); !seen[k] {
			seen[k] = true
			out = append(out, v)
		}
	}
	return out
}
