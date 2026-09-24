package main

import (
	"io"
	"log/slog"
	"net/http"

	"rss-er/internal/config"
	"rss-er/internal/fetch"
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
	var hc *http.Client
	if transport != nil {
		hc = &http.Client{Transport: transport}
	}
	return fetch.NewClient(hc, ua, g.Fetch.MaxBodyBytes, log)
}

func siteFetcher(c *fetch.Client, s *config.Site) fetch.Fetcher {
	r := s.Fetch.Rate.PerSecond
	if rateOverride > 0 {
		r = rateOverride
	}
	return c.Site(fetch.Options{
		Rate:          r,
		Timeout:       s.Fetch.Timeout.D,
		Headers:       s.Fetch.Headers,
		RespectRobots: s.RespectRobots(),
	})
}
