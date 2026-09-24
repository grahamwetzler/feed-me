package fetch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

func TestPublicAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"93.184.215.14":                        true,
		"2606:2800:21f:cb07::":                 true,
		"127.0.0.1":                            false,
		"::1":                                  false,
		"10.1.2.3":                             false,
		"172.18.0.2":                           false, // a Docker bridge network
		"192.168.1.10":                         false,
		"169.254.169.254":                      false, // cloud metadata
		"fe80::1":                              false,
		"fd00:ec2::254":                        false, // unique local, AWS's IPv6 metadata endpoint
		"100.100.100.200":                      false, // carrier-grade NAT, Alibaba's metadata endpoint
		"0.0.0.0":                              false,
		"::":                                   false,
		"224.0.0.1":                            false,
		"255.255.255.255":                      false,
		"::ffff:127.0.0.1":                     false, // IPv4-mapped
		"::ffff:10.0.0.1":                      false,
		"64:ff9b::a00:1":                       false, // NAT64 of 10.0.0.1
		"2002:a00:1::":                         false, // 6to4 of 10.0.0.1
		"64:ff9b:1::a00:1":                     false, // local-use NAT64
		"2001:0:4136:e378:8000:63bf:f5ff:fffe": false, // Teredo
		"::a00:1":                              false, // IPv4-compatible
	} {
		if got := publicAddr(netip.MustParseAddr(addr)); got != want {
			t.Errorf("publicAddr(%s) = %v, want %v", addr, got, want)
		}
	}
}

func TestDefaultClientRefusesNonPublicAddresses(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()

	c := NewClient(nil, "ua", 1<<10, nil)
	c.backoff = func(int) time.Duration { return time.Millisecond }
	// A hostname is resolved first and the dialed address checked, so this
	// covers names that resolve to loopback, not just literal IPs.
	su, _ := url.Parse(srv.URL)
	u := "http://localhost:" + su.Port() + "/a"
	_, err := c.Site(fast).Fetch(context.Background(), Request{URL: u})
	var be *BlockedAddressError
	if !errors.As(err, &be) {
		t.Fatalf("err = %v, want a BlockedAddressError", err)
	}
	if !be.Addr.IsLoopback() {
		t.Errorf("blocked address = %s, want loopback", be.Addr)
	}
	if retryable(err) {
		t.Error("a blocked address is retried")
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("server received %d requests", n)
	}
}
