// Package discovery produces candidate article URLs for a site (§3.1).
package discovery

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"feed-me/internal/config"
	"feed-me/internal/fetch"
)

// Candidate is a discovered article URL with optional hints that
// listing:<field> sources can read.
type Candidate struct {
	URL   string
	Hints map[string]string
	Order int // position in discovery, for deterministic tie-breaks (§5.2)
}

// Discover runs every strategy for the site and returns the union, deduplicated
// by URL (fragment removed), in discovery order. A strategy that fails fails
// the whole run: a partial list would make missing items look deleted.
func Discover(ctx context.Context, f fetch.Fetcher, site *config.Site) ([]Candidate, error) {
	var out []Candidate
	index := map[string]int{}
	for i := range site.Discovery {
		d := &site.Discovery[i]
		var cands []Candidate
		var err error
		switch d.Type {
		case config.DiscoverySitemap:
			cands, err = fromSitemap(ctx, f, d.URL)
		case config.DiscoveryFeed:
			cands, err = fromFeed(ctx, f, d.URL)
		case config.DiscoveryIndex:
			cands, err = fromIndex(ctx, f, d)
		case config.DiscoveryLinks:
			for _, u := range d.URLs {
				cands = append(cands, Candidate{URL: u})
			}
		default:
			err = fmt.Errorf("unknown discovery type %q", d.Type)
		}
		if err != nil {
			return nil, fmt.Errorf("discovery[%d] (%s %s): %w", i, d.Type, d.URL, err)
		}
		for _, c := range cands {
			u := cleanURL(c.URL)
			if u == "" || !d.Accept(u) {
				continue
			}
			if j, seen := index[u]; seen {
				for k, v := range c.Hints { // merge hints; the first strategy wins per field
					if _, ok := out[j].Hints[k]; !ok {
						out[j].Hints[k] = v
					}
				}
				continue
			}
			if c.Hints == nil {
				c.Hints = map[string]string{}
			}
			c.URL, c.Order = u, len(out)
			index[u] = len(out)
			out = append(out, c)
		}
	}
	return out, nil
}

// cleanURL trims whitespace and drops the fragment. It returns "" for anything
// that is not an absolute http(s) URL.
func cleanURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	u.Fragment, u.RawFragment = "", ""
	return u.String()
}

// site is the set of hosts a strategy may traverse: the host of its
// configured URL, plus wherever that first request redirected to. Sitemap
// children and next pages come from remote content, so without this a
// compromised site could send feed-me anywhere.
type site []string

func siteOf(configured, final string) site {
	var s site
	for _, raw := range []string{configured, final} {
		if u, err := url.Parse(raw); err == nil && u.Host != "" && !s.has(u.Host) {
			s = append(s, u.Host)
		}
	}
	return s
}

func (s site) has(host string) bool {
	for _, h := range s {
		if strings.EqualFold(h, host) {
			return true
		}
	}
	return false
}

// check returns an error unless raw is on one of the site's hosts.
func (s site) check(what, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || !s.has(u.Host) {
		return fmt.Errorf("%s %q is not on %s; refusing to follow it off-site", what, raw, strings.Join(s, " or "))
	}
	return nil
}

// resolve makes ref absolute against base.
func resolve(base *url.URL, ref string) string {
	r, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return ""
	}
	return base.ResolveReference(r).String()
}
