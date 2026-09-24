package fetch

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// BlockedAddressError is a connection refused because the destination is not
// a public address. Discovery follows URLs that remote content chooses, so
// without this a sitemap or a redirect could point feed-me at localhost, the
// container network or a cloud metadata endpoint.
type BlockedAddressError struct {
	Addr netip.Addr
}

func (e *BlockedAddressError) Error() string {
	return fmt.Sprintf("refusing to connect to non-public address %s (set fetch.allow_private_networks to allow it)", e.Addr)
}

// nonPublic lists special-purpose ranges that netip's predicates don't cover.
var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),     // "this network"
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),  // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"), // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),   // reserved, and broadcast
	netip.MustParsePrefix("64:ff9b::/96"),  // NAT64, which can reach any IPv4 address
	netip.MustParsePrefix("2002::/16"),     // 6to4, likewise
}

// publicAddr reports whether a is a globally routable unicast address.
func publicAddr(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || !a.IsGlobalUnicast() || a.IsPrivate() {
		// IsGlobalUnicast excludes loopback, link-local (169.254.169.254
		// included), multicast and unspecified; IsPrivate covers RFC 1918
		// and IPv6 unique local addresses.
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// guardControl runs after DNS resolution, on the address actually dialed, so
// a hostname that resolves (or re-resolves) to a private address is caught too.
func guardControl(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("unexpected dial address %q: %w", address, err)
	}
	if !publicAddr(ap.Addr()) {
		return &BlockedAddressError{Addr: ap.Addr().Unmap()}
	}
	return nil
}

// PublicOnlyTransport is http.DefaultTransport's configuration, minus the
// proxy (which would hide the real destination from the check), dialing only
// public addresses.
func PublicOnlyTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.DialContext = (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   guardControl,
	}).DialContext
	return t
}
