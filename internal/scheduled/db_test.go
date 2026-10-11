package scheduled

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/agent"
	"github.com/jking323/ws/internal/store"
)

// TestClaimAgainstPostgres runs the real queries: two workers racing for
// one due prompt, and a disabled member's prompt left alone. It connects to
// WS_TEST_DATABASE_URL (skipping without one), for example:
//
//	WS_TEST_DATABASE_URL=postgres://ws:ws@127.0.0.1:55432/wstest?sslmode=disable go test -p 1 ./internal/scheduled/
func TestClaimAgainstPostgres(t *testing.T) {
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
	mk := func() (uuid.UUID, uuid.UUID) {
		u, err := db.CreateUser(ctx, store.CreateUserParams{Email: "s-" + uuid.NewString()[:8] + "@test.invalid", DisplayName: "s", Role: "member"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = db.Pool.Exec(context.Background(), "DELETE FROM users WHERE id = $1", u.ID) })
		p, err := db.CreateProject(ctx, store.CreateProjectParams{OwnerID: u.ID, Name: "p", Kind: "chat", Settings: json.RawMessage("{}")})
		if err != nil {
			t.Fatal(err)
		}
		return u.ID, p.ID
	}
	user, proj := mk()
	other, otherProj := mk()
	rt := agent.New(db, nil, agent.NewRegistry(), nil, nil)
	var enq int32
	svc := &Service{DB: db, Runs: rt, Enqueue: func(context.Context, uuid.UUID) error { atomic.AddInt32(&enq, 1); return nil }}
	at := time.Now().Add(time.Hour)
	mkSched := func(u, p uuid.UUID) *Schedule {
		s, err := svc.Create(ctx, u, Spec{ProjectID: p, Prompt: "hello", Kind: KindOnce, RunAt: &at, Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = db.Pool.Exec(ctx, "UPDATE scheduled_prompts SET next_run_at = now() - interval '1 minute' WHERE id = $1", s.ID)
		return s
	}
	s := mkSched(user, proj)
	off := mkSched(other, otherProj)
	_, _ = db.Pool.Exec(ctx, "UPDATE users SET disabled_at = now() WHERE id = $1", other)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = svc.Tick(ctx)
		}()
	}
	wg.Wait()
	if n := atomic.LoadInt32(&enq); n != 1 {
		t.Errorf("four racing ticks fired %d runs, want 1", n)
	}
	row, _ := db.GetScheduledPromptByID(ctx, s.ID)
	if row.NextRunAt != nil || row.LastStatus == "" {
		t.Errorf("row after firing: next=%v status=%q", row.NextRunAt, row.LastStatus)
	}
	hist, _ := svc.History(ctx, user, s.ID, 10)
	if len(hist) != 1 || !hist[0].RunID.Valid {
		t.Errorf("history = %+v", hist)
	}
	offRow, _ := db.GetScheduledPromptByID(ctx, off.ID)
	if offRow.NextRunAt == nil || offRow.LastStatus != "" {
		t.Errorf("a disabled member's prompt fired: %+v", offRow)
	}
	// Deleting the conversation or the schedule leaves no orphans to trip over.
	if err := svc.Delete(ctx, user, s.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = db.Pool.QueryRow(ctx, "SELECT count(*) FROM scheduled_prompt_runs WHERE prompt_id = $1", s.ID).Scan(&n)
	if n != 0 {
		t.Errorf("%d runs left behind", n)
	}
}
