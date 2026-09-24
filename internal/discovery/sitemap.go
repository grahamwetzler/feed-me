package discovery

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/xml"
	"fmt"
	"io"

	"golang.org/x/net/html/charset"

	"feed-me/internal/fetch"
)

const (
	maxSitemapDepth   = 3        // sitemap index → sitemap index → urlset
	maxChildSitemaps  = 200      // per run, across all indexes
	maxGunzippedBytes = 64 << 20 // cap on a decompressed .xml.gz
)

type sitemapURL struct {
	Loc     string `xml:"loc"`
	Lastmod string `xml:"lastmod"`
}

type sitemapDoc struct {
	XMLName  xml.Name
	URLs     []sitemapURL `xml:"url"`
	Sitemaps []sitemapURL `xml:"sitemap"`
}

// fromSitemap reads a sitemap or sitemap index (plain or gzipped). Each URL's
// <lastmod> becomes the lastmod hint.
func fromSitemap(ctx context.Context, f fetch.Fetcher, root string) ([]Candidate, error) {
	var out []Candidate
	visited := map[string]bool{}
	children := 0
	var walk func(u string, depth int) error
	walk = func(u string, depth int) error {
		if visited[u] {
			return nil
		}
		visited[u] = true
		resp, err := f.Fetch(ctx, fetch.Request{URL: u})
		if err != nil {
			return err
		}
		doc, err := parseSitemap(resp.Body)
		if err != nil {
			return fmt.Errorf("%s: %w", u, err)
		}
		switch doc.XMLName.Local {
		case "urlset":
			for _, x := range doc.URLs {
				c := Candidate{URL: x.Loc, Hints: map[string]string{}}
				if x.Lastmod != "" {
					c.Hints["lastmod"] = x.Lastmod
				}
				out = append(out, c)
			}
		case "sitemapindex":
			if depth >= maxSitemapDepth {
				return fmt.Errorf("%s: sitemap indexes nested more than %d deep", u, maxSitemapDepth)
			}
			for _, x := range doc.Sitemaps {
				if children++; children > maxChildSitemaps {
					return fmt.Errorf("more than %d child sitemaps", maxChildSitemaps)
				}
				if err := walk(cleanURL(x.Loc), depth+1); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("%s: root element is <%s>, want <urlset> or <sitemapindex>", u, doc.XMLName.Local)
		}
		return nil
	}
	if err := walk(root, 1); err != nil {
		return nil, err
	}
	return out, nil
}

func parseSitemap(body []byte) (*sitemapDoc, error) {
	var r io.Reader = bytes.NewReader(body)
	if len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b {
		zr, err := gzip.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("gunzip: %w", err)
		}
		r = io.LimitReader(zr, maxGunzippedBytes)
	}
	dec := xml.NewDecoder(r)
	dec.CharsetReader = charset.NewReaderLabel
	var doc sitemapDoc
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("parsing sitemap XML: %w", err)
	}
	return &doc, nil
}
