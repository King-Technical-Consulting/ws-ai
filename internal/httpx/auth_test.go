package httpx

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/auth"
)

func withPrincipal(r *http.Request, p *auth.Principal) *http.Request {
	if p == nil {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), principalKey, p))
}

func keyPrincipal(scopes ...string) *auth.Principal {
	return &auth.Principal{UserID: uuid.New(), Role: "owner", APIKeyID: uuid.NullUUID{UUID: uuid.New(), Valid: true}, Scopes: scopes}
}

// TestRequireAuthRefusesAPIKeys: the web /api routes take a browser
// session; an API key, whatever its scopes and whoever owns it, is
// refused there. A route group mounted with requireAuthScope takes a key
// carrying that scope and nothing less.
func TestRequireAuthRefusesAPIKeys(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	session := &auth.Principal{UserID: uuid.New(), Role: "owner"}
	try := func(mw func(http.Handler) http.Handler, method string, p *auth.Principal, hdr map[string]string) int {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/api/x", nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		mw(ok).ServeHTTP(rec, withPrincipal(req, p))
		return rec.Code
	}
	jobs := requireAuthScope(auth.ScopeJobs)
	cases := []struct {
		name string
		mw   func(http.Handler) http.Handler
		meth string
		p    *auth.Principal
		hdr  map[string]string
		want int
	}{
		{"anonymous", requireAuth, "GET", nil, nil, 401},
		{"session", requireAuth, "GET", session, nil, 204},
		{"session post same-origin", requireAuth, "POST", session, map[string]string{"Sec-Fetch-Site": "same-origin"}, 204},
		{"session post no header", requireAuth, "POST", session, nil, 204},
		{"session post cross-site", requireAuth, "POST", session, map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
		{"chat key", requireAuth, "GET", keyPrincipal(auth.ScopeChat), nil, 403},
		{"chat+mcp key", requireAuth, "GET", keyPrincipal(auth.ScopeChat, auth.ScopeMCP), nil, 403},
		{"jobs key on a plain route", requireAuth, "GET", keyPrincipal(auth.ScopeJobs), nil, 403},
		{"session on the jobs routes", jobs, "GET", session, nil, 204},
		{"jobs key on the jobs routes", jobs, "POST", keyPrincipal(auth.ScopeJobs), nil, 204},
		{"jobs key cross-site header ignored (not a browser)", jobs, "POST", keyPrincipal(auth.ScopeJobs), map[string]string{"Sec-Fetch-Site": "cross-site"}, 204},
		{"chat key on the jobs routes", jobs, "POST", keyPrincipal(auth.ScopeChat), nil, 403},
		{"anonymous on the jobs routes", jobs, "GET", nil, nil, 401},
	}
	for _, c := range cases {
		if got := try(c.mw, c.meth, c.p, c.hdr); got != c.want {
			t.Errorf("%s: %d want %d", c.name, got, c.want)
		}
	}
}

// TestRequireAuthV1Scopes: /v1 takes a chat key or a session, in the
// protocol's error shape when it refuses.
func TestRequireAuthV1Scopes(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	try := func(path string, p *auth.Principal) (int, string) {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", path, nil)
		requireAuthV1(ok).ServeHTTP(rec, withPrincipal(req, p))
		return rec.Code, rec.Body.String()
	}
	if code, _ := try("/v1/chat/completions", keyPrincipal(auth.ScopeChat)); code != 204 {
		t.Errorf("chat key: %d", code)
	}
	if code, _ := try("/v1/messages", &auth.Principal{UserID: uuid.New()}); code != 204 {
		t.Errorf("session: %d", code)
	}
	if code, body := try("/v1/messages", keyPrincipal(auth.ScopeMCP)); code != 403 || !contains(body, "permission_error") {
		t.Errorf("mcp-only key on /v1/messages: %d %s", code, body)
	}
	if code, body := try("/v1/chat/completions", keyPrincipal(auth.ScopeJobs)); code != 403 || !contains(body, "insufficient_scope") {
		t.Errorf("jobs-only key on /v1: %d %s", code, body)
	}
	if code, body := try("/v1/models", nil); code != 401 || !contains(body, "invalid_api_key") {
		t.Errorf("anonymous: %d %s", code, body)
	}
}

// TestRouterKeyGates walks the real router: a key with every scope still
// gets 403 on /api/me, on an owner-only admin route and on key minting,
// while the jobs routes take it.
func TestRouterKeyGates(t *testing.T) {
	s := &Server{Auth: &auth.Service{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h := s.appHandler()
	key := keyPrincipal(auth.ScopeChat, auth.ScopeMCP, auth.ScopeJobs)
	try := func(method, path string, p *auth.Principal) int {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, nil)
		req.RemoteAddr = "127.0.0.1:1"
		// Bypass s.authenticate (it needs a database) by handing the
		// principal to the route the way the middleware would.
		req = withPrincipal(req, p)
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/api/me", 403},
		{"GET", "/api/projects", 403},
		{"POST", "/api/keys", 403},
		{"GET", "/api/admin/users", 403},
		{"GET", "/api/auth/passkeys", 403},
		{"GET", "/api/jobs/cc", 503}, // past auth, owner and the net gate; no registry in this test
		{"HEAD", "/api/hello", 200},
	} {
		if got := try(c.method, c.path, key); got != c.want {
			t.Errorf("%s %s with an all-scope key: %d want %d", c.method, c.path, got, c.want)
		}
	}
	if got := try("GET", "/api/jobs/cc", keyPrincipal(auth.ScopeChat)); got != 403 {
		t.Errorf("chat key on the jobs routes: %d want 403", got)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
