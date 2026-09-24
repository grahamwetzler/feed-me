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
