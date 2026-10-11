package documents

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/agent"
	"github.com/jking323/ws/internal/docread"
	"github.com/jking323/ws/internal/gateway"
)

// ToolName is the model's way back into an uploaded document.
const ToolName = "read_document"

// Tool is read_document: pages of, or a search in, a document the person
// attached. It reads only the caller's own attachments.
type Tool struct {
	Svc *Service
	// OCR says whether images can be read; it only changes the description.
	OCR bool
}

// Def implements agent.Tool.
func (t *Tool) Def() gateway.ToolDef {
	desc := "Read an attached document (PDF, Word, Excel, PowerPoint or text file) that was uploaded to this chat. Give the attachment id from the attachment's header, and either a page range such as \"3-5\" (pages are pages, sheets or slides) or a query to find the most relevant passages. Without either it reads from the start, up to about 16,000 characters per call."
	if t.OCR {
		desc += " Scanned pages were read by OCR and may contain recognition errors."
	}
	return gateway.ToolDef{
		Name:        ToolName,
		Description: desc,
		InputSchema: json.RawMessage(`{"type":"object","properties":{"attachment_id":{"type":"string","description":"The attachment id from the document header."},"pages":{"type":"string","description":"A page, or a range like 3-5."},"query":{"type":"string","description":"What to look for; returns the nearest passages with their page numbers."}},"required":["attachment_id"],"additionalProperties":false}`),
	}
}

// DefaultPolicy implements agent.Tool.
func (t *Tool) DefaultPolicy() agent.Policy { return agent.PolicyAuto }

// Idempotent implements agent.Tool.
func (t *Tool) Idempotent() bool { return true }

// Call implements agent.Tool.
func (t *Tool) Call(ctx context.Context, tc agent.ToolCtx, args json.RawMessage) (agent.Result, error) {
	var in struct {
		AttachmentID string `json:"attachment_id"`
		Pages        string `json:"pages"`
		Query        string `json:"query"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return agent.ErrorResult(err), nil
	}
	id, err := uuid.Parse(strings.TrimSpace(in.AttachmentID))
	if err != nil {
		return agent.ErrorResult(errors.New("attachment_id is the id in the document header")), nil
	}
	if tc.UserID == uuid.Nil {
		return agent.ErrorResult(ErrNotFound), nil
	}
	res, err := t.Svc.Read(ctx, tc.UserID, id, in.Pages, in.Query)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return agent.ErrorResult(errors.New("no such document among this person's attachments")), nil
		}
		return agent.ErrorResult(err), nil
	}
	return agent.Result{Text: res.Text, Data: map[string]any{"attachment_id": res.AttachmentID, "kind": res.Kind, "pages": res.Pages, "unit": docread.Unit(res.Kind)}}, nil
}
