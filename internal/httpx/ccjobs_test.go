package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestNetGate: the job tab's routes are served to the tailnet and
// loopback only, by default; "*" opens them; an unparsable list refuses.
func TestNetGate(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	try := func(allow, remote string) int {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/jobs/cc", nil)
		req.RemoteAddr = remote
		netGate(allow)(ok).ServeHTTP(rec, req)
		return rec.Code
	}
	def := "100.64.0.0/10,127.0.0.0/8,::1/128"
	cases := []struct {
		allow, remote string
		want          int
	}{
		{def, "100.101.102.103:5555", 204}, // tailnet
		{def, "127.0.0.1:1", 204},
		{def, "[::1]:1", 204},
		{def, "203.0.113.9:443", 403}, // public
		{def, "192.168.1.5:443", 403}, // LAN is not the tailnet
		{def, "garbage", 403},
		{"*", "203.0.113.9:443", 204},
		{"192.168.0.0/16, 10.0.0.1", "192.168.1.5:443", 204},
		{"192.168.0.0/16, 10.0.0.1", "10.0.0.1:9", 204},
		{"192.168.0.0/16, 10.0.0.1", "10.0.0.2:9", 403},
		{"not-a-cidr", "127.0.0.1:1", 403}, // never fail open
		{"", "127.0.0.1:1", 403},
	}
	for _, c := range cases {
		if got := try(c.allow, c.remote); got != c.want {
			t.Errorf("allow %q remote %q: %d want %d", c.allow, c.remote, got, c.want)
		}
	}
}

// TestCCJobsRoutesNeedRegistry: without a registry the routes say so
// instead of panicking.
func TestCCJobsRoutesNeedRegistry(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	s.handleListCCJobs(rec, httptest.NewRequest("GET", "/api/jobs/cc", nil))
	if rec.Code != 503 {
		t.Errorf("status = %d", rec.Code)
	}
}
