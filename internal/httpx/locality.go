package httpx

// Home and away (PLAN M5, docs/design/m5-stepup-home-away.md). Every
// request is judged Home or Away from where it came from, so a later step
// can ask for more proof of a sensitive action made from outside. This file
// only decides and reports; nothing here refuses a request.
//
// Two listeners serve the app:
//
//   - The main listener (WS_LISTEN) is reached directly: over the tailnet or
//     the LAN, or through a reverse proxy the operator trusts. Its client is
//     the TCP peer, or the address a trusted proxy forwarded (clientip.go).
//     It is Home when that address is in WS_HOME_NETWORKS. A request that
//     carries a Cloudflare header (WS_AWAY_HEADERS) is Away whatever its
//     address: a tunnel route pointed at the wrong listener then fails safe,
//     and a caller who adds such a header to its own request only makes
//     itself look more away.
//   - The tunnel listener (WS_TUNNEL_LISTEN) is reached only by the
//     Cloudflare tunnel. That is the one place CF-Connecting-IP is believed,
//     because nothing else is supposed to reach that port (bind it to
//     loopback, or firewall it to the connector: see infra/compose/README.md).
//     A request without a usable CF-Connecting-IP is Away.
//
// WS_HOME_NETWORKS unset means every request is Home and the feature is
// off; the address and listener are still reported, so the owner can read
// what ws sees and copy it into the setting.

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// Locality says where a request came from.
type Locality string

// Home and Away are the two localities.
const (
	Home Locality = "home"
	Away Locality = "away"
)

// Listener names the listener a request arrived on.
const (
	ListenerMain   = "main"
	ListenerTunnel = "tunnel"
)

// LocalityInfo is how a request was judged, for /api/me and the sidebar.
type LocalityInfo struct {
	// Configured is whether WS_HOME_NETWORKS is set; when it is not, every
	// request is Home and nothing is gated.
	Configured bool     `json:"configured"`
	Locality   Locality `json:"locality"`
	Listener   string   `json:"listener"`
	// Address is the client address the request was judged by ("" when
	// there was none).
	Address string `json:"address"`
	// Reason says how it was decided, in words fit for a tooltip.
	Reason string `json:"reason"`
}

type localityCtxKey struct{}

// LocalityOf returns how a request was judged. A request that did not pass
// the locality middleware (a test) reads as Home on the main listener.
func LocalityOf(ctx context.Context) LocalityInfo {
	if i, ok := ctx.Value(localityCtxKey{}).(LocalityInfo); ok {
		return i
	}
	return LocalityInfo{Locality: Home, Listener: ListenerMain, Reason: "not judged"}
}

func withLocality(r *http.Request, i LocalityInfo) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), localityCtxKey{}, i))
}

// ParseHomeNetworks reads WS_HOME_NETWORKS: comma-separated CIDRs or single
// addresses, IPv4 and IPv6. An entry it cannot read is an error, and so is
// a non-empty list that holds none: a typo must not silently turn the
// feature off, or everything away.
func ParseHomeNetworks(list string) ([]netip.Prefix, error) {
	return parseNetworkList("WS_HOME_NETWORKS", list)
}

// ParseTunnelPeers reads WS_TUNNEL_PEERS, the hosts allowed to connect to the
// tunnel listener, in the same form. It is required: an empty list is an
// error, so the listener can never run believing every peer.
func ParseTunnelPeers(list string) ([]netip.Prefix, error) {
	p, err := parseNetworkList("WS_TUNNEL_PEERS", list)
	if err == nil && len(p) == 0 {
		return nil, fmt.Errorf("WS_TUNNEL_PEERS is required with WS_TUNNEL_LISTEN: name the connector's address (CIDRs or addresses, comma separated), so the listener does not believe every peer")
	}
	return p, err
}

func parseNetworkList(name, list string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	var bad []string
	for _, s := range strings.Split(list, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if p, err := netip.ParsePrefix(s); err == nil {
			out = append(out, p.Masked())
		} else if a, err := netip.ParseAddr(s); err == nil {
			a = a.Unmap()
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
		} else {
			bad = append(bad, s)
		}
	}
	if len(bad) > 0 {
		return nil, fmt.Errorf("%s: cannot read %q (give CIDRs or addresses, comma separated)", name, strings.Join(bad, ", "))
	}
	if strings.TrimSpace(list) != "" && len(out) == 0 {
		return nil, fmt.Errorf("%s: %q holds no network", name, list)
	}
	return out, nil
}

// localityPolicy judges requests.
type localityPolicy struct {
	home        []netip.Prefix
	awayHeaders []string
}

func newLocalityPolicy(homeNetworks, awayHeaders string) localityPolicy {
	home, _ := ParseHomeNetworks(homeNetworks) // startup has already refused a bad list
	var hs []string
	for _, h := range strings.Split(awayHeaders, ",") {
		if h = strings.TrimSpace(h); h != "" {
			hs = append(hs, h)
		}
	}
	return localityPolicy{home: home, awayHeaders: hs}
}

func (p localityPolicy) configured() bool { return len(p.home) > 0 }

func (p localityPolicy) isHome(a netip.Addr) bool {
	for _, n := range p.home {
		if n.Contains(a.Unmap()) {
			return true
		}
	}
	return false
}

// main judges a request on the main listener; r.RemoteAddr already holds the
// forwarded client when the peer is a trusted proxy.
func (p localityPolicy) main(r *http.Request) LocalityInfo {
	info := LocalityInfo{Configured: p.configured(), Locality: Home, Listener: ListenerMain}
	if a, ok := addrOf(r.RemoteAddr); ok {
		info.Address = a.String()
	}
	if !info.Configured {
		info.Reason = "WS_HOME_NETWORKS is not set, so every request counts as home"
		return info
	}
	for _, h := range p.awayHeaders {
		if r.Header.Get(h) != "" {
			info.Locality = Away
			info.Reason = "direct, but the request carries a " + h + " header (counted as away)"
			return info
		}
	}
	a, ok := addrOf(r.RemoteAddr)
	switch {
	case !ok:
		info.Locality, info.Reason = Away, "direct, no readable client address (counted as away)"
	case p.isHome(a):
		info.Reason = "direct, address in the home networks"
	default:
		info.Locality, info.Reason = Away, "direct, address not in the home networks"
	}
	return info
}

// tunnel judges a request on the tunnel listener, where CF-Connecting-IP is
// the client. More than one value, or a value that is not an address, is
// not believed: Away.
func (p localityPolicy) tunnel(r *http.Request) LocalityInfo {
	info := LocalityInfo{Configured: p.configured(), Locality: Home, Listener: ListenerTunnel}
	vals := r.Header.Values("CF-Connecting-IP")
	var a netip.Addr
	ok := false
	if len(vals) == 1 {
		a, ok = addrOf(strings.TrimSpace(vals[0]))
	}
	if ok {
		info.Address = a.String()
	}
	if !info.Configured {
		info.Reason = "WS_HOME_NETWORKS is not set, so every request counts as home"
		return info
	}
	switch {
	case !ok:
		info.Locality, info.Reason = Away, "tunnel, no usable client address (counted as away)"
	case p.isHome(a):
		info.Reason = "tunnel, from your home address"
	default:
		info.Locality, info.Reason = Away, "tunnel, address not in the home networks"
	}
	return info
}

// localityMW judges requests on the main listener. A request the tunnel
// wrapper has already judged keeps that verdict. Nothing a client sends can
// set the verdict: it lives in the request context.
func (s *Server) localityMW(p localityPolicy) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := r.Context().Value(localityCtxKey{}).(LocalityInfo); !ok {
				r = withLocality(r, p.main(r))
			}
			next.ServeHTTP(w, r)
		})
	}
}

// localityPolicy builds the policy from the config.
func (s *Server) localityPolicy() localityPolicy {
	if s.Cfg == nil {
		return localityPolicy{}
	}
	return newLocalityPolicy(s.Cfg.HomeNetworks, s.Cfg.AwayHeaders)
}

// tunnelGate refuses a connection to the tunnel listener from any peer that
// is not in WS_TUNNEL_PEERS, before anything else looks at the request: the
// listener believes CF-Connecting-IP, so only the connector may reach it.
type tunnelGate struct {
	peers []netip.Prefix
	log   *slog.Logger

	mu       sync.Mutex
	last     time.Time
	refused  int // refusals since the last log line
	interval time.Duration
}

func (g *tunnelGate) allows(r *http.Request) bool {
	peer, ok := addrOf(r.RemoteAddr)
	if ok {
		for _, p := range g.peers {
			if p.Contains(peer) {
				return true
			}
		}
	}
	g.noteRefusal(r.RemoteAddr)
	return false
}

// noteRefusal logs a refused peer at most once a minute, with how many
// were refused since the last line.
func (g *tunnelGate) noteRefusal(peer string) {
	g.mu.Lock()
	g.refused++
	n := g.refused
	every := g.interval
	if every == 0 {
		every = time.Minute
	}
	due := g.last.IsZero() || time.Since(g.last) >= every
	if due {
		g.last, g.refused = time.Now(), 0
	}
	g.mu.Unlock()
	if !due {
		return
	}
	l := g.log
	if l == nil {
		l = slog.Default()
	}
	l.Warn("tunnel listener: refused a peer that is not in WS_TUNNEL_PEERS (it would be able to claim any client address)", "peer", peer, "refused_since_last_line", n)
}

// tunnelWrap judges requests on the tunnel listener and takes the client
// address from CF-Connecting-IP.
func tunnelWrap(pol localityPolicy, gate *tunnelGate, inner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !gate.allows(r) {
			writeErr(w, 403, "forbidden")
			return
		}
		info := pol.tunnel(r)
		if info.Address != "" {
			// What the request log, the recorded session address and the job tab's
			// network gate see is the real client, not the tunnel connector.
			r.RemoteAddr = info.Address
		}
		inner.ServeHTTP(w, withLocality(r, info))
	})
}

// TunnelHandler serves the app on the tunnel listener: the same routes as
// Handler, with the client address taken from CF-Connecting-IP (the only
// place it is believed) and the locality decided from it. Only the peers in
// WS_TUNNEL_PEERS may connect; startup has already refused an empty list.
func (s *Server) TunnelHandler() http.Handler {
	var peers []netip.Prefix
	if s.Cfg != nil {
		peers, _ = ParseTunnelPeers(s.Cfg.TunnelPeers)
	}
	return tunnelWrap(s.localityPolicy(), &tunnelGate{peers: peers, log: s.Log}, s.Handler())
}
