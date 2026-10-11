package documents

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/docread"
	"github.com/jking323/ws/internal/store"
	"github.com/jking323/ws/internal/store/blob"
)

// TestSQLAgainstPostgres runs ingest and read through the real queries:
// page-bound chunks, ordered paging, nearest-chunk search and the member
// scope. It connects to WS_TEST_DATABASE_URL (skipping without one):
//
//	WS_TEST_DATABASE_URL=postgres://ws:ws@127.0.0.1:55432/wstest?sslmode=disable go test -p 1 ./internal/documents/
//
// (-p 1 when several packages with such tests share the database: each
// migrates it, and two at once race.)
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
		u, err := db.CreateUser(ctx, store.CreateUserParams{Email: "d-" + uuid.NewString()[:8] + "@test.invalid", DisplayName: "d", Role: "member"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = db.Pool.Exec(context.Background(), "DELETE FROM users WHERE id = $1", u.ID) })
		return u.ID
	}
	a, b := mk(), mk()
	blobs, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{DB: db, Blobs: blobs, Reader: &docread.Reader{}, GW: fakeEmbed{}, InlineChars: 200}
	text := "zebra paragraph one.\n" + strings.Repeat("filler line about nothing.\n", 400)
	in, err := s.Ingest(ctx, a, uuid.NullUUID{}, "text/plain", "z.txt", []byte(text))
	if err != nil {
		t.Fatal(err)
	}
	if in.Pages < 2 {
		t.Fatalf("pages = %d", in.Pages)
	}
	p2, err := s.Read(ctx, a, in.AttachmentID, "2", "")
	if err != nil || !strings.HasPrefix(p2.Text, "[page 2]") {
		t.Fatalf("page 2: %v %.60q", err, p2.Text)
	}
	hit, err := s.Read(ctx, a, in.AttachmentID, "", "zebra")
	if err != nil || !strings.Contains(hit.Text, "zebra paragraph one") {
		t.Fatalf("search: %v %.80q", err, hit.Text)
	}
	if _, err := s.Read(ctx, b, in.AttachmentID, "", ""); err != ErrNotFound {
		t.Errorf("another member: %v", err)
	}
	// Deleting the attachment takes the document and its chunks with it.
	if _, err := db.Pool.Exec(ctx, "DELETE FROM attachments WHERE id = $1", in.AttachmentID); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = db.Pool.QueryRow(ctx, "SELECT count(*) FROM document_chunks WHERE attachment_id = $1", in.AttachmentID).Scan(&n)
	if n != 0 {
		t.Errorf("%d chunks left behind", n)
	}
}
