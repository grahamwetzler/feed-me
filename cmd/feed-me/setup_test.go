package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"feed-me/internal/config"
	"feed-me/internal/fetch"
)

func TestSiteHosts(t *testing.T) {
	s := &config.Site{Discovery: []config.Discovery{
		{Type: config.DiscoverySitemap, URL: "https://example.com/sitemap.xml"},
		{Type: config.DiscoveryFeed, URL: "https://example.com/feed.xml"},
		{Type: config.DiscoveryLinks, URLs: []string{"https://blog.example.com/a", "http://localhost:8081/b"}},
	}}
	want := []string{"example.com", "blog.example.com", "localhost:8081"}
	if got := siteHosts(s); !slices.Equal(got, want) {
		t.Errorf("siteHosts = %v, want %v", got, want)
	}
}

func TestSiteFetcherAddsHeaderHosts(t *testing.T) {
	var got []string
	transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		got = append(got, r.Host+"="+r.Header.Get("X-Key"))
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: r}, nil
	})
	defer func() { transport = nil }()
	robots := false
	s := &config.Site{Discovery: []config.Discovery{{Type: config.DiscoveryFeed, URL: "https://example.com/feed.xml"}}}
	s.Fetch.Rate.PerSecond, s.Fetch.RespectRobots = 1000, &robots
	s.Fetch.Headers = map[string]string{"X-Key": "k"}
	s.Fetch.HeaderHosts = []string{"www.example.com"}
	f := siteFetcher(newClient(&config.Global{}, nil), s)
	for _, u := range []string{"https://example.com/a", "https://www.example.com/b", "https://other.example/c"} {
		if _, err := f.Fetch(context.Background(), fetch.Request{URL: u}); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"example.com=k", "www.example.com=k", "other.example="}
	if !slices.Equal(got, want) {
		t.Errorf("requests = %v, want %v", got, want)
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAllowPrivateNetworks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer srv.Close()
	o := fetch.Options{Rate: 1000, Timeout: 5 * time.Second}

	for _, allow := range []bool{false, true} {
		g := &config.Global{Fetch: config.GlobalFetch{AllowPrivateNetworks: allow}}
		_, err := newClient(g, nil).Site(o).Fetch(context.Background(), fetch.Request{URL: srv.URL})
		var be *fetch.BlockedAddressError
		if blocked := errors.As(err, &be); blocked == allow {
			t.Errorf("allow_private_networks: %v: err = %v", allow, err)
		}
	}
}
