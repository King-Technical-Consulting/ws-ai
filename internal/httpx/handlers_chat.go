package httpx

import (
	"encoding/json"
	"net/http"

	"github.com/jking323/ws/internal/chat"
	"github.com/jking323/ws/internal/httpx/aistream"
	"github.com/jking323/ws/internal/store"
)

// chatRequest is what @ai-sdk/react's DefaultChatTransport posts.
type chatRequest struct {
	ID       string `json:"id"`
	Trigger  string `json:"trigger"` // submit-message | regenerate-message
	Messages []struct {
		ID    string            `json:"id"`
		Role  string            `json:"role"`
		Parts []json.RawMessage `json:"parts"`
	} `json:"messages"`
	// Extra body fields we add on the client:
	Model     string `json:"model"`
	Reasoning bool   `json:"reasoning"`
}

// handleChat runs one turn and streams the UI Message Stream protocol.
// Two shapes arrive: a new user message (start a run), or the assistant
// message echoed back with approval decisions (resume the paused run).
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	conv, ok := s.loadConversation(w, r)
	if !ok {
		return
	}
	var in chatRequest
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	if len(in.Messages) == 0 {
		writeErr(w, 400, "no messages")
		return
	}
	p := Principal(r.Context())
	last := in.Messages[len(in.Messages)-1]

	if last.Role == "assistant" {
		decisions := chat.ApprovalDecisions(last.Parts)
		if len(decisions) == 0 {
			writeErr(w, 400, "assistant message carries no approval decisions")
			return
		}
		sw := aistream.NewHTTP(w)
		if err := s.Chat.ResumeWithApprovals(r.Context(), sw, conv.ID, p.UserID, decisions); err != nil {
			s.Log.Warn("chat resume failed", "conversation", conv.ID, "err", err)
			_ = sw.Error(err.Error())
			_ = sw.Finish("error")
			_ = sw.Done()
		}
		return
	}
	if last.Role != "user" {
		writeErr(w, 400, "last message must be from the user")
		return
	}
	parts, err := chat.FromUIParts(last.Parts)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	sw := aistream.NewHTTP(w)
	if _, err := s.Chat.Run(r.Context(), sw, chat.Turn{
		ConversationID: conv.ID, UserID: p.UserID, Parts: parts, Selector: in.Model, Reasoning: in.Reasoning,
	}); err != nil {
		s.Log.Warn("chat turn failed", "conversation", conv.ID, "err", err)
	}
}

// handleListRuns returns a conversation's runs (most recent first).
func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	conv, ok := s.loadConversation(w, r)
	if !ok {
		return
	}
	rows, err := s.DB.ListRunsForConversation(r.Context(), store.ListRunsForConversationParams{ConversationID: conv.ID, Limit: 20})
	if err != nil {
		writeErr(w, 500, "db")
		return
	}
	writeJSON(w, 200, rows)
}

// handleGetRun returns a run with its steps and approvals.
func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	id, err := uuidParam(r, "id")
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	run, err := s.DB.GetRun(r.Context(), id)
	if err != nil {
		writeErr(w, 404, "not found")
		return
	}
	conv, err := s.DB.GetConversation(r.Context(), run.ConversationID)
	if err != nil || !s.canAccessProject(r.Context(), conv.ProjectID) {
		writeErr(w, 404, "not found")
		return
	}
	steps, _ := s.DB.ListSteps(r.Context(), id)
	approvals, _ := s.DB.ListApprovalsForRun(r.Context(), id)
	writeJSON(w, 200, map[string]any{"run": run, "steps": steps, "approvals": approvals})
}

// handleDecideApproval records a decision without streaming (agent monitor).
// The run resumes on the next chat request or via the worker reaper.
func (s *Server) handleDecideApproval(w http.ResponseWriter, r *http.Request) {
	id, err := uuidParam(r, "id")
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	var in struct {
		Approved bool   `json:"approved"`
		Note     string `json:"note"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	a, err := s.DB.GetApproval(r.Context(), id)
	if err != nil {
		writeErr(w, 404, "not found")
		return
	}
	run, err := s.DB.GetRun(r.Context(), a.RunID)
	if err != nil {
		writeErr(w, 404, "not found")
		return
	}
	conv, err := s.DB.GetConversation(r.Context(), run.ConversationID)
	if err != nil || !s.canAccessProject(r.Context(), conv.ProjectID) {
		writeErr(w, 404, "not found")
		return
	}
	status := "denied"
	if in.Approved {
		status = "approved"
	}
	var note *string
	if in.Note != "" {
		note = &in.Note
	}
	p := Principal(r.Context())
	row, err := s.DB.DecideApproval(r.Context(), store.DecideApprovalParams{ID: id, Status: status, DecidedBy: store.NullUUID(p.UserID), Note: note})
	if err != nil {
		writeErr(w, 409, "already decided")
		return
	}
	// hand the run to the worker so it continues even with no client streaming
	if s.Jobs != nil {
		if pending, _ := s.DB.ListPendingApprovals(r.Context(), run.ID); len(pending) == 0 {
			_ = s.DB.SetRunStatus(r.Context(), store.SetRunStatusParams{ID: run.ID, Status: "queued"})
			_ = s.Jobs.EnqueueRun(r.Context(), run.ID)
		}
	}
	writeJSON(w, 200, row)
}
