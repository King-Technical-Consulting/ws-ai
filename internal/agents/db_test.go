package agents

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/store"
	"github.com/jking323/ws/internal/store/blob"
)

// TestSkillShelfAgainstPostgres runs the shelf's queries and the migration
// that moves skills approved before the shelf (agents.skills, 0033) onto
// their owner's shelf. It connects to WS_TEST_DATABASE_URL (skipping
// without one) and steps the schema down and up once:
//
//	WS_TEST_DATABASE_URL=postgres://ws:ws@127.0.0.1:55432/wstest?sslmode=disable go test -p 1 ./internal/agents/
func TestSkillShelfAgainstPostgres(t *testing.T) {
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
	var ver int
	if err := db.Pool.QueryRow(ctx, `SELECT COALESCE(max(version_id), 0) FROM goose_db_version`).Scan(&ver); err != nil {
		t.Fatal(err)
	}
	const shelfVersion = 35 // the migration under test, 0035_skill_shelf
	if ver != shelfVersion {
		t.Skipf("schema is at %d, the shelf migration is the newest at %d: this test steps it down once", ver, shelfVersion)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	one := func(sql string, args ...any) uuid.UUID {
		t.Helper()
		var id uuid.UUID
		if err := db.Pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := db.Pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	owner := one(`INSERT INTO users (email, display_name, role) VALUES ($1, 's', 'member') RETURNING id`, "s-"+uuid.NewString()[:8]+"@test.invalid")
	t.Cleanup(func() { _, _ = db.Pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, owner) })

	// Step down: skills approved before the shelf live on agents.skills.
	if err := db.Migrate(ctx, "down"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Migrate(context.Background(), "up") })
	a1 := one(`INSERT INTO agents (owner_id, name) VALUES ($1, 'one') RETURNING id`, owner)
	a2 := one(`INSERT INTO agents (owner_id, name) VALUES ($1, 'two') RETURNING id`, owner)
	skill := func(name string) string {
		return `{"name":"` + name + `","title":"` + strings.ToUpper(name) + `","description":"d","instructions":"Do ` + name + `.","has_scripts":true,` +
			`"files":[{"path":"scripts/x.sh","kind":"script","size":3,"sha256":"aa"}],"refused_secrets":[],"findings":["f"],"warnings":[],` +
			`"approved_by":"` + owner.String() + `","approved_at":"2026-10-10T20:00:00Z"}`
	}
	exec(`UPDATE agents SET skills = $2::jsonb WHERE id = $1`, a1, `[`+skill("alpha")+`,`+skill("beta")+`]`)
	exec(`UPDATE agents SET skills = $2::jsonb WHERE id = $1`, a2, `[`+skill("alpha")+`]`)

	if err := db.Migrate(ctx, "up"); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT count(*) FROM skills WHERE owner_id = $1`, owner); n != 2 {
		t.Fatalf("shelf has %d skills, want alpha and beta once each", n)
	}
	if n := count(`SELECT count(*) FROM agent_skills WHERE agent_id = ANY($1)`, []uuid.UUID{a1, a2}); n != 3 {
		t.Fatalf("attachments = %d, want 3 (one agent has two, the other one)", n)
	}
	if n := count(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'agents' AND column_name = 'skills'`); n != 0 {
		t.Fatal("agents.skills should be gone")
	}

	// The shelf through the real queries.
	blobs, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st := &SkillStore{DB: db, Blobs: blobs}
	zip := makeSkillZip(t, map[string]string{
		"SKILL.md":        "---\nname: Gamma\ndescription: g\n---\nDo gamma.\n",
		"scripts/run.sh":  "#!/bin/bash\necho g\n",
		"references/n.md": "notes",
	})
	g, err := st.ApproveForAgent(ctx, a1, owner, zip)
	if err != nil {
		t.Fatal(err)
	}
	forAgent, err := st.ForAgent(ctx, a1)
	if err != nil || len(forAgent) != 3 {
		t.Fatalf("agent skills = %d, %v", len(forAgent), err)
	}
	shelf, err := st.Shelf(ctx, owner)
	if err != nil || len(shelf) != 3 {
		t.Fatalf("shelf = %d, %v", len(shelf), err)
	}
	for _, k := range shelf {
		if k.Instructions != "" {
			t.Error("the shelf listing carries instructions")
		}
		if k.Name == "alpha" && k.AgentCount != 2 {
			t.Errorf("alpha used by %d agents, want 2", k.AgentCount)
		}
	}
	pv, err := st.Preview(ctx, owner, zip)
	if err != nil || pv.Diff == nil || !pv.Diff.Same || len(pv.Agents) != 1 {
		t.Fatalf("preview of the same folder = %+v, %v", pv, err)
	}
	// Deleting from the shelf takes it off the agent; another member cannot.
	other := one(`INSERT INTO users (email, display_name, role) VALUES ($1, 'o', 'member') RETURNING id`, "o-"+uuid.NewString()[:8]+"@test.invalid")
	t.Cleanup(func() { _, _ = db.Pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, other) })
	_ = st.Delete(ctx, other, g.ID)
	if n := count(`SELECT count(*) FROM skills WHERE id = $1`, g.ID); n != 1 {
		t.Fatal("another member deleted a skill")
	}
	_ = st.Delete(ctx, owner, g.ID)
	if n := count(`SELECT count(*) FROM agent_skills WHERE skill_id = $1`, g.ID); n != 0 {
		t.Fatal("a deleted skill is still attached")
	}
	// An agent's deletion leaves the shelf alone.
	exec(`DELETE FROM agents WHERE id = ANY($1)`, []uuid.UUID{a1, a2})
	if n := count(`SELECT count(*) FROM skills WHERE owner_id = $1`, owner); n != 2 {
		t.Fatalf("shelf after deleting the agents = %d, want 2", n)
	}
}
