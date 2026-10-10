package httpx

import (
	"context"
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jking323/ws/internal/auth"
	"github.com/jking323/ws/internal/store"
)

// adminTestServer connects to the database in WS_TEST_DATABASE_URL (skipping
// without one), migrates it, and returns a server whose routes are the
// owner-only user routes, as the app mounts them. Example:
//
//	WS_TEST_DATABASE_URL=postgres://ws:ws@127.0.0.1:55432/wstest?sslmode=disable go test ./internal/httpx/ -run AdminUser
func adminTestServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	dsn := os.Getenv("WS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	db, err := store.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err := db.Migrate(ctx, "up"); err != nil {
		t.Fatal(err)
	}
	a, err := auth.New(db, auth.LogMailer{Log: slog.Default()}, auth.Config{RPID: "localhost", RPDisplayName: "ws", RPOrigins: []string{"http://localhost"}, PublicURL: "http://localhost"})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{DB: db, Auth: a}
	r := chi.NewRouter()
	r.Group(func(ad chi.Router) {
		ad.Use(requireOwner)
		ad.Delete("/api/admin/users/{id}/sessions", s.handleAdminRevokeUserSessions)
		ad.Get("/api/admin/users/{id}/keys", s.handleAdminListUserKeys)
		ad.Delete("/api/admin/users/{id}/keys/{keyId}", s.handleAdminRevokeUserKey)
	})
	return s, r
}

// newTestUser makes a user with a fresh address and removes them after the test.
func newTestUser(t *testing.T, s *Server, role string) store.User {
	t.Helper()
	u, err := s.DB.CreateUser(context.Background(), store.CreateUserParams{Email: "t-" + uuid.NewString()[:8] + "@test.invalid", DisplayName: "t", Role: role})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.DB.Pool.Exec(context.Background(), "DELETE FROM users WHERE id = $1", u.ID) })
	return u
}

// sessionCookie signs userID in and returns the session cookie.
func sessionCookie(t *testing.T, s *Server, userID uuid.UUID) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	if err := s.Auth.CreateSession(context.Background(), rec, httptest.NewRequest("GET", "/", nil), userID); err != nil {
		t.Fatal(err)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookie {
			return c
		}
	}
	t.Fatal("no session cookie set")
	return nil
}

func (s *Server) authenticates(t *testing.T, c *http.Cookie) bool {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/me", nil)
	req.AddCookie(c)
	_, err := s.Auth.Authenticate(context.Background(), req)
	return err == nil
}

func call(h http.Handler, method, path string, p *auth.Principal) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withPrincipal(httptest.NewRequest(method, path, nil), p))
	return rec
}

func principalOf(u store.User) *auth.Principal {
	return &auth.Principal{UserID: u.ID, Email: u.Email, Role: u.Role}
}

// TestAdminUserSessionsPurge: the owner ends every session of a member; the
// member's cookies stop authenticating, the member's keys and account stay,
// a member cannot do it, and the owner may do it to themselves.
func TestAdminUserSessionsPurge(t *testing.T) {
	s, h := adminTestServer(t)
	owner := newTestUser(t, s, "owner")
	member := newTestUser(t, s, "member")
	c1, c2 := sessionCookie(t, s, member.ID), sessionCookie(t, s, member.ID)
	oc := sessionCookie(t, s, owner.ID)
	raw, _, err := s.Auth.CreateAPIKey(context.Background(), member.ID, "k", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if !s.authenticates(t, c1) || !s.authenticates(t, c2) {
		t.Fatal("fresh sessions should authenticate")
	}

	if rec := call(h, "DELETE", "/api/admin/users/"+member.ID.String()+"/sessions", principalOf(member)); rec.Code != 403 {
		t.Fatalf("member purging sessions: %d, want 403", rec.Code)
	}
	if rec := call(h, "DELETE", "/api/admin/users/"+uuid.NewString()+"/sessions", principalOf(owner)); rec.Code != 404 {
		t.Fatalf("unknown user: %d, want 404", rec.Code)
	}
	if rec := call(h, "DELETE", "/api/admin/users/not-a-uuid/sessions", principalOf(owner)); rec.Code != 400 {
		t.Fatalf("bad id: %d, want 400", rec.Code)
	}
	if rec := call(h, "DELETE", "/api/admin/users/"+member.ID.String()+"/sessions", principalOf(owner)); rec.Code != 200 {
		t.Fatalf("owner purging sessions: %d %s", rec.Code, rec.Body)
	}
	if s.authenticates(t, c1) || s.authenticates(t, c2) {
		t.Fatal("member sessions should be dead")
	}
	if !s.authenticates(t, oc) {
		t.Fatal("the owner's session should be untouched")
	}
	// The account and its API key stay; a new sign-in works at once.
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	if _, err := s.Auth.Authenticate(context.Background(), req); err != nil {
		t.Fatal("the member's API key should still work:", err)
	}
	if !s.authenticates(t, sessionCookie(t, s, member.ID)) {
		t.Fatal("a fresh session should authenticate")
	}
	// The owner may sign themselves out everywhere too.
	if rec := call(h, "DELETE", "/api/admin/users/"+owner.ID.String()+"/sessions", principalOf(owner)); rec.Code != 200 {
		t.Fatalf("owner purging own sessions: %d", rec.Code)
	}
	if s.authenticates(t, oc) {
		t.Fatal("the owner's session should be dead after their own purge")
	}
}

// TestAdminUserKeyRevoke: the owner lists a member's keys (never the hash)
// and revokes one; the key stops authenticating, the others stay, and a key
// that is not that member's is 404.
func TestAdminUserKeyRevoke(t *testing.T) {
	s, h := adminTestServer(t)
	owner := newTestUser(t, s, "owner")
	member := newTestUser(t, s, "member")
	other := newTestUser(t, s, "member")
	raw1, k1, err := s.Auth.CreateAPIKey(context.Background(), member.ID, "first", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	raw2, _, err := s.Auth.CreateAPIKey(context.Background(), member.ID, "second", []string{auth.ScopeChat, auth.ScopeMCP}, "")
	if err != nil {
		t.Fatal(err)
	}
	_, ok, err := s.Auth.CreateAPIKey(context.Background(), other.ID, "theirs", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	keyWorks := func(raw string) bool {
		req := httptest.NewRequest("GET", "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+raw)
		_, err := s.Auth.Authenticate(context.Background(), req)
		return err == nil
	}

	if rec := call(h, "GET", "/api/admin/users/"+member.ID.String()+"/keys", principalOf(member)); rec.Code != 403 {
		t.Fatalf("member listing keys: %d, want 403", rec.Code)
	}
	rec := call(h, "GET", "/api/admin/users/"+member.ID.String()+"/keys", principalOf(owner))
	if rec.Code != 200 {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{k1.ID.String(), `"first"`, `"second"`, k1.Prefix} {
		if !strings.Contains(body, want) {
			t.Errorf("list lacks %s: %s", want, body)
		}
	}
	for _, never := range []string{base64.StdEncoding.EncodeToString(k1.KeyHash), raw1, "key_hash", "theirs"} {
		if strings.Contains(body, never) {
			t.Errorf("list leaks %q: %s", never, body)
		}
	}

	// Another member's key through this member's route: not theirs, 404.
	if rec := call(h, "DELETE", "/api/admin/users/"+member.ID.String()+"/keys/"+ok.ID.String(), principalOf(owner)); rec.Code != 404 {
		t.Fatalf("someone else's key: %d, want 404", rec.Code)
	}
	if rec := call(h, "DELETE", "/api/admin/users/"+member.ID.String()+"/keys/"+k1.ID.String(), principalOf(member)); rec.Code != 403 {
		t.Fatalf("member revoking: %d, want 403", rec.Code)
	}
	if rec := call(h, "DELETE", "/api/admin/users/"+member.ID.String()+"/keys/"+k1.ID.String(), principalOf(owner)); rec.Code != 200 {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body)
	}
	if keyWorks(raw1) {
		t.Fatal("revoked key still authenticates")
	}
	if !keyWorks(raw2) {
		t.Fatal("the other key should still work")
	}
	// Revoking it again: already gone.
	if rec := call(h, "DELETE", "/api/admin/users/"+member.ID.String()+"/keys/"+k1.ID.String(), principalOf(owner)); rec.Code != 404 {
		t.Fatalf("second revoke: %d, want 404", rec.Code)
	}
	rec = call(h, "GET", "/api/admin/users/"+member.ID.String()+"/keys", principalOf(owner))
	if strings.Contains(rec.Body.String(), `"first"`) {
		t.Error("revoked key still listed")
	}
}
