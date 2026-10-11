package auth

import (
	"net/http/httptest"
	"testing"
)

// The address recorded on a session is the one the server already settled on
// (r.RemoteAddr, after the trusted-proxy and tunnel steps), never a header
// the caller chose.
func TestClientIPIsTheSettledAddressOnly(t *testing.T) {
	for _, c := range []struct {
		name, remote string
		headers      map[string]string
		want         string
	}{
		{"plain peer", "198.51.100.9:4321", nil, "198.51.100.9"},
		{"no port, as the middleware sets it", "198.51.100.9", nil, "198.51.100.9"},
		{"ipv6 with a port", "[2001:db8::7]:4321", nil, "2001:db8::7"},
		{"ipv6 without a port", "2001:db8::7", nil, "2001:db8::7"},
		{"a forwarded header is ignored", "198.51.100.9:4321", map[string]string{"X-Forwarded-For": "10.0.0.1"}, "198.51.100.9"},
		{"a cloudflare header is ignored", "198.51.100.9:4321", map[string]string{"CF-Connecting-IP": "10.0.0.1"}, "198.51.100.9"},
		{"both are ignored", "198.51.100.9:4321", map[string]string{"X-Forwarded-For": "10.0.0.1", "CF-Connecting-IP": "10.0.0.2"}, "198.51.100.9"},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		for k, v := range c.headers {
			r.Header.Set(k, v)
		}
		if got := clientIP(r); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
