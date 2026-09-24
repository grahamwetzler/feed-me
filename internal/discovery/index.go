package discovery

import (
	"bytes"
	"context"
	"fmt"
	"net/url"

	"github.com/PuerkitoBio/goquery"

	"rss-er/internal/config"
	"rss-er/internal/fetch"
	"rss-er/internal/normalize"
)

// fromIndex reads listing pages: links matching link_selector become
// candidates (their text is the title hint), and next_selector is followed up
// to max_pages.
func fromIndex(ctx context.Context, f fetch.Fetcher, d *config.Discovery) ([]Candidate, error) {
	var out []Candidate
	seen := map[string]bool{}
	next := d.URL
	for page := 0; page < d.MaxPages && next != "" && !seen[next]; page++ {
		seen[next] = true
		resp, err := f.Fetch(ctx, fetch.Request{URL: next})
		if err != nil {
			return nil, err
		}
		doc, err := goquery.NewDocumentFromReader(bytes.NewReader(resp.Body))
		if err != nil {
			return nil, fmt.Errorf("%s: parsing HTML: %w", next, err)
		}
		base := baseURL(doc, resp.URL)

		doc.FindMatcher(d.LinkSelector.Matcher).Each(func(_ int, s *goquery.Selection) {
			href, text := linkOf(s)
			if href == "" {
				return
			}
			h := hints{}
			h.set("title", text)
			out = append(out, Candidate{URL: resolve(base, href), Hints: h})
		})

		next = ""
		if d.NextSelector.Matcher != nil {
			if href, _ := linkOf(doc.FindMatcher(d.NextSelector.Matcher).First()); href != "" {
				next = cleanURL(resolve(base, href))
			}
		}
	}
	return out, nil
}

// linkOf returns the href and text of s, or of its first descendant link when
// the selector matched a container such as a card.
func linkOf(s *goquery.Selection) (href, text string) {
	if s.Length() == 0 {
		return "", ""
	}
	if h, ok := s.Attr("href"); ok {
		return h, s.Text()
	}
	a := s.Find("a[href]").First()
	h, _ := a.Attr("href")
	return h, normalize.CleanText(s.Text())
}

// baseURL honors <base href> when present.
func baseURL(doc *goquery.Document, pageURL string) *url.URL {
	base, _ := url.Parse(pageURL)
	if h, ok := doc.Find("base[href]").First().Attr("href"); ok {
		if b, err := base.Parse(h); err == nil {
			return b
		}
	}
	return base
}
