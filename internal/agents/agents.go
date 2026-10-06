// Package agents runs long-lived agents (PLAN.md M7): durable entities
// with a goal, a system prompt, a model policy, a tool allowlist and
// triggers. A trigger firing (cron from the worker's minute tick, a
// webhook, or a person pressing Run) starts an agent run: a new
// conversation in the agent's project, the trigger's input as the first
// user message, and an agent_runs row driven by the worker through the
// same runtime chat uses, so approvals, checkpoints, resume, budgets and
// the ledger all apply. Steering posts a message into a finished run's
// conversation and starts the next run there. Memory and reflection are
// the next piece.
package agents

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/robfig/cron/v3"

	"github.com/jking323/ws/internal/agent"
	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
)

// Trigger kinds (agent_triggers.kind).
const (
	KindCron     = "cron"
	KindWebhook  = "webhook"
	KindRepoPush = "repo_push" // planned: GitHub App push events
	KindManual   = "manual"
)

// Errors callers map to HTTP statuses.
var (
	ErrInvalid   = errors.New("agents: invalid")
	ErrDisabled  = errors.New("agents: agent is disabled")
	ErrBusy      = errors.New("agents: a run is already open")
	ErrNoProject = errors.New("agents: agent has no project")
	ErrSecret    = errors.New("agents: bad secret")
)

// MaxInputBytes bounds a trigger's input (a webhook body, a steer message).
const MaxInputBytes = 64 << 10

// ModelPolicy is agents.model_policy.
type ModelPolicy struct {
	Selector  string            `json:"selector,omitempty"`
	TaskClass gateway.TaskClass `json:"task_class,omitempty"`
	Reasoning bool              `json:"reasoning,omitempty"`
}

// CronSpec is agent_triggers.spec for kind cron.
type CronSpec struct {
	Expr string `json:"expr"`
	// Input is the first user message of each run; empty means the goal.
	Input string `json:"input,omitempty"`
}

// Store is the slice of the store the service uses (*store.DB implements it).
type Store interface {
	GetAgent(ctx context.Context, id uuid.UUID) (store.Agent, error)
	TouchAgentRun(ctx context.Context, id uuid.UUID) error
	CountOpenRunsForAgent(ctx context.Context, agentID uuid.NullUUID) (int64, error)
	GetRun(ctx context.Context, id uuid.UUID) (store.AgentRun, error)
	CancelRun(ctx context.Context, id uuid.UUID) error
	CreateConversation(ctx context.Context, arg store.CreateConversationParams) (store.Conversation, error)
	SetConversationAgent(ctx context.Context, arg store.SetConversationAgentParams) error
	NextMessageSeq(ctx context.Context, conversationID uuid.UUID) (int32, error)
	InsertMessage(ctx context.Context, arg store.InsertMessageParams) (store.Message, error)
	GetTrigger(ctx context.Context, id uuid.UUID) (store.AgentTrigger, error)
	CreateTrigger(ctx context.Context, arg store.CreateTriggerParams) (store.AgentTrigger, error)
	ListDueCronTriggers(ctx context.Context) ([]store.AgentTrigger, error)
	SetTriggerFired(ctx context.Context, arg store.SetTriggerFiredParams) error
}

// Runtime creates runs (*agent.Runtime does).
type Runtime interface {
	Create(ctx context.Context, p agent.StartParams) (*store.AgentRun, error)
}

// Service starts and steers agent runs.
type Service struct {
	DB      Store
	Runtime Runtime
	// Enqueue hands a run to the worker (River agent.run).
	Enqueue func(ctx context.Context, runID uuid.UUID) error
	// Memory, when set, recalls earlier runs' memories into the system
	// prompt of each new run.
	Memory *Memory
	Log    *slog.Logger
	Now    func() time.Time
}

// StartParams describe one firing.
type StartParams struct {
	AgentID   uuid.UUID
	TriggerID uuid.NullUUID
	// Input is the first user message; empty uses the trigger's input, then
	// the agent's goal.
	Input string
	// UserID is who pressed Run; zero means the agent's owner.
	UserID uuid.UUID
	// Conversation, when set, continues an existing conversation (steer)
	// instead of opening a new one.
	Conversation uuid.NullUUID
	Label        string // conversation title suffix: "cron", "webhook", "manual"
}

// Start creates a conversation (or continues one), posts the input and
// queues the run on the worker. One open run per agent at a time.
func (s *Service) Start(ctx context.Context, p StartParams) (*store.AgentRun, error) {
	ag, err := s.DB.GetAgent(ctx, p.AgentID)
	if err != nil {
		return nil, err
	}
	if !ag.Enabled {
		return nil, ErrDisabled
	}
	if !ag.ProjectID.Valid {
		return nil, ErrNoProject
	}
	if n, err := s.DB.CountOpenRunsForAgent(ctx, store.NullUUID(ag.ID)); err != nil {
		return nil, err
	} else if n > 0 {
		return nil, ErrBusy
	}
	input := strings.TrimSpace(p.Input)
	if len(input) > MaxInputBytes {
		return nil, fmt.Errorf("%w: input over %d bytes", ErrInvalid, MaxInputBytes)
	}
	if input == "" {
		input = strings.TrimSpace(ag.Goal)
	}
	if input == "" {
		input = "Begin."
	}
	userID := p.UserID
	if userID == uuid.Nil {
		userID = ag.OwnerID
	}
	var mp ModelPolicy
	_ = json.Unmarshal(ag.ModelPolicy, &mp)
	if mp.Selector == "" {
		mp.Selector = "auto"
	}
	if mp.TaskClass == "" {
		mp.TaskClass = gateway.TaskChat
	}
	var policies map[string]agent.Policy
	_ = json.Unmarshal(ag.ToolPolicies, &policies)

	convID := p.Conversation
	if !convID.Valid {
		label := p.Label
		if label == "" {
			label = "run"
		}
		title := fmt.Sprintf("%s · %s · %s", ag.Name, label, s.now().UTC().Format("Jan 2 15:04"))
		conv, err := s.DB.CreateConversation(ctx, store.CreateConversationParams{
			ProjectID: ag.ProjectID.UUID, UserID: userID, Title: title, Mode: "agent", ModelSelector: mp.Selector, Settings: json.RawMessage("{}"),
		})
		if err != nil {
			return nil, err
		}
		_ = s.DB.SetConversationAgent(ctx, store.SetConversationAgentParams{ID: conv.ID, AgentID: store.NullUUID(ag.ID)})
		convID = store.NullUUID(conv.ID)
	}
	seq, err := s.DB.NextMessageSeq(ctx, convID.UUID)
	if err != nil {
		return nil, err
	}
	parts, _ := json.Marshal([]gateway.Part{gateway.TextPart(input)})
	if _, err := s.DB.InsertMessage(ctx, store.InsertMessageParams{ConversationID: convID.UUID, Seq: int64(seq), Role: "user", Parts: parts}); err != nil {
		return nil, err
	}
	maxSteps := int(ag.MaxSteps)
	if maxSteps <= 0 {
		maxSteps = 50
	}
	// Memories from earlier runs go into the system prompt; a recall
	// failure starts the run without them rather than not at all.
	system := SystemPrompt(ag)
	if s.Memory != nil {
		if mem, err := s.Memory.Recall(ctx, ag, input); err != nil {
			s.log().Warn("agents: recall failed, starting without memories", "agent", ag.ID, "err", err)
		} else if block := RenderRecall(mem); block != "" {
			system += "\n\n" + block
		}
	}
	run, err := s.Runtime.Create(ctx, agent.StartParams{
		ConversationID: convID.UUID, UserID: userID, AgentID: store.NullUUID(ag.ID), TriggerID: p.TriggerID,
		Request:  agent.Request{Selector: mp.Selector, System: system, TaskClass: mp.TaskClass, Reasoning: mp.Reasoning, ToolAllow: ag.ToolAllowlist},
		Policies: policies, MaxSteps: maxSteps,
	})
	if err != nil {
		return nil, err
	}
	if s.Enqueue != nil {
		if err := s.Enqueue(ctx, run.ID); err != nil {
			return nil, fmt.Errorf("agents: enqueue: %w", err)
		}
	}
	_ = s.DB.TouchAgentRun(ctx, ag.ID)
	return run, nil
}

// SystemPrompt is what an agent run sends as the system prompt: the
// agent's own prompt, its goal, and how to behave as an unattended run.
func SystemPrompt(ag store.Agent) string {
	var b strings.Builder
	if p := strings.TrimSpace(ag.SystemPrompt); p != "" {
		b.WriteString(p)
	} else {
		b.WriteString("You are " + ag.Name + ", an autonomous agent running unattended in the ws workspace.")
	}
	if g := strings.TrimSpace(ag.Goal); g != "" {
		b.WriteString("\n\nYour standing goal: " + g)
	}
	b.WriteString("\n\nYou run as a background job: nobody is watching live. Work the goal with the tools you have, then end with a short report of what you did, what you found and what you could not do. Use ask_user only for a decision you truly cannot make; it ends this run and the question waits in the agent's monitor until someone answers, which starts the next run with their reply.")
	return b.String()
}

// Steer posts a message into a run's conversation and starts the next run
// there. The run must not be open; an open one is busy.
func (s *Service) Steer(ctx context.Context, runID uuid.UUID, text string, userID uuid.UUID) (*store.AgentRun, error) {
	run, err := s.DB.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if !run.AgentID.Valid {
		return nil, fmt.Errorf("%w: not an agent run", ErrInvalid)
	}
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("%w: message is required", ErrInvalid)
	}
	switch run.Status {
	case "queued", "running", "paused_approval", "paused_steer":
		return nil, ErrBusy
	}
	return s.Start(ctx, StartParams{AgentID: run.AgentID.UUID, Input: text, UserID: userID, Conversation: store.NullUUID(run.ConversationID), Label: "steer"})
}

// Cancel marks a run cancelled; the runtime stops at its next step.
func (s *Service) Cancel(ctx context.Context, runID uuid.UUID) error {
	return s.DB.CancelRun(ctx, runID)
}

// ---- triggers ----

// ParseCron parses a 5-field cron expression (or @hourly and friends).
func ParseCron(expr string) (cron.Schedule, error) {
	sch, err := cron.ParseStandard(strings.TrimSpace(expr))
	if err != nil {
		return nil, fmt.Errorf("%w: cron %q: %v", ErrInvalid, expr, err)
	}
	return sch, nil
}

// NewTrigger validates and creates a trigger. For a webhook it returns the
// secret once; only its hash is stored.
func (s *Service) NewTrigger(ctx context.Context, agentID uuid.UUID, kind, name string, spec json.RawMessage) (*store.AgentTrigger, string, error) {
	var hash []byte
	var next *time.Time
	secret := ""
	if len(spec) == 0 {
		spec = json.RawMessage("{}")
	}
	switch kind {
	case KindCron:
		var cs CronSpec
		if err := json.Unmarshal(spec, &cs); err != nil || cs.Expr == "" {
			return nil, "", fmt.Errorf("%w: cron trigger needs spec.expr", ErrInvalid)
		}
		sch, err := ParseCron(cs.Expr)
		if err != nil {
			return nil, "", err
		}
		n := sch.Next(s.now())
		next = &n
		spec, _ = json.Marshal(cs)
	case KindWebhook:
		b := make([]byte, 24)
		if _, err := rand.Read(b); err != nil {
			return nil, "", err
		}
		secret = hex.EncodeToString(b)
		sum := sha256.Sum256([]byte(secret))
		hash = sum[:]
	case KindManual:
	default:
		return nil, "", fmt.Errorf("%w: trigger kind %q", ErrInvalid, kind)
	}
	t, err := s.DB.CreateTrigger(ctx, store.CreateTriggerParams{AgentID: agentID, Kind: kind, Name: strings.TrimSpace(name), Spec: spec, SecretHash: hash, NextRunAt: next})
	if err != nil {
		return nil, "", err
	}
	return &t, secret, nil
}

// Fire starts a run from a webhook trigger after checking its secret.
// The body becomes the run's input.
func (s *Service) Fire(ctx context.Context, triggerID uuid.UUID, secret string, body string) (*store.AgentRun, error) {
	t, err := s.DB.GetTrigger(ctx, triggerID)
	if err != nil {
		return nil, ErrSecret // do not reveal whether the id exists
	}
	sum := sha256.Sum256([]byte(secret))
	if t.Kind != KindWebhook || !t.Enabled || len(t.SecretHash) != len(sum) || subtle.ConstantTimeCompare(t.SecretHash, sum[:]) != 1 {
		return nil, ErrSecret
	}
	input := strings.TrimSpace(body)
	if len(input) > MaxInputBytes {
		input = input[:MaxInputBytes]
	}
	if input != "" {
		input = "Webhook payload:\n\n" + input
	}
	run, err := s.Start(ctx, StartParams{AgentID: t.AgentID, TriggerID: store.NullUUID(t.ID), Input: input, Label: "webhook"})
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	_ = s.DB.SetTriggerFired(ctx, store.SetTriggerFiredParams{ID: t.ID, NextRunAt: nil, LastError: nilIfEmpty(msg)})
	return run, err
}

// Tick starts every due cron trigger and schedules its next firing. A
// busy agent skips this firing (recorded as last_error) rather than
// queueing a second run. The worker calls it every minute.
func (s *Service) Tick(ctx context.Context) error {
	due, err := s.DB.ListDueCronTriggers(ctx)
	if err != nil {
		return err
	}
	for _, t := range due {
		var cs CronSpec
		_ = json.Unmarshal(t.Spec, &cs)
		sch, perr := ParseCron(cs.Expr)
		var next *time.Time
		if perr == nil {
			n := sch.Next(s.now())
			next = &n
		}
		_, serr := s.Start(ctx, StartParams{AgentID: t.AgentID, TriggerID: store.NullUUID(t.ID), Input: cs.Input, Label: "cron"})
		msg := ""
		switch {
		case perr != nil:
			msg = perr.Error() // next stays nil: the trigger stops until fixed
		case serr != nil:
			msg = serr.Error()
		}
		if err := s.DB.SetTriggerFired(ctx, store.SetTriggerFiredParams{ID: t.ID, NextRunAt: next, LastError: nilIfEmpty(msg)}); err != nil {
			s.log().Warn("agents: trigger update", "trigger", t.ID, "err", err)
		}
		if serr != nil && !errors.Is(serr, ErrBusy) {
			s.log().Warn("agents: cron start failed", "trigger", t.ID, "agent", t.AgentID, "err", serr)
		}
	}
	return nil
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
