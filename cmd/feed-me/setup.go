package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"

	"feed-me/internal/config"
	"feed-me/internal/fetch"
)

func newLogger(c config.Log, w io.Writer) *slog.Logger {
	var level slog.Level
	_ = level.UnmarshalText([]byte(c.Level))
	opts := &slog.HandlerOptions{Level: level}
	if c.Format == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

// transport and rateOverride replace the network and the politeness delay in tests.
var (
	transport    http.RoundTripper
	rateOverride float64
)

func newClient(g *config.Global, log *slog.Logger) *fetch.Client {
	ua := g.UserAgent
	if ua == "" {
		ua = fetch.UserAgent(version, g.ContactURL)
	}
	var hc *http.Client // nil: public addresses only
	switch {
	case transport != nil:
		hc = &http.Client{Transport: transport}
	case g.Fetch.AllowPrivateNetworks:
		hc = &http.Client{}
	}
	return fetch.NewClient(hc, ua, g.Fetch.MaxBodyBytes, log)
}

func siteFetcher(c *fetch.Client, s *config.Site) fetch.Fetcher {
	r := s.Fetch.Rate.PerSecond
	if rateOverride > 0 {
		r = rateOverride
	}
	return c.Site(fetch.Options{
		Site:          s.ID,
		Rate:          r,
		Timeout:       s.Fetch.Timeout.D,
		Headers:       s.Fetch.Headers,
		RespectRobots: s.RespectRobots(),
		HeaderHosts:   append(siteHosts(s), s.Fetch.HeaderHosts...),
	})
}

// siteHosts are the hosts named by the site's discovery URLs: the ones its
// configured headers were written for.
func siteHosts(s *config.Site) []string {
	var hosts []string
	add := func(raw string) {
		if u, err := url.Parse(raw); err == nil && u.Host != "" && !slices.Contains(hosts, u.Host) {
			hosts = append(hosts, u.Host)
		}
	}
	for _, d := range s.Discovery {
		add(d.URL)
		for _, u := range d.URLs {
			add(u)
		}
	}
	return hosts
}
