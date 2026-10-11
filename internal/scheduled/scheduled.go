// Package scheduled runs a member's prompts on their own (PLAN M13 cut 5):
// once at a time, or on a cron expression in the member's time zone. A due
// prompt starts an ordinary chat run in a conversation of the member's
// project; it is not an agent and has no tools unless the member allows
// web_fetch. The worker's minute tick fires due rows, claiming each by
// moving next_run_at first so two workers cannot both fire it.
package scheduled

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/robfig/cron/v3"

	"github.com/jking323/ws/internal/agent"
	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
)

// Limits and kinds.
const (
	KindOnce = "once"
	KindCron = "cron"

	// MaxPerUser bounds how many schedules a member keeps.
	MaxPerUser = 20
	// MinInterval is the shortest gap between two firings of a cron
	// expression; a schedule that fires faster is refused, so a typo cannot
	// spend a budget in an afternoon.
	MinInterval = 15 * time.Minute
	// MaxPromptBytes bounds the prompt.
	MaxPromptBytes = 8 << 10
	// MaxSteps bounds a scheduled run (a few tool rounds at most).
	MaxSteps = 8
)

// Errors with messages fit to show.
var (
	ErrInvalid  = errors.New("scheduled: invalid")
	ErrNotFound = errors.New("scheduled: not found")
)

// Store is what the service uses (*store.DB).
type Store interface {
	CreateScheduledPrompt(ctx context.Context, a store.CreateScheduledPromptParams) (store.ScheduledPrompt, error)
	GetScheduledPrompt(ctx context.Context, a store.GetScheduledPromptParams) (store.ScheduledPrompt, error)
	GetScheduledPromptByID(ctx context.Context, id uuid.UUID) (store.ScheduledPrompt, error)
	ListScheduledPrompts(ctx context.Context, userID uuid.UUID) ([]store.ScheduledPrompt, error)
	CountScheduledPrompts(ctx context.Context, userID uuid.UUID) (int64, error)
	UpdateScheduledPrompt(ctx context.Context, a store.UpdateScheduledPromptParams) (store.ScheduledPrompt, error)
	DeleteScheduledPrompt(ctx context.Context, a store.DeleteScheduledPromptParams) error
	ListDueScheduledPrompts(ctx context.Context, now time.Time) ([]store.ScheduledPrompt, error)
	ClaimScheduledPrompt(ctx context.Context, a store.ClaimScheduledPromptParams) (store.ScheduledPrompt, error)
	SetScheduledPromptResult(ctx context.Context, a store.SetScheduledPromptResultParams) error
	InsertScheduledPromptRun(ctx context.Context, a store.InsertScheduledPromptRunParams) (store.ScheduledPromptRun, error)
	ListScheduledPromptRuns(ctx context.Context, a store.ListScheduledPromptRunsParams) ([]store.ScheduledPromptRun, error)
	ListOpenScheduledPromptRuns(ctx context.Context) ([]store.ListOpenScheduledPromptRunsRow, error)
	FinishScheduledPromptRun(ctx context.Context, a store.FinishScheduledPromptRunParams) error
	SetScheduledPromptRunStatus(ctx context.Context, a store.SetScheduledPromptRunStatusParams) error

	UserCanAccessProject(ctx context.Context, a store.UserCanAccessProjectParams) (bool, error)
	CreateConversation(ctx context.Context, a store.CreateConversationParams) (store.Conversation, error)
	GetConversation(ctx context.Context, id uuid.UUID) (store.Conversation, error)
	NextMessageSeq(ctx context.Context, conversationID uuid.UUID) (int32, error)
	InsertMessage(ctx context.Context, a store.InsertMessageParams) (store.Message, error)
	GetUserByID(ctx context.Context, id uuid.UUID) (store.User, error)
}

// Runs creates a run (*agent.Runtime).
type Runs interface {
	Create(ctx context.Context, p agent.StartParams) (*store.AgentRun, error)
}

// Mailer sends one message (auth.Mailer).
type Mailer interface {
	Send(ctx context.Context, to, subject, text, html string) error
}

// PersonalContext puts the person's card and memories into a run's prompt
// (*memory.Service); optional.
type PersonalContext interface {
	ContextText(ctx context.Context, userID uuid.UUID, input string) string
}

// Service owns schedules and fires them.
type Service struct {
	DB   Store
	Runs Runs
	// Enqueue hands the run to the worker.
	Enqueue func(ctx context.Context, runID uuid.UUID) error
	Mail    Mailer
	Memory  PersonalContext
	// PublicURL is the app origin for links in mail.
	PublicURL string
	Log       *slog.Logger
	Now       func() time.Time
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

// Spec is what a member sets.
type Spec struct {
	ProjectID         uuid.UUID  `json:"project_id"`
	Title             string     `json:"title"`
	Prompt            string     `json:"prompt"`
	Model             string     `json:"model"`
	Kind              string     `json:"kind"`
	Cron              string     `json:"cron"`
	Timezone          string     `json:"timezone"`
	RunAt             *time.Time `json:"run_at"`
	Enabled           bool       `json:"enabled"`
	AllowWebFetch     bool       `json:"allow_web_fetch"`
	NotifyEmail       bool       `json:"notify_email"`
	ReuseConversation bool       `json:"reuse_conversation"`
}

// Schedule is a stored schedule as the page sees it.
type Schedule struct {
	ID                uuid.UUID     `json:"id"`
	ProjectID         uuid.UUID     `json:"project_id"`
	Title             string        `json:"title"`
	Prompt            string        `json:"prompt"`
	Model             string        `json:"model"`
	Kind              string        `json:"kind"`
	Cron              string        `json:"cron"`
	Timezone          string        `json:"timezone"`
	RunAt             *time.Time    `json:"run_at"`
	Enabled           bool          `json:"enabled"`
	AllowWebFetch     bool          `json:"allow_web_fetch"`
	NotifyEmail       bool          `json:"notify_email"`
	ReuseConversation bool          `json:"reuse_conversation"`
	ConversationID    uuid.NullUUID `json:"conversation_id"`
	NextRunAt         *time.Time    `json:"next_run_at"`
	LastRunAt         *time.Time    `json:"last_run_at"`
	LastStatus        string        `json:"last_status"`
	LastError         string        `json:"last_error"`
}

func view(r store.ScheduledPrompt) Schedule {
	return Schedule{ID: r.ID, ProjectID: r.ProjectID, Title: r.Title, Prompt: r.Prompt, Model: r.ModelSelector, Kind: r.Kind, Cron: r.CronExpr,
		Timezone: r.Timezone, RunAt: r.RunAt, Enabled: r.Enabled, AllowWebFetch: r.AllowWebFetch, NotifyEmail: r.NotifyEmail,
		ReuseConversation: r.ReuseConversation, ConversationID: r.ConversationID, NextRunAt: r.NextRunAt, LastRunAt: r.LastRunAt,
		LastStatus: r.LastStatus, LastError: r.LastError}
}

// validate normalises a spec and computes its next firing from now.
func (s *Service) validate(in Spec) (Spec, *time.Time, error) {
	in.Prompt = strings.TrimSpace(in.Prompt)
	in.Title = strings.TrimSpace(in.Title)
	in.Model = strings.TrimSpace(in.Model)
	if in.Prompt == "" {
		return in, nil, fmt.Errorf("%w: the prompt is empty", ErrInvalid)
	}
	if len(in.Prompt) > MaxPromptBytes {
		return in, nil, fmt.Errorf("%w: the prompt is over %d bytes", ErrInvalid, MaxPromptBytes)
	}
	if len(in.Title) > 120 {
		in.Title = in.Title[:120]
	}
	if in.Title == "" {
		in.Title = clip(in.Prompt, 60)
	}
	if in.Model == "" {
		in.Model = "auto"
	}
	if in.Timezone == "" {
		in.Timezone = "UTC"
	}
	loc, err := time.LoadLocation(in.Timezone)
	if err != nil {
		return in, nil, fmt.Errorf("%w: unknown time zone %q", ErrInvalid, in.Timezone)
	}
	now := s.now()
	switch in.Kind {
	case KindOnce:
		if in.RunAt == nil {
			return in, nil, fmt.Errorf("%w: choose when to run it", ErrInvalid)
		}
		if !in.RunAt.After(now) {
			return in, nil, fmt.Errorf("%w: that time has passed", ErrInvalid)
		}
		in.Cron = ""
		t := in.RunAt.UTC()
		return in, &t, nil
	case KindCron:
		sch, err := ParseCron(in.Cron)
		if err != nil {
			return in, nil, err
		}
		first := sch.Next(now.In(loc))
		second := sch.Next(first)
		if first.IsZero() || second.IsZero() {
			return in, nil, fmt.Errorf("%w: that schedule never fires", ErrInvalid)
		}
		if second.Sub(first) < MinInterval {
			return in, nil, fmt.Errorf("%w: a repeating prompt may run at most every %d minutes", ErrInvalid, int(MinInterval.Minutes()))
		}
		in.RunAt = nil
		t := first.UTC()
		return in, &t, nil
	}
	return in, nil, fmt.Errorf("%w: kind is once or cron", ErrInvalid)
}

// ParseCron parses a 5-field cron expression (or @daily and friends).
func ParseCron(expr string) (cron.Schedule, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, fmt.Errorf("%w: a repeating prompt needs a cron expression", ErrInvalid)
	}
	sch, err := cron.ParseStandard(expr)
	if err != nil {
		return nil, fmt.Errorf("%w: cron %q: %v", ErrInvalid, expr, err)
	}
	return sch, nil
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// Create stores a schedule.
func (s *Service) Create(ctx context.Context, userID uuid.UUID, in Spec) (*Schedule, error) {
	in, next, err := s.validate(in)
	if err != nil {
		return nil, err
	}
	if n, err := s.DB.CountScheduledPrompts(ctx, userID); err == nil && n >= MaxPerUser {
		return nil, fmt.Errorf("%w: you have %d scheduled prompts, the most ws keeps; delete one first", ErrInvalid, MaxPerUser)
	}
	if err := s.checkProject(ctx, userID, in.ProjectID); err != nil {
		return nil, err
	}
	if !in.Enabled {
		next = nil
	}
	row, err := s.DB.CreateScheduledPrompt(ctx, store.CreateScheduledPromptParams{
		UserID: userID, ProjectID: in.ProjectID, Title: in.Title, Prompt: in.Prompt, ModelSelector: in.Model, Kind: in.Kind, CronExpr: in.Cron,
		Timezone: in.Timezone, RunAt: in.RunAt, Enabled: in.Enabled, AllowWebFetch: in.AllowWebFetch, NotifyEmail: in.NotifyEmail,
		ReuseConversation: in.ReuseConversation, NextRunAt: next,
	})
	if err != nil {
		return nil, err
	}
	v := view(row)
	return &v, nil
}

// Update replaces a schedule's settings and recomputes its next firing.
func (s *Service) Update(ctx context.Context, userID, id uuid.UUID, in Spec) (*Schedule, error) {
	in, next, err := s.validate(in)
	if err != nil {
		return nil, err
	}
	if err := s.checkProject(ctx, userID, in.ProjectID); err != nil {
		return nil, err
	}
	if !in.Enabled {
		next = nil
	}
	row, err := s.DB.UpdateScheduledPrompt(ctx, store.UpdateScheduledPromptParams{
		ID: id, UserID: userID, ProjectID: in.ProjectID, Title: in.Title, Prompt: in.Prompt, ModelSelector: in.Model, Kind: in.Kind,
		CronExpr: in.Cron, Timezone: in.Timezone, RunAt: in.RunAt, Enabled: in.Enabled, AllowWebFetch: in.AllowWebFetch,
		NotifyEmail: in.NotifyEmail, ReuseConversation: in.ReuseConversation, NextRunAt: next,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	v := view(row)
	return &v, nil
}

func (s *Service) checkProject(ctx context.Context, userID, projectID uuid.UUID) error {
	if projectID == uuid.Nil {
		return fmt.Errorf("%w: choose a project", ErrInvalid)
	}
	ok, err := s.DB.UserCanAccessProject(ctx, store.UserCanAccessProjectParams{ID: projectID, UserID: userID})
	if err != nil || !ok {
		return fmt.Errorf("%w: that project is not yours", ErrInvalid)
	}
	return nil
}

// List returns the member's schedules, newest first.
func (s *Service) List(ctx context.Context, userID uuid.UUID) ([]Schedule, error) {
	rows, err := s.DB.ListScheduledPrompts(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]Schedule, len(rows))
	for i, r := range rows {
		out[i] = view(r)
	}
	return out, nil
}

// Delete removes a schedule and its history.
func (s *Service) Delete(ctx context.Context, userID, id uuid.UUID) error {
	if _, err := s.DB.GetScheduledPrompt(ctx, store.GetScheduledPromptParams{ID: id, UserID: userID}); err != nil {
		return ErrNotFound
	}
	return s.DB.DeleteScheduledPrompt(ctx, store.DeleteScheduledPromptParams{ID: id, UserID: userID})
}

// Run is one execution in a schedule's history.
type Run struct {
	ID             uuid.UUID     `json:"id"`
	RunID          uuid.NullUUID `json:"run_id"`
	ConversationID uuid.NullUUID `json:"conversation_id"`
	Status         string        `json:"status"`
	Error          string        `json:"error"`
	StartedAt      time.Time     `json:"started_at"`
	EndedAt        *time.Time    `json:"ended_at"`
}

// Runs returns a schedule's most recent executions.
func (s *Service) History(ctx context.Context, userID, id uuid.UUID, limit int) ([]Run, error) {
	if _, err := s.DB.GetScheduledPrompt(ctx, store.GetScheduledPromptParams{ID: id, UserID: userID}); err != nil {
		return nil, ErrNotFound
	}
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	rows, err := s.DB.ListScheduledPromptRuns(ctx, store.ListScheduledPromptRunsParams{PromptID: id, Limit: int32(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]Run, len(rows))
	for i, r := range rows {
		out[i] = Run{ID: r.ID, RunID: r.RunID, ConversationID: r.ConversationID, Status: r.Status, Error: r.Error, StartedAt: r.StartedAt, EndedAt: r.EndedAt}
	}
	return out, nil
}

// RunNow fires a schedule immediately without touching its next firing.
func (s *Service) RunNow(ctx context.Context, userID, id uuid.UUID) (*Run, error) {
	row, err := s.DB.GetScheduledPrompt(ctx, store.GetScheduledPromptParams{ID: id, UserID: userID})
	if err != nil {
		return nil, ErrNotFound
	}
	return s.fire(ctx, row)
}

// fire starts the run for one schedule row and records it. Failures are
// recorded in the history rather than returned, except that RunNow wants
// to know: the Run it returns carries the status and error.
func (s *Service) fire(ctx context.Context, row store.ScheduledPrompt) (*Run, error) {
	conv, err := s.conversationFor(ctx, row)
	if err != nil {
		return s.record(ctx, row, uuid.NullUUID{}, uuid.NullUUID{}, "failed", err.Error()), nil
	}
	seq, err := s.DB.NextMessageSeq(ctx, conv.ID)
	if err != nil {
		return s.record(ctx, row, uuid.NullUUID{}, store.NullUUID(conv.ID), "failed", "could not write the prompt"), nil
	}
	parts, _ := json.Marshal([]gateway.Part{gateway.TextPart(row.Prompt)})
	if _, err := s.DB.InsertMessage(ctx, store.InsertMessageParams{ConversationID: conv.ID, Seq: int64(seq), Role: "user", Parts: parts}); err != nil {
		return s.record(ctx, row, uuid.NullUUID{}, store.NullUUID(conv.ID), "failed", "could not write the prompt"), nil
	}
	system := "You are answering a prompt the person scheduled to run on its own; nobody is watching live. Answer it fully in one reply and do not ask questions back. Use Markdown. Today is " + s.now().UTC().Format("Monday 2 January 2006, 15:04 UTC") + "."
	if s.Memory != nil {
		if t := s.Memory.ContextText(ctx, row.UserID, row.Prompt); t != "" {
			system += "\n\n" + t
		}
	}
	allow := []string{agent.NoTools}
	if row.AllowWebFetch {
		allow = []string{"web_fetch"}
	}
	run, err := s.Runs.Create(ctx, agent.StartParams{
		ConversationID: conv.ID, UserID: row.UserID,
		Request:  agent.Request{Selector: row.ModelSelector, System: system, TaskClass: gateway.TaskChat, ToolAllow: allow},
		MaxSteps: MaxSteps,
	})
	if err != nil {
		return s.record(ctx, row, uuid.NullUUID{}, store.NullUUID(conv.ID), "failed", "could not start the run"), nil
	}
	if s.Enqueue != nil {
		if err := s.Enqueue(ctx, run.ID); err != nil {
			return s.record(ctx, row, store.NullUUID(run.ID), store.NullUUID(conv.ID), "failed", "could not queue the run"), nil
		}
	}
	return s.record(ctx, row, store.NullUUID(run.ID), store.NullUUID(conv.ID), "queued", ""), nil
}

// conversationFor is the conversation a firing writes to: the schedule's
// own when it keeps one, else a new one titled after the schedule.
func (s *Service) conversationFor(ctx context.Context, row store.ScheduledPrompt) (store.Conversation, error) {
	if row.ReuseConversation && row.ConversationID.Valid {
		if c, err := s.DB.GetConversation(ctx, row.ConversationID.UUID); err == nil {
			return c, nil
		}
	}
	ok, err := s.DB.UserCanAccessProject(ctx, store.UserCanAccessProjectParams{ID: row.ProjectID, UserID: row.UserID})
	if err != nil || !ok {
		return store.Conversation{}, errors.New("the project is no longer available")
	}
	c, err := s.DB.CreateConversation(ctx, store.CreateConversationParams{
		ProjectID: row.ProjectID, UserID: row.UserID, Title: row.Title, Mode: "chat", ModelSelector: row.ModelSelector, Settings: json.RawMessage("{}"),
	})
	if err != nil {
		return store.Conversation{}, errors.New("could not create the conversation")
	}
	return c, nil
}

func (s *Service) record(ctx context.Context, row store.ScheduledPrompt, run, conv uuid.NullUUID, status, errText string) *Run {
	var ended *time.Time
	if status == "failed" || status == "skipped" {
		t := s.now()
		ended = &t
	}
	rr, err := s.DB.InsertScheduledPromptRun(ctx, store.InsertScheduledPromptRunParams{PromptID: row.ID, RunID: run, ConversationID: conv, Status: status, Error: errText, EndedAt: ended})
	if err != nil {
		s.log().Warn("scheduled: record run", "prompt", row.ID, "err", err)
	}
	if err := s.DB.SetScheduledPromptResult(ctx, store.SetScheduledPromptResultParams{ID: row.ID, LastStatus: status, LastError: errText, ConversationID: conv}); err != nil {
		s.log().Warn("scheduled: set result", "prompt", row.ID, "err", err)
	}
	return &Run{ID: rr.ID, RunID: run, ConversationID: conv, Status: status, Error: errText, StartedAt: rr.StartedAt, EndedAt: ended}
}

// Tick fires what is due and then settles runs that finished. A run missed
// while the worker was down fires once and the next firing is computed from
// now, not replayed.
func (s *Service) Tick(ctx context.Context) error {
	now := s.now()
	due, err := s.DB.ListDueScheduledPrompts(ctx, now)
	if err != nil {
		return err
	}
	for _, d := range due {
		next := s.nextAfter(d, now)
		claimed, err := s.DB.ClaimScheduledPrompt(ctx, store.ClaimScheduledPromptParams{ID: d.ID, Now: now, NextRunAt: next})
		if err != nil {
			continue // another worker took it, or it was edited
		}
		if _, err := s.fire(ctx, claimed); err != nil {
			s.log().Warn("scheduled: fire", "prompt", d.ID, "err", err)
		}
	}
	return s.settle(ctx)
}

// nextAfter is the firing after now, nil when a once-prompt has run.
func (s *Service) nextAfter(r store.ScheduledPrompt, now time.Time) *time.Time {
	if r.Kind != KindCron {
		return nil
	}
	sch, err := ParseCron(r.CronExpr)
	if err != nil {
		return nil
	}
	loc, err := time.LoadLocation(r.Timezone)
	if err != nil {
		loc = time.UTC
	}
	n := sch.Next(now.In(loc)).UTC()
	return &n
}

// settle copies the final status of finished runs into the history and, when
// asked, mails the person a link.
func (s *Service) settle(ctx context.Context) error {
	open, err := s.DB.ListOpenScheduledPromptRuns(ctx)
	if err != nil {
		return err
	}
	for _, o := range open {
		if o.RunStatus == nil {
			// The run row is gone (deleted conversation): close the entry.
			_ = s.DB.FinishScheduledPromptRun(ctx, store.FinishScheduledPromptRunParams{ID: o.ID, Status: "cancelled", Error: "the run no longer exists"})
			continue
		}
		switch *o.RunStatus {
		case "done", "failed", "cancelled":
			errText := o.RunError
			if err := s.DB.FinishScheduledPromptRun(ctx, store.FinishScheduledPromptRunParams{ID: o.ID, Status: *o.RunStatus, Error: errText, Notified: s.notify(ctx, o, *o.RunStatus)}); err != nil {
				s.log().Warn("scheduled: finish run", "err", err)
				continue
			}
			p, perr := s.DB.GetScheduledPromptByID(ctx, o.PromptID)
			if perr == nil {
				_ = s.DB.SetScheduledPromptResult(ctx, store.SetScheduledPromptResultParams{ID: p.ID, LastStatus: *o.RunStatus, LastError: errText})
			}
		case "running", "paused_approval", "paused_steer":
			if o.Status != "running" {
				_ = s.DB.SetScheduledPromptRunStatus(ctx, store.SetScheduledPromptRunStatusParams{ID: o.ID, Status: "running"})
			}
		}
	}
	return nil
}

// notify mails the person when their schedule asked for it; it reports
// whether a mail went out.
func (s *Service) notify(ctx context.Context, o store.ListOpenScheduledPromptRunsRow, status string) bool {
	if s.Mail == nil {
		return false
	}
	p, err := s.DB.GetScheduledPromptByID(ctx, o.PromptID)
	if err != nil || !p.NotifyEmail {
		return false
	}
	u, err := s.DB.GetUserByID(ctx, p.UserID)
	if err != nil || u.DisabledAt != nil {
		return false
	}
	link := ""
	if o.ConversationID.Valid && s.PublicURL != "" {
		link = strings.TrimRight(s.PublicURL, "/") + "/c/" + o.ConversationID.UUID.String()
	}
	subject := "ws: " + p.Title
	body := "Your scheduled prompt \"" + p.Title + "\" finished with status " + status + "."
	if link != "" {
		body += "\n\nThe answer is in the conversation: " + link
	}
	if err := s.Mail.Send(ctx, u.Email, subject, body, ""); err != nil {
		s.log().Warn("scheduled: mail", "err", err)
		return false
	}
	return true
}

// TickJob is Tick under the name the job layer's interface uses.
func (s *Service) TickJob(ctx context.Context) error { return s.Tick(ctx) }
