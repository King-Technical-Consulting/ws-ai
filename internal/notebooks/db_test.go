package notebooks

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/agent"
	"github.com/jking323/ws/internal/docread"
	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
	"github.com/jking323/ws/internal/store/blob"
)

// embedByFirstByte puts a text at the dimension of its first letter, so a
// query "zebra" lands on the chunk that starts with "zebra".
type embedByFirstByte struct{}

func (embedByFirstByte) Embed(_ context.Context, r *gateway.EmbedRequest) (*gateway.EmbedResponse, error) {
	out := make([][]float32, len(r.Inputs))
	for i, t := range r.Inputs {
		v := make([]float32, EmbedDims)
		v[int(strings.ToLower(strings.TrimSpace(t))[0])] = 1
		out[i] = v
	}
	return &gateway.EmbedResponse{Vectors: out}, nil
}

// TestNotebooksAgainstPostgres runs the service over the real queries. It
// connects to WS_TEST_DATABASE_URL (skipping without one):
//
//	WS_TEST_DATABASE_URL=postgres://ws:ws@127.0.0.1:55432/wstest?sslmode=disable go test -p 1 ./internal/notebooks/
func TestNotebooksAgainstPostgres(t *testing.T) {
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
		u, err := db.CreateUser(ctx, store.CreateUserParams{Email: "n-" + uuid.NewString()[:8] + "@test.invalid", DisplayName: "n", Role: "member"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = db.Pool.Exec(context.Background(), "DELETE FROM users WHERE id = $1", u.ID) })
		return u.ID
	}
	a, b := mk(), mk()
	blobs, _ := blob.NewFS(t.TempDir())
	svc := &Service{DB: db, Blobs: blobs, Reader: &docread.Reader{}, GW: embedByFirstByte{},
		Fetch: func(_ context.Context, u string) (string, error) {
			if strings.Contains(u, "private") {
				return "", errors.New("refused: private address")
			}
			return "zebra page. Stripes are black and white.", nil
		}}

	nb, err := svc.Create(ctx, a, "Zoology")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, a, "  "); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty name: %v", err)
	}

	txt, err := svc.AddText(ctx, a, nb.ID, "", "okapi facts: the okapi is a relative of the giraffe.\nIt lives in the Ituri forest.")
	if err != nil || txt.Kind != KindText || txt.Title == "" {
		t.Fatalf("text: %+v %v", txt, err)
	}
	file, err := svc.AddFile(ctx, a, nb.ID, "text/plain", "notes.txt", []byte("zebra notes: stripes are unique to each animal."))
	if err != nil || file.Kind != KindDocument || !file.AttachmentID.Valid {
		t.Fatalf("file: %+v %v", file, err)
	}
	page, err := svc.AddURL(ctx, a, nb.ID, "https://example.com/zebras")
	if err != nil || page.Kind != KindPage || page.URL == "" {
		t.Fatalf("page: %+v %v", page, err)
	}
	if _, err := svc.AddURL(ctx, a, nb.ID, "http://private.example/x"); !errors.Is(err, ErrInvalid) {
		t.Errorf("a refused fetch should be a plain error: %v", err)
	}
	if _, err := svc.AddURL(ctx, a, nb.ID, "ftp://example.com/x"); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad scheme: %v", err)
	}
	if _, err := svc.AddFile(ctx, a, nb.ID, "application/zip", "x.zip", []byte("PK")); !errors.Is(err, ErrInvalid) {
		t.Errorf("unsupported file: %v", err)
	}

	// A conversation of the member's is a source; another member's is refused.
	proj, _ := db.CreateProject(ctx, store.CreateProjectParams{OwnerID: a, Name: "p", Kind: "chat", Settings: json.RawMessage("{}")})
	conv, _ := db.CreateConversation(ctx, store.CreateConversationParams{ProjectID: proj.ID, UserID: a, Title: "Earlier chat", Mode: "chat", ModelSelector: "auto", Settings: json.RawMessage("{}")})
	parts, _ := json.Marshal([]gateway.Part{gateway.TextPart("tell me about quokkas")})
	_, _ = db.InsertMessage(ctx, store.InsertMessageParams{ConversationID: conv.ID, Seq: 1, Role: "user", Parts: parts})
	cs, err := svc.AddConversation(ctx, a, nb.ID, conv.ID)
	if err != nil || cs.Kind != KindConversation || cs.Title != "Earlier chat" {
		t.Fatalf("conversation: %+v %v", cs, err)
	}
	if _, err := svc.AddConversation(ctx, b, mustNotebook(t, svc, b), conv.ID); !errors.Is(err, ErrInvalid) {
		t.Errorf("another member's conversation: %v", err)
	}

	// The notebook's context names every source and puts the nearest chunk
	// first, marked with its source id.
	blk, err := svc.Context(ctx, a, nb.ID, "zebra stripes")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{txt.ID, file.ID, page.ID, cs.ID} {
		if !strings.Contains(blk, id.String()) {
			t.Errorf("source %s missing from the context", id)
		}
	}
	if !strings.Contains(blk, "src: <source id>") || !strings.Contains(blk, "Passages most relevant") {
		t.Errorf("instructions or passages missing:\n%s", blk)
	}
	if len(blk) > ContextChars+2000 {
		t.Errorf("context is %d characters", len(blk))
	}
	first := strings.Index(blk, "[source ")
	if first < 0 || !strings.Contains(blk[first:first+260], "zebra") {
		t.Errorf("the nearest passage should be about zebras:\n%s", blk[first:min(first+300, len(blk))])
	}

	// The model's notes: summary is replaced in place, notes accumulate.
	conversationID, err := svc.Chat(ctx, a, nb.ID)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := svc.Chat(ctx, a, nb.ID)
	if again != conversationID {
		t.Error("the notebook has one chat")
	}
	tool := &Tool{Svc: svc}
	call := func(user uuid.UUID, args string) agent.Result {
		r, _ := tool.Call(ctx, agent.ToolCtx{UserID: user, ConversationID: conversationID}, json.RawMessage(args))
		return r
	}
	if r := call(a, `{"kind":"summary","text":"First summary"}`); r.IsError {
		t.Fatalf("summary: %+v", r)
	}
	if r := call(a, `{"kind":"summary","text":"Second summary"}`); r.IsError {
		t.Fatalf("summary again: %+v", r)
	}
	if r := call(a, `{"kind":"open_questions","text":"What about quokkas?"}`); r.IsError {
		t.Fatal("open questions")
	}
	if r := call(a, `{"kind":"note","title":"T","text":"a note"}`); r.IsError {
		t.Fatal("note")
	}
	if r := call(a, `{"kind":"mood","text":"x"}`); !r.IsError {
		t.Error("an unknown kind should be refused")
	}
	if r := call(b, `{"kind":"summary","text":"hijack"}`); !r.IsError {
		t.Error("another member must not write to this notebook through its chat")
	}
	det, err := svc.Get(ctx, a, nb.ID)
	if err != nil {
		t.Fatal(err)
	}
	var summaries, notes int
	for _, n := range det.Notes {
		switch n.Kind {
		case NoteSummary:
			summaries++
			if n.Body != "Second summary" || n.Author != "model" {
				t.Errorf("summary = %+v", n)
			}
		case NoteNote:
			notes++
		}
	}
	if summaries != 1 || notes != 1 || len(det.Sources) != 4 {
		t.Errorf("summaries %d notes %d sources %d", summaries, notes, len(det.Sources))
	}

	// Scoping: nothing is reachable by another member.
	if _, err := svc.Get(ctx, b, nb.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("get: %v", err)
	}
	if _, err := svc.Source(ctx, b, nb.ID, txt.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("source: %v", err)
	}
	if err := svc.DeleteSource(ctx, b, nb.ID, txt.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete source: %v", err)
	}
	if _, err := svc.AddNote(ctx, b, nb.ID, "", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("add note: %v", err)
	}
	if blk, _ := svc.Context(ctx, b, nb.ID, "zebra"); blk != "" {
		t.Errorf("another member got a context: %.60q", blk)
	}
	if _, err := svc.Chat(ctx, b, nb.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("chat: %v", err)
	}
	if id, ok := svc.NotebookForConversation(ctx, b, json.RawMessage(`{"notebook_id":"`+nb.ID.String()+`"}`)); ok || id != uuid.Nil {
		t.Error("a settings blob naming another member's notebook must not resolve")
	}

	// Source text reads back by page; deleting a source takes its chunks.
	st, err := svc.Source(ctx, a, nb.ID, file.ID)
	if err != nil || len(st.Chunks) != 1 || !strings.Contains(st.Chunks[0].Text, "zebra notes") {
		t.Errorf("source text: %+v %v", st, err)
	}
	if err := svc.DeleteSource(ctx, a, nb.ID, file.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = db.Pool.QueryRow(ctx, "SELECT count(*) FROM notebook_chunks WHERE source_id = $1", file.ID).Scan(&n)
	if n != 0 {
		t.Errorf("%d chunks left", n)
	}
	// Deleting the notebook leaves its chat.
	if err := svc.Delete(ctx, a, nb.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetConversation(ctx, conversationID); err != nil {
		t.Errorf("the chat should remain: %v", err)
	}
}

func mustNotebook(t *testing.T, svc *Service, user uuid.UUID) uuid.UUID {
	t.Helper()
	nb, err := svc.Create(context.Background(), user, "Other")
	if err != nil {
		t.Fatal(err)
	}
	return nb.ID
}
