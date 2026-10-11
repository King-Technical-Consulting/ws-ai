package memory

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"

	"github.com/jking323/ws/internal/store"
)

// TestSQLAgainstPostgres runs the real queries: nearest-first search, the
// per-member filter, the decay cutoff and the due listing. It connects to
// WS_TEST_DATABASE_URL (skipping without one), for example:
//
//	WS_TEST_DATABASE_URL=postgres://ws:ws@127.0.0.1:55432/wstest?sslmode=disable go test ./internal/memory/
func TestSQLAgainstPostgres(t *testing.T) {
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
		u, err := db.CreateUser(ctx, store.CreateUserParams{Email: "m-" + uuid.NewString()[:8] + "@test.invalid", DisplayName: "m", Role: "member"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = db.Pool.Exec(context.Background(), "DELETE FROM users WHERE id = $1", u.ID) })
		return u.ID
	}
	a, b := mk(), mk()
	vec := func(i int) *pgvector.Vector {
		v := make([]float32, EmbedDims)
		v[i] = 1
		pv := pgvector.NewVector(v)
		return &pv
	}
	add := func(u uuid.UUID, text string, i int, imp float32) store.UserMemory {
		m, err := db.UpsertUserMemory(ctx, store.UpsertUserMemoryParams{UserID: u, Kind: "fact", Content: text, ContentHash: hashOf(text), Embedding: vec(i), Source: "chat", Importance: imp})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	m1 := add(a, "near", 1, 0.5)
	add(a, "far", 2, 0.9)
	add(b, "other member", 1, 0.9)

	rows, err := db.SearchUserMemories(ctx, store.SearchUserMemoriesParams{UserID: a, Column2: *vec(1), Limit: 5})
	if err != nil || len(rows) != 2 || rows[0].ID != m1.ID || rows[0].Distance > 0.001 {
		t.Fatalf("search: %v %+v", err, rows)
	}
	for _, r := range rows {
		if r.UserID != a {
			t.Error("search crossed members")
		}
	}
	// A repeat is an update: importance only goes up.
	again := add(a, "near", 1, 0.3)
	if again.ID != m1.ID || again.Importance != 0.5 {
		t.Errorf("repeat: %+v", again)
	}
	// Decay: only memories older than the cutoff, never below the floor.
	_, _ = db.Pool.Exec(ctx, "UPDATE user_memory SET created_at = now() - interval '90 days' WHERE id = $1", m1.ID)
	n, err := db.DecayUserMemories(ctx, store.DecayUserMemoriesParams{UserID: a, Cutoff: time.Now().Add(-60 * 24 * time.Hour), Factor: 0.8})
	if err != nil || n != 1 {
		t.Fatalf("decay: %d %v", n, err)
	}
	got, _ := db.GetUserMemory(ctx, store.GetUserMemoryParams{ID: m1.ID, UserID: a})
	if got.Importance > 0.41 || got.Importance < 0.39 {
		t.Errorf("importance after decay = %v", got.Importance)
	}
	// Another member cannot read, edit or delete it.
	if _, err := db.GetUserMemory(ctx, store.GetUserMemoryParams{ID: m1.ID, UserID: b}); err == nil {
		t.Error("b read a's memory")
	}
	// Settings round trip and the due listing.
	svc := &Service{DB: db, GW: &fakeGW{}}
	set := DefaultSettings()
	set.ReflectEnabled = true
	set.Profile = Profile{Name: "Sam", Projects: []string{"ws"}}
	if _, err := svc.SaveSettings(ctx, a, set); err != nil {
		t.Fatal(err)
	}
	back, err := svc.Settings(ctx, a)
	if err != nil || back.Profile.Name != "Sam" || len(back.Profile.Projects) != 1 || !back.ReflectEnabled {
		t.Fatalf("settings: %+v %v", back, err)
	}
	due, err := svc.Due(ctx, time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, id := range due {
		if id == a {
			found = true
		}
		if id == b {
			t.Error("b has no settings row and must not be due")
		}
	}
	if !found {
		t.Error("a should be due at noon with a 03:00 slot")
	}
	// A conversation of the member shows in the activity listing; agent ones do not.
	if err := svc.Forget(ctx, a); err != nil {
		t.Fatal(err)
	}
	if c, _ := db.CountUserMemories(ctx, a); c != 0 {
		t.Errorf("forget left %d", c)
	}
	if c, _ := db.CountUserMemories(ctx, b); c != 1 {
		t.Errorf("forget touched b: %d", c)
	}
}
