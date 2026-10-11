package notebooks

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/agent"
	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
)

// ToolName is how the model keeps a notebook's notes current.
const ToolName = "notebook_note"

// Tool writes the notebook's summary or open-questions note, or adds a
// note. It works only in the chat of a notebook of the person asking.
type Tool struct{ Svc *Service }

// Def implements agent.Tool.
func (t *Tool) Def() gateway.ToolDef {
	return gateway.ToolDef{
		Name:        ToolName,
		Description: "Update this notebook's notes. kind \"summary\" replaces the notebook's summary of what the sources say so far; \"open_questions\" replaces the list of questions the sources leave open; \"note\" adds a separate note with a title. Keep each short and specific, cite sources with [label](src: <source id> p.<page>), and write the whole note each time, not a diff.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"kind":{"type":"string","enum":["summary","open_questions","note"]},"title":{"type":"string","description":"A short title (for kind note)."},"text":{"type":"string","description":"The note's whole text, Markdown."}},"required":["kind","text"],"additionalProperties":false}`),
	}
}

// DefaultPolicy implements agent.Tool.
func (t *Tool) DefaultPolicy() agent.Policy { return agent.PolicyAuto }

// Idempotent implements agent.Tool: the singleton notes are replaced, so a
// repeat leaves the same state; a repeated "note" adds a second one, which
// is why a run that died mid-call is not retried by the runtime.
func (t *Tool) Idempotent() bool { return false }

// Call implements agent.Tool.
func (t *Tool) Call(ctx context.Context, tc agent.ToolCtx, args json.RawMessage) (agent.Result, error) {
	var in struct {
		Kind  string `json:"kind"`
		Title string `json:"title"`
		Text  string `json:"text"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return agent.ErrorResult(err), nil
	}
	if tc.UserID == uuid.Nil {
		return agent.ErrorResult(errors.New("no signed-in person on this run")), nil
	}
	nb, err := t.Svc.DB.GetNotebookByConversation(ctx, store.GetNotebookByConversationParams{ConversationID: store.NullUUID(tc.ConversationID), UserID: tc.UserID})
	if err != nil {
		return agent.ErrorResult(errors.New("this conversation is not a notebook's chat")), nil
	}
	n, err := t.Svc.SetModelNote(ctx, nb.ID, in.Kind, in.Title, in.Text)
	if err != nil {
		return agent.ErrorResult(err), nil
	}
	return agent.Result{Text: "Saved the " + n.Kind + " note.", Data: map[string]any{"note": n}}, nil
}
