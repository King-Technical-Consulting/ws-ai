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

// Tool names on the agent runtime.
const (
	ToolName      = "generate_image"
	ToolNameVideo = "generate_video"
)

// Conversations resolves a conversation to its project (*store.DB does).
type Conversations interface {
	GetConversation(ctx context.Context, id uuid.UUID) (store.Conversation, error)
}

// Tool is generate_image or generate_video on the agent runtime: it
// creates a job in the conversation's project, waits for the worker to run
// it, and returns the attachments. The chat renders them from the tool
// output; the model only sees ids. With a source attachment id,
// generate_image edits that image and generate_video animates it.
type Tool struct {
	Svc   *Service
	Convs Conversations
	// Video makes this the generate_video tool.
	Video bool
	// Wait bounds how long a call waits for the job (default 4 minutes for
	// images, 15 for video).
	Wait time.Duration
	// Poll is the status poll interval (default 1 s).
	Poll time.Duration
	// URL builds the browser URL of an attachment (default /api/attachments/<id>).
	URL func(attachmentID uuid.UUID) string
}

// NewTool wires generate_image.
func NewTool(svc *Service, convs Conversations) *Tool { return &Tool{Svc: svc, Convs: convs} }

// NewVideoTool wires generate_video.
func NewVideoTool(svc *Service, convs Conversations) *Tool {
	return &Tool{Svc: svc, Convs: convs, Video: true}
}

// Def implements agent.Tool.
func (t *Tool) Def() gateway.ToolDef {
	if t.Video {
		return gateway.ToolDef{
			Name:        ToolNameVideo,
			Description: "Generate a short video from a text prompt with the workspace's video model, or animate an existing image when source_attachment_id names one (an image the user attached, or one generate_image made: use its attachment_id). The video is shown to the user in the chat and saved to the project's gallery. Rendering takes minutes and costs money per second; describe the scene, motion and camera in the prompt and do not ask the user to confirm it first.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"prompt":{"type":"string","description":"What happens in the video, in detail: subject, motion, camera, lighting."},"seconds":{"type":"integer","minimum":1,"maximum":60,"description":"Length in seconds; omit for the model's default. Some models accept only certain lengths."},"size":{"type":"string","description":"WxH such as 1280x720 (landscape) or 720x1280 (portrait); omit for the model's default."},"source_attachment_id":{"type":"string","description":"An image attachment id to animate (image to video); omit for text to video."},"model":{"type":"string","description":"A video endpoint id from the models list; omit for the routed default."}},"required":["prompt"],"additionalProperties":false}`),
		}
	}
	return gateway.ToolDef{
		Name:        ToolName,
		Description: "Generate an image from a text prompt with the workspace's image model, or edit an existing image when source_attachment_id names one (an image the user attached, or one this tool made earlier: use its attachment_id). The image is shown to the user in the chat and saved to the project's gallery. Describe the subject, style, composition and lighting in the prompt; for an edit describe the change; do not ask the user to confirm the prompt first.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"prompt":{"type":"string","description":"What to draw, in detail; for an edit, what to change."},"size":{"type":"string","description":"WxH such as 1024x1024, 1536x1024 (landscape) or 1024x1536 (portrait); omit for the model's default."},"n":{"type":"integer","minimum":1,"maximum":4,"description":"How many variants (default 1). Each costs money."},"quality":{"type":"string","enum":["low","medium","high"],"description":"Detail level; omit for the default."},"source_attachment_id":{"type":"string","description":"An image attachment id to edit; omit to generate from the prompt alone."},"mask_attachment_id":{"type":"string","description":"For an edit: an image attachment whose transparent area marks where the change applies; omit to let the edit touch the whole image."},"model":{"type":"string","description":"An image endpoint id from the models list; omit for the routed default."}},"required":["prompt"],"additionalProperties":false}`),
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
		Seconds int    `json:"seconds"`
		Source  string `json:"source_attachment_id"`
		Mask    string `json:"mask_attachment_id"`
		Model   string `json:"model"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return agent.ErrorResult(err), nil
	}
	if t.Convs == nil || t.Svc == nil {
		return agent.ErrorResult(errors.New("media generation is not configured")), nil
	}
	conv, err := t.Convs.GetConversation(ctx, tc.ConversationID)
	if err != nil {
		return agent.ErrorResult(fmt.Errorf("conversation: %w", err)), nil
	}
	kind := KindImage
	noun := "image"
	if t.Video {
		kind, noun = KindVideo, "video"
	} else if in.Source != "" {
		kind = KindEdit
	}
	job, err := t.Svc.Create(ctx, CreateParams{
		UserID: tc.UserID, ProjectID: conv.ProjectID, ConversationID: uuid.NullUUID{UUID: conv.ID, Valid: true},
		Kind: kind, Selector: in.Model,
		Inputs: Inputs{Prompt: in.Prompt, Size: in.Size, N: in.N, Quality: in.Quality, Seconds: in.Seconds, SourceAttachmentID: in.Source, MaskAttachmentID: in.Mask},
	})
	if err != nil {
		return agent.ErrorResult(err), nil
	}
	if tc.Emit != nil {
		tc.Emit("media-job", map[string]any{"job_id": job.ID, "status": job.Status, "kind": job.Kind})
	}
	wait, poll := t.Wait, t.Poll
	if wait <= 0 {
		wait = 4 * time.Minute
		if t.Video {
			wait = 15 * time.Minute
		}
	}
	done, err := t.Svc.Wait(ctx, job.ID, wait, poll)
	if err != nil && done == nil {
		return agent.ErrorResult(err), nil
	}
	if err != nil {
		return agent.ErrorResult(fmt.Errorf("%w (job %s is still %s; it finishes in the background and shows in the gallery; is the worker running?)", err, job.ID, done.Status)), nil
	}
	if done.Status != StatusDone {
		msg := noun + " generation " + done.Status
		if done.Error != nil {
			msg += ": " + *done.Error
		}
		return agent.ErrorResult(errors.New(msg)), nil
	}
	atts, err := t.Svc.Outputs(ctx, done)
	if err != nil {
		return agent.ErrorResult(err), nil
	}
	type file struct {
		AttachmentID uuid.UUID `json:"attachment_id"`
		URL          string    `json:"url"`
		MIME         string    `json:"mime"`
		Width        *int32    `json:"width,omitempty"`
		Height       *int32    `json:"height,omitempty"`
	}
	files := make([]file, 0, len(atts))
	ids := make([]string, 0, len(atts))
	for _, a := range atts {
		files = append(files, file{AttachmentID: a.ID, URL: t.url(a.ID), MIME: a.Mime, Width: a.Width, Height: a.Height})
		ids = append(ids, a.ID.String())
	}
	ep := ""
	if done.EndpointID != nil {
		ep = *done.EndpointID
	}
	data := map[string]any{"job_id": done.ID, "status": done.Status, "kind": done.Kind, "endpoint": ep, "cost_usd": done.CostUsd}
	if t.Video {
		data["videos"] = files
	} else {
		data["images"] = files
	}
	verb := "Generated"
	if kind == KindEdit {
		verb = "Edited into"
	}
	text := fmt.Sprintf("%s %d %s(s) with %s (attachment %s). The user sees them in the chat and in the project's gallery; refer to them as the %s(s), not by id. To edit or animate one later, pass its attachment id as source_attachment_id.",
		verb, len(files), noun, ep, strings.Join(ids, ", "), noun)
	return agent.Result{Text: text, Data: data}, nil
}

func (t *Tool) url(id uuid.UUID) string {
	if t.URL != nil {
		return t.URL(id)
	}
	return "/api/attachments/" + id.String()
}
