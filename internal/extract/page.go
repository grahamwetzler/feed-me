// Package extract resolves an article's fields from its HTML using a site's
// ordered source lists (§3.1, Extractor).
package extract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"

	"feed-me/internal/config"
	"feed-me/internal/normalize"
)

// Page is a fetched article, parsed once and queried per source.
type Page struct {
	URL   string // the fetched URL
	Base  *url.URL
	Doc   *goquery.Document
	Hints map[string]string

	meta     map[string]string
	jsonld   []map[string]any
	warnings []string
}

// NewPage parses an article.
func NewPage(pageURL string, body []byte, hints map[string]string) (*Page, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("parsing HTML: %w", err)
	}
	base, err := url.Parse(pageURL)
	if err != nil {
		return nil, err
	}
	if h, ok := doc.Find("base[href]").First().Attr("href"); ok {
		if b, err := base.Parse(h); err == nil {
			base = b
		}
	}
	p := &Page{URL: pageURL, Base: base, Doc: doc, Hints: hints}
	p.loadMeta()
	p.loadJSONLD()
	return p, nil
}

// loadMeta indexes <meta> tags by name, property and itemprop. The first
// non-empty value for a key wins.
func (p *Page) loadMeta() {
	p.meta = map[string]string{}
	p.Doc.Find("meta[content]").Each(func(_ int, s *goquery.Selection) {
		content, _ := s.Attr("content")
		if strings.TrimSpace(content) == "" {
			return
		}
		for _, attr := range []string{"property", "name", "itemprop"} {
			if k, ok := s.Attr(attr); ok {
				k = strings.ToLower(strings.TrimSpace(k))
				if _, dup := p.meta[k]; !dup {
					p.meta[k] = content
				}
			}
		}
	})
}

// loadJSONLD collects every JSON-LD object on the page in document order,
// descending into arrays, @graph and nested values, so jsonld:Type.path can
// match an object wherever it appears.
func (p *Page) loadJSONLD() {
	p.Doc.Find(`script[type="application/ld+json"]`).Each(func(i int, s *goquery.Selection) {
		var v any
		if err := json.Unmarshal([]byte(s.Text()), &v); err != nil {
			p.warnings = append(p.warnings, fmt.Sprintf("JSON-LD script %d is not valid JSON: %v", i, err))
			return
		}
		var walk func(any)
		walk = func(v any) {
			switch t := v.(type) {
			case []any:
				for _, x := range t {
					walk(x)
				}
			case map[string]any:
				p.jsonld = append(p.jsonld, t)
				// Sorted, not map order, so the same page always resolves the same way.
				for _, k := range slices.Sorted(maps.Keys(t)) {
					switch t[k].(type) {
					case map[string]any, []any:
						walk(t[k])
					}
				}
			}
		}
		walk(v)
	})
}

// values evaluates one source. Multi-valued fields use every result; scalar
// fields use the first. URL-typed results are made absolute.
func (p *Page) values(src *config.Source) []string { return p.eval(src, false) }

// allValues is values, except that jsonld yields the values of every matching
// object rather than the first, so a field that validates its values (dates)
// can fall through an unusable one.
func (p *Page) allValues(src *config.Source) []string { return p.eval(src, true) }

func (p *Page) eval(src *config.Source, allJSONLD bool) []string {
	switch src.Kind {
	case config.SourceMeta:
		if v := p.meta[strings.ToLower(src.Expr)]; v != "" {
			return []string{v}
		}
	case config.SourceJSONLD:
		var out []string
		for _, obj := range p.jsonld {
			if hasType(obj, src.JSONLDType) {
				if vs := jsonldPath(obj, src.JSONLDPath); len(vs) > 0 {
					if !allJSONLD {
						return vs
					}
					out = append(out, vs...)
				}
			}
		}
		return out
	case config.SourceCSS, config.SourceTime:
		var out []string
		p.Doc.FindMatcher(src.Matcher).Each(func(_ int, s *goquery.Selection) {
			v, ok := s.Attr(src.Attr)
			if src.Attr == "" || (!ok && src.Kind == config.SourceTime) {
				v = s.Text() // <time> without the attribute: fall back to its text
			}
			if strings.TrimSpace(v) != "" {
				out = append(out, v)
			}
		})
		return out
	case config.SourceURL:
		m := src.Regexp.FindStringSubmatch(p.URL)
		if m == nil {
			return nil
		}
		if len(m) == 1 {
			return []string{m[0]}
		}
		var parts []string
		for _, g := range m[1:] {
			if g != "" {
				parts = append(parts, g)
			}
		}
		return []string{strings.Join(parts, "-")}
	case config.SourceListing:
		if v := p.Hints[src.Expr]; v != "" {
			return strings.Split(v, "\n")
		}
	case config.SourceRequest:
		return []string{p.URL}
	}
	return nil
}

func hasType(obj map[string]any, want string) bool {
	switch t := obj["@type"].(type) {
	case string:
		return t == want
	case []any:
		for _, x := range t {
			if s, ok := x.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}

// jsonldPath walks path from obj. Arrays fan out; an object at the end of the
// path yields its url, name, @id or @value (so image and author objects work).
func jsonldPath(v any, path []string) []string {
	if len(path) == 0 {
		return jsonldLeaf(v)
	}
	switch t := v.(type) {
	case map[string]any:
		return jsonldPath(t[path[0]], path[1:])
	case []any:
		var out []string
		for _, x := range t {
			out = append(out, jsonldPath(x, path)...)
		}
		return out
	}
	return nil
}

func jsonldLeaf(v any) []string {
	switch t := v.(type) {
	case string:
		// Some CMSes HTML-escape inside JSON strings (the Claude blog's "&#39;").
		if s := html.UnescapeString(t); strings.TrimSpace(s) != "" {
			return []string{s}
		}
	case float64:
		return []string{strconv.FormatFloat(t, 'f', -1, 64)}
	case []any:
		var out []string
		for _, x := range t {
			out = append(out, jsonldLeaf(x)...)
		}
		return out
	case map[string]any:
		for _, k := range []string{"url", "name", "@id", "@value"} {
			if vs := jsonldLeaf(t[k]); len(vs) > 0 {
				return vs
			}
		}
	}
	return nil
}

// absolute resolves a URL-typed value against the page, rejecting non-http(s) results.
func (p *Page) absolute(v string) string {
	u, err := p.Base.Parse(strings.TrimSpace(v))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return u.String()
}

// resolved is the winning value of a field and the source it came from.
type resolved struct {
	values []string
	source string
}

// resolve returns the first source's non-empty, cleaned values. isURL fields
// are made absolute.
func (p *Page) resolve(srcs []config.Source, isURL bool) resolved {
	for i := range srcs {
		var vals []string
		for _, v := range p.values(&srcs[i]) {
			if isURL {
				v = p.absolute(normalize.StripInvisible(v))
			} else {
				v = normalize.CleanText(v)
			}
			if v != "" {
				vals = append(vals, v)
			}
		}
		if len(vals) > 0 {
			return resolved{values: vals, source: srcs[i].Raw}
		}
	}
	return resolved{}
}
