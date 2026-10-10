package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// seen runs a request through the middleware and returns the RemoteAddr the
// handler sees.
func seen(t *testing.T, trusted, peer string, headers map[string][]string) string {
	t.Helper()
	var got string
	h := trustedRealIP(parsePrefixes(trusted))(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r.RemoteAddr }))
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = peer
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	h.ServeHTTP(httptest.NewRecorder(), req)
	return got
}

func TestClientIPUntrustedPeerIsNeverBelieved(t *testing.T) {
	// The default: nothing trusted, so a forged header changes nothing.
	if got := seen(t, "", "172.24.0.9:5555", map[string][]string{"X-Forwarded-For": {"100.64.0.1"}}); got != "172.24.0.9:5555" {
		t.Errorf("no trusted proxies: %q", got)
	}
	// A sandbox on another subnet than the trusted ingress.
	for _, h := range []string{"X-Forwarded-For", "X-Real-IP", "True-Client-IP"} {
		if got := seen(t, "172.18.0.0/16", "172.24.0.9:5555", map[string][]string{h: {"100.64.0.1"}}); got != "172.24.0.9:5555" {
			t.Errorf("untrusted peer with %s: %q", h, got)
		}
	}
}

func TestClientIPTrustedPeer(t *testing.T) {
	trusted := "172.18.0.0/16"
	peer := "172.18.0.2:4000" // the ingress
	cases := []struct {
		name string
		h    map[string][]string
		want string
	}{
		{"forwarded client", map[string][]string{"X-Forwarded-For": {"100.64.0.7"}}, "100.64.0.7"},
		{"a client-supplied prefix is ignored", map[string][]string{"X-Forwarded-For": {"1.2.3.4, 100.64.0.7"}}, "100.64.0.7"},
		{"a chain of trusted proxies is walked", map[string][]string{"X-Forwarded-For": {"100.64.0.7, 172.18.0.5"}}, "100.64.0.7"},
		{"several header lines", map[string][]string{"X-Forwarded-For": {"9.9.9.9", "100.64.0.7"}}, "100.64.0.7"},
		{"x-real-ip when there is no x-forwarded-for", map[string][]string{"X-Real-IP": {"100.64.0.8"}}, "100.64.0.8"},
		{"true-client-ip is not used", map[string][]string{"True-Client-IP": {"100.64.0.9"}}, peer},
		{"no headers: the peer", nil, peer},
		{"garbage in the chain: believe nothing", map[string][]string{"X-Forwarded-For": {"not-an-ip"}}, peer},
		{"only trusted addresses: the peer", map[string][]string{"X-Forwarded-For": {"172.18.0.3"}}, peer},
		{"ipv6", map[string][]string{"X-Forwarded-For": {"fd7a::1"}}, "fd7a::1"},
	}
	for _, c := range cases {
		if got := seen(t, trusted, peer, c.h); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestParsePrefixes(t *testing.T) {
	got := parsePrefixes(" 172.18.0.0/16, 10.0.0.5 ,,junk, ::1/128 ")
	if len(got) != 3 {
		t.Fatalf("prefixes = %v", got)
	}
	if got[1].String() != "10.0.0.5/32" {
		t.Errorf("a bare address becomes a /32: %v", got[1])
	}
}

// A request that reaches the tailnet gate from a sandbox cannot talk its way
// in with a forged header.
func TestNetGateCannotBeSpoofedFromAnUntrustedPeer(t *testing.T) {
	gate := netGate("100.64.0.0/10,127.0.0.0/8")
	chain := trustedRealIP(parsePrefixes("172.18.0.0/16"))(gate(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })))
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "172.24.0.9:5555"
	req.Header.Set("X-Forwarded-For", "100.64.0.1")
	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("a forged header passed the gate: %d", rec.Code)
	}
	// The same header through the trusted ingress is believed.
	req = httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "172.18.0.2:4000"
	req.Header.Set("X-Forwarded-For", "100.64.0.1")
	rec = httptest.NewRecorder()
	chain.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("the ingress's forwarded client was refused: %d", rec.Code)
	}
}
