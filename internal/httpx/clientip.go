package httpx

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// trustedRealIP replaces the client address with the one a trusted proxy
// forwarded, and only then. chi's RealIP rewrote RemoteAddr from
// X-Forwarded-For, X-Real-IP or True-Client-IP for any caller, so anyone,
// a sandbox on the internal network included, could pick the address that
// the tailnet gate on /api/jobs/cc, the request log and the recorded
// session address see. Here a request is believed only when its TCP peer
// is inside one of the trusted networks (WS_TRUSTED_PROXIES: the ingress);
// any other peer keeps its own address, whatever headers it sends. With no
// trusted networks, which is the default, no header is believed.
//
// For a trusted peer the client is the rightmost X-Forwarded-For entry that
// is not itself a trusted proxy (so a proxy chain is walked from the near
// end, and a client-supplied prefix is ignored); X-Real-IP is used when
// there is no X-Forwarded-For. The address is set without a port.
func trustedRealIP(trusted []netip.Prefix) func(http.Handler) http.Handler {
	if len(trusted) == 0 {
		return func(next http.Handler) http.Handler { return next }
	}
	in := func(a netip.Addr) bool {
		for _, p := range trusted {
			if p.Contains(a) {
				return true
			}
		}
		return false
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if peer, ok := addrOf(r.RemoteAddr); ok && in(peer) {
				if c, ok := forwardedClient(r, in); ok {
					r.RemoteAddr = c.String()
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// forwardedClient picks the client address out of the forwarding headers.
func forwardedClient(r *http.Request, trusted func(netip.Addr) bool) (netip.Addr, bool) {
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		a, ok := addrOf(strings.TrimSpace(hops[i]))
		if !ok {
			return netip.Addr{}, false // an entry that is not an address: believe nothing
		}
		if !trusted(a) {
			return a, true
		}
	}
	if len(hops) == 0 {
		return addrOf(strings.TrimSpace(r.Header.Get("X-Real-IP")))
	}
	return netip.Addr{}, false
}

// addrOf parses "ip", "ip:port" or "[ip]:port".
func addrOf(s string) (netip.Addr, bool) {
	if h, _, err := net.SplitHostPort(s); err == nil {
		s = h
	}
	a, err := netip.ParseAddr(strings.Trim(s, "[]"))
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

// parsePrefixes reads a comma-separated list of CIDRs or single addresses,
// skipping entries it cannot read.
func parsePrefixes(list string) []netip.Prefix {
	var out []netip.Prefix
	for _, s := range strings.Split(list, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if p, err := netip.ParsePrefix(s); err == nil {
			out = append(out, p)
		} else if a, err := netip.ParseAddr(s); err == nil {
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
		}
	}
	return out
}
