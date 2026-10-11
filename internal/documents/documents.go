// Package documents stores what was read from an uploaded document and
// serves it back to the model (PLAN M13 cut 4): the original in the blob
// store behind an attachment row, the extracted text as page-bound chunks
// with embeddings, and the read_document tool over them. Extraction itself
// is internal/docread.
package documents

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"

	"github.com/jking323/ws/internal/docread"
	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
	"github.com/jking323/ws/internal/store/blob"
)

// Tuning.
const (
	// ChunkChars is the target chunk size; a chunk never spans two pages.
	ChunkChars = 1200
	// EmbedDims matches document_chunks.embedding.
	EmbedDims = 768
	// embedBatch is how many chunks go to the embedding model per call.
	embedBatch = 32
	// ReadChars caps what one read_document call returns to the model.
	ReadChars = 16000
)

// ErrNotFound is a document that does not exist or is another member's.
var ErrNotFound = errors.New("documents: not found")

// Store is the slice of the store used (*store.DB).
type Store interface {
	CreateAttachment(ctx context.Context, arg store.CreateAttachmentParams) (store.Attachment, error)
	CreateDocument(ctx context.Context, arg store.CreateDocumentParams) (store.Document, error)
	GetDocument(ctx context.Context, arg store.GetDocumentParams) (store.Document, error)
	DeleteDocumentChunks(ctx context.Context, attachmentID uuid.UUID) error
	InsertDocumentChunk(ctx context.Context, arg store.InsertDocumentChunkParams) error
	ListDocumentChunksByPages(ctx context.Context, arg store.ListDocumentChunksByPagesParams) ([]store.ListDocumentChunksByPagesRow, error)
	SearchDocumentChunks(ctx context.Context, arg store.SearchDocumentChunksParams) ([]store.SearchDocumentChunksRow, error)
}

// Embedder is the gateway's embedding call.
type Embedder interface {
	Embed(ctx context.Context, req *gateway.EmbedRequest) (*gateway.EmbedResponse, error)
}

// Service ingests and reads documents.
type Service struct {
	DB     Store
	Blobs  blob.Store
	Reader *docread.Reader
	GW     Embedder
	Log    *slog.Logger
	// InlineChars is how much extracted text goes straight into a message
	// before the rest is left for read_document (default 12000).
	InlineChars int
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Ingested is the result of one upload.
type Ingested struct {
	AttachmentID uuid.UUID
	Name         string
	Kind         string
	Method       string
	Pages        int
	Chars        int
	Truncated    bool
	// Text is the message text for the model: a header naming the file and
	// its attachment id, then page-marked text up to InlineChars.
	Text string
}

// Handles reports whether Ingest takes a file of this type.
func Handles(mime, name string) bool {
	switch docread.Kind(mime, name) {
	case "pdf", "docx", "xlsx", "pptx", "text":
		return true
	}
	return false
}

// Ingest stores the original, extracts its text, chunks and embeds it, and
// returns the text to put in the message. Extraction errors are returned as
// they are (docread.ErrNeedsOCR and friends carry a message fit to show).
func (s *Service) Ingest(ctx context.Context, userID uuid.UUID, conv uuid.NullUUID, mime, name string, data []byte) (*Ingested, error) {
	doc, err := s.Reader.Extract(ctx, mime, name, data)
	if err != nil {
		return nil, err
	}
	key, err := blob.PutBytes(ctx, s.Blobs, data)
	if err != nil {
		return nil, fmt.Errorf("documents: store original: %w", err)
	}
	sum := sha256.Sum256(data)
	if name == "" {
		name = "document"
	}
	att, err := s.DB.CreateAttachment(ctx, store.CreateAttachmentParams{UserID: userID, BlobKey: key, Mime: mime, Bytes: int64(len(data)), Sha256: sum[:], Filename: name})
	if err != nil {
		return nil, err
	}
	kind := docread.Kind(mime, name)
	if _, err := s.DB.CreateDocument(ctx, store.CreateDocumentParams{
		AttachmentID: att.ID, UserID: userID, ConversationID: conv, Kind: kind, Method: doc.Method,
		Pages: int32(len(doc.Pages)), Chars: int32(doc.Chars()), Truncated: doc.Truncated,
	}); err != nil {
		return nil, err
	}
	if err := s.storeChunks(ctx, userID, att.ID, doc); err != nil {
		return nil, err
	}
	out := &Ingested{AttachmentID: att.ID, Name: name, Kind: kind, Method: doc.Method, Pages: len(doc.Pages), Chars: doc.Chars(), Truncated: doc.Truncated}
	out.Text = s.render(out, doc)
	return out, nil
}

// Chunk is a piece of text bound to one page, sheet or slide.
type Chunk struct {
	Page int
	Text string
}

// ChunkDoc splits each page into chunks of about ChunkChars on line breaks.
func ChunkDoc(doc *docread.Doc) []Chunk {
	var out []Chunk
	for _, p := range doc.Pages {
		t := strings.TrimSpace(p.Text)
		if t == "" {
			continue
		}
		var cur strings.Builder
		for _, line := range strings.SplitAfter(t, "\n") {
			for len(line) > ChunkChars { // a very long line
				if cur.Len() > 0 {
					out = append(out, Chunk{p.N, strings.TrimSpace(cur.String())})
					cur.Reset()
				}
				cut := ChunkChars
				for cut < len(line) && line[cut]&0xC0 == 0x80 { // stay on a rune boundary
					cut++
				}
				out = append(out, Chunk{p.N, strings.TrimSpace(line[:cut])})
				line = line[cut:]
			}
			if cur.Len() > 0 && cur.Len()+len(line) > ChunkChars {
				out = append(out, Chunk{p.N, strings.TrimSpace(cur.String())})
				cur.Reset()
			}
			cur.WriteString(line)
		}
		if strings.TrimSpace(cur.String()) != "" {
			out = append(out, Chunk{p.N, strings.TrimSpace(cur.String())})
		}
	}
	return out
}

func (s *Service) storeChunks(ctx context.Context, userID, attID uuid.UUID, doc *docread.Doc) error {
	if err := s.DB.DeleteDocumentChunks(ctx, attID); err != nil {
		return err
	}
	chunks := ChunkDoc(doc)
	vecs := make([]*pgvector.Vector, len(chunks))
	if s.GW != nil {
		for i := 0; i < len(chunks); i += embedBatch {
			j := min(i+embedBatch, len(chunks))
			texts := make([]string, 0, j-i)
			for _, c := range chunks[i:j] {
				texts = append(texts, c.Text)
			}
			resp, err := s.GW.Embed(ctx, &gateway.EmbedRequest{Model: "auto", Inputs: texts, Metadata: gateway.Metadata{UserID: userID.String()}})
			if err != nil || len(resp.Vectors) != len(texts) {
				s.log().Warn("documents: embedding failed, storing without vectors", "err", err)
				break
			}
			for k, v := range resp.Vectors {
				if len(v) == EmbedDims {
					pv := pgvector.NewVector(v)
					vecs[i+k] = &pv
				}
			}
		}
	}
	for i, c := range chunks {
		if err := s.DB.InsertDocumentChunk(ctx, store.InsertDocumentChunkParams{AttachmentID: attID, Seq: int32(i), Page: int32(c.Page), Content: c.Text, Embedding: vecs[i]}); err != nil {
			return err
		}
	}
	return nil
}

// render builds the message text: a header the model can cite, then the
// pages that fit the inline budget.
func (s *Service) render(in *Ingested, doc *docread.Doc) string {
	budget := s.InlineChars
	if budget <= 0 {
		budget = 12000
	}
	unit := docread.Unit(in.Kind)
	var b strings.Builder
	via := "text layer"
	if in.Method == docread.MethodOCR {
		via = "read by OCR, so expect recognition errors"
	}
	fmt.Fprintf(&b, "[Attached document: %s · %s · %d %s(s) · %s · attachment %s]\n", in.Name, in.Kind, in.Pages, unit, via, in.AttachmentID)
	used, shown := 0, 0
	for _, p := range doc.Pages {
		t := strings.TrimSpace(p.Text)
		if used+len(t) > budget && shown > 0 {
			break
		}
		if used+len(t) > budget { // one page larger than the budget: cut it
			t = t[:budget]
			for len(t) > 0 && t[len(t)-1]&0xC0 == 0x80 {
				t = t[:len(t)-1]
			}
		}
		fmt.Fprintf(&b, "\n[%s %d]\n%s\n", unit, p.N, t)
		used += len(t)
		shown++
	}
	if shown < len(doc.Pages) || doc.Truncated {
		fmt.Fprintf(&b, "\n[Only the first %d of %d %s(s) are shown here. Use read_document with attachment_id %s and a page range or a query to read the rest.]", shown, len(doc.Pages), unit, in.AttachmentID)
	}
	return strings.TrimRight(b.String(), "\n")
}

// ReadResult is what read_document returns.
type ReadResult struct {
	AttachmentID uuid.UUID
	Kind         string
	Pages        int
	Text         string
}

// Read returns a page range ("3", "3-5") of a member's document, or, with
// a query, the chunks nearest to it (page-marked). Neither given reads
// from page 1. Output is capped at ReadChars.
func (s *Service) Read(ctx context.Context, userID, attID uuid.UUID, pages, query string) (*ReadResult, error) {
	doc, err := s.DB.GetDocument(ctx, store.GetDocumentParams{AttachmentID: attID, UserID: userID})
	if err != nil {
		return nil, ErrNotFound
	}
	unit := docread.Unit(doc.Kind)
	res := &ReadResult{AttachmentID: attID, Kind: doc.Kind, Pages: int(doc.Pages)}
	var b strings.Builder
	if q := strings.TrimSpace(query); q != "" && s.GW != nil {
		resp, err := s.GW.Embed(ctx, &gateway.EmbedRequest{Model: "auto", Inputs: []string{q}, Metadata: gateway.Metadata{UserID: userID.String()}})
		if err == nil && len(resp.Vectors) == 1 && len(resp.Vectors[0]) == EmbedDims {
			rows, err := s.DB.SearchDocumentChunks(ctx, store.SearchDocumentChunksParams{AttachmentID: attID, QueryEmbedding: pgvector.NewVector(resp.Vectors[0]), MaxRows: 6})
			if err != nil {
				return nil, err
			}
			for _, r := range rows {
				if b.Len()+len(r.Content) > ReadChars {
					break
				}
				fmt.Fprintf(&b, "[%s %d]\n%s\n\n", unit, r.Page, r.Content)
			}
			if b.Len() > 0 {
				res.Text = strings.TrimSpace(b.String())
				return res, nil
			}
		}
		// No embedding model, or no vectors stored: say so and fall back to the pages.
		b.WriteString("[Search by meaning is unavailable for this document; here are the pages instead.]\n\n")
	}
	first, last, err := parsePages(pages, int(doc.Pages))
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.ListDocumentChunksByPages(ctx, store.ListDocumentChunksByPagesParams{AttachmentID: attID, FirstPage: int32(first), LastPage: int32(last)})
	if err != nil {
		return nil, err
	}
	cur := int32(-1)
	for _, r := range rows {
		if b.Len()+len(r.Content) > ReadChars {
			fmt.Fprintf(&b, "\n[Cut at %d characters; ask for the next pages.]", ReadChars)
			break
		}
		if r.Page != cur {
			cur = r.Page
			fmt.Fprintf(&b, "[%s %d]\n", unit, r.Page)
		}
		b.WriteString(r.Content)
		b.WriteString("\n")
	}
	if len(rows) == 0 {
		fmt.Fprintf(&b, "No text on %s(s) %d to %d of %d.", unit, first, last, doc.Pages)
	}
	res.Text = strings.TrimSpace(b.String())
	return res, nil
}

// parsePages reads "", "4" or "3-5" against a document of n pages.
func parsePages(spec string, n int) (int, int, error) {
	spec = strings.TrimSpace(spec)
	if n < 1 {
		n = 1
	}
	if spec == "" {
		return 1, n, nil
	}
	a, b, found := strings.Cut(spec, "-")
	first, err := strconv.Atoi(strings.TrimSpace(a))
	if err != nil || first < 1 {
		return 0, 0, fmt.Errorf("pages is a number or a range like 3-5")
	}
	last := first
	if found {
		if last, err = strconv.Atoi(strings.TrimSpace(b)); err != nil || last < first {
			return 0, 0, fmt.Errorf("pages is a number or a range like 3-5")
		}
	}
	if first > n {
		return 0, 0, fmt.Errorf("the document has %d pages", n)
	}
	return first, min(last, n), nil
}
