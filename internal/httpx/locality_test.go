package httpx

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const homeNets = "192.168.1.0/24,100.64.0.0/10,203.0.113.7,2001:db8:abcd::/48"

// judged runs a request through the main listener's chain (the trusted-proxy
// step, then locality) and returns what the handler behind it sees.
func judged(t *testing.T, trusted, peer string, headers map[string][]string, home string) (LocalityInfo, string) {
	t.Helper()
	pol := newLocalityPolicy(home, "CF-Connecting-IP,CF-Ray,Cf-Access-Jwt-Assertion")
	var info LocalityInfo
	var addr string
	s := &Server{}
	h := trustedRealIP(parsePrefixes(trusted))(s.localityMW(pol)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		info, addr = LocalityOf(r.Context()), r.RemoteAddr
	})))
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = peer
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	h.ServeHTTP(httptest.NewRecorder(), req)
	return info, addr
}

func TestLocalityMainListener(t *testing.T) {
	cases := []struct {
		name    string
		trusted string
		peer    string
		h       map[string][]string
		want    Locality
	}{
		{"lan address", "", "192.168.1.20:5000", nil, Home},
		{"tailnet address", "", "100.101.102.103:5000", nil, Home},
		{"a single home address", "", "203.0.113.7:5000", nil, Home},
		{"ipv6 in a home prefix", "", "[2001:db8:abcd:1::9]:5000", nil, Home},
		{"ipv4 mapped in ipv6", "", "[::ffff:192.168.1.5]:5000", nil, Home},
		{"public address", "", "198.51.100.9:5000", nil, Away},
		{"ipv6 outside the prefix", "", "[2001:db8:ffff::1]:5000", nil, Away},
		{"neighbour of the single address", "", "203.0.113.8:5000", nil, Away},
		// a trusted ingress forwards the client; the peer itself is on the LAN
		{"trusted peer, forwarded home client", "172.18.0.0/16", "172.18.0.2:4000", map[string][]string{"X-Forwarded-For": {"192.168.1.30"}}, Home},
		{"trusted peer, forwarded public client", "172.18.0.0/16", "172.18.0.2:4000", map[string][]string{"X-Forwarded-For": {"198.51.100.9"}}, Away},
		// an untrusted peer cannot claim a home address
		{"untrusted peer claims a home address", "", "198.51.100.9:5000", map[string][]string{"X-Forwarded-For": {"192.168.1.30"}}, Away},
		{"another subnet claims a home address", "172.18.0.0/16", "172.24.0.9:5555", map[string][]string{"X-Forwarded-For": {"192.168.1.30"}}, Away},
		// Cloudflare's headers make a LAN-looking request away, from any peer
		{"cloudflare header with a lan origin address", "", "192.168.1.20:5000", map[string][]string{"CF-Connecting-IP": {"198.51.100.9"}}, Away},
		{"cf-ray alone", "", "192.168.1.20:5000", map[string][]string{"Cf-Ray": {"8f0a-AMS"}}, Away},
		{"access jwt alone", "", "192.168.1.20:5000", map[string][]string{"Cf-Access-Jwt-Assertion": {"x"}}, Away},
		{"a home address in the cloudflare header does not make it home", "", "192.168.1.20:5000", map[string][]string{"CF-Connecting-IP": {"192.168.1.20"}}, Away},
		{"cloudflare header from a trusted peer", "172.18.0.0/16", "172.18.0.2:4000", map[string][]string{"CF-Connecting-IP": {"192.168.1.30"}, "X-Forwarded-For": {"192.168.1.30"}}, Away},
	}
	for _, c := range cases {
		info, _ := judged(t, c.trusted, c.peer, c.h, homeNets)
		if info.Locality != c.want || info.Listener != ListenerMain || !info.Configured {
			t.Errorf("%s: %+v, want %s", c.name, info, c.want)
		}
	}
}

func TestLocalityOffWhenUnset(t *testing.T) {
	info, _ := judged(t, "", "198.51.100.9:5000", map[string][]string{"CF-Ray": {"x"}}, "")
	if info.Configured || info.Locality != Home || info.Address != "198.51.100.9" || !strings.Contains(info.Reason, "not set") {
		t.Errorf("unset: %+v (every request is home, but the address is still reported)", info)
	}
}

func TestParseHomeNetworks(t *testing.T) {
	p, err := ParseHomeNetworks(" 192.168.1.0/24 , 100.64.0.0/10,203.0.113.7,2001:db8::/32,::1,192.168.5.9/24 ")
	if err != nil || len(p) != 6 {
		t.Fatalf("%v %v", p, err)
	}
	if p[5].String() != "192.168.5.0/24" {
		t.Errorf("a prefix with host bits is masked: %v", p[5])
	}
	if p, err := ParseHomeNetworks(""); err != nil || len(p) != 0 {
		t.Errorf("empty is off, not an error: %v %v", p, err)
	}
	for _, bad := range []string{"not-a-network", "192.168.1.0/24,oops", "192.168.1.0/33", ",,", "10.0.0.0/8;10.1.0.0/16"} {
		if p, err := ParseHomeNetworks(bad); err == nil {
			t.Errorf("%q: want an error, got %v", bad, p)
		}
	}
}

// tunnel runs a request through the tunnel listener's wrapper.
func tunnel(t *testing.T, headers map[string][]string, home string) (LocalityInfo, string) {
	t.Helper()
	peers, _ := ParseTunnelPeers("172.18.0.2/32")
	pol := newLocalityPolicy(home, "CF-Connecting-IP,CF-Ray")
	var info LocalityInfo
	var addr string
	// the inner chain is the main chain too: the tunnel's verdict must survive it
	s := &Server{}
	inner := trustedRealIP(parsePrefixes("172.18.0.0/16"))(s.localityMW(pol)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		info, addr = LocalityOf(r.Context()), r.RemoteAddr
	})))
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "172.18.0.2:4000" // the tunnel connector
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	tunnelWrap(pol, &tunnelGate{peers: peers}, inner).ServeHTTP(httptest.NewRecorder(), req)
	return info, addr
}

func TestLocalityTunnelListener(t *testing.T) {
	cf := func(v ...string) map[string][]string { return map[string][]string{"CF-Connecting-IP": v} }
	cases := []struct {
		name string
		h    map[string][]string
		want Locality
		addr string
	}{
		{"home address", cf("203.0.113.7"), Home, "203.0.113.7"},
		{"home ipv6", cf("2001:db8:abcd::5"), Home, "2001:db8:abcd::5"},
		{"public address", cf("198.51.100.9"), Away, "198.51.100.9"},
		{"public ipv6", cf("2606:4700::1"), Away, "2606:4700::1"},
		{"no header: away", nil, Away, "172.18.0.2:4000"},
		{"not an address: away", cf("hello"), Away, "172.18.0.2:4000"},
		{"two values are not believed", cf("203.0.113.7", "198.51.100.9"), Away, "172.18.0.2:4000"},
		{"a forwarded header does not stand in for it", map[string][]string{"X-Forwarded-For": {"203.0.113.7"}}, Away, "203.0.113.7"},
	}
	for _, c := range cases {
		info, addr := tunnel(t, c.h, homeNets)
		if info.Locality != c.want || info.Listener != ListenerTunnel {
			t.Errorf("%s: %+v, want %s on the tunnel", c.name, info, c.want)
		}
		if addr != c.addr {
			t.Errorf("%s: the address behind the wrapper is %q, want %q", c.name, addr, c.addr)
		}
	}
	// reasons are fit for a tooltip
	if i, _ := tunnel(t, cf("203.0.113.7"), homeNets); i.Reason != "tunnel, from your home address" {
		t.Errorf("reason = %q", i.Reason)
	}
	if i, _ := tunnel(t, cf("198.51.100.9"), homeNets); i.Reason != "tunnel, address not in the home networks" {
		t.Errorf("reason = %q", i.Reason)
	}
	// unconfigured: home, with the address reported so the owner can copy it
	if i, _ := tunnel(t, cf("198.51.100.9"), ""); i.Configured || i.Locality != Home || i.Address != "198.51.100.9" {
		t.Errorf("unconfigured tunnel: %+v", i)
	}
}

// A client cannot make the main listener believe it is the tunnel: the
// verdict is only ever set by the wrappers, never read from a request.
func TestLocalityCannotBeClaimedByAClient(t *testing.T) {
	for _, h := range []string{"X-Locality", "X-Listener", "X-Ws-Locality", "Forwarded"} {
		info, _ := judged(t, "", "198.51.100.9:5000", map[string][]string{h: {"home"}}, homeNets)
		if info.Locality != Away || info.Listener != ListenerMain {
			t.Errorf("%s: %+v", h, info)
		}
	}
	// a request that reaches the main listener with a tunnel-style header is away even from a home address
	info, _ := judged(t, "", "192.168.1.20:5000", map[string][]string{"CF-Connecting-IP": {"192.168.1.20"}}, homeNets)
	if info.Locality != Away || info.Listener != ListenerMain {
		t.Errorf("tunnel header on the main listener: %+v", info)
	}
}

func TestLocalityOfDefaultsToHome(t *testing.T) {
	i := LocalityOf(httptest.NewRequest("GET", "/", nil).Context())
	if i.Locality != Home || i.Configured {
		t.Errorf("a request that was never judged: %+v", i)
	}
}

func TestParseTunnelPeers(t *testing.T) {
	p, err := ParseTunnelPeers("127.0.0.1/32, 100.100.0.9,::1")
	if err != nil || len(p) != 3 {
		t.Fatalf("%v %v", p, err)
	}
	// required: an empty list must not start a listener that believes everyone
	if _, err := ParseTunnelPeers(""); err == nil || !strings.Contains(err.Error(), "WS_TUNNEL_PEERS is required") {
		t.Errorf("empty: %v", err)
	}
	if _, err := ParseTunnelPeers(" , "); err == nil {
		t.Error("blank entries only: want an error")
	}
	for _, bad := range []string{"nope", "10.0.0.1,oops", "10.0.0.0/40"} {
		if _, err := ParseTunnelPeers(bad); err == nil || !strings.Contains(err.Error(), "WS_TUNNEL_PEERS") {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

// tunnelFrom runs a request from a given TCP peer through the tunnel wrapper.
func tunnelFrom(t *testing.T, peers, peer string, headers map[string][]string, gate *tunnelGate) (*httptest.ResponseRecorder, LocalityInfo, bool) {
	t.Helper()
	pr, err := ParseTunnelPeers(peers)
	if err != nil {
		t.Fatal(err)
	}
	if gate == nil {
		gate = &tunnelGate{}
	}
	gate.peers = pr
	pol := newLocalityPolicy(homeNets, "CF-Connecting-IP")
	var info LocalityInfo
	reached := false
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { info, reached = LocalityOf(r.Context()), true })
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = peer
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	rec := httptest.NewRecorder()
	tunnelWrap(pol, gate, inner).ServeHTTP(rec, req)
	return rec, info, reached
}

func TestTunnelListenerRefusesUnlistedPeers(t *testing.T) {
	home := map[string][]string{"CF-Connecting-IP": {"203.0.113.7"}}
	// the connector is listed: a home address reads Home
	rec, info, reached := tunnelFrom(t, "100.100.0.9/32", "100.100.0.9:41000", home, nil)
	if rec.Code != 200 || !reached || info.Locality != Home || info.Listener != ListenerTunnel {
		t.Errorf("listed peer: %d reached=%v %+v", rec.Code, reached, info)
	}
	// anyone else on the tailnet who claims the same home address is refused before the app sees it
	rec, _, reached = tunnelFrom(t, "100.100.0.9/32", "100.100.0.77:41000", home, nil)
	if rec.Code != 403 || reached || !strings.Contains(rec.Body.String(), "forbidden") {
		t.Errorf("unlisted peer: %d reached=%v %q", rec.Code, reached, rec.Body.String())
	}
	// a neighbour of a /32, a public address, and an ipv6 peer are all unlisted
	for _, peer := range []string{"100.100.0.10:1", "198.51.100.9:1", "[2001:db8::1]:1", "not-an-address"} {
		if rec, _, reached := tunnelFrom(t, "100.100.0.9/32", peer, home, nil); rec.Code != 403 || reached {
			t.Errorf("%s: %d reached=%v", peer, rec.Code, reached)
		}
	}
	// a listed network, v4 and v6
	if rec, _, _ := tunnelFrom(t, "100.100.0.0/24,2001:db8::/32", "[2001:db8::5]:1", home, nil); rec.Code != 200 {
		t.Errorf("listed v6 peer: %d", rec.Code)
	}
	// a forwarded header cannot make an unlisted peer a listed one
	if rec, _, _ := tunnelFrom(t, "100.100.0.9/32", "100.100.0.77:1", map[string][]string{"X-Forwarded-For": {"100.100.0.9"}, "CF-Connecting-IP": {"203.0.113.7"}}, nil); rec.Code != 403 {
		t.Errorf("forged forwarded peer: %d", rec.Code)
	}
}

func TestTunnelRefusalsAreLoggedOnceAMinute(t *testing.T) {
	var buf strings.Builder
	g := &tunnelGate{log: slog.New(slog.NewTextHandler(&buf, nil)), interval: time.Hour}
	for i := 0; i < 5; i++ {
		tunnelFrom(t, "100.100.0.9/32", "100.100.0.77:1", nil, g)
	}
	if n := strings.Count(buf.String(), "refused a peer"); n != 1 {
		t.Errorf("%d log lines for five refusals in a window:\n%s", n, buf.String())
	}
	// after the window the next refusal says how many were held back
	g.last = time.Now().Add(-2 * time.Hour)
	tunnelFrom(t, "100.100.0.9/32", "100.100.0.77:1", nil, g)
	if !strings.Contains(buf.String(), "refused_since_last_line=5") && !strings.Contains(buf.String(), "refused_since_last_line=1") {
		t.Errorf("no count in the second line:\n%s", buf.String())
	}
}
