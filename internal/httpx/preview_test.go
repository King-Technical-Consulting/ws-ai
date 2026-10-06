package httpx

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestPreviewHostPattern(t *testing.T) {
	p := NewPreviewProxy("ws-p-{port}-{id}.home.arpa", "https", []byte("secret"), nil, nil, nil)
	if p == nil {
		t.Fatal("pattern should compile")
	}
	id := uuid.MustParse("0a912836-c3c3-42e4-9f00-000000000000")
	host := p.Host(id, 3000)
	if host != "ws-p-3000-0a912836c3c3.home.arpa" {
		t.Errorf("Host = %q", host)
	}
	port, short, ok := p.Match(host + ":443")
	if !ok || port != 3000 || short != "0a912836c3c3" {
		t.Errorf("Match = %d %q %v", port, short, ok)
	}
	if _, _, ok := p.Match("ws.home.arpa"); ok {
		t.Errorf("app host must not match")
	}
	if _, _, ok := p.Match("ws-p-3000-0a912836c3c3.home.arpa.evil.com"); ok {
		t.Errorf("suffix spoof must not match")
	}
	if NewPreviewProxy("no-placeholders.example", "http", nil, nil, nil, nil) != nil {
		t.Errorf("pattern without placeholders must be rejected")
	}
	// Default dev pattern.
	d := NewPreviewProxy("{port}-{id}.preview.localhost", "http", []byte("s"), nil, nil, nil)
	if port, _, ok := d.Match("5173-0a912836c3c3.preview.localhost:8080"); !ok || port != 5173 {
		t.Errorf("dev pattern: %d %v", port, ok)
	}
}

func TestPreviewTokenAndCookieFlow(t *testing.T) {
	p := NewPreviewProxy("{port}-{id}.preview.localhost", "http", []byte("secret"), nil, nil, nil)
	id := uuid.New()
	short := shortID(id)
	authURL := p.AuthURL(id, 3000)
	u, err := url.Parse(authURL)
	if err != nil || u.Path != "/__ws/auth" || u.Host != p.Host(id, 3000) {
		t.Fatalf("AuthURL = %q (%v)", authURL, err)
	}
	tok := u.Query().Get("t")
	if !p.verify(tok, short) {
		t.Fatal("fresh token must verify")
	}
	if p.verify(tok, "ffffffffffff") {
		t.Error("token must be bound to the sandbox")
	}
	if p.verify(tok+"x", short) || p.verify("garbage", short) {
		t.Error("tampered token must fail")
	}
	other := NewPreviewProxy("{port}-{id}.preview.localhost", "http", []byte("other"), nil, nil, nil)
	if other.verify(tok, short) {
		t.Error("token must not verify under another secret")
	}

	// /__ws/auth sets the cookie and redirects to /.
	h := p.Handler(3000, short)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://"+u.Host+u.RequestURI(), nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
		t.Fatalf("auth: %d %v", rec.Code, rec.Header())
	}
	var cookie string
	for _, c := range rec.Result().Cookies() {
		if c.Name == previewCookie {
			cookie = c.Value
			if !c.HttpOnly || c.Path != "/" {
				t.Errorf("cookie flags: %+v", c)
			}
		}
	}
	if cookie == "" {
		t.Fatal("no preview cookie set")
	}

	// Without the cookie: 401, not a proxy attempt.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://"+u.Host+"/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no cookie: %d", rec.Code)
	}
	// Expired-token path on /__ws/auth.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://"+u.Host+"/__ws/auth?t=bad", nil))
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "expired") {
		t.Errorf("bad token: %d %q", rec.Code, rec.Body.String())
	}
}
