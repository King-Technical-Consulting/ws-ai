package purge

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/store"
	"github.com/jking323/ws/internal/store/blob"
)

// TestPurgeAgainstPostgres runs archived-conversation and project deletion
// through the real schema. It connects to WS_TEST_DATABASE_URL (skipping
// without one):
//
//	WS_TEST_DATABASE_URL=postgres://ws:ws@127.0.0.1:55432/wstest?sslmode=disable go test -p 1 ./internal/purge/
func TestPurgeAgainstPostgres(t *testing.T) {
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
	blobs, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pg := &Purger{Pool: db.Pool, Blobs: blobs}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", strings.Fields(sql)[0]+" "+strings.Fields(sql)[2], err)
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
	put := func(s string) string {
		t.Helper()
		k, _, err := blobs.Put(ctx, bytes.NewReader([]byte(s)))
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	has := func(k string) bool {
		_, err := blobs.Stat(ctx, k)
		return err == nil
	}

	user := one(`INSERT INTO users (email, display_name, role) VALUES ($1, 'p', 'member') RETURNING id`, "p-"+uuid.NewString()[:8]+"@test.invalid")
	t.Cleanup(func() { _, _ = db.Pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, user) })
	proj := one(`INSERT INTO projects (owner_id, name, kind) VALUES ($1, 'Purge test', 'chat') RETURNING id`, user)
	conv := func(archived bool) uuid.UUID {
		id := one(`INSERT INTO conversations (project_id, user_id, title) VALUES ($1, $2, 'c') RETURNING id`, proj, user)
		if archived {
			exec(`UPDATE conversations SET archived_at = now() WHERE id = $1`, id)
		}
		return id
	}
	msg := func(c uuid.UUID, seq int) uuid.UUID {
		return one(`INSERT INTO messages (conversation_id, seq, role, parts) VALUES ($1, $2, 'user', '[{"type":"text","text":"hello hello hello"}]') RETURNING id`, c, seq)
	}
	att := func(key string, size int) uuid.UUID {
		return one(`INSERT INTO attachments (user_id, blob_key, mime, bytes, sha256) VALUES ($1, $2, 'text/plain', $3, '\x00') RETURNING id`, user, key, size)
	}
	link := func(m, a uuid.UUID) {
		exec(`INSERT INTO message_attachments (message_id, attachment_id) VALUES ($1, $2)`, m, a)
	}

	archived, live := conv(true), conv(false)
	mA, mLive := msg(archived, 1), msg(live, 1)

	kOwn, kShared, kOnlyHere, kArt := put("own bytes"), put("shared bytes"), put("kept by a notebook"), put("artifact blob")
	own := att(kOwn, 9) // only this conversation: goes, with its file
	link(mA, own)
	dup := att(kOwn, 9) // another row with the same bytes, in the live conversation: the file stays
	link(mLive, dup)
	shared := att(kShared, 12) // used by both conversations: row and file stay
	link(mA, shared)
	link(mLive, shared)
	nb := att(kOnlyHere, 18) // a notebook source points at it: stays
	link(mA, nb)
	nbook := one(`INSERT INTO notebooks (user_id, name) VALUES ($1, 'n') RETURNING id`, user)
	exec(`INSERT INTO notebook_sources (notebook_id, kind, title, attachment_id) VALUES ($1, 'document', 'd', $2)`, nbook, nb)
	nbConv := one(`INSERT INTO notebook_sources (notebook_id, kind, title, from_conversation_id) VALUES ($1, 'conversation', 'from chat', $2) RETURNING id`, nbook, archived)
	exec(`INSERT INTO documents (attachment_id, user_id, conversation_id, kind, method, pages, chars) VALUES ($1, $2, $3, 'text', 'text', 1, 9)`, own, user, archived)
	exec(`INSERT INTO document_chunks (attachment_id, seq, page, content) VALUES ($1, 0, 1, 'own bytes')`, own)

	art := one(`INSERT INTO artifacts (conversation_id, kind, title) VALUES ($1, 'html', 'a') RETURNING id`, archived)
	exec(`INSERT INTO artifact_versions (artifact_id, version, content, blob_key) VALUES ($1, 1, '<p>hi</p>', $2)`, art, kArt)
	exec(`INSERT INTO usage_ledger (conversation_id, user_id, cost_usd) VALUES ($1, $2, 0.5)`, archived, user)
	exec(`INSERT INTO favorites (user_id, kind, ref_id, conversation_id, project_id) VALUES ($1, 'message', $2, $3, $4)`, user, mA, archived, proj)
	exec(`INSERT INTO user_memory (user_id, kind, content, content_hash, source_conversation_id) VALUES ($1, 'fact', 'likes tea', '\x01', $2)`, user, archived)
	run := one(`INSERT INTO agent_runs (conversation_id, user_id, status) VALUES ($1, $2, 'running') RETURNING id`, archived, user)
	exec(`UPDATE usage_ledger SET agent_run_id = $1 WHERE conversation_id = $2`, run, archived)

	// An approved skill file with the same bytes as an attachment keeps the blob.
	kSkill := put("skill file bytes")
	skillAtt := att(kSkill, 16)
	link(mA, skillAtt)
	exec(`INSERT INTO skills (owner_id, name, title, instructions, files) VALUES ($1, 's', 's', 'x', $2::jsonb)`, user,
		`[{"path":"scripts/x.sh","sha256":"`+strings.TrimPrefix(kSkill, "sha256/")+`"}]`)

	// Not archived: refused, nothing changes.
	if _, err := pg.DeleteConversations(ctx, []uuid.UUID{live}); !errors.Is(err, ErrNotArchived) {
		t.Fatalf("live conversation: %v", err)
	}
	// An open run: refused.
	if _, err := pg.DeleteConversations(ctx, []uuid.UUID{archived}); !errors.Is(err, ErrBusy) {
		t.Fatalf("open run: %v", err)
	}
	exec(`UPDATE agent_runs SET status = 'done' WHERE id = $1`, run)

	pl, err := pg.PlanConversations(ctx, []uuid.UUID{archived})
	if err != nil {
		t.Fatal(err)
	}
	// Only the attachment nothing else uses frees space: the shared one,
	// the notebook's one and the same-bytes twin stay.
	if pl.Attachments != 2 || pl.Messages != 1 || pl.Artifacts != 1 || pl.Runs != 1 || pl.Bytes < 9+9+16 {
		t.Fatalf("plan = %+v", pl)
	}

	rep, err := pg.DeleteConversations(ctx, []uuid.UUID{archived})
	if err != nil {
		t.Fatal(err)
	}
	if rep.BlobsFailed != 0 {
		t.Fatalf("report = %+v", rep)
	}
	if count(`SELECT count(*) FROM conversations WHERE id = $1`, archived) != 0 || count(`SELECT count(*) FROM messages WHERE conversation_id = $1`, archived) != 0 {
		t.Fatal("conversation or messages remain")
	}
	if count(`SELECT count(*) FROM artifacts WHERE id = $1`, art) != 0 || count(`SELECT count(*) FROM artifact_versions WHERE artifact_id = $1`, art) != 0 {
		t.Fatal("artifact remains")
	}
	if count(`SELECT count(*) FROM agent_runs WHERE conversation_id = $1`, archived) != 0 {
		t.Fatal("agent runs remain")
	}
	if count(`SELECT count(*) FROM attachments WHERE id = $1`, own) != 0 || count(`SELECT count(*) FROM documents WHERE attachment_id = $1`, own) != 0 || count(`SELECT count(*) FROM document_chunks WHERE attachment_id = $1`, own) != 0 {
		t.Fatal("the attachment, its document or its chunks remain")
	}
	if count(`SELECT count(*) FROM attachments WHERE id = ANY($1)`, []uuid.UUID{dup, shared, nb}) != 3 {
		t.Fatal("an attachment still in use was deleted")
	}
	// Files: gone from disk where nothing uses them, kept where something does.
	if has(kArt) {
		t.Error("the artifact's blob is still on disk")
	}
	for name, k := range map[string]string{"an approved skill's file": kSkill, "same bytes in another attachment": kOwn, "shared with the live chat": kShared, "notebook source": kOnlyHere} {
		if !has(k) {
			t.Errorf("blob removed although in use (%s)", name)
		}
	}
	if rep.BlobsDeleted != 1 || rep.BlobsShared < 2 {
		t.Errorf("report = %+v", rep)
	}
	// The ledger keeps its row with the references cleared; the memory and
	// the notebook source stay with their link cleared; the favorite goes.
	if n := count(`SELECT count(*) FROM usage_ledger WHERE user_id = $1 AND conversation_id IS NULL AND agent_run_id IS NULL`, user); n != 1 {
		t.Fatalf("ledger rows with references cleared = %d", n)
	}
	if count(`SELECT count(*) FROM user_memory WHERE user_id = $1 AND source_conversation_id IS NULL`, user) != 1 {
		t.Fatal("memory was removed or still points at the conversation")
	}
	if count(`SELECT count(*) FROM notebook_sources WHERE id = $1 AND from_conversation_id IS NULL`, nbConv) != 1 {
		t.Fatal("the notebook source from the conversation was removed")
	}
	if count(`SELECT count(*) FROM favorites WHERE user_id = $1 AND conversation_id IS NULL AND kind = 'message'`, user) != 0 || count(`SELECT count(*) FROM favorites WHERE user_id = $1`, user) != 0 {
		t.Fatal("a favorite of the deleted conversation remains")
	}
	if count(`SELECT count(*) FROM conversations WHERE id = $1`, live) != 1 {
		t.Fatal("the live conversation was deleted")
	}

	// A project: not archived is refused; archived goes with everything.
	extra := conv(false)
	mx := msg(extra, 1)
	kProj := put("project only")
	px := att(kProj, 12)
	link(mx, px)
	job := one(`INSERT INTO media_jobs (user_id, project_id, kind, status, output_attachment_ids) VALUES ($1, $2, 'image', 'done', ARRAY[$3]::uuid[]) RETURNING id`, user, proj, att(put("generated image"), 15))
	_ = job
	if _, err := pg.DeleteProject(ctx, proj); !errors.Is(err, ErrNotArchived) {
		t.Fatalf("live project: %v", err)
	}
	exec(`UPDATE projects SET archived_at = now() WHERE id = $1`, proj)
	ppl, err := pg.PlanProject(ctx, proj)
	if err != nil || ppl.Conversations != 2 {
		t.Fatalf("project plan = %+v, %v", ppl, err)
	}
	if _, err := pg.DeleteProject(ctx, proj); err != nil {
		t.Fatal(err)
	}
	if count(`SELECT count(*) FROM projects WHERE id = $1`, proj) != 0 || count(`SELECT count(*) FROM conversations WHERE project_id = $1`, proj) != 0 {
		t.Fatal("project or its conversations remain")
	}
	if count(`SELECT count(*) FROM media_jobs WHERE project_id = $1`, proj) != 0 {
		t.Fatal("media jobs remain")
	}
	// Its own files are gone; the notebook's file stays (a notebook is the
	// member's, not the project's).
	if has(kProj) || has(kShared) || has(kOwn) {
		t.Error("project files remain on disk")
	}
	if !has(kOnlyHere) {
		t.Error("a notebook source's file was removed with the project")
	}
}
