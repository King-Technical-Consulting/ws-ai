package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
	"github.com/jking323/ws/internal/store/blob"
)

// Runtime executes runs. One instance per process.
type Runtime struct {
	DB    *store.DB
	GW    *gateway.Gateway
	Tools *Registry
	Blobs blob.Store
	Log   *slog.Logger
	// PID identifies this process in agent_runs.owner_pid.
	PID string
}

// New builds a runtime.
func New(db *store.DB, gw *gateway.Gateway, tools *Registry, blobs blob.Store, log *slog.Logger) *Runtime {
	host, _ := os.Hostname()
	if log == nil {
		log = slog.Default()
	}
	return &Runtime{DB: db, GW: gw, Tools: tools, Blobs: blobs, Log: log, PID: fmt.Sprintf("%s:%d", host, os.Getpid())}
}

// Request is the per-run request skeleton stored on agent_runs.request.
// Messages are never stored here; they come from the conversation.
type Request struct {
	Selector  string            `json:"selector"`
	System    string            `json:"system"`
	TaskClass gateway.TaskClass `json:"task_class"`
	Reasoning bool              `json:"reasoning"`
	ToolAllow []string          `json:"tool_allow,omitempty"` // empty = all registered tools
	NoTools   bool              `json:"no_tools,omitempty"`
}

// StartParams describe a new run.
type StartParams struct {
	ConversationID uuid.UUID
	UserID         uuid.UUID
	AgentID        uuid.NullUUID
	TriggerID      uuid.NullUUID
	Request        Request
	Policies       map[string]Policy // overrides on tool defaults
	MaxSteps       int
}

// Create inserts a run in the queued state.
func (rt *Runtime) Create(ctx context.Context, p StartParams) (*store.AgentRun, error) {
	reqJSON, _ := json.Marshal(p.Request)
	pol := rt.Tools.Policies(p.Policies)
	polJSON, _ := json.Marshal(pol)
	if p.MaxSteps <= 0 {
		p.MaxSteps = 12
	}
	run, err := rt.DB.CreateRun(ctx, store.CreateRunParams{
		AgentID: p.AgentID, ConversationID: p.ConversationID, UserID: uuid.NullUUID{UUID: p.UserID, Valid: p.UserID != uuid.Nil},
		TriggerID: p.TriggerID, Status: "queued", Request: reqJSON, ToolPolicies: polJSON, MaxSteps: int32(p.MaxSteps),
	})
	if err != nil {
		return nil, err
	}
	return &run, nil
}

// Errors.
var (
	ErrRunNotClaimable = errors.New("agent: run is not claimable (running elsewhere or finished)")
	ErrPaused          = errors.New("agent: run paused for approval")
)

// Drive claims the run and executes steps until it finishes, pauses, or
// fails. It is safe to call on a run another process abandoned (stale
// heartbeat): reconciliation finishes any half-done tool work first.
func (rt *Runtime) Drive(ctx context.Context, runID uuid.UUID, sink Sink) error {
	run, err := rt.DB.ClaimRun(ctx, store.ClaimRunParams{ID: runID, OwnerPid: &rt.PID})
	if err != nil {
		return ErrRunNotClaimable
	}
	return rt.drive(ctx, &run, sink)
}

// Resume continues a run paused for approval after decisions were recorded.
func (rt *Runtime) Resume(ctx context.Context, runID uuid.UUID, sink Sink) error {
	run, err := rt.DB.ResumeRunAfterApproval(ctx, store.ResumeRunAfterApprovalParams{ID: runID, OwnerPid: &rt.PID})
	if err != nil {
		return ErrRunNotClaimable
	}
	return rt.drive(ctx, &run, sink)
}

func (rt *Runtime) drive(ctx context.Context, run *store.AgentRun, sink Sink) (retErr error) {
	if sink == nil {
		sink = NopSink{}
	}
	var req Request
	_ = json.Unmarshal(run.Request, &req)
	var policies map[string]Policy
	_ = json.Unmarshal(run.ToolPolicies, &policies)
	// Re-resolve against this process's registry: the run may have been
	// created by serve, which doesn't register the worker's sandbox tools,
	// so tools unknown there must still get their own default (bash: ask).
	policies = rt.Tools.Policies(policies)
	bg := context.WithoutCancel(ctx)

	// heartbeat while we own the run
	hbCtx, stopHB := context.WithCancel(ctx)
	defer stopHB()
	go func() {
		t := time.NewTicker(20 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-t.C:
				_ = rt.DB.HeartbeatRun(bg, store.HeartbeatRunParams{ID: run.ID, OwnerPid: &rt.PID})
			}
		}
	}()

	fail := func(err error) error {
		msg := userFacing(err)
		sink.Error(msg)
		sink.Finish("error")
		_ = rt.DB.SetRunStatus(bg, store.SetRunStatusParams{ID: run.ID, Status: "failed", Error: &msg})
		return err
	}

	// Stream into the run's first assistant message id; create it if new.
	firstID := run.FirstMessageID
	started := firstID.Valid
	if started {
		sink.Start(firstID.UUID.String())
	}

	// 1. Reconcile pending tool calls left by a previous process or by an
	//    approval pause: execute decided ones, keep waiting on undecided.
	paused, err := rt.reconcile(ctx, run, req, policies, sink)
	if err != nil {
		return fail(err)
	}
	if paused {
		sink.Finish("tool-calls")
		return ErrPaused
	}

	// 2. Step loop
	for int(run.StepCount) < int(run.MaxSteps) {
		if ctx.Err() != nil {
			// Client went away mid-run. Leave the run running with a fresh
			// heartbeat; the reaper will hand it to the worker.
			return ctx.Err()
		}
		// Cancelled or paused from the monitor between steps: stop here,
		// keeping that status (and not marking the run done). A paused run
		// is re-queued by the monitor's resume.
		if cur, err := rt.DB.GetRun(ctx, run.ID); err == nil {
			switch cur.Status {
			case "cancelled":
				sink.Data("notice", map[string]string{"text": "cancelled"}, false)
				sink.Finish("stop")
				return nil
			case "paused_manual":
				sink.Data("notice", map[string]string{"text": "paused"}, false)
				sink.Finish("stop")
				return ErrPaused
			}
		}
		history, err := rt.history(ctx, run.ConversationID)
		if err != nil {
			return fail(err)
		}
		// If the last message is an assistant message with no tool calls,
		// the previous step finished the turn.
		if n := len(history); n > 0 && history[n-1].Role == gateway.RoleAssistant && !hasToolCalls(history[n-1]) && run.StepCount > 0 {
			break
		}

		arow, err := rt.placeholder(ctx, run.ConversationID)
		if err != nil {
			return fail(err)
		}
		if !started {
			started = true
			firstID = uuid.NullUUID{UUID: arow.ID, Valid: true}
			_ = rt.DB.SetRunFirstMessage(bg, store.SetRunFirstMessageParams{ID: run.ID, FirstMessageID: firstID})
			sink.Start(arow.ID.String())
		}
		sink.StartStep()

		seq, _ := rt.DB.NextStepSeq(ctx, run.ID)
		stepIn, _ := json.Marshal(map[string]any{"selector": req.Selector, "task_class": req.TaskClass})
		step, err := rt.DB.InsertStep(ctx, store.InsertStepParams{RunID: run.ID, Seq: int32(seq), Kind: "llm", Input: stepIn})
		if err != nil {
			return fail(err)
		}

		gwReq := rt.buildRequest(run, req, history)
		ch, err := rt.GW.Stream(ctx, gwReq)
		if err != nil && errors.Is(err, gateway.ErrNoRoute) && len(gwReq.Tools) > 0 {
			gwReq.Tools = nil // model can't call tools; answer without them
			ch, err = rt.GW.Stream(ctx, gwReq)
		}
		if err != nil {
			rt.finishStep(bg, step.ID, nil, nil, err)
			_ = rt.DB.DeleteMessagesFromSeq(bg, store.DeleteMessagesFromSeqParams{ConversationID: run.ConversationID, Seq: arow.Seq})
			sink.FinishStep()
			return fail(err)
		}
		resp, streamErr := relay(ch, sink)
		rt.persistAssistant(bg, arow.ID, resp, streamErr)
		cost := rt.cost(resp)
		_ = rt.DB.BumpRunStep(bg, store.BumpRunStepParams{ID: run.ID, CostUsd: cost})
		run.StepCount++
		out, _ := json.Marshal(map[string]any{"endpoint": resp.EndpointID, "model": resp.Model, "finish": resp.FinishReason, "cost_usd": cost})
		usage, _ := json.Marshal(resp.Usage)
		cp, _ := json.Marshal(map[string]any{"message_seq": arow.Seq, "pending_tool_calls": callIDs(resp.ToolCalls())})
		rt.finishStep(bg, step.ID, out, usage, streamErr)
		if cp != nil {
			_ = rt.DB.UpdateStepCheckpoint(bg, store.UpdateStepCheckpointParams{ID: step.ID, Checkpoint: cp})
		}
		if streamErr != nil {
			sink.FinishStep()
			return fail(streamErr)
		}

		calls := resp.ToolCalls()
		if len(calls) == 0 {
			sink.FinishStep()
			break
		}
		// 3. Tools
		paused, err := rt.runTools(ctx, run, policies, arow, calls, sink)
		sink.FinishStep()
		if err != nil {
			return fail(err)
		}
		if paused {
			sink.Finish("tool-calls")
			return ErrPaused
		}
	}

	if int(run.StepCount) >= int(run.MaxSteps) {
		msg := fmt.Sprintf("stopped after %d steps", run.MaxSteps)
		sink.Data("notice", map[string]string{"text": msg}, false)
	}
	sink.Finish("stop")
	_ = rt.DB.SetRunStatus(bg, store.SetRunStatusParams{ID: run.ID, Status: "done"})
	_ = rt.DB.TouchConversation(bg, run.ConversationID)
	return nil
}

// reconcile handles tool calls from the latest assistant message that have
// no result yet: approvals that were decided get executed (or denied),
// undecided ones keep the run paused, and calls abandoned by a crashed
// process are re-run if idempotent or failed with a clear note otherwise.
func (rt *Runtime) reconcile(ctx context.Context, run *store.AgentRun, req Request, policies map[string]Policy, sink Sink) (paused bool, err error) {
	history, err := rt.history(ctx, run.ConversationID)
	if err != nil {
		return false, err
	}
	// find the last assistant message and whether a tool message follows it
	var last *gateway.Message
	var lastRow store.Message
	rows, _ := rt.DB.ListMessages(ctx, run.ConversationID)
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].Role == "assistant" {
			var parts []gateway.Part
			_ = json.Unmarshal(rows[i].Parts, &parts)
			if len(parts) > 0 {
				m := gateway.Message{Role: gateway.RoleAssistant, Parts: parts, Seq: rows[i].Seq}
				last, lastRow = &m, rows[i]
			}
			break
		}
		if rows[i].Role == "user" {
			break
		}
	}
	_ = history
	if last == nil {
		return false, nil
	}
	calls := last.Parts
	answered := map[string]bool{}
	for _, r := range rows {
		if r.Seq <= lastRow.Seq || r.Role != "tool" {
			continue
		}
		var parts []gateway.Part
		_ = json.Unmarshal(r.Parts, &parts)
		for _, p := range parts {
			if p.Kind == gateway.PartToolResult {
				answered[p.ToolCallID] = true
			}
		}
	}
	var pending []gateway.Part
	for _, p := range calls {
		if p.Kind == gateway.PartToolCall && !answered[p.ToolCallID] {
			pending = append(pending, p)
		}
	}
	if len(pending) == 0 {
		return false, nil
	}
	return rt.runTools(ctx, run, policies, &lastRow, pending, sink)
}

// runTools executes a batch of tool calls from assistant row `arow`.
// Returns paused=true if any call awaits approval.
func (rt *Runtime) runTools(ctx context.Context, run *store.AgentRun, policies map[string]Policy, arow *store.Message, calls []gateway.Part, sink Sink) (bool, error) {
	bg := context.WithoutCancel(ctx)
	approvals, _ := rt.DB.ListApprovalsForRun(ctx, run.ID)
	decided := map[string]store.Approval{}
	for _, a := range approvals {
		decided[a.ToolCallID] = a
	}
	var results []gateway.Part
	paused := false
	for _, c := range calls {
		tool, ok := rt.Tools.Get(c.ToolName)
		pol := policies[c.ToolName]
		if !ok {
			results = append(results, toolResult(c.ToolCallID, fmt.Sprintf("Error: tool %q is not available", c.ToolName), true))
			sink.ToolOutput(c.ToolCallID, nil, true, "tool not available")
			continue
		}
		if a, had := decided[c.ToolCallID]; had {
			switch a.Status {
			case "approved":
				// fall through to execution
			case "denied", "expired":
				msg := "The user declined this action."
				if a.Note != nil && *a.Note != "" {
					msg += " Note: " + *a.Note
				}
				results = append(results, toolResult(c.ToolCallID, msg, true))
				sink.ToolOutput(c.ToolCallID, nil, true, msg)
				continue
			default: // pending
				sink.ApprovalRequest(a.ID.String(), c.ToolCallID)
				paused = true
				continue
			}
		} else if pol == PolicyDeny {
			results = append(results, toolResult(c.ToolCallID, "Error: this tool is disabled by policy for this run.", true))
			sink.ToolOutput(c.ToolCallID, nil, true, "disabled by policy")
			continue
		} else if pol == PolicyAsk {
			seq, _ := rt.DB.NextStepSeq(ctx, run.ID)
			a, err := rt.DB.CreateApproval(ctx, store.CreateApprovalParams{RunID: run.ID, StepSeq: int32(seq), ToolCallID: c.ToolCallID, ToolName: c.ToolName, Args: nonEmptyJSON(c.Args)})
			if err != nil {
				return false, err
			}
			sink.ApprovalRequest(a.ID.String(), c.ToolCallID)
			paused = true
			continue
		}

		// idempotency: a started-but-unfinished tool step for this call means a
		// previous process died mid-call
		if prev, err := rt.DB.FindToolStep(ctx, store.FindToolStepParams{RunID: run.ID, CallID: c.ToolCallID}); err == nil && prev.EndedAt == nil && !tool.Idempotent() {
			msg := fmt.Sprintf("Error: %s was interrupted before it finished and is not safe to retry automatically. Ask the user whether to run it again.", c.ToolName)
			results = append(results, toolResult(c.ToolCallID, msg, true))
			sink.ToolOutput(c.ToolCallID, nil, true, msg)
			_ = rt.DB.FinishStep(bg, store.FinishStepParams{ID: prev.ID, Error: &msg})
			continue
		}

		seq, _ := rt.DB.NextStepSeq(ctx, run.ID)
		stepIn, _ := json.Marshal(map[string]any{"name": c.ToolName, "call_id": c.ToolCallID, "args": json.RawMessage(nonEmptyJSON(c.Args))})
		step, _ := rt.DB.InsertStep(ctx, store.InsertStepParams{RunID: run.ID, Seq: int32(seq), Kind: "tool", Input: stepIn})

		tc := ToolCtx{RunID: run.ID, ConversationID: run.ConversationID, Blobs: rt.Blobs,
			MessageID: uuid.NullUUID{UUID: arow.ID, Valid: true},
			Emit:      func(name string, data any) { sink.Data(name, data, true) }}
		if run.UserID.Valid {
			tc.UserID = run.UserID.UUID
		}
		start := time.Now()
		res, err := tool.Call(ctx, tc, nonEmptyJSON(c.Args))
		if err != nil {
			res = ErrorResult(err)
		}
		text, externalized := externalize(bg, rt.Blobs, c.ToolName, res.Text)
		out := res.Data
		if out == nil {
			out = Truncate(res.Text, 4000)
		}
		if res.IsError {
			sink.ToolOutput(c.ToolCallID, out, true, Truncate(res.Text, 500))
		} else {
			sink.ToolOutput(c.ToolCallID, out, false, "")
		}
		results = append(results, toolResult(c.ToolCallID, text, res.IsError))
		stepOut, _ := json.Marshal(map[string]any{"duration_ms": time.Since(start).Milliseconds(), "externalized": externalized, "is_error": res.IsError, "preview": Truncate(res.Text, 1000)})
		var stepErr error
		if res.IsError {
			stepErr = errors.New(Truncate(res.Text, 500))
		}
		rt.finishStep(bg, step.ID, stepOut, nil, stepErr)

		if c.ToolName == "ask_user" {
			// hand control back; the user's reply starts a new run
			paused = false
		}
	}
	if len(results) > 0 {
		seq, err := rt.DB.NextMessageSeq(ctx, run.ConversationID)
		if err != nil {
			return false, err
		}
		b, _ := json.Marshal(results)
		if _, err := rt.DB.InsertMessage(bg, store.InsertMessageParams{ConversationID: run.ConversationID, Seq: int64(seq), Role: "tool", Parts: b}); err != nil {
			return false, err
		}
	}
	if paused {
		_ = rt.DB.SetRunStatus(bg, store.SetRunStatusParams{ID: run.ID, Status: "paused_approval"})
	}
	return paused, nil
}

// ---- helpers ----

func (rt *Runtime) history(ctx context.Context, convID uuid.UUID) ([]gateway.Message, error) {
	rows, err := rt.DB.ListMessages(ctx, convID)
	if err != nil {
		return nil, err
	}
	out := make([]gateway.Message, 0, len(rows))
	for _, r := range rows {
		var parts []gateway.Part
		if err := json.Unmarshal(r.Parts, &parts); err != nil || len(parts) == 0 {
			continue
		}
		out = append(out, gateway.Message{Role: gateway.Role(r.Role), Parts: parts, Seq: r.Seq})
	}
	return out, nil
}

func (rt *Runtime) placeholder(ctx context.Context, convID uuid.UUID) (*store.Message, error) {
	seq, err := rt.DB.NextMessageSeq(ctx, convID)
	if err != nil {
		return nil, err
	}
	row, err := rt.DB.InsertMessage(ctx, store.InsertMessageParams{ConversationID: convID, Seq: int64(seq), Role: "assistant", Parts: json.RawMessage("[]")})
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// buildRequest assembles the gateway request: system prompt, tools, and
// history with a cache breakpoint before the newest user turn so the
// stable prefix is cached across steps.
func (rt *Runtime) buildRequest(run *store.AgentRun, req Request, history []gateway.Message) *gateway.Request {
	msgs := make([]gateway.Message, len(history))
	copy(msgs, history)
	// cache hint on the last part of the message preceding the latest user message
	lastUser := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == gateway.RoleUser {
			lastUser = i
			break
		}
	}
	if lastUser > 0 {
		prev := msgs[lastUser-1]
		parts := make([]gateway.Part, len(prev.Parts))
		copy(parts, prev.Parts)
		if n := len(parts); n > 0 && parts[n-1].Kind == gateway.PartText {
			parts[n-1].CacheHint = true
		}
		msgs[lastUser-1].Parts = parts
	}
	g := &gateway.Request{
		Model: req.Selector, System: req.System, SystemCache: true, Messages: msgs,
		Metadata: gateway.Metadata{ConversationID: run.ConversationID.String(), AgentRunID: run.ID.String(), TaskClass: req.TaskClass},
	}
	if run.UserID.Valid {
		g.Metadata.UserID = run.UserID.UUID.String()
	}
	if run.AgentID.Valid {
		g.Metadata.AgentID = run.AgentID.UUID.String()
	}
	if req.Reasoning {
		g.Reasoning = &gateway.ReasoningConfig{Enabled: true}
	}
	if !req.NoTools {
		g.Tools = rt.Tools.Defs(req.ToolAllow)
	}
	return g
}

func (rt *Runtime) persistAssistant(ctx context.Context, id uuid.UUID, resp *gateway.Response, streamErr error) {
	parts, _ := json.Marshal(resp.Parts)
	usage, _ := json.Marshal(resp.Usage)
	fr := string(resp.FinishReason)
	if streamErr != nil && fr == "" {
		fr = string(gateway.FinishError)
	}
	if err := rt.DB.UpdateMessageParts(ctx, store.UpdateMessagePartsParams{
		ID: id, Parts: parts, EndpointID: strPtr(resp.EndpointID), Model: strPtr(resp.Model), Usage: usage, FinishReason: strPtr(fr),
	}); err != nil {
		rt.Log.Error("agent: persist assistant", "err", err)
	}
}

func (rt *Runtime) finishStep(ctx context.Context, id uuid.UUID, out, usage json.RawMessage, err error) {
	var e *string
	if err != nil {
		s := err.Error()
		e = &s
	}
	_ = rt.DB.FinishStep(ctx, store.FinishStepParams{ID: id, Output: out, Usage: usage, Error: e})
}

func (rt *Runtime) cost(resp *gateway.Response) float64 {
	if ep, ok := rt.GW.Registry.Endpoint(resp.EndpointID); ok {
		return ep.Pricing.Cost(resp.Usage)
	}
	return 0
}

// relay forwards gateway events to the sink and accumulates the response.
func relay(ch <-chan gateway.StreamEvent, sink Sink) (*gateway.Response, error) {
	tee := make(chan gateway.StreamEvent, 64)
	done := make(chan struct{})
	var resp *gateway.Response
	var accErr error
	go func() {
		resp, accErr = gateway.Accumulate(tee)
		close(done)
	}()
	for ev := range ch {
		tee <- ev
		sink.Model(ev)
	}
	close(tee)
	<-done
	return resp, accErr
}

func toolResult(callID, text string, isErr bool) gateway.Part {
	return gateway.Part{Kind: gateway.PartToolResult, ToolCallID: callID, IsError: isErr, Content: []gateway.Part{gateway.TextPart(text)}}
}

func hasToolCalls(m gateway.Message) bool {
	for _, p := range m.Parts {
		if p.Kind == gateway.PartToolCall {
			return true
		}
	}
	return false
}

func callIDs(parts []gateway.Part) []string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, p.ToolCallID)
	}
	return out
}

func nonEmptyJSON(b json.RawMessage) json.RawMessage {
	if len(strings.TrimSpace(string(b))) == 0 {
		return json.RawMessage("{}")
	}
	return b
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func userFacing(err error) string {
	switch {
	case errors.Is(err, gateway.ErrBudgetExceeded):
		return "Your budget for this period is spent. Ask the owner to raise it, or pick a local model."
	case errors.Is(err, gateway.ErrNoRoute):
		return "No model is available for this request right now."
	case errors.Is(err, context.Canceled):
		return "Stopped."
	}
	return err.Error()
}
