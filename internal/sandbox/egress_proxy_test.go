package sandbox

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestAllowed(t *testing.T) {
	p := &Proxy{Allow: []string{"github.com", "registry.npmjs.org"}}
	for host, want := range map[string]bool{
		"github.com": true, "api.github.com": true, "GitHub.com:443": true, "evilgithub.com": false,
		"registry.npmjs.org": true, "example.com": false, "github.com.evil.io": false,
	} {
		if got := p.allowed(host); got != want {
			t.Errorf("allowed(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestParseAllow(t *testing.T) {
	got := ParseAllow(" a.com, https://ws.home.arpa:443/path ,, b.io ")
	if strings.Join(got, ",") != "a.com,ws.home.arpa,b.io" {
		t.Errorf("ParseAllow = %v", got)
	}
}

// A plain-HTTP request to an allowlisted host is forwarded with the
// sandbox's own Authorization replaced by the injected credential.
func TestForwardInjectsCredential(t *testing.T) {
	var gotAuth, gotHost string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotHost = r.Host
		w.Header().Set("X-Up", "1")
		w.WriteHeader(201)
		_, _ = io.WriteString(w, "ok")
	}))
	defer up.Close()
	upURL, _ := url.Parse(up.URL)

	p := &Proxy{
		Allow:   []string{upURL.Hostname()},
		Resolve: func(ctx context.Context, ip string) (*Sandbox, bool) { return &Sandbox{}, true },
		Credential: func(ctx context.Context, sb *Sandbox, host string) string {
			return GitHubBasic("tok")
		},
	}
	px := httptest.NewServer(p)
	defer px.Close()

	pxURL, _ := url.Parse(px.URL)
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pxURL)}}
	req, _ := http.NewRequest("GET", up.URL+"/info/refs", nil)
	req.Header.Set("Authorization", "Basic sandbox-made-this-up")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 201 || string(body) != "ok" || resp.Header.Get("X-Up") != "1" {
		t.Errorf("response = %d %q %v", resp.StatusCode, body, resp.Header)
	}
	if gotAuth != GitHubBasic("tok") {
		t.Errorf("upstream saw Authorization %q", gotAuth)
	}
	if gotHost != upURL.Host {
		t.Errorf("upstream Host = %q", gotHost)
	}
}

func TestRefusals(t *testing.T) {
	p := &Proxy{Allow: []string{"allowed.example"}}
	px := httptest.NewServer(p)
	defer px.Close()
	pxURL, _ := url.Parse(px.URL)
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pxURL)}}

	// Plain HTTP to a host off the list.
	resp, err := c.Get("http://blocked.example/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("blocked host: status %d", resp.StatusCode)
	}

	// CONNECT to a host off the list is refused before any dial.
	conn, err := net.Dial("tcp", pxURL.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = io.WriteString(conn, "CONNECT blocked.example:443 HTTP/1.1\r\nHost: blocked.example:443\r\n\r\n")
	buf := make([]byte, 64)
	n, _ := conn.Read(buf)
	if !strings.Contains(string(buf[:n]), "403") {
		t.Errorf("CONNECT to blocked host: %q", buf[:n])
	}

	// Unknown client when a resolver is set.
	p2 := &Proxy{Allow: []string{"allowed.example"}, Resolve: func(context.Context, string) (*Sandbox, bool) { return nil, false }}
	px2 := httptest.NewServer(p2)
	defer px2.Close()
	px2URL, _ := url.Parse(px2.URL)
	c2 := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(px2URL)}}
	resp, err = c2.Get("http://allowed.example/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("unknown client: status %d", resp.StatusCode)
	}
}

func TestResolvePath(t *testing.T) {
	cases := map[string]string{
		"": Workspace, ".": Workspace, "src/main.go": Workspace + "/src/main.go",
		"/workspace/a/../b": Workspace + "/b", "/workspace": Workspace,
	}
	for in, want := range cases {
		got, err := resolvePath(in)
		if err != nil || got != want {
			t.Errorf("resolvePath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"../etc/passwd", "/etc/passwd", "/workspace/../etc", "/workspaces/x"} {
		if _, err := resolvePath(bad); err == nil {
			t.Errorf("resolvePath(%q) should fail", bad)
		}
	}
}
