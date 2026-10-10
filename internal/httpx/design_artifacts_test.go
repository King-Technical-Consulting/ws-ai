package httpx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jking323/ws/internal/artifacts"
	"github.com/jking323/ws/internal/auth"
	"github.com/jking323/ws/internal/store"
)

// designTestServer is adminTestServer plus an artifact service and the
// conversation and artifact routes as the app mounts them (behind a
// principal the tests set per request).
func designTestServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	s, _ := adminTestServer(t)
	s.Artifacts = &artifacts.Service{DB: s.DB, Secret: []byte("test-secret"), BaseURL: "http://art.test"}
	r := chi.NewRouter()
	r.Post("/api/projects/{id}/conversations", s.handleCreateConversation)
	r.Get("/api/conversations/{id}/artifacts", s.handleListArtifacts)
	r.Get("/api/artifacts/{id}", s.handleGetArtifact)
	r.Get("/api/artifacts/{id}/versions/{v}", s.handleGetArtifact)
	r.Get("/api/artifacts/{id}/export", s.handleExportArtifact)
	r.Post("/api/artifacts/{id}/variants", s.handleArtifactVariants)
	return s, r
}

func newTestProject(t *testing.T, s *Server, owner store.User, kind string) store.Project {
	t.Helper()
	p, err := s.DB.CreateProject(context.Background(), store.CreateProjectParams{OwnerID: owner.ID, Name: "t-" + kind, Kind: kind, Settings: json.RawMessage("{}")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.DB.Pool.Exec(context.Background(), "DELETE FROM projects WHERE id = $1", p.ID) })
	return p
}

func send(h http.Handler, method, path, body string, p *auth.Principal) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withPrincipal(httptest.NewRequest(method, path, strings.NewReader(body)), p))
	return rec
}

// TestCreateConversationMode: a conversation takes the project's kind as
// its mode unless the body names one, and an images project falls back to
// chat. The Media page's "use in design" relies on a design-mode
// conversation being creatable in any project the caller can reach.
func TestCreateConversationMode(t *testing.T) {
	s, h := designTestServer(t)
	u := newTestUser(t, s, "member")
	other := newTestUser(t, s, "member")
	me := principalOf(u)

	mode := func(projectKind, body string) string {
		t.Helper()
		p := newTestProject(t, s, u, projectKind)
		rec := send(h, "POST", "/api/projects/"+p.ID.String()+"/conversations", body, me)
		if rec.Code != 201 {
			t.Fatalf("%s %s: %d %s", projectKind, body, rec.Code, rec.Body)
		}
		var c store.Conversation
		if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		return c.Mode
	}
	if got := mode("design", `{}`); got != "design" {
		t.Errorf("design project default mode = %q", got)
	}
	if got := mode("images", `{}`); got != "chat" {
		t.Errorf("images project default mode = %q", got)
	}
	if got := mode("images", `{"mode":"design"}`); got != "design" {
		t.Errorf("images project, mode design = %q", got)
	}
	if got := mode("chat", `{"mode":"design"}`); got != "design" {
		t.Errorf("chat project, mode design = %q", got)
	}

	// Someone with no access to the project gets a 404, not a conversation.
	p := newTestProject(t, s, u, "design")
	if rec := send(h, "POST", "/api/projects/"+p.ID.String()+"/conversations", `{"mode":"design"}`, principalOf(other)); rec.Code != 404 {
		t.Errorf("other user: %d", rec.Code)
	}
}

// TestDesignArtifactHandlers: a design version created with a
// design_context comes back from the artifact routes with it, per
// version; the list shows the artifact; export is the raw document;
// another user sees none of it; variants without a gateway is a 503.
func TestDesignArtifactHandlers(t *testing.T) {
	s, h := designTestServer(t)
	ctx := context.Background()
	u := newTestUser(t, s, "member")
	other := newTestUser(t, s, "member")
	me, them := principalOf(u), principalOf(other)
	p := newTestProject(t, s, u, "design")
	conv, err := s.DB.CreateConversation(ctx, store.CreateConversationParams{ProjectID: p.ID, UserID: u.ID, Mode: "design", ModelSelector: "auto", Settings: json.RawMessage("{}")})
	if err != nil {
		t.Fatal(err)
	}

	v1ctx := `{"library":"tailwind","colors":{"primary":"#b85a1a"},"radius":"8px"}`
	ref, err := s.Artifacts.Create(ctx, conv.ID, artifacts.KindDesign, "Landing Page!", "", "<html>one</html>", json.RawMessage(v1ctx), uuid.NullUUID{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Artifacts.Update(ctx, uuid.MustParse(ref.ArtifactID), "", "<html>two</html>", json.RawMessage(`{"library":"plain"}`), uuid.NullUUID{}); err != nil {
		t.Fatal(err)
	}
	base := "/api/artifacts/" + ref.ArtifactID

	type got struct {
		Version       int             `json:"version"`
		Kind          string          `json:"kind"`
		Title         string          `json:"title"`
		Content       string          `json:"content"`
		URL           string          `json:"url"`
		DesignContext json.RawMessage `json:"design_context"`
		Versions      []struct {
			Version int `json:"version"`
		} `json:"versions"`
	}
	fetch := func(path string) got {
		t.Helper()
		rec := send(h, "GET", path, "", me)
		if rec.Code != 200 {
			t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body)
		}
		var g got
		if err := json.Unmarshal(rec.Body.Bytes(), &g); err != nil {
			t.Fatal(err)
		}
		return g
	}

	cur := fetch(base)
	if cur.Version != 2 || cur.Kind != "design" || cur.Content != "<html>two</html>" || len(cur.Versions) != 2 {
		t.Errorf("current = %+v", cur)
	}
	if !strings.Contains(string(cur.DesignContext), `"plain"`) || !strings.HasPrefix(cur.URL, "http://art.test/a/") {
		t.Errorf("current context = %s, url = %s", cur.DesignContext, cur.URL)
	}
	// Each version answers with the context it was made under, by path and by query.
	for _, path := range []string{base + "/versions/1", base + "?version=1"} {
		v1 := fetch(path)
		if v1.Version != 1 || v1.Content != "<html>one</html>" || !strings.Contains(string(v1.DesignContext), "#b85a1a") {
			t.Errorf("%s = %+v / %s", path, v1, v1.DesignContext)
		}
	}
	if rec := send(h, "GET", base+"/versions/7", "", me); rec.Code != 404 {
		t.Errorf("missing version: %d", rec.Code)
	}

	// A non-design artifact has no design_context in the response.
	md, err := s.Artifacts.Create(ctx, conv.ID, artifacts.KindMarkdown, "Notes", "", "# hi", nil, uuid.NullUUID{})
	if err != nil {
		t.Fatal(err)
	}
	if g := fetch("/api/artifacts/" + md.ArtifactID); len(g.DesignContext) != 0 && string(g.DesignContext) != "null" {
		t.Errorf("markdown design_context = %s", g.DesignContext)
	}

	// The conversation lists both.
	rec := send(h, "GET", "/api/conversations/"+conv.ID.String()+"/artifacts", "", me)
	var list []struct {
		ID   string `json:"id"`
		Kind string `json:"kind"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &list) != nil || len(list) != 2 {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	kinds := map[string]bool{}
	for _, a := range list {
		kinds[a.Kind] = true
	}
	if !kinds["design"] || !kinds["markdown"] {
		t.Errorf("list kinds = %v", kinds)
	}

	// Export is the raw document of the asked version, as an HTML download.
	rec = send(h, "GET", base+"/export?version=1", "", me)
	if rec.Code != 200 || rec.Body.String() != "<html>one</html>" {
		t.Errorf("export: %d %q", rec.Code, rec.Body)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != `attachment; filename="landing-page-v1.html"` {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Errorf("Content-Type = %q", rec.Header().Get("Content-Type"))
	}

	// No gateway: variants say so; a stranger sees nothing anywhere.
	if rec := send(h, "POST", base+"/variants", `{"n":2}`, me); rec.Code != 503 {
		t.Errorf("variants without gateway: %d", rec.Code)
	}
	for _, c := range []struct{ method, path string }{
		{"GET", base}, {"GET", base + "/versions/1"}, {"GET", base + "/export"},
		{"POST", base + "/variants"}, {"GET", "/api/conversations/" + conv.ID.String() + "/artifacts"},
	} {
		if rec := send(h, c.method, c.path, `{}`, them); rec.Code != 404 {
			t.Errorf("%s %s as a stranger: %d", c.method, c.path, rec.Code)
		}
	}
	if rec := send(h, "GET", "/api/artifacts/not-a-uuid", "", me); rec.Code != 400 {
		t.Errorf("bad id: %d", rec.Code)
	}
}
