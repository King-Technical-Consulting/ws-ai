package favorites

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

// TestFavoritesAgainstPostgres runs the service over the real queries. It
// connects to WS_TEST_DATABASE_URL (skipping without one):
//
//	WS_TEST_DATABASE_URL=postgres://ws:ws@127.0.0.1:55432/wstest?sslmode=disable go test -p 1 ./internal/favorites/
func TestFavoritesAgainstPostgres(t *testing.T) {
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
	mk := func() uuid.UUID {
		u, err := db.CreateUser(ctx, store.CreateUserParams{Email: "f-" + uuid.NewString()[:8] + "@test.invalid", DisplayName: "f", Role: "member"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = db.Pool.Exec(context.Background(), "DELETE FROM users WHERE id = $1", u.ID) })
		return u.ID
	}
	a, b := mk(), mk()
	proj, err := db.CreateProject(ctx, store.CreateProjectParams{OwnerID: a, Name: "p", Kind: "chat", Settings: json.RawMessage("{}")})
	if err != nil {
		t.Fatal(err)
	}
	conv, _ := db.CreateConversation(ctx, store.CreateConversationParams{ProjectID: proj.ID, UserID: a, Title: "Heat pumps", Mode: "chat", ModelSelector: "auto", Settings: json.RawMessage("{}")})
	parts, _ := json.Marshal([]gateway.Part{gateway.TextPart("A heat pump moves heat\n  instead   of making it.")})
	msg, err := db.InsertMessage(ctx, store.InsertMessageParams{ConversationID: conv.ID, Seq: 1, Role: "assistant", Parts: parts})
	if err != nil {
		t.Fatal(err)
	}
	art, err := db.CreateArtifact(ctx, store.CreateArtifactParams{ConversationID: conv.ID, Kind: "markdown", Title: "Notes"})
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{DB: db}

	f, err := svc.Add(ctx, a, KindMessage, msg.ID)
	if err != nil || f.Title != "Heat pumps" || f.Snippet != "A heat pump moves heat instead of making it." || !f.ConversationID.Valid {
		t.Fatalf("message favorite: %+v %v", f, err)
	}
	if again, err := svc.Add(ctx, a, KindMessage, msg.ID); err != nil || again.ID != f.ID {
		t.Errorf("starring twice should be one favorite: %+v %v", again, err)
	}
	if _, err := svc.Add(ctx, a, KindArtifact, art.ID); err != nil {
		t.Fatalf("artifact favorite: %v", err)
	}
	if _, err := svc.Add(ctx, a, "photo", uuid.New()); !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown kind: %v", err)
	}
	if _, err := svc.Add(ctx, a, KindMessage, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing message: %v", err)
	}
	// Another member cannot star what they cannot reach, and the answer does
	// not say whether it exists.
	if _, err := svc.Add(ctx, b, KindMessage, msg.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("another member's message: %v", err)
	}
	if _, err := svc.Add(ctx, b, KindArtifact, art.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("another member's artifact: %v", err)
	}
	if l, _ := svc.List(ctx, b); len(l) != 0 {
		t.Errorf("b lists %d favorites", len(l))
	}

	l, err := svc.List(ctx, a)
	if err != nil || len(l) != 2 {
		t.Fatalf("list: %d %v", len(l), err)
	}
	// A deleted target drops out of the list; so does a project the member
	// lost access to.
	if _, err := db.Pool.Exec(ctx, "DELETE FROM artifacts WHERE id = $1", art.ID); err != nil {
		t.Fatal(err)
	}
	l, _ = svc.List(ctx, a)
	if len(l) != 1 || l[0].Kind != KindMessage {
		t.Errorf("after deleting the artifact: %+v", l)
	}
	if _, err := db.Pool.Exec(ctx, "UPDATE projects SET owner_id = $2 WHERE id = $1", proj.ID, b); err != nil {
		t.Fatal(err)
	}
	if l, _ = svc.List(ctx, a); len(l) != 0 {
		t.Errorf("a favorite in a project the member can no longer reach is listed: %+v", l)
	}

	if err := svc.Remove(ctx, a, KindMessage, msg.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.Remove(ctx, a, KindMessage, msg.ID); err != nil {
		t.Errorf("removing twice: %v", err)
	}

	// Saved prompts.
	if _, err := svc.SavePrompt(ctx, a, "", "x"); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty name: %v", err)
	}
	if _, err := svc.SavePrompt(ctx, a, "two\nlines", "x"); !errors.Is(err, ErrInvalid) {
		t.Errorf("multi-line name: %v", err)
	}
	if _, err := svc.SavePrompt(ctx, a, "big", strings.Repeat("x", MaxPromptBytes+1)); !errors.Is(err, ErrInvalid) {
		t.Errorf("too long: %v", err)
	}
	p, err := svc.SavePrompt(ctx, a, "Weekly review", "Summarise my week")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdatePrompt(ctx, b, p.ID, "x", "y"); !errors.Is(err, ErrNotFound) {
		t.Errorf("another member updating: %v", err)
	}
	if err := svc.DeletePrompt(ctx, b, p.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.ListPrompts(ctx, a); len(got) != 1 {
		t.Errorf("another member's delete removed it: %d", len(got))
	}
	if got, _ := svc.ListPrompts(ctx, b); len(got) != 0 {
		t.Errorf("b lists %d prompts", len(got))
	}
	up, err := svc.UpdatePrompt(ctx, a, p.ID, "Weekly", "Summarise the week")
	if err != nil || up.Name != "Weekly" {
		t.Errorf("update: %+v %v", up, err)
	}
	for i := 0; i < MaxSavedPrompts; i++ {
		if _, err := svc.SavePrompt(ctx, a, "p", "t"); err != nil {
			if i+1 >= MaxSavedPrompts {
				break
			}
			t.Fatalf("prompt %d: %v", i, err)
		}
	}
	if _, err := svc.SavePrompt(ctx, a, "one more", "t"); !errors.Is(err, ErrInvalid) {
		t.Errorf("past the limit: %v", err)
	}
}
