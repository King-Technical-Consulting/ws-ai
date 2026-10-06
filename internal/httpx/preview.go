package httpx

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/sandbox"
	"github.com/jking323/ws/internal/store"
)

// PreviewProxy serves sandbox dev servers on their own hostnames
// (`{port}-{id}.preview.<domain>` or any pattern with those placeholders).
// A preview host is reached only after a redirect from the app origin
// that sets a signed, host-only cookie, so a stranger who learns the
// hostname gets a 401, and the preview page itself (untrusted code) never
// sees the app's session cookie.
type PreviewProxy struct {
	Pattern string // e.g. "{port}-{id}.preview.localhost" or "ws-p-{port}-{id}.home.arpa"
	Scheme  string // http | https (from WS_PUBLIC_URL)
	Secret  []byte
	Worker  *sandbox.WorkerClient
	DB      *store.DB
	Log     *slog.Logger

	re *regexp.Regexp
}

const previewCookie = "ws_preview"

// NewPreviewProxy compiles the host pattern.
func NewPreviewProxy(pattern, scheme string, secret []byte, worker *sandbox.WorkerClient, db *store.DB, log *slog.Logger) *PreviewProxy {
	if pattern == "" || !strings.Contains(pattern, "{port}") || !strings.Contains(pattern, "{id}") {
		return nil
	}
	esc := regexp.QuoteMeta(pattern)
	esc = strings.Replace(esc, regexp.QuoteMeta("{port}"), `(\d{2,5})`, 1)
	esc = strings.Replace(esc, regexp.QuoteMeta("{id}"), `([0-9a-f]{12})`, 1)
	re, err := regexp.Compile("^" + esc + "$")
	if err != nil {
		return nil
	}
	return &PreviewProxy{Pattern: pattern, Scheme: scheme, Secret: secret, Worker: worker, DB: db, Log: log, re: re}
}

func shortID(id uuid.UUID) string { return strings.ReplaceAll(id.String(), "-", "")[:12] }

// Host renders the preview hostname.
func (p *PreviewProxy) Host(id uuid.UUID, port int) string {
	h := strings.Replace(p.Pattern, "{id}", shortID(id), 1)
	return strings.Replace(h, "{port}", strconv.Itoa(port), 1)
}

// Match reports whether a request Host is a preview host.
func (p *PreviewProxy) Match(host string) (port int, short string, ok bool) {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	m := p.re.FindStringSubmatch(strings.ToLower(host))
	if m == nil {
		return 0, "", false
	}
	port, _ = strconv.Atoi(m[1])
	return port, m[2], true
}

func (p *PreviewProxy) sign(short string, exp int64) string {
	msg := short + "|" + strconv.FormatInt(exp, 10)
	m := hmac.New(sha256.New, p.Secret)
	m.Write([]byte(msg))
	return base64.RawURLEncoding.EncodeToString([]byte(msg)) + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (p *PreviewProxy) verify(tok, wantShort string) bool {
	i := strings.IndexByte(tok, '.')
	if i < 0 {
		return false
	}
	msgB, err := base64.RawURLEncoding.DecodeString(tok[:i])
	if err != nil {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(tok[i+1:])
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, p.Secret)
	m.Write(msgB)
	if !hmac.Equal(sig, m.Sum(nil)) {
		return false
	}
	parts := strings.SplitN(string(msgB), "|", 2)
	if len(parts) != 2 || parts[0] != wantShort {
		return false
	}
	exp, _ := strconv.ParseInt(parts[1], 10, 64)
	return time.Now().Unix() < exp
}

// AuthURL is where the app redirects the browser to open a preview: the
// preview host's /__ws/auth with a short-lived token.
func (p *PreviewProxy) AuthURL(id uuid.UUID, port int) string {
	host := p.Host(id, port)
	tok := p.sign(shortID(id), time.Now().Add(12*time.Hour).Unix())
	return p.Scheme + "://" + host + "/__ws/auth?t=" + url.QueryEscape(tok)
}

// Handler serves a request that arrived on a preview host.
func (p *PreviewProxy) Handler(port int, short string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__ws/auth" {
			tok := r.URL.Query().Get("t")
			if !p.verify(tok, short) {
				http.Error(w, "preview link expired; open it again from ws", http.StatusUnauthorized)
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name: previewCookie, Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
				Secure: p.Scheme == "https", MaxAge: 12 * 3600,
			})
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		c, err := r.Cookie(previewCookie)
		if err != nil || !p.verify(c.Value, short) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			http.Error(w, "This is a ws sandbox preview. Open it from the project's Preview tab.", http.StatusUnauthorized)
			return
		}
		info, err := p.resolve(r.Context(), short)
		if err != nil {
			http.Error(w, "sandbox is not running: "+err.Error(), http.StatusBadGateway)
			return
		}
		target := &url.URL{Scheme: "http", Host: net.JoinHostPort(info.IP, strconv.Itoa(port))}
		rp := &httputil.ReverseProxy{
			// Sandboxes are one hop away on the internal network; a dev server
			// that isn't up yet should fail fast, not hang the tab.
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
				ResponseHeaderTimeout: 60 * time.Second,
				Proxy:                 nil,
			},
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(target)
				// Dev servers (Vite, Next) check Host against allowed hosts;
				// present as a local request.
				pr.Out.Host = "localhost:" + strconv.Itoa(port)
				pr.Out.Header.Del("Cookie") // the preview cookie is ours, not the app's
			},
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				http.Error(w, fmt.Sprintf("nothing is listening on port %d in the sandbox yet (%v)", port, err), http.StatusBadGateway)
			},
			FlushInterval: -1,
		}
		rp.ServeHTTP(w, r)
	})
}

// resolve maps a short id to a running sandbox's IP, via the worker.
func (p *PreviewProxy) resolve(ctx context.Context, short string) (sandbox.Info, error) {
	row, err := p.DB.GetSandboxByShortID(ctx, short)
	if err != nil {
		return sandbox.Info{}, fmt.Errorf("unknown sandbox")
	}
	if info, ok := p.Worker.Cached(row.ID, 30*time.Second); ok {
		return info, nil
	}
	info, err := p.Worker.Get(ctx, row.ID)
	if err != nil {
		return sandbox.Info{}, err
	}
	if info.Status != "running" || info.IP == "" {
		return sandbox.Info{}, fmt.Errorf("status %s", info.Status)
	}
	return info, nil
}
