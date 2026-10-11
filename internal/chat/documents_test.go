package chat

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/docread"
	"github.com/jking323/ws/internal/documents"
	"github.com/jking323/ws/internal/gateway"
)

type fakeIngester struct {
	err   error
	calls []string
}

func (f *fakeIngester) Ingest(_ context.Context, _ uuid.UUID, _ uuid.NullUUID, _, name string, _ []byte) (*documents.Ingested, error) {
	f.calls = append(f.calls, name)
	if f.err != nil {
		return nil, f.err
	}
	kind := "docx"
	if strings.HasSuffix(name, ".pdf") {
		kind = "pdf"
	}
	return &documents.Ingested{Name: name, Kind: kind, Text: "[Attached document: " + name + "]\n[page 1]\nhello"}, nil
}

func newDocService(ing DocumentIngester) *Service {
	return &Service{Documents: ing, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func filePart(name, mime string) gateway.Part {
	return gateway.Part{Kind: gateway.PartFile, MIME: mime, Name: name, Data: []byte("bytes")}
}

func TestReadDocumentsReplacesOfficeFilesWithText(t *testing.T) {
	ing := &fakeIngester{}
	s := newDocService(ing)
	in := []gateway.Part{gateway.TextPart("summarise"), filePart("memo.docx", ""), {Kind: gateway.PartImage, MIME: "image/png", Data: []byte("img")}}
	out := s.readDocuments(context.Background(), uuid.New(), uuid.New(), in)
	if len(out) != 3 || out[1].Kind != gateway.PartText || !strings.Contains(out[1].Text, "memo.docx") || out[1].Alt {
		t.Fatalf("out = %+v", out)
	}
	if out[2].Kind != gateway.PartImage {
		t.Error("images must pass through untouched")
	}
	if len(ing.calls) != 1 {
		t.Errorf("ingested %v", ing.calls)
	}
}

func TestReadDocumentsKeepsPDFAndAddsAlternative(t *testing.T) {
	s := newDocService(&fakeIngester{})
	out := s.readDocuments(context.Background(), uuid.New(), uuid.New(), []gateway.Part{filePart("a.pdf", "application/pdf")})
	if len(out) != 2 || out[0].Kind != gateway.PartFile || out[1].Kind != gateway.PartText || !out[1].Alt {
		t.Fatalf("out = %+v", out)
	}
	// What a model that cannot read PDFs is sent: the text, not the file.
	got := gateway.ResolveAlt(out, false)
	if len(got) != 1 || got[0].Kind != gateway.PartText || !strings.Contains(got[0].Text, "hello") {
		t.Errorf("text-only model sees %+v", got)
	}
}

func TestReadDocumentsFailureBecomesANote(t *testing.T) {
	s := newDocService(&fakeIngester{err: docread.ErrNeedsOCR})
	out := s.readDocuments(context.Background(), uuid.New(), uuid.New(), []gateway.Part{filePart("scan.pdf", "application/pdf")})
	if len(out) != 1 || out[0].Kind != gateway.PartText || !strings.Contains(out[0].Text, "no OCR service") || !strings.Contains(out[0].Text, "scan.pdf") {
		t.Fatalf("out = %+v", out)
	}
	s = newDocService(&fakeIngester{err: errors.New("db down at 10.0.0.5")})
	out = s.readDocuments(context.Background(), uuid.New(), uuid.New(), []gateway.Part{filePart("a.docx", "")})
	if strings.Contains(out[0].Text, "10.0.0.5") {
		t.Errorf("an internal error leaked into the message: %q", out[0].Text)
	}
}

func TestReadDocumentsNoServiceOrUnknownType(t *testing.T) {
	in := []gateway.Part{filePart("a.docx", "")}
	if out := (&Service{}).readDocuments(context.Background(), uuid.New(), uuid.New(), in); len(out) != 1 || out[0].Kind != gateway.PartFile {
		t.Errorf("no service must leave parts alone: %+v", out)
	}
	ing := &fakeIngester{}
	out := newDocService(ing).readDocuments(context.Background(), uuid.New(), uuid.New(), []gateway.Part{filePart("a.zip", "application/zip")})
	if len(ing.calls) != 0 || out[0].Kind != gateway.PartFile {
		t.Errorf("unknown type: %+v calls %v", out, ing.calls)
	}
}
