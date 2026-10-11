package documents

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jking323/ws/internal/agent"
	"github.com/jking323/ws/internal/docread"
	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
)

type fakeDB struct {
	atts   map[uuid.UUID]store.Attachment
	docs   map[uuid.UUID]store.Document
	chunks map[uuid.UUID][]store.InsertDocumentChunkParams
}

func newFakeDB() *fakeDB {
	return &fakeDB{atts: map[uuid.UUID]store.Attachment{}, docs: map[uuid.UUID]store.Document{}, chunks: map[uuid.UUID][]store.InsertDocumentChunkParams{}}
}

func (f *fakeDB) CreateAttachment(_ context.Context, a store.CreateAttachmentParams) (store.Attachment, error) {
	at := store.Attachment{ID: uuid.New(), UserID: a.UserID, BlobKey: a.BlobKey, Mime: a.Mime, Bytes: a.Bytes, Filename: a.Filename}
	f.atts[at.ID] = at
	return at, nil
}
func (f *fakeDB) CreateDocument(_ context.Context, a store.CreateDocumentParams) (store.Document, error) {
	d := store.Document{AttachmentID: a.AttachmentID, UserID: a.UserID, Kind: a.Kind, Method: a.Method, Pages: a.Pages, Chars: a.Chars, Truncated: a.Truncated}
	f.docs[a.AttachmentID] = d
	return d, nil
}
func (f *fakeDB) GetDocument(_ context.Context, a store.GetDocumentParams) (store.Document, error) {
	d, ok := f.docs[a.AttachmentID]
	if !ok || d.UserID != a.UserID {
		return store.Document{}, pgx.ErrNoRows
	}
	return d, nil
}
func (f *fakeDB) DeleteDocumentChunks(_ context.Context, id uuid.UUID) error {
	delete(f.chunks, id)
	return nil
}
func (f *fakeDB) InsertDocumentChunk(_ context.Context, a store.InsertDocumentChunkParams) error {
	f.chunks[a.AttachmentID] = append(f.chunks[a.AttachmentID], a)
	return nil
}
func (f *fakeDB) ListDocumentChunksByPages(_ context.Context, a store.ListDocumentChunksByPagesParams) ([]store.ListDocumentChunksByPagesRow, error) {
	var out []store.ListDocumentChunksByPagesRow
	for _, c := range f.chunks[a.AttachmentID] {
		if c.Page >= a.FirstPage && c.Page <= a.LastPage {
			out = append(out, store.ListDocumentChunksByPagesRow{Seq: c.Seq, Page: c.Page, Content: c.Content})
		}
	}
	return out, nil
}
func (f *fakeDB) SearchDocumentChunks(_ context.Context, a store.SearchDocumentChunksParams) ([]store.SearchDocumentChunksRow, error) {
	var out []store.SearchDocumentChunksRow
	q := a.QueryEmbedding.Slice()
	for _, c := range f.chunks[a.AttachmentID] {
		if c.Embedding == nil {
			continue
		}
		v := c.Embedding.Slice()
		d := 1.0
		for i := range q {
			if q[i] != 0 && q[i] == v[i] {
				d = 0
			}
		}
		if d == 0 {
			out = append(out, store.SearchDocumentChunksRow{Seq: c.Seq, Page: c.Page, Content: c.Content, Distance: d})
		}
	}
	return out, nil
}

// fakeEmbed maps a text to a one-hot vector at its first byte, so a query
// "zebra" finds the chunk that starts with "zebra".
type fakeEmbed struct{ fail bool }

func (e fakeEmbed) Embed(_ context.Context, r *gateway.EmbedRequest) (*gateway.EmbedResponse, error) {
	if e.fail {
		return nil, errors.New("no embedding model")
	}
	out := make([][]float32, len(r.Inputs))
	for i, t := range r.Inputs {
		v := make([]float32, EmbedDims)
		v[int(strings.ToLower(strings.TrimSpace(t))[0])] = 1
		out[i] = v
	}
	return &gateway.EmbedResponse{Vectors: out}, nil
}

func newService(t *testing.T, emb Embedder) (*Service, *fakeDB) {
	t.Helper()
	db := newFakeDB()
	return &Service{DB: db, Blobs: nil, Reader: &docread.Reader{}, GW: emb, InlineChars: 300}, db
}

func (s *Service) ingestText(t *testing.T, user uuid.UUID, name, text string) *Ingested {
	t.Helper()
	doc, err := s.Reader.Extract(context.Background(), "text/plain", name, []byte(text))
	if err != nil {
		t.Fatal(err)
	}
	att, _ := s.DB.CreateAttachment(context.Background(), store.CreateAttachmentParams{UserID: user, Filename: name})
	_, _ = s.DB.CreateDocument(context.Background(), store.CreateDocumentParams{AttachmentID: att.ID, UserID: user, Kind: "text", Method: doc.Method, Pages: int32(len(doc.Pages)), Chars: int32(doc.Chars())})
	if err := s.storeChunks(context.Background(), user, att.ID, doc); err != nil {
		t.Fatal(err)
	}
	in := &Ingested{AttachmentID: att.ID, Name: name, Kind: "text", Method: doc.Method, Pages: len(doc.Pages), Chars: doc.Chars()}
	in.Text = s.render(in, doc)
	return in
}

func TestChunksStayOnOnePageAndCoverTheText(t *testing.T) {
	doc := &docread.Doc{Pages: []docread.Page{
		{N: 1, Text: strings.Repeat("alpha line\n", 300)},
		{N: 2, Text: "short page"},
		{N: 3, Text: "   "},
		{N: 4, Text: strings.Repeat("x", ChunkChars*2+5)},
	}}
	cs := ChunkDoc(doc)
	var p1, p4 int
	for _, c := range cs {
		if len(c.Text) > ChunkChars {
			t.Errorf("chunk of %d characters on page %d", len(c.Text), c.Page)
		}
		switch c.Page {
		case 1:
			p1 += len(strings.ReplaceAll(c.Text, "\n", ""))
		case 3:
			t.Error("a blank page produced a chunk")
		case 4:
			p4 += len(c.Text)
		}
	}
	if want := 300 * len("alpha line"); p1 != want {
		t.Errorf("page 1 text = %d, want %d", p1, want)
	}
	if p4 != ChunkChars*2+5 {
		t.Errorf("page 4 text = %d", p4)
	}
}

func TestRenderRespectsTheInlineBudget(t *testing.T) {
	s, _ := newService(t, nil)
	text := strings.Repeat("word ", 40) + "\n" // 201 bytes
	in := s.ingestText(t, uuid.New(), "long.txt", strings.Repeat(text, 60))
	if !strings.HasPrefix(in.Text, "[Attached document: long.txt") || !strings.Contains(in.Text, in.AttachmentID.String()) {
		t.Errorf("header: %.120q", in.Text)
	}
	if !strings.Contains(in.Text, "[page 1]") || !strings.Contains(in.Text, "Use read_document with attachment_id") {
		t.Errorf("markers or the pointer to the tool are missing: %.200q", in.Text)
	}
	if len(in.Text) > 300+500 {
		t.Errorf("inline text is %d characters for a budget of 300", len(in.Text))
	}
	short := s.ingestText(t, uuid.New(), "s.txt", "tiny")
	if strings.Contains(short.Text, "read_document") {
		t.Error("a document that fits should not point at the tool")
	}
}

func TestReadPagesAndScoping(t *testing.T) {
	s, _ := newService(t, nil)
	owner, other := uuid.New(), uuid.New()
	in := s.ingestText(t, owner, "a.txt", strings.Repeat("sentence about nothing in particular.\n", 400))
	ctx := context.Background()
	if in.Pages < 3 {
		t.Fatalf("test needs a few pages, got %d", in.Pages)
	}
	res, err := s.Read(ctx, owner, in.AttachmentID, "2", "")
	if err != nil || !strings.HasPrefix(res.Text, "[page 2]") || strings.Contains(res.Text, "[page 3]") {
		t.Errorf("page 2: %v %.80q", err, res.Text)
	}
	res, err = s.Read(ctx, owner, in.AttachmentID, "1-2", "")
	if err != nil || !strings.Contains(res.Text, "[page 1]") || !strings.Contains(res.Text, "[page 2]") {
		t.Errorf("range: %v", err)
	}
	if _, err := s.Read(ctx, owner, in.AttachmentID, "99", ""); err == nil {
		t.Error("a page past the end should say so")
	}
	if _, err := s.Read(ctx, owner, in.AttachmentID, "x-y", ""); err == nil {
		t.Error("a bad range should say so")
	}
	if _, err := s.Read(ctx, other, in.AttachmentID, "", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("another member reading: %v", err)
	}
	if _, err := s.Read(ctx, owner, uuid.New(), "", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id: %v", err)
	}
}

func TestReadCapsOutput(t *testing.T) {
	s, _ := newService(t, nil)
	u := uuid.New()
	in := s.ingestText(t, u, "big.txt", strings.Repeat("0123456789\n", 6000)) // 66 KB, many pages
	res, err := s.Read(context.Background(), u, in.AttachmentID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Text) > ReadChars+200 {
		t.Errorf("returned %d characters", len(res.Text))
	}
	if !strings.Contains(res.Text, "Cut at") {
		t.Error("a cut read should say so")
	}
}

func TestReadQuerySearchesAndFallsBack(t *testing.T) {
	s, _ := newService(t, fakeEmbed{})
	u := uuid.New()
	// One page, two lines: the first chunk starts with "zebra".
	in := s.ingestText(t, u, "z.txt", "zebra facts: stripes.")
	res, err := s.Read(context.Background(), u, in.AttachmentID, "", "zebra")
	if err != nil || !strings.Contains(res.Text, "[page 1]") || !strings.Contains(res.Text, "zebra facts") {
		t.Errorf("search: %v %q", err, res.Text)
	}
	// No embedding model: the pages are returned with a note, not an error.
	s2, _ := newService(t, fakeEmbed{fail: true})
	in2 := s2.ingestText(t, u, "z.txt", "zebra facts: stripes.")
	res, err = s2.Read(context.Background(), u, in2.AttachmentID, "", "zebra")
	if err != nil || !strings.Contains(res.Text, "unavailable") || !strings.Contains(res.Text, "zebra facts") {
		t.Errorf("fallback: %v %q", err, res.Text)
	}
}

func TestToolIsScopedToTheCaller(t *testing.T) {
	s, _ := newService(t, nil)
	owner := uuid.New()
	in := s.ingestText(t, owner, "a.txt", "private text")
	tool := &Tool{Svc: s}
	args, _ := json.Marshal(map[string]string{"attachment_id": in.AttachmentID.String()})
	res, _ := tool.Call(context.Background(), agent.ToolCtx{UserID: owner}, args)
	if res.IsError || !strings.Contains(res.Text, "private text") {
		t.Errorf("owner: %+v", res)
	}
	res, _ = tool.Call(context.Background(), agent.ToolCtx{UserID: uuid.New()}, args)
	if !res.IsError || strings.Contains(res.Text, "private text") {
		t.Errorf("other member got %+v", res)
	}
	res, _ = tool.Call(context.Background(), agent.ToolCtx{}, args)
	if !res.IsError {
		t.Error("no user should be refused")
	}
	res, _ = tool.Call(context.Background(), agent.ToolCtx{UserID: owner}, json.RawMessage(`{"attachment_id":"nope"}`))
	if !res.IsError {
		t.Error("a bad id should be an error")
	}
	if got := tool.Def().Name; got != ToolName || tool.DefaultPolicy() != agent.PolicyAuto {
		t.Errorf("def = %s policy = %s", got, tool.DefaultPolicy())
	}
}

func TestHandles(t *testing.T) {
	for name, want := range map[string]bool{"a.pdf": true, "a.docx": true, "a.xlsx": true, "a.pptx": true, "a.txt": true, "a.png": false, "a.zip": false} {
		if got := Handles("", name); got != want {
			t.Errorf("Handles(%s) = %v", name, got)
		}
	}
}
