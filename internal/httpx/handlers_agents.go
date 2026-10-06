package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jking323/ws/internal/agent"
	"github.com/jking323/ws/internal/agents"
	"github.com/jking323/ws/internal/store"
)

// agentView is an agent as the browser sees it.
type agentView struct {
	store.Agent
	ProjectID *uuid.UUID `json:"project_id"`
}

func viewAgent(a store.Agent) agentView {
	v := agentView{Agent: a}
	if a.ProjectID.Valid {
		id := a.ProjectID.UUID
		v.ProjectID = &id
	}
	return v
}

// triggerView hides the secret hash.
type triggerView struct {
	ID        uuid.UUID       `json:"id"`
	AgentID   uuid.UUID       `json:"agent_id"`
	Kind      string          `json:"kind"`
	Name      string          `json:"name"`
	Spec      json.RawMessage `json:"spec"`
	Enabled   bool            `json:"enabled"`
	CreatedAt time.Time       `json:"created_at"`
	NextRunAt *time.Time      `json:"next_run_at"`
	LastRunAt *time.Time      `json:"last_run_at"`
	LastError *string         `json:"last_error"`
	// URL is the webhook path (the secret is shown only at creation).
	URL string `json:"url,omitempty"`
}

func viewTrigger(t store.AgentTrigger) triggerView {
	v := triggerView{ID: t.ID, AgentID: t.AgentID, Kind: t.Kind, Name: t.Name, Spec: t.Spec, Enabled: t.Enabled, CreatedAt: t.CreatedAt, NextRunAt: t.NextRunAt, LastRunAt: t.LastRunAt, LastError: t.LastError}
	if len(v.Spec) == 0 {
		v.Spec = json.RawMessage("{}")
	}
	if t.Kind == agents.KindWebhook || t.Kind == agents.KindRepoPush {
		v.URL = "/hooks/agents/" + t.ID.String() + "/<secret>"
	}
	return v
}

// agentInput is the create/update body.
type agentInput struct {
	Name          string                  `json:"name"`
	Goal          string                  `json:"goal"`
	SystemPrompt  string                  `json:"system_prompt"`
	ProjectID     string                  `json:"project_id"`
	Model         agents.ModelPolicy      `json:"model"`
	ToolAllowlist []string                `json:"tool_allowlist"`
	ToolPolicies  map[string]agent.Policy `json:"tool_policies"`
	MaxSteps      int                     `json:"max_steps"`
	Enabled       *bool                   `json:"enabled"`
}

func (in *agentInput) validate() (uuid.NullUUID, string) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return uuid.NullUUID{}, "name required"
	}
	if in.ProjectID == "" {
		return uuid.NullUUID{}, "project_id required"
	}
	pid, err := uuid.Parse(in.ProjectID)
	if err != nil {
		return uuid.NullUUID{}, "bad project_id"
	}
	if in.MaxSteps <= 0 {
		in.MaxSteps = 50
	}
	if in.MaxSteps > 500 {
		return uuid.NullUUID{}, "max_steps at most 500"
	}
	for _, p := range in.ToolPolicies {
		switch p {
		case agent.PolicyAuto, agent.PolicyAsk, agent.PolicyDeny:
		default:
			return uuid.NullUUID{}, "tool_policies values are auto, ask or deny"
		}
	}
	if in.ToolAllowlist == nil {
		in.ToolAllowlist = []string{}
	}
	if in.ToolPolicies == nil {
		in.ToolPolicies = map[string]agent.Policy{}
	}
	return store.NullUUID(pid), ""
}

// loadAgent checks the caller owns the agent.
func (s *Server) loadAgent(w http.ResponseWriter, r *http.Request) (*store.Agent, bool) {
	id, err := uuidParam(r, "id")
	if err != nil {
		writeErr(w, 400, "bad id")
		return nil, false
	}
	ag, err := s.DB.GetAgent(r.Context(), id)
	if err != nil || ag.OwnerID != Principal(r.Context()).UserID {
		writeErr(w, 404, "not found")
		return nil, false
	}
	return &ag, true
}

func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	p := Principal(r.Context())
	rows, err := s.DB.ListAgentsForUser(r.Context(), p.UserID)
	if err != nil {
		writeErr(w, 500, "db")
		return
	}
	out := make([]agentView, 0, len(rows))
	for _, a := range rows {
		out = append(out, viewAgent(a))
	}
	writeJSON(w, 200, out)
}

// handleListPresets is GET /api/agents/presets: the starting points the
// new-agent form offers (config/presets), in name order.
func (s *Server) handleListPresets(w http.ResponseWriter, r *http.Request) {
	presets := []agents.Preset{}
	if s.Agents != nil {
		presets = append(presets, s.Agents.Presets...)
	}
	writeJSON(w, 200, map[string]any{"presets": presets})
}

// handleImportSkill is POST /api/agents/presets/import {text}: reads one
// SKILL.md (AgentSkills shape) into a preset preview with warnings. It
// saves nothing; the form copies the preview and the person presses
// Create. The text is untrusted: no tools are granted, hidden text is
// removed, and what deserves a look is reported.
func (s *Server) handleImportSkill(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text string `json:"text"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, int64(agents.MaxSkillBytes)*2+4096)
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json, or the file is larger than 64 KB")
		return
	}
	out, err := agents.ImportSkill([]byte(in.Text))
	if err != nil {
		writeErr(w, 400, strings.TrimPrefix(err.Error(), agents.ErrSkill.Error()+": "))
		return
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleCreateAgent(w http.ResponseWriter, r *http.Request) {
	p := Principal(r.Context())
	var in agentInput
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	pid, msg := in.validate()
	if msg != "" {
		writeErr(w, 400, msg)
		return
	}
	if !s.canAccessProject(r.Context(), pid.UUID) {
		writeErr(w, 400, "project not found")
		return
	}
	mp, _ := json.Marshal(in.Model)
	tp, _ := json.Marshal(in.ToolPolicies)
	row, err := s.DB.CreateAgent(r.Context(), store.CreateAgentParams{
		OwnerID: p.UserID, ProjectID: pid, Name: in.Name, Goal: in.Goal, SystemPrompt: in.SystemPrompt,
		ModelPolicy: mp, ToolAllowlist: in.ToolAllowlist, ToolPolicies: tp, MaxSteps: int32(in.MaxSteps),
	})
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 201, viewAgent(row))
}

func (s *Server) handleUpdateAgent(w http.ResponseWriter, r *http.Request) {
	ag, ok := s.loadAgent(w, r)
	if !ok {
		return
	}
	var in agentInput
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	pid, msg := in.validate()
	if msg != "" {
		writeErr(w, 400, msg)
		return
	}
	if !s.canAccessProject(r.Context(), pid.UUID) {
		writeErr(w, 400, "project not found")
		return
	}
	enabled := ag.Enabled
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	mp, _ := json.Marshal(in.Model)
	tp, _ := json.Marshal(in.ToolPolicies)
	row, err := s.DB.UpdateAgent(r.Context(), store.UpdateAgentParams{
		ID: ag.ID, Name: in.Name, Goal: in.Goal, SystemPrompt: in.SystemPrompt, ModelPolicy: mp,
		ToolAllowlist: in.ToolAllowlist, ToolPolicies: tp, MaxSteps: int32(in.MaxSteps), Enabled: enabled, ProjectID: pid,
	})
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, viewAgent(row))
}

func (s *Server) handleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	ag, ok := s.loadAgent(w, r)
	if !ok {
		return
	}
	if err := s.DB.DeleteAgent(r.Context(), ag.ID); err != nil {
		writeErr(w, 500, "db")
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (s *Server) handleSetAgentEnabled(w http.ResponseWriter, r *http.Request) {
	ag, ok := s.loadAgent(w, r)
	if !ok {
		return
	}
	var in struct {
		Enabled bool `json:"enabled"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	if err := s.DB.SetAgentEnabled(r.Context(), store.SetAgentEnabledParams{ID: ag.ID, Enabled: in.Enabled}); err != nil {
		writeErr(w, 500, "db")
		return
	}
	writeJSON(w, 200, map[string]any{"status": "ok", "enabled": in.Enabled})
}

// handleGetAgent is the monitor's one call: the agent, its triggers, its
// recent runs, pending approvals, and this month's spend against any
// agent-scoped budget.
func (s *Server) handleGetAgent(w http.ResponseWriter, r *http.Request) {
	ag, ok := s.loadAgent(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	trigs, _ := s.DB.ListTriggersForAgent(ctx, ag.ID)
	tv := make([]triggerView, 0, len(trigs))
	for _, t := range trigs {
		tv = append(tv, viewTrigger(t))
	}
	runs, _ := s.DB.ListRunsForAgent(ctx, store.ListRunsForAgentParams{AgentID: store.NullUUID(ag.ID), Limit: 30})
	if runs == nil {
		runs = []store.AgentRun{}
	}
	pending, _ := s.DB.ListPendingApprovalsForAgent(ctx, store.NullUUID(ag.ID))
	if pending == nil {
		pending = []store.Approval{}
	}
	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	spent, _ := s.DB.SumUsageForAgentSince(ctx, store.SumUsageForAgentSinceParams{AgentID: store.NullUUID(ag.ID), CreatedAt: monthStart})
	budgets, _ := s.DB.ListBudgetsForScope(ctx, store.ListBudgetsForScopeParams{Scope: "agent", ScopeID: store.NullUUID(ag.ID)})
	if budgets == nil {
		budgets = []store.Budget{}
	}
	writeJSON(w, 200, map[string]any{
		"agent": viewAgent(*ag), "triggers": tv, "runs": runs, "pending_approvals": pending,
		"spend": map[string]any{"since": monthStart, "usd": spent, "budgets": budgets},
	})
}

func (s *Server) handleRunAgent(w http.ResponseWriter, r *http.Request) {
	ag, ok := s.loadAgent(w, r)
	if !ok {
		return
	}
	if s.Agents == nil {
		writeErr(w, 503, "agents are not configured")
		return
	}
	var in struct {
		Input string `json:"input"`
	}
	_ = decode(r, &in)
	run, err := s.Agents.Start(r.Context(), agents.StartParams{AgentID: ag.ID, Input: in.Input, UserID: Principal(r.Context()).UserID, Label: "manual"})
	if err != nil {
		writeAgentsErr(w, err)
		return
	}
	writeJSON(w, 201, run)
}

func (s *Server) handleCreateTrigger(w http.ResponseWriter, r *http.Request) {
	ag, ok := s.loadAgent(w, r)
	if !ok {
		return
	}
	if s.Agents == nil {
		writeErr(w, 503, "agents are not configured")
		return
	}
	var in struct {
		Kind string          `json:"kind"`
		Name string          `json:"name"`
		Spec json.RawMessage `json:"spec"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	t, secret, err := s.Agents.NewTrigger(r.Context(), ag.ID, in.Kind, in.Name, in.Spec)
	if err != nil {
		writeAgentsErr(w, err)
		return
	}
	out := map[string]any{"trigger": viewTrigger(*t)}
	if secret != "" {
		// Shown once. The hook URL needs no session: the secret is the auth.
		out["secret"] = secret
		out["url"] = "/hooks/agents/" + t.ID.String() + "/" + secret
	}
	writeJSON(w, 201, out)
}

func (s *Server) handleDeleteTrigger(w http.ResponseWriter, r *http.Request) {
	ag, ok := s.loadAgent(w, r)
	if !ok {
		return
	}
	tid, err := uuid.Parse(chi.URLParam(r, "tid"))
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	if err := s.DB.DeleteTrigger(r.Context(), store.DeleteTriggerParams{ID: tid, AgentID: ag.ID}); err != nil {
		writeErr(w, 500, "db")
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (s *Server) handleSetTriggerEnabled(w http.ResponseWriter, r *http.Request) {
	ag, ok := s.loadAgent(w, r)
	if !ok {
		return
	}
	tid, err := uuid.Parse(chi.URLParam(r, "tid"))
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	var in struct {
		Enabled bool `json:"enabled"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	if err := s.DB.SetTriggerEnabled(r.Context(), store.SetTriggerEnabledParams{ID: tid, AgentID: ag.ID, Enabled: in.Enabled}); err != nil {
		writeErr(w, 500, "db")
		return
	}
	writeJSON(w, 200, map[string]any{"status": "ok", "enabled": in.Enabled})
}

// memoryView is one agent_memory row without its hash and vector; embedded
// says whether a vector is stored (recall by similarity) or not
// (importance only).
type memoryView struct {
	ID          uuid.UUID     `json:"id"`
	Kind        string        `json:"kind"`
	Content     string        `json:"content"`
	Importance  float32       `json:"importance"`
	Embedded    bool          `json:"embedded"`
	SourceRunID uuid.NullUUID `json:"source_run_id"`
	LastUsedAt  *time.Time    `json:"last_used_at"`
	CreatedAt   time.Time     `json:"created_at"`
}

func viewMemory(m store.AgentMemory) memoryView {
	return memoryView{ID: m.ID, Kind: m.Kind, Content: m.Content, Importance: m.Importance, Embedded: m.Embedding != nil, SourceRunID: m.SourceRunID, LastUsedAt: m.LastUsedAt, CreatedAt: m.CreatedAt}
}

// handleListMemories lists what reflection kept, newest first:
// `?limit=` (default 50, at most 500) and `?offset=`, with the total.
func (s *Server) handleListMemories(w http.ResponseWriter, r *http.Request) {
	ag, ok := s.loadAgent(w, r)
	if !ok {
		return
	}
	limit, offset := 50, 0
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = min(v, 500)
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && v > 0 {
		offset = v
	}
	rows, err := s.DB.ListMemoriesForAgent(r.Context(), store.ListMemoriesForAgentParams{AgentID: ag.ID, Limit: int32(limit), Offset: int32(offset)})
	if err != nil {
		writeErr(w, 500, "db")
		return
	}
	total, _ := s.DB.CountMemoriesForAgent(r.Context(), ag.ID)
	out := make([]memoryView, 0, len(rows))
	for _, m := range rows {
		out = append(out, viewMemory(m))
	}
	writeJSON(w, 200, map[string]any{"memories": out, "total": total})
}

func (s *Server) handleDeleteMemory(w http.ResponseWriter, r *http.Request) {
	ag, ok := s.loadAgent(w, r)
	if !ok {
		return
	}
	mid, err := uuid.Parse(chi.URLParam(r, "mid"))
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	if err := s.DB.DeleteMemory(r.Context(), store.DeleteMemoryParams{ID: mid, AgentID: ag.ID}); err != nil {
		writeErr(w, 500, "db")
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (s *Server) handleClearMemories(w http.ResponseWriter, r *http.Request) {
	ag, ok := s.loadAgent(w, r)
	if !ok {
		return
	}
	if err := s.DB.DeleteMemoriesForAgent(r.Context(), ag.ID); err != nil {
		writeErr(w, 500, "db")
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// loadAgentRun checks the caller owns the run's agent.
func (s *Server) loadAgentRun(w http.ResponseWriter, r *http.Request) (*store.AgentRun, bool) {
	id, err := uuidParam(r, "id")
	if err != nil {
		writeErr(w, 400, "bad id")
		return nil, false
	}
	run, err := s.DB.GetRun(r.Context(), id)
	if err != nil || !run.AgentID.Valid {
		writeErr(w, 404, "not found")
		return nil, false
	}
	ag, err := s.DB.GetAgent(r.Context(), run.AgentID.UUID)
	if err != nil || ag.OwnerID != Principal(r.Context()).UserID {
		writeErr(w, 404, "not found")
		return nil, false
	}
	return &run, true
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	run, ok := s.loadAgentRun(w, r)
	if !ok {
		return
	}
	if s.Agents == nil {
		writeErr(w, 503, "agents are not configured")
		return
	}
	if err := s.Agents.Cancel(r.Context(), run.ID); err != nil {
		writeErr(w, 500, "db")
		return
	}
	writeJSON(w, 200, map[string]string{"status": "cancelled"})
}

// handlePauseRun and handleResumeRun hold and release a run from the
// monitor: pause stops it between steps keeping its place, resume queues
// it again.
func (s *Server) handlePauseRun(w http.ResponseWriter, r *http.Request) {
	run, ok := s.loadAgentRun(w, r)
	if !ok {
		return
	}
	if s.Agents == nil {
		writeErr(w, 503, "agents are not configured")
		return
	}
	if err := s.Agents.Pause(r.Context(), run.ID); err != nil {
		writeAgentsErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "paused_manual"})
}

func (s *Server) handleResumeRun(w http.ResponseWriter, r *http.Request) {
	run, ok := s.loadAgentRun(w, r)
	if !ok {
		return
	}
	if s.Agents == nil {
		writeErr(w, 503, "agents are not configured")
		return
	}
	if err := s.Agents.Resume(r.Context(), run.ID); err != nil {
		writeAgentsErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "queued"})
}

func (s *Server) handleSteerRun(w http.ResponseWriter, r *http.Request) {
	run, ok := s.loadAgentRun(w, r)
	if !ok {
		return
	}
	if s.Agents == nil {
		writeErr(w, 503, "agents are not configured")
		return
	}
	var in struct {
		Text string `json:"text"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	next, err := s.Agents.Steer(r.Context(), run.ID, in.Text, Principal(r.Context()).UserID)
	if err != nil {
		writeAgentsErr(w, err)
		return
	}
	writeJSON(w, 201, next)
}

// handleAgentHook is the unauthenticated webhook: the secret in the path
// is the credential. For a webhook trigger the body (any content type, up
// to 64 KB) becomes the run's input; for a repo_push trigger it is a
// GitHub delivery (X-GitHub-Event names the event; GitHub's payloads can
// be larger, so 1 MB is read and the renderer clips) and one the trigger
// does not want answers 200 without a run.
func (s *Server) handleAgentHook(w http.ResponseWriter, r *http.Request) {
	if s.Agents == nil {
		writeErr(w, 503, "agents are not configured")
		return
	}
	id, err := uuidParam(r, "id")
	if err != nil {
		writeErr(w, 404, "not found")
		return
	}
	secret := chi.URLParam(r, "secret")
	ghEvent := r.Header.Get("X-GitHub-Event")
	limit := int64(agents.MaxInputBytes)
	if ghEvent != "" {
		limit = 1 << 20
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if int64(len(body)) > limit {
		writeErr(w, 413, "body too large")
		return
	}
	run, err := s.Agents.FireHook(r.Context(), agents.FireParams{TriggerID: id, Secret: secret, Body: string(body), GitHubEvent: ghEvent, Signature: r.Header.Get("X-Hub-Signature-256")})
	if err != nil {
		switch {
		case errors.Is(err, agents.ErrSecret):
			writeErr(w, 404, "not found")
		case errors.Is(err, agents.ErrIgnored):
			writeJSON(w, 200, map[string]any{"status": "ignored"})
		default:
			writeAgentsErr(w, err)
		}
		return
	}
	writeJSON(w, 202, map[string]any{"run_id": run.ID, "status": run.Status})
}

func writeAgentsErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, agents.ErrBusy):
		writeErr(w, 409, "the agent already has an open run")
	case errors.Is(err, agents.ErrDisabled):
		writeErr(w, 409, "the agent is disabled")
	case errors.Is(err, agents.ErrNoProject):
		writeErr(w, 400, "the agent has no project")
	case errors.Is(err, agents.ErrInvalid):
		writeErr(w, 400, strings.TrimPrefix(err.Error(), agents.ErrInvalid.Error()+": "))
	default:
		writeErr(w, 500, err.Error())
	}
}
