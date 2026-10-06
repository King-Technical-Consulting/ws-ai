package sandbox

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DefaultAllow is the egress allowlist every sandbox gets: package
// registries, GitHub, and module proxies. Hosts match exactly or as a
// suffix (".example.com" matches a.example.com).
var DefaultAllow = []string{
	"github.com", "api.github.com", "codeload.github.com", "objects.githubusercontent.com", "raw.githubusercontent.com",
	"ghcr.io", "pkg-containers.githubusercontent.com",
	"registry.npmjs.org", "registry.yarnpkg.com", "nodejs.org",
	"pypi.org", "files.pythonhosted.org",
	"proxy.golang.org", "sum.golang.org", "index.golang.org", "storage.googleapis.com", "go.dev", "golang.org",
	"crates.io", "static.crates.io", "index.crates.io",
	"deb.debian.org", "security.debian.org",
}

// Proxy is the sandboxes' only way out: an HTTP forward proxy with a host
// allowlist. HTTPS goes through as a CONNECT tunnel (opaque). Plain-HTTP
// requests to hosts in Upgrade are rewritten to HTTPS with a credential
// injected, which is how git authenticates to GitHub without a token ever
// existing inside the container.
type Proxy struct {
	Allow []string
	// Upgrade lists hosts whose plain-HTTP requests are upgraded to HTTPS
	// with Credential applied (github.com).
	Upgrade map[string]bool
	// Resolve maps a client IP to a sandbox; nil accepts any client.
	Resolve func(ctx context.Context, ip string) (*Sandbox, bool)
	// Credential returns the Authorization header value for a host on behalf
	// of a sandbox, or "" for none.
	Credential func(ctx context.Context, sb *Sandbox, host string) string
	Log        *slog.Logger

	dialer    net.Dialer
	transport *http.Transport
	once      sync.Once
}

// GitHubBasic builds the Authorization value git expects for a token.
func GitHubBasic(token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
}

func (p *Proxy) init() {
	p.dialer = net.Dialer{Timeout: 15 * time.Second}
	p.transport = &http.Transport{
		Proxy:                 nil, // never chain to an outer proxy
		DialContext:           p.dialer.DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		MaxIdleConns:          32,
		IdleConnTimeout:       90 * time.Second,
	}
}

func (p *Proxy) allowed(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	for _, a := range p.Allow {
		a = strings.ToLower(a)
		if host == a || strings.HasSuffix(host, "."+a) {
			return true
		}
	}
	return false
}

func (p *Proxy) logf(level slog.Level, msg string, args ...any) {
	if p.Log != nil {
		p.Log.Log(context.Background(), level, msg, args...)
	}
}

// ServeHTTP implements the proxy.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.once.Do(p.init)
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	var sb *Sandbox
	if p.Resolve != nil {
		var ok bool
		sb, ok = p.Resolve(r.Context(), ip)
		if !ok {
			p.logf(slog.LevelWarn, "egress: unknown client", "ip", ip, "host", r.Host)
			http.Error(w, "egress: unknown client", http.StatusForbidden)
			return
		}
	}
	if r.Method == http.MethodConnect {
		p.connect(w, r, sb)
		return
	}
	if !r.URL.IsAbs() {
		http.Error(w, "egress: this is a proxy; use an absolute URL", http.StatusBadRequest)
		return
	}
	host := r.URL.Hostname()
	if !p.allowed(host) {
		p.logf(slog.LevelInfo, "egress: refused", "host", host, "ip", ip)
		http.Error(w, "egress: host not allowed: "+host, http.StatusForbidden)
		return
	}
	p.forward(w, r, sb, host)
}

func (p *Proxy) connect(w http.ResponseWriter, r *http.Request, sb *Sandbox) {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		host, port = r.Host, "443"
	}
	if !p.allowed(host) || (port != "443" && port != "80") {
		p.logf(slog.LevelInfo, "egress: refused CONNECT", "host", r.Host)
		http.Error(w, "egress: host not allowed: "+host, http.StatusForbidden)
		return
	}
	up, err := p.dialer.DialContext(r.Context(), "tcp", net.JoinHostPort(host, port))
	if err != nil {
		http.Error(w, "egress: dial: "+err.Error(), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		up.Close()
		http.Error(w, "egress: no hijack", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	down, buf, err := hj.Hijack()
	if err != nil {
		up.Close()
		return
	}
	if buf != nil && buf.Reader.Buffered() > 0 {
		b := make([]byte, buf.Reader.Buffered())
		_, _ = io.ReadFull(buf.Reader, b)
		_, _ = up.Write(b)
	}
	go func() { _, _ = io.Copy(up, down); closeWrite(up) }()
	_, _ = io.Copy(down, up)
	down.Close()
	up.Close()
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

var hopHeaders = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, sb *Sandbox, host string) {
	target := *r.URL
	if p.Upgrade[strings.ToLower(host)] {
		target.Scheme = "https"
		if target.Port() == "80" {
			target.Host = host
		}
	}
	out, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), r.Body)
	if err != nil {
		http.Error(w, "egress: bad request", http.StatusBadRequest)
		return
	}
	out.Header = r.Header.Clone()
	for _, h := range hopHeaders {
		out.Header.Del(h)
	}
	out.Host = target.Host
	// The sandbox never has a real credential; whatever it sent is replaced.
	out.Header.Del("Authorization")
	if p.Credential != nil {
		if v := p.Credential(r.Context(), sb, strings.ToLower(host)); v != "" {
			out.Header.Set("Authorization", v)
		}
	}
	resp, err := p.transport.RoundTrip(out)
	if err != nil {
		http.Error(w, "egress: upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// Server returns an http.Server for the proxy on addr.
func (p *Proxy) Server(addr string) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           p,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

// ParseAllow splits a comma-separated allowlist, ignoring blanks.
func ParseAllow(s string) []string {
	var out []string
	for _, h := range strings.Split(s, ",") {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		if u, err := url.Parse(h); err == nil && u.Host != "" {
			h = u.Hostname()
		}
		out = append(out, h)
	}
	return out
}
