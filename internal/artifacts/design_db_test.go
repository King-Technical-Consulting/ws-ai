package artifacts

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
)

// designTestService connects to WS_TEST_DATABASE_URL (skipping without one),
// migrates it, and returns a service plus a fresh design-mode conversation
// that is removed with its user after the test. Example:
//
//	WS_TEST_DATABASE_URL=postgres://ws:ws@127.0.0.1:55432/wstest?sslmode=disable go test ./internal/artifacts/
func designTestService(t *testing.T) (*Service, uuid.UUID) {
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
	u, err := db.CreateUser(ctx, store.CreateUserParams{Email: "t-" + uuid.NewString()[:8] + "@test.invalid", DisplayName: "t", Role: "member"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Pool.Exec(context.Background(), "DELETE FROM users WHERE id = $1", u.ID) })
	p, err := db.CreateProject(ctx, store.CreateProjectParams{OwnerID: u.ID, Name: "design test", Kind: "design", Settings: json.RawMessage("{}")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Pool.Exec(context.Background(), "DELETE FROM projects WHERE id = $1", p.ID) })
	c, err := db.CreateConversation(ctx, store.CreateConversationParams{ProjectID: p.ID, UserID: u.ID, Mode: "design", ModelSelector: "auto", Settings: json.RawMessage("{}")})
	if err != nil {
		t.Fatal(err)
	}
	return &Service{DB: db, Secret: []byte("test-secret"), BaseURL: "http://art.test"}, c.ID
}

const testDesign = `{"library":"tailwind","colors":{"primary":"#b85a1a"},"radius":"8px"}`

// TestDesignContextStoredPerVersion: a design artifact keeps the
// design_context it was made under on each version; an update with none
// inherits the previous version's, an update with one replaces it, a bad
// one is refused, and other kinds drop it.
func TestDesignContextStoredPerVersion(t *testing.T) {
	s, conv := designTestService(t)
	ctx := context.Background()

	ref, err := s.Create(ctx, conv, KindDesign, "Landing", "", "<html>one</html>", json.RawMessage(testDesign), uuid.NullUUID{})
	if err != nil {
		t.Fatal(err)
	}
	if ref.Version != 1 || ref.Kind != KindDesign {
		t.Fatalf("ref = %+v", ref)
	}
	id := uuid.MustParse(ref.ArtifactID)

	// v2 without a context inherits v1's.
	if _, err := s.Update(ctx, id, "", "<html>two</html>", nil, uuid.NullUUID{}); err != nil {
		t.Fatal(err)
	}
	// v3 with a new context replaces it.
	if _, err := s.Update(ctx, id, "Landing 3", "<html>three</html>", json.RawMessage(`{"library":"plain","radius":"0"}`), uuid.NullUUID{}); err != nil {
		t.Fatal(err)
	}
	// A bad context is refused and adds no version.
	if _, err := s.Update(ctx, id, "", "<html>x</html>", json.RawMessage(`{"library":"bootstrap"}`), uuid.NullUUID{}); err == nil {
		t.Error("unknown library accepted on update")
	}
	if _, err := s.Create(ctx, conv, KindDesign, "Bad", "", "<html/>", json.RawMessage(`{"colour":"red"}`), uuid.NullUUID{}); err == nil {
		t.Error("unknown field accepted on create")
	}

	a, versions, cur, _, err := s.Get(ctx, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if a.CurrentVersion != 3 || len(versions) != 3 || a.Title != "Landing 3" {
		t.Fatalf("artifact = v%d, %d versions, %q", a.CurrentVersion, len(versions), a.Title)
	}
	if d, _ := ParseDesignContext(cur.DesignContext); d == nil || d.Library != "plain" || d.Radius != "0" {
		t.Errorf("v3 context = %s", cur.DesignContext)
	}
	_, _, v2, ref2, err := s.Get(ctx, id, 2)
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := ParseDesignContext(v2.DesignContext); d == nil || d.Library != "tailwind" || d.Colors["primary"] != "#b85a1a" {
		t.Errorf("v2 context = %s, want v1's inherited", v2.DesignContext)
	}
	if ref2.Version != 2 || v2.Content == nil || *v2.Content != "<html>two</html>" {
		t.Errorf("v2 = %+v", ref2)
	}
	if _, _, _, _, err := s.Get(ctx, id, 9); err == nil {
		t.Error("missing version found")
	}

	// A non-design kind drops a context it is handed.
	h, err := s.Create(ctx, conv, KindHTML, "Page", "", "<p>x</p>", json.RawMessage(testDesign), uuid.NullUUID{})
	if err != nil {
		t.Fatal(err)
	}
	_, _, hv, _, _ := s.Get(ctx, uuid.MustParse(h.ArtifactID), 0)
	if len(hv.DesignContext) != 0 {
		t.Errorf("html version kept a design context: %s", hv.DesignContext)
	}
}

// fakeCompleter answers each variant call from a script keyed by the
// variant number in the prompt ("Variant k of n").
type fakeCompleter struct {
	reply func(i int) (string, error)
}

func (f *fakeCompleter) Complete(_ context.Context, req *gateway.Request) (*gateway.Response, error) {
	i := 0
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			if k := strings.Index(p.Text, "Variant "); k >= 0 && k+8 < len(p.Text) {
				i = int(p.Text[k+8] - '0')
			}
		}
	}
	text, err := f.reply(i)
	if err != nil {
		return nil, err
	}
	return &gateway.Response{Parts: []gateway.Part{{Kind: gateway.PartText, Text: text}}}, nil
}

// TestVariantsStoreDesignArtifacts: each variant becomes a new design
// artifact in the same conversation carrying the original's design_context;
// a failed variant is reported while the others land; all failing is an
// error; a non-design kind is refused.
func TestVariantsStoreDesignArtifacts(t *testing.T) {
	s, conv := designTestService(t)
	ctx := context.Background()
	src, err := s.Create(ctx, conv, KindDesign, "Landing", "", "<html>base</html>", json.RawMessage(testDesign), uuid.NullUUID{})
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.MustParse(src.ArtifactID)

	gw := &fakeCompleter{reply: func(i int) (string, error) {
		if i == 2 {
			return "", errors.New("model down")
		}
		return "```html\n<!doctype html><html>variant " + string(rune('0'+i)) + "</html>\n```", nil
	}}
	refs, errs, err := s.Variants(ctx, gw, VariantParams{ArtifactID: id, N: 3, Instruction: "denser"})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 || len(errs) != 1 || !strings.Contains(errs[0].Error(), "variant 2") {
		t.Fatalf("refs = %d, errs = %v", len(refs), errs)
	}
	for _, r := range refs {
		if r.Kind != KindDesign || !strings.HasPrefix(r.Title, "Landing · variant ") || r.Version != 1 {
			t.Errorf("ref = %+v", r)
		}
		_, _, v, _, err := s.Get(ctx, uuid.MustParse(r.ArtifactID), 0)
		if err != nil {
			t.Fatal(err)
		}
		if d, _ := ParseDesignContext(v.DesignContext); d == nil || d.Library != "tailwind" {
			t.Errorf("variant lost the design context: %s", v.DesignContext)
		}
		if v.Content == nil || strings.Contains(*v.Content, "```") || !strings.Contains(*v.Content, "<html>variant") {
			t.Errorf("variant content = %v", v.Content)
		}
		if c, err := s.DB.GetArtifactConversation(ctx, uuid.MustParse(r.ArtifactID)); err != nil || c.ID != conv {
			t.Errorf("variant not in the source conversation: %v", err)
		}
	}
	// The source is untouched.
	if a, _, _, _, _ := s.Get(ctx, id, 0); a.CurrentVersion != 1 {
		t.Errorf("source version = %d", a.CurrentVersion)
	}

	allFail := &fakeCompleter{reply: func(int) (string, error) { return "no html here", nil }}
	if _, errs, err := s.Variants(ctx, allFail, VariantParams{ArtifactID: id, N: 2}); err == nil || len(errs) != 2 {
		t.Errorf("all failing: err = %v, errs = %v", err, errs)
	}
	if _, _, err := s.Variants(ctx, gw, VariantParams{ArtifactID: id, N: MaxVariants + 1}); err == nil {
		t.Error("too many variants accepted")
	}

	md, err := s.Create(ctx, conv, KindMarkdown, "Notes", "", "# hi", nil, uuid.NullUUID{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Variants(ctx, gw, VariantParams{ArtifactID: uuid.MustParse(md.ArtifactID), N: 1}); err == nil {
		t.Error("variants of a markdown artifact accepted")
	}
}
