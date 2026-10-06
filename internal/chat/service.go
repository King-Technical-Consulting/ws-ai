// Package chat turns a user message into an agent run and streams it to the
// browser. The step loop, tools, checkpoints and resume live in the agent
// runtime; this package owns the AI SDK stream adapter and titles.
package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/agent"
	"github.com/jking323/ws/internal/artifacts"
	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/httpx/aistream"
	"github.com/jking323/ws/internal/store"
)

// Service runs chat turns.
type Service struct {
	DB      *store.DB
	GW      *gateway.Gateway
	Runtime *agent.Runtime
	Log     *slog.Logger
	// Enqueue hands a run to the worker (River). When set, code-mode runs
	// execute there and stream back over NOTIFY; chat-mode runs still run
	// in-process.
	Enqueue func(ctx context.Context, runID uuid.UUID) error
}

// Turn is one user message to answer.
type Turn struct {
	ConversationID uuid.UUID
	UserID         uuid.UUID
	Parts          []gateway.Part // the new user message
	Selector       string         // model selector; "" uses the conversation default
	System         string         // optional system prompt override
	Reasoning      bool
}

// Run persists the user message, creates a run, and drives it while
// streaming to w. Returns the first assistant message id.
func (s *Service) Run(ctx context.Context, w *aistream.Writer, t Turn) (uuid.UUID, error) {
	conv, err := s.DB.GetConversation(ctx, t.ConversationID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("chat: conversation: %w", err)
	}
	selector := t.Selector
	if selector == "" {
		selector = conv.ModelSelector
	}
	userParts, _ := json.Marshal(t.Parts)
	seq, err := s.DB.NextMessageSeq(ctx, conv.ID)
	if err != nil {
		return uuid.Nil, err
	}
	if _, err := s.DB.InsertMessage(ctx, store.InsertMessageParams{ConversationID: conv.ID, Seq: int64(seq), Role: "user", Parts: userParts}); err != nil {
		return uuid.Nil, err
	}

	var policies map[string]agent.Policy
	var design *artifacts.DesignContext
	if len(conv.Settings) > 0 {
		var st struct {
			ToolPolicies  map[string]agent.Policy `json:"tool_policies"`
			DesignContext json.RawMessage         `json:"design_context"`
		}
		_ = json.Unmarshal(conv.Settings, &st)
		policies = st.ToolPolicies
		design, _ = artifacts.ParseDesignContext(st.DesignContext)
	}
	system := t.System
	if system == "" {
		system = systemPrompt(conv.Mode, design)
	}
	run, err := s.Runtime.Create(ctx, agent.StartParams{
		ConversationID: conv.ID, UserID: t.UserID,
		Request:  agent.Request{Selector: selector, System: system, TaskClass: taskFor(conv.Mode), Reasoning: t.Reasoning, ToolAllow: toolsFor(conv.Mode)},
		Policies: policies, MaxSteps: maxStepsFor(conv.Mode),
	})
	if err != nil {
		return uuid.Nil, err
	}
	sink := NewAIStreamSink(w)
	if s.remote(conv.Mode) {
		err = s.relay(ctx, run.ID, sink)
	} else {
		err = s.Runtime.Drive(ctx, run.ID, sink)
	}
	bg := context.WithoutCancel(ctx)
	if conv.Title == "" {
		go s.title(bg, conv.ID)
	}
	if err != nil && !errors.Is(err, agent.ErrPaused) {
		return sink.firstID, err
	}
	return sink.firstID, nil
}

// remote reports whether runs for this conversation mode execute in the
// worker (which has the Docker socket for sandbox tools) rather than in
// this process. Requires an Enqueue hook.
func (s *Service) remote(mode string) bool { return mode == "code" && s.Enqueue != nil }

// relay enqueues the run for the worker and streams its NOTIFY events to
// the sink until it finishes or pauses.
func (s *Service) relay(ctx context.Context, runID uuid.UUID, sink agent.Sink) error {
	ready := make(chan struct{})
	done := make(chan error, 1)
	status := func(ctx context.Context, id uuid.UUID) (string, error) {
		r, err := s.DB.GetRun(ctx, id)
		if err != nil {
			return "", err
		}
		return r.Status, nil
	}
	go func() { done <- agent.Relay(ctx, s.DB.Pool, runID, sink, status, ready) }()
	select {
	case <-ready:
	case err := <-done:
		return err
	}
	if err := s.Enqueue(ctx, runID); err != nil {
		return fmt.Errorf("chat: enqueue run: %w", err)
	}
	return <-done
}

// chatTools is what non-code conversations advertise to the model;
// sandbox tools exist only in code projects.
var chatTools = []string{"create_artifact", "update_artifact", "web_fetch", "read_blob", "ask_user"}

// ResumeWithApprovals records the user's decisions on pending approvals
// for the conversation's paused run and continues it, streaming to w.
func (s *Service) ResumeWithApprovals(ctx context.Context, w *aistream.Writer, convID, userID uuid.UUID, decisions map[string]bool) error {
	run, err := s.DB.LatestRunForConversation(ctx, convID)
	if err != nil {
		return errors.New("no run to resume")
	}
	if run.Status != "paused_approval" {
		return fmt.Errorf("run is %s, not awaiting approval", run.Status)
	}
	for callID, approved := range decisions {
		status := "denied"
		if approved {
			status = "approved"
		}
		if _, err := s.DB.DecideApprovalByCall(ctx, store.DecideApprovalByCallParams{RunID: run.ID, ToolCallID: callID, Status: status, DecidedBy: uuid.NullUUID{UUID: userID, Valid: true}}); err != nil {
			s.Log.Warn("approval decide", "call", callID, "err", err)
		}
	}
	sink := NewAIStreamSink(w)
	if conv, cerr := s.DB.GetConversation(ctx, convID); cerr == nil && s.remote(conv.Mode) {
		// The worker claims queued runs; a paused one would be refused.
		if err := s.DB.SetRunStatus(ctx, store.SetRunStatusParams{ID: run.ID, Status: "queued"}); err != nil {
			return err
		}
		err = s.relay(ctx, run.ID, sink)
	} else {
		err = s.Runtime.Resume(ctx, run.ID, sink)
	}
	if err != nil && !errors.Is(err, agent.ErrPaused) {
		return err
	}
	return nil
}

// ---- AI SDK sink ----

// AIStreamSink adapts agent.Sink to the Vercel AI SDK UI Message Stream.
type AIStreamSink struct {
	w        *aistream.Writer
	firstID  uuid.UUID
	textID   string
	reasonID string
	n        int
	toolName map[string]string
	toolArgs map[string]*strings.Builder
	finished bool
}

// NewAIStreamSink wraps a writer.
func NewAIStreamSink(w *aistream.Writer) *AIStreamSink {
	return &AIStreamSink{w: w, toolName: map[string]string{}, toolArgs: map[string]*strings.Builder{}}
}

func (s *AIStreamSink) next(prefix string) string { s.n++; return fmt.Sprintf("%s_%d", prefix, s.n) }
func (s *AIStreamSink) closeText() {
	if s.textID != "" {
		_ = s.w.TextEnd(s.textID)
		s.textID = ""
	}
}
func (s *AIStreamSink) closeReason() {
	if s.reasonID != "" {
		_ = s.w.ReasoningEnd(s.reasonID)
		s.reasonID = ""
	}
}

func (s *AIStreamSink) Start(id string) {
	if s.firstID == uuid.Nil {
		s.firstID, _ = uuid.Parse(id)
		_ = s.w.Start(id)
	}
}
func (s *AIStreamSink) StartStep() { _ = s.w.StartStep() }
func (s *AIStreamSink) FinishStep() {
	s.closeText()
	s.closeReason()
	_ = s.w.FinishStep()
}
func (s *AIStreamSink) Model(ev gateway.StreamEvent) {
	switch ev.Type {
	case gateway.EventStart:
		_ = s.w.Data("model", map[string]string{"endpoint": ev.EndpointID, "model": ev.Model}, false)
	case gateway.EventTextDelta:
		s.closeReason()
		if s.textID == "" {
			s.textID = s.next("txt")
			_ = s.w.TextStart(s.textID)
		}
		_ = s.w.TextDelta(s.textID, ev.Text)
	case gateway.EventReasoningDelta:
		s.closeText()
		if s.reasonID == "" {
			s.reasonID = s.next("rsn")
			_ = s.w.ReasoningStart(s.reasonID)
		}
		_ = s.w.ReasoningDelta(s.reasonID, ev.Text)
	case gateway.EventToolCallStart:
		s.closeText()
		s.closeReason()
		s.toolName[ev.ToolCallID] = ev.ToolName
		s.toolArgs[ev.ToolCallID] = &strings.Builder{}
		_ = s.w.ToolInputStart(ev.ToolCallID, ev.ToolName)
	case gateway.EventToolCallDelta:
		if b, ok := s.toolArgs[ev.ToolCallID]; ok {
			b.WriteString(ev.ArgsDelta)
		}
		_ = s.w.ToolInputDelta(ev.ToolCallID, ev.ArgsDelta)
	case gateway.EventToolCallEnd:
		args := "{}"
		if b, ok := s.toolArgs[ev.ToolCallID]; ok && b.Len() > 0 {
			args = b.String()
		}
		_ = s.w.ToolInputAvailable(ev.ToolCallID, s.toolName[ev.ToolCallID], json.RawMessage(args))
	case gateway.EventUsage:
		if ev.Usage != nil {
			_ = s.w.Data("usage", ev.Usage, true)
		}
	}
}
func (s *AIStreamSink) ToolOutput(id string, out any, isErr bool, errText string) {
	if isErr {
		_ = s.w.ToolOutputError(id, errText)
		return
	}
	_ = s.w.ToolOutputAvailable(id, out)
}
func (s *AIStreamSink) ApprovalRequest(approvalID, callID string) {
	_ = s.w.ToolApprovalRequest(approvalID, callID)
}
func (s *AIStreamSink) Data(name string, v any, transient bool) { _ = s.w.Data(name, v, transient) }
func (s *AIStreamSink) Error(msg string)                        { _ = s.w.Error(msg) }
func (s *AIStreamSink) Finish(reason string) {
	if s.finished {
		return
	}
	s.finished = true
	s.closeText()
	s.closeReason()
	_ = s.w.Finish(reason)
	_ = s.w.Done()
}

// ---- titles ----

func (s *Service) title(ctx context.Context, convID uuid.UUID) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rows, err := s.DB.ListMessages(ctx, convID)
	if err != nil {
		return
	}
	var first, reply string
	for _, r := range rows {
		var parts []gateway.Part
		_ = json.Unmarshal(r.Parts, &parts)
		for _, p := range parts {
			if p.Kind != gateway.PartText || p.Text == "" {
				continue
			}
			if r.Role == "user" && first == "" {
				first = p.Text
			}
			if r.Role == "assistant" && first != "" && reply == "" {
				reply = p.Text
			}
		}
	}
	if first == "" {
		return
	}
	if len(first) > 1500 {
		first = first[:1500]
	}
	if len(reply) > 800 {
		reply = reply[:800]
	}
	r, err := s.GW.Complete(ctx, &gateway.Request{
		Model:     "auto",
		System:    "You write 3-6 word titles for chat conversations. Reply with the title only, no quotes, no trailing period.",
		Messages:  []gateway.Message{{Role: gateway.RoleUser, Parts: []gateway.Part{gateway.TextPart("User: " + first + "\n\nAssistant: " + reply + "\n\nTitle:")}}},
		MaxTokens: 24,
		Metadata:  gateway.Metadata{ConversationID: convID.String(), TaskClass: gateway.TaskTitle},
	})
	if err != nil {
		s.Log.Warn("chat: title generation failed", "err", err)
		return
	}
	title := strings.Trim(strings.TrimSpace(strings.Split(r.Text(), "\n")[0]), `"'.`)
	if title == "" || len(title) > 80 {
		return
	}
	_ = s.DB.UpdateConversationTitle(ctx, store.UpdateConversationTitleParams{ID: convID, Title: title})
}

// systemPrompt is the default prompt for a mode. A design conversation
// gets the design instructions and its design system (PLAN M6).
func systemPrompt(mode string, design *artifacts.DesignContext) string {
	base := "You are a helpful, direct assistant. Use Markdown. Keep answers as short as the question allows."
	switch mode {
	case "code":
		base += " You are helping with software engineering. Prefer concrete code over prose."
	case "design":
		base += artifacts.DesignSystemPrompt(design)
	}
	base += " When the user asks for a document, web page, diagram, or a file-sized piece of code they will keep, use create_artifact and then describe it in one or two sentences; use update_artifact to revise an existing artifact. Use web_fetch to read a public page when the user gives a URL or asks about something you'd need to look up. If a tool result says a large output was stored as a blob, use read_blob to read more of it only when needed."
	return base
}

func taskFor(mode string) gateway.TaskClass {
	if mode == "code" {
		return gateway.TaskCode
	}
	return gateway.TaskChat
}

// toolsFor restricts the advertised tools by mode: code conversations get
// everything the driving process registers (the worker adds sandbox
// tools); others get the chat set.
func toolsFor(mode string) []string {
	if mode == "code" {
		return nil
	}
	return chatTools
}

// AllowTools appends tool names to the chat allowlist. Called at boot for
// tools that chat-mode conversations may use besides the built-in set.
func AllowTools(names ...string) {
	chatTools = append(chatTools, names...)
}

// WithMCP appends MCP tool names to the chat allowlist. Called at boot
// with the names the registry ended up with.
func WithMCP(names []string) {
	for _, n := range names {
		if strings.HasPrefix(n, "mcp__") {
			chatTools = append(chatTools, n)
		}
	}
}

func maxStepsFor(mode string) int {
	if mode == "code" {
		return 40
	}
	return 12
}

// ---- UI message shapes ----

// UIPart is an AI SDK UIMessage part.
type UIPart map[string]any

// UIMessage is what the client renders.
type UIMessage struct {
	ID       string         `json:"id"`
	Role     string         `json:"role"`
	Parts    []UIPart       `json:"parts"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// MergeUI converts stored rows into UI messages. A user row starts a new
// message; the assistant and tool rows that follow fold into a single
// assistant message with each tool result attached to its call. Pending
// approvals mark their tool part so the client shows the approval card.
func MergeUI(rows []store.Message, pending map[string]string) []UIMessage {
	var out []UIMessage
	var cur *UIMessage
	for _, m := range rows {
		var parts []gateway.Part
		_ = json.Unmarshal(m.Parts, &parts)
		if len(parts) == 0 {
			continue
		}
		switch m.Role {
		case "user":
			out = append(out, UIMessage{ID: m.ID.String(), Role: "user", Parts: ToUIParts(parts)})
			cur = nil
		case "assistant":
			if cur == nil {
				out = append(out, UIMessage{ID: m.ID.String(), Role: "assistant", Metadata: map[string]any{}})
				cur = &out[len(out)-1]
			}
			ui := ToUIParts(parts)
			for i, p := range ui {
				if id, ok := p["toolCallId"].(string); ok {
					if aid, pend := pending[id]; pend {
						p["state"] = "approval-requested"
						p["approval"] = map[string]any{"id": aid}
						ui[i] = p
					}
				}
			}
			cur.Parts = append(cur.Parts, ui...)
			if m.Model != nil {
				cur.Metadata["model"] = *m.Model
			}
			if m.EndpointID != nil {
				cur.Metadata["endpoint"] = *m.EndpointID
			}
			if len(m.Usage) > 0 {
				cur.Metadata["usage"] = json.RawMessage(m.Usage)
			}
		case "tool":
			if cur == nil {
				continue
			}
			for _, p := range parts {
				if p.Kind == gateway.PartToolResult {
					attachOutput(cur, p)
				}
			}
		}
	}
	return out
}

func attachOutput(msg *UIMessage, res gateway.Part) {
	var sb strings.Builder
	for _, c := range res.Content {
		if c.Kind == gateway.PartText {
			sb.WriteString(c.Text)
		}
	}
	var output any = sb.String()
	var obj any
	if json.Unmarshal([]byte(sb.String()), &obj) == nil {
		output = obj
	}
	for i, p := range msg.Parts {
		if p["toolCallId"] == res.ToolCallID {
			if res.IsError {
				p["state"] = "output-error"
				p["errorText"] = sb.String()
			} else {
				p["state"] = "output-available"
				p["output"] = output
			}
			delete(p, "approval")
			msg.Parts[i] = p
			return
		}
	}
	msg.Parts = append(msg.Parts, UIPart{"type": "dynamic-tool", "toolName": "", "toolCallId": res.ToolCallID, "state": "output-available", "output": output})
}

// ToUIParts converts persisted canonical parts to UIMessage parts.
func ToUIParts(parts []gateway.Part) []UIPart {
	var out []UIPart
	for _, p := range parts {
		switch p.Kind {
		case gateway.PartText:
			out = append(out, UIPart{"type": "text", "text": p.Text, "state": "done"})
		case gateway.PartReasoning:
			if p.Text != "" {
				out = append(out, UIPart{"type": "reasoning", "text": p.Text, "state": "done"})
			}
		case gateway.PartImage, gateway.PartFile:
			url := p.URL
			if url == "" && p.BlobKey != "" {
				url = "/api/blobs/" + p.BlobKey
			}
			if url == "" && len(p.Data) > 0 {
				url = "data:" + p.MIME + ";base64," + b64(p.Data)
			}
			part := UIPart{"type": "file", "mediaType": p.MIME, "url": url}
			if p.Name != "" {
				part["filename"] = p.Name
			}
			out = append(out, part)
		case gateway.PartToolCall:
			var input any
			_ = json.Unmarshal(p.Args, &input)
			out = append(out, UIPart{"type": "tool-" + p.ToolName, "toolCallId": p.ToolCallID, "state": "input-available", "input": input})
		case gateway.PartToolResult:
			var sb strings.Builder
			for _, c := range p.Content {
				if c.Kind == gateway.PartText {
					sb.WriteString(c.Text)
				}
			}
			out = append(out, UIPart{"type": "dynamic-tool", "toolName": "", "toolCallId": p.ToolCallID, "state": "output-available", "output": sb.String()})
		}
	}
	return out
}

// FromUIParts converts the client's parts for a new user message.
func FromUIParts(raw []json.RawMessage) ([]gateway.Part, error) {
	var out []gateway.Part
	for _, r := range raw {
		var hdr struct {
			Type      string `json:"type"`
			Text      string `json:"text"`
			MediaType string `json:"mediaType"`
			URL       string `json:"url"`
			Filename  string `json:"filename"`
		}
		if err := json.Unmarshal(r, &hdr); err != nil {
			return nil, err
		}
		switch hdr.Type {
		case "text":
			if strings.TrimSpace(hdr.Text) != "" {
				out = append(out, gateway.TextPart(hdr.Text))
			}
		case "file":
			p := gateway.Part{MIME: hdr.MediaType, Name: hdr.Filename}
			if strings.HasPrefix(hdr.URL, "data:") {
				data, err := decodeDataURL(hdr.URL)
				if err != nil {
					return nil, err
				}
				p.Data = data
			} else if strings.HasPrefix(hdr.URL, "/api/blobs/") {
				p.BlobKey = strings.TrimPrefix(hdr.URL, "/api/blobs/")
			} else {
				p.URL = hdr.URL
			}
			if strings.HasPrefix(hdr.MediaType, "image/") {
				p.Kind = gateway.PartImage
			} else {
				p.Kind = gateway.PartFile
			}
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty message")
	}
	return out, nil
}

// ApprovalDecisions extracts {toolCallId: approved} from an assistant
// UIMessage's parts as sent back by the AI SDK after the user responds to
// approval requests (parts carry approval: {id, approved, reason}).
func ApprovalDecisions(parts []json.RawMessage) map[string]bool {
	out := map[string]bool{}
	for _, r := range parts {
		var p struct {
			ToolCallID string `json:"toolCallId"`
			State      string `json:"state"`
			Approval   *struct {
				Approved *bool `json:"approved"`
			} `json:"approval"`
		}
		if json.Unmarshal(r, &p) != nil || p.ToolCallID == "" || p.Approval == nil || p.Approval.Approved == nil {
			continue
		}
		out[p.ToolCallID] = *p.Approval.Approved
	}
	return out
}
