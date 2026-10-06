package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/agent"
	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
)

// ToolName is the agent tool.
const ToolName = "generate_image"

// Conversations resolves a conversation to its project (*store.DB does).
type Conversations interface {
	GetConversation(ctx context.Context, id uuid.UUID) (store.Conversation, error)
}

// Tool is generate_image on the agent runtime: it creates a job in the
// conversation's project, waits for the worker to run it, and returns the
// attachments. The chat renders them from the tool output; the model
// only sees ids.
type Tool struct {
	Svc   *Service
	Convs Conversations
	// Wait bounds how long a call waits for the job (default 4 minutes).
	Wait time.Duration
	// Poll is the status poll interval (default 1 s).
	Poll time.Duration
	// URL builds the browser URL of an attachment (default /api/attachments/<id>).
	URL func(attachmentID uuid.UUID) string
}

// NewTool wires the tool.
func NewTool(svc *Service, convs Conversations) *Tool { return &Tool{Svc: svc, Convs: convs} }

// Def implements agent.Tool.
func (t *Tool) Def() gateway.ToolDef {
	return gateway.ToolDef{
		Name:        ToolName,
		Description: "Generate an image from a text prompt with the workspace's image model. The image is shown to the user in the chat and saved to the project's gallery. Describe the subject, style, composition and lighting in the prompt; do not ask the user to confirm the prompt first.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"prompt":{"type":"string","description":"What to draw, in detail."},"size":{"type":"string","description":"WxH such as 1024x1024, 1536x1024 (landscape) or 1024x1536 (portrait); omit for the model's default."},"n":{"type":"integer","minimum":1,"maximum":4,"description":"How many variants (default 1). Each costs money."},"quality":{"type":"string","enum":["low","medium","high"],"description":"Detail level; omit for the default."},"model":{"type":"string","description":"An image endpoint id from the models list; omit for the routed default."}},"required":["prompt"],"additionalProperties":false}`),
	}
}

// DefaultPolicy implements agent.Tool.
func (t *Tool) DefaultPolicy() agent.Policy { return agent.PolicyAuto }

// Idempotent implements agent.Tool: a re-run would bill again.
func (t *Tool) Idempotent() bool { return false }

// Call implements agent.Tool.
func (t *Tool) Call(ctx context.Context, tc agent.ToolCtx, args json.RawMessage) (agent.Result, error) {
	var in struct {
		Prompt  string `json:"prompt"`
		Size    string `json:"size"`
		N       int    `json:"n"`
		Quality string `json:"quality"`
		Model   string `json:"model"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return agent.ErrorResult(err), nil
	}
	if t.Convs == nil || t.Svc == nil {
		return agent.ErrorResult(errors.New("image generation is not configured")), nil
	}
	conv, err := t.Convs.GetConversation(ctx, tc.ConversationID)
	if err != nil {
		return agent.ErrorResult(fmt.Errorf("conversation: %w", err)), nil
	}
	job, err := t.Svc.Create(ctx, CreateParams{
		UserID: tc.UserID, ProjectID: conv.ProjectID, ConversationID: uuid.NullUUID{UUID: conv.ID, Valid: true},
		Kind: KindImage, Selector: in.Model,
		Inputs: Inputs{Prompt: in.Prompt, Size: in.Size, N: in.N, Quality: in.Quality},
	})
	if err != nil {
		return agent.ErrorResult(err), nil
	}
	if tc.Emit != nil {
		tc.Emit("media-job", map[string]any{"job_id": job.ID, "status": job.Status})
	}
	wait, poll := t.Wait, t.Poll
	if wait <= 0 {
		wait = 4 * time.Minute
	}
	done, err := t.Svc.Wait(ctx, job.ID, wait, poll)
	if err != nil && done == nil {
		return agent.ErrorResult(err), nil
	}
	if err != nil {
		return agent.ErrorResult(fmt.Errorf("%w (job %s is still %s; it finishes in the background and shows in the gallery; is the worker running?)", err, job.ID, done.Status)), nil
	}
	if done.Status != StatusDone {
		msg := "image generation " + done.Status
		if done.Error != nil {
			msg += ": " + *done.Error
		}
		return agent.ErrorResult(errors.New(msg)), nil
	}
	atts, err := t.Svc.Outputs(ctx, done)
	if err != nil {
		return agent.ErrorResult(err), nil
	}
	type img struct {
		AttachmentID uuid.UUID `json:"attachment_id"`
		URL          string    `json:"url"`
		MIME         string    `json:"mime"`
		Width        *int32    `json:"width,omitempty"`
		Height       *int32    `json:"height,omitempty"`
	}
	imgs := make([]img, 0, len(atts))
	ids := make([]string, 0, len(atts))
	for _, a := range atts {
		imgs = append(imgs, img{AttachmentID: a.ID, URL: t.url(a.ID), MIME: a.Mime, Width: a.Width, Height: a.Height})
		ids = append(ids, a.ID.String())
	}
	ep := ""
	if done.EndpointID != nil {
		ep = *done.EndpointID
	}
	data := map[string]any{"job_id": done.ID, "status": done.Status, "endpoint": ep, "cost_usd": done.CostUsd, "images": imgs}
	text := fmt.Sprintf("Generated %d image(s) with %s (attachment %s). The user sees them in the chat and in the project's gallery; refer to them as the image(s), not by id.",
		len(imgs), ep, strings.Join(ids, ", "))
	return agent.Result{Text: text, Data: data}, nil
}

func (t *Tool) url(id uuid.UUID) string {
	if t.URL != nil {
		return t.URL(id)
	}
	return "/api/attachments/" + id.String()
}
