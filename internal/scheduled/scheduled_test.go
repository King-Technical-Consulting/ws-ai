package scheduled

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jking323/ws/internal/agent"
	"github.com/jking323/ws/internal/store"
)

type fakeDB struct {
	mu       sync.Mutex
	prompts  map[uuid.UUID]store.ScheduledPrompt
	runs     []store.ScheduledPromptRun
	convs    map[uuid.UUID]store.Conversation
	msgs     map[uuid.UUID]int
	projects map[uuid.UUID]uuid.UUID // project -> owner
	users    map[uuid.UUID]store.User
	runState map[uuid.UUID]string // agent run id -> status
}

func newFake() *fakeDB {
	return &fakeDB{prompts: map[uuid.UUID]store.ScheduledPrompt{}, convs: map[uuid.UUID]store.Conversation{}, msgs: map[uuid.UUID]int{},
		projects: map[uuid.UUID]uuid.UUID{}, users: map[uuid.UUID]store.User{}, runState: map[uuid.UUID]string{}}
}

func (f *fakeDB) CreateScheduledPrompt(_ context.Context, a store.CreateScheduledPromptParams) (store.ScheduledPrompt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := store.ScheduledPrompt{ID: uuid.New(), UserID: a.UserID, ProjectID: a.ProjectID, Title: a.Title, Prompt: a.Prompt, ModelSelector: a.ModelSelector,
		Kind: a.Kind, CronExpr: a.CronExpr, Timezone: a.Timezone, RunAt: a.RunAt, Enabled: a.Enabled, AllowWebFetch: a.AllowWebFetch,
		NotifyEmail: a.NotifyEmail, ReuseConversation: a.ReuseConversation, NextRunAt: a.NextRunAt}
	f.prompts[r.ID] = r
	return r, nil
}
func (f *fakeDB) GetScheduledPrompt(_ context.Context, a store.GetScheduledPromptParams) (store.ScheduledPrompt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.prompts[a.ID]
	if !ok || r.UserID != a.UserID {
		return store.ScheduledPrompt{}, pgx.ErrNoRows
	}
	return r, nil
}
func (f *fakeDB) GetScheduledPromptByID(_ context.Context, id uuid.UUID) (store.ScheduledPrompt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.prompts[id]
	if !ok {
		return store.ScheduledPrompt{}, pgx.ErrNoRows
	}
	return r, nil
}
func (f *fakeDB) ListScheduledPrompts(_ context.Context, u uuid.UUID) ([]store.ScheduledPrompt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.ScheduledPrompt
	for _, r := range f.prompts {
		if r.UserID == u {
			out = append(out, r)
		}
	}
	return out, nil
}
func (f *fakeDB) CountScheduledPrompts(ctx context.Context, u uuid.UUID) (int64, error) {
	l, _ := f.ListScheduledPrompts(ctx, u)
	return int64(len(l)), nil
}
func (f *fakeDB) UpdateScheduledPrompt(_ context.Context, a store.UpdateScheduledPromptParams) (store.ScheduledPrompt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.prompts[a.ID]
	if !ok || r.UserID != a.UserID {
		return store.ScheduledPrompt{}, pgx.ErrNoRows
	}
	r.Title, r.Prompt, r.Kind, r.CronExpr, r.Timezone, r.RunAt, r.Enabled, r.NextRunAt = a.Title, a.Prompt, a.Kind, a.CronExpr, a.Timezone, a.RunAt, a.Enabled, a.NextRunAt
	f.prompts[a.ID] = r
	return r, nil
}
func (f *fakeDB) DeleteScheduledPrompt(_ context.Context, a store.DeleteScheduledPromptParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.prompts, a.ID)
	return nil
}
func (f *fakeDB) ListDueScheduledPrompts(_ context.Context, now time.Time) ([]store.ScheduledPrompt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.ScheduledPrompt
	for _, r := range f.prompts {
		if u := f.users[r.UserID]; u.DisabledAt != nil {
			continue
		}
		if r.Enabled && r.NextRunAt != nil && !r.NextRunAt.After(now) {
			out = append(out, r)
		}
	}
	return out, nil
}
func (f *fakeDB) ClaimScheduledPrompt(_ context.Context, a store.ClaimScheduledPromptParams) (store.ScheduledPrompt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.prompts[a.ID]
	if !ok || !r.Enabled || r.NextRunAt == nil || r.NextRunAt.After(a.Now) {
		return store.ScheduledPrompt{}, pgx.ErrNoRows
	}
	r.NextRunAt, r.LastStatus = a.NextRunAt, "queued"
	f.prompts[a.ID] = r
	return r, nil
}
func (f *fakeDB) SetScheduledPromptResult(_ context.Context, a store.SetScheduledPromptResultParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.prompts[a.ID]
	r.LastStatus, r.LastError = a.LastStatus, a.LastError
	if a.ConversationID.Valid {
		r.ConversationID = a.ConversationID
	}
	f.prompts[a.ID] = r
	return nil
}
func (f *fakeDB) InsertScheduledPromptRun(_ context.Context, a store.InsertScheduledPromptRunParams) (store.ScheduledPromptRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := store.ScheduledPromptRun{ID: uuid.New(), PromptID: a.PromptID, RunID: a.RunID, ConversationID: a.ConversationID, Status: a.Status, Error: a.Error, EndedAt: a.EndedAt, StartedAt: time.Now()}
	f.runs = append(f.runs, r)
	return r, nil
}
func (f *fakeDB) ListScheduledPromptRuns(_ context.Context, a store.ListScheduledPromptRunsParams) ([]store.ScheduledPromptRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.ScheduledPromptRun
	for _, r := range f.runs {
		if r.PromptID == a.PromptID {
			out = append(out, r)
		}
	}
	return out, nil
}
func (f *fakeDB) ListOpenScheduledPromptRuns(_ context.Context) ([]store.ListOpenScheduledPromptRunsRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.ListOpenScheduledPromptRunsRow
	for _, r := range f.runs {
		if r.EndedAt != nil {
			continue
		}
		row := store.ListOpenScheduledPromptRunsRow{ID: r.ID, PromptID: r.PromptID, RunID: r.RunID, ConversationID: r.ConversationID, Status: r.Status, Notified: r.Notified}
		if r.RunID.Valid {
			if st, ok := f.runState[r.RunID.UUID]; ok {
				row.RunStatus = &st
			}
		}
		out = append(out, row)
	}
	return out, nil
}
func (f *fakeDB) FinishScheduledPromptRun(_ context.Context, a store.FinishScheduledPromptRunParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, r := range f.runs {
		if r.ID == a.ID {
			t := time.Now()
			f.runs[i].Status, f.runs[i].Error, f.runs[i].EndedAt, f.runs[i].Notified = a.Status, a.Error, &t, a.Notified
		}
	}
	return nil
}
func (f *fakeDB) SetScheduledPromptRunStatus(_ context.Context, a store.SetScheduledPromptRunStatusParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, r := range f.runs {
		if r.ID == a.ID {
			f.runs[i].Status = a.Status
		}
	}
	return nil
}
func (f *fakeDB) UserCanAccessProject(_ context.Context, a store.UserCanAccessProjectParams) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.projects[a.ID] == a.UserID, nil
}
func (f *fakeDB) CreateConversation(_ context.Context, a store.CreateConversationParams) (store.Conversation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := store.Conversation{ID: uuid.New(), ProjectID: a.ProjectID, UserID: a.UserID, Title: a.Title, Mode: a.Mode}
	f.convs[c.ID] = c
	return c, nil
}
func (f *fakeDB) GetConversation(_ context.Context, id uuid.UUID) (store.Conversation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.convs[id]
	if !ok {
		return store.Conversation{}, pgx.ErrNoRows
	}
	return c, nil
}
func (f *fakeDB) NextMessageSeq(_ context.Context, id uuid.UUID) (int32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int32(f.msgs[id] + 1), nil
}
func (f *fakeDB) InsertMessage(_ context.Context, a store.InsertMessageParams) (store.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs[a.ConversationID]++
	return store.Message{ID: uuid.New(), ConversationID: a.ConversationID, Role: a.Role, Parts: a.Parts}, nil
}
func (f *fakeDB) GetUserByID(_ context.Context, id uuid.UUID) (store.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[id]
	if !ok {
		return store.User{}, pgx.ErrNoRows
	}
	return u, nil
}

type fakeRuns struct {
	created []agent.StartParams
	fail    bool
}

func (r *fakeRuns) Create(_ context.Context, p agent.StartParams) (*store.AgentRun, error) {
	if r.fail {
		return nil, errors.New("boom")
	}
	r.created = append(r.created, p)
	return &store.AgentRun{ID: uuid.New(), ConversationID: p.ConversationID}, nil
}

type fakeMail struct{ sent []string }

func (m *fakeMail) Send(_ context.Context, to, subject, text, _ string) error {
	m.sent = append(m.sent, to+"|"+subject+"|"+text)
	return nil
}

type env struct {
	svc   *Service
	db    *fakeDB
	runs  *fakeRuns
	mail  *fakeMail
	user  uuid.UUID
	proj  uuid.UUID
	now   time.Time
	queue []uuid.UUID
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{db: newFake(), runs: &fakeRuns{}, mail: &fakeMail{}, user: uuid.New(), proj: uuid.New(), now: time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC)}
	e.db.projects[e.proj] = e.user
	e.db.users[e.user] = store.User{ID: e.user, Email: "sam@example.com"}
	e.svc = &Service{DB: e.db, Runs: e.runs, Mail: e.mail, PublicURL: "https://ws.example.com", Now: func() time.Time { return e.now },
		Enqueue: func(_ context.Context, id uuid.UUID) error { e.queue = append(e.queue, id); return nil }}
	return e
}

func (e *env) spec() Spec {
	at := e.now.Add(time.Hour)
	return Spec{ProjectID: e.proj, Prompt: "Summarise the news", Kind: KindOnce, RunAt: &at, Enabled: true}
}

func TestValidation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	bad := map[string]func(*Spec){
		"empty prompt":       func(s *Spec) { s.Prompt = "  " },
		"a time in the past": func(s *Spec) { p := e.now.Add(-time.Minute); s.RunAt = &p },
		"no time":            func(s *Spec) { s.RunAt = nil },
		"bad zone":           func(s *Spec) { s.Timezone = "Mars/Olympus" },
		"bad kind":           func(s *Spec) { s.Kind = "weekly" },
		"bad cron":           func(s *Spec) { s.Kind, s.Cron = KindCron, "every day" },
		"too frequent":       func(s *Spec) { s.Kind, s.Cron = KindCron, "*/5 * * * *" },
		"no cron":            func(s *Spec) { s.Kind, s.Cron = KindCron, "" },
		"no project":         func(s *Spec) { s.ProjectID = uuid.Nil },
		"someone else's":     func(s *Spec) { s.ProjectID = uuid.New() },
		"prompt too long":    func(s *Spec) { s.Prompt = strings.Repeat("x", MaxPromptBytes+1) },
	}
	for name, mod := range bad {
		sp := e.spec()
		mod(&sp)
		if _, err := e.svc.Create(ctx, e.user, sp); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	ok := e.spec()
	ok.Kind, ok.Cron, ok.RunAt = KindCron, "0 9 * * 1-5", nil
	got, err := e.svc.Create(ctx, e.user, ok)
	if err != nil || got.NextRunAt == nil || got.Title == "" {
		t.Fatalf("good cron: %+v %v", got, err)
	}
	if got.NextRunAt.Hour() != 9 {
		t.Errorf("next run %v is not 09:00 UTC", got.NextRunAt)
	}
}

func TestLimitPerMember(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < MaxPerUser; i++ {
		if _, err := e.svc.Create(context.Background(), e.user, e.spec()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.svc.Create(context.Background(), e.user, e.spec()); !errors.Is(err, ErrInvalid) {
		t.Errorf("past the limit: %v", err)
	}
}

func TestTickFiresOnceAndOnlyOnce(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	s, err := e.svc.Create(ctx, e.user, e.spec())
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Tick(ctx); err != nil || len(e.runs.created) != 0 {
		t.Fatalf("not due yet: %v %d", err, len(e.runs.created))
	}
	e.now = e.now.Add(2 * time.Hour)
	if err := e.svc.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(e.runs.created) != 1 || len(e.queue) != 1 {
		t.Fatalf("created %d queued %d", len(e.runs.created), len(e.queue))
	}
	p := e.runs.created[0]
	if len(p.Request.ToolAllow) != 1 || p.Request.ToolAllow[0] != agent.NoTools {
		t.Errorf("a scheduled prompt has no tools by default: %v", p.Request.ToolAllow)
	}
	if p.MaxSteps != MaxSteps || p.UserID != e.user {
		t.Errorf("params = %+v", p)
	}
	row := e.db.prompts[s.ID]
	if row.NextRunAt != nil || row.LastStatus != "queued" {
		t.Errorf("a once-prompt must not fire again: next=%v status=%s", row.NextRunAt, row.LastStatus)
	}
	if len(e.db.convs) != 1 || e.db.msgs[p.ConversationID] != 1 {
		t.Errorf("conversation %d messages %d", len(e.db.convs), e.db.msgs[p.ConversationID])
	}
	if err := e.svc.Tick(ctx); err != nil || len(e.runs.created) != 1 {
		t.Errorf("second tick fired again: %d", len(e.runs.created))
	}
}

func TestCronMissedRunsFireOnceAndSkipAhead(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	sp := e.spec()
	sp.Kind, sp.Cron, sp.RunAt, sp.Timezone = KindCron, "0 9 * * *", nil, "Europe/Berlin"
	s, err := e.svc.Create(ctx, e.user, sp)
	if err != nil {
		t.Fatal(err)
	}
	// 09:00 in Berlin on 2026-10-10 is 07:00 UTC (CEST), already passed at 08:00 UTC,
	// so the first firing is tomorrow 07:00 UTC.
	if got := s.NextRunAt; got == nil || got.Day() != 11 || got.Hour() != 7 {
		t.Fatalf("first firing = %v", got)
	}
	// The worker was down for four days.
	e.now = e.now.Add(4*24*time.Hour + time.Hour)
	if err := e.svc.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(e.runs.created) != 1 {
		t.Errorf("missed days should fire once, got %d", len(e.runs.created))
	}
	next := e.db.prompts[s.ID].NextRunAt
	if next == nil || !next.After(e.now) || next.Hour() != 7 {
		t.Errorf("next after catching up = %v", next)
	}
}

func TestDisabledAndDisabledMember(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	sp := e.spec()
	sp.Enabled = false
	off, err := e.svc.Create(ctx, e.user, sp)
	if err != nil || off.NextRunAt != nil {
		t.Fatalf("disabled create: %+v %v", off, err)
	}
	on, _ := e.svc.Create(ctx, e.user, e.spec())
	_ = on
	u := e.db.users[e.user]
	now := e.now
	u.DisabledAt = &now
	e.db.users[e.user] = u
	e.now = e.now.Add(3 * time.Hour)
	if err := e.svc.Tick(ctx); err != nil || len(e.runs.created) != 0 {
		t.Errorf("a disabled member's schedules must not fire: %v %d", err, len(e.runs.created))
	}
}

func TestRunNowAndFailures(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	s, _ := e.svc.Create(ctx, e.user, e.spec())
	run, err := e.svc.RunNow(ctx, e.user, s.ID)
	if err != nil || run.Status != "queued" || !run.RunID.Valid {
		t.Fatalf("run now: %+v %v", run, err)
	}
	if e.db.prompts[s.ID].NextRunAt == nil {
		t.Error("run now must not consume the next firing")
	}
	e.runs.fail = true
	run, _ = e.svc.RunNow(ctx, e.user, s.ID)
	if run.Status != "failed" || strings.Contains(run.Error, "boom") {
		t.Errorf("failure should be recorded without internals: %+v", run)
	}
	hist, _ := e.svc.History(ctx, e.user, s.ID, 10)
	if len(hist) != 2 {
		t.Errorf("history = %d", len(hist))
	}
	if _, err := e.svc.RunNow(ctx, uuid.New(), s.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("another member: %v", err)
	}
	if _, err := e.svc.History(ctx, uuid.New(), s.ID, 10); !errors.Is(err, ErrNotFound) {
		t.Errorf("another member's history: %v", err)
	}
	if err := e.svc.Delete(ctx, uuid.New(), s.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("another member deleting: %v", err)
	}
	if _, err := e.svc.Update(ctx, uuid.New(), s.ID, e.spec()); !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrNotFound) {
		t.Errorf("another member updating: %v", err)
	}
}

func TestReuseConversationKeepsAddingToOne(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	sp := e.spec()
	sp.ReuseConversation = true
	s, _ := e.svc.Create(ctx, e.user, sp)
	r1, _ := e.svc.RunNow(ctx, e.user, s.ID)
	r2, _ := e.svc.RunNow(ctx, e.user, s.ID)
	if !r1.ConversationID.Valid || r1.ConversationID != r2.ConversationID || len(e.db.convs) != 1 {
		t.Errorf("conversations: %v %v (%d)", r1.ConversationID, r2.ConversationID, len(e.db.convs))
	}
	// Without it, each firing is a new conversation.
	e2 := newEnv(t)
	s2, _ := e2.svc.Create(ctx, e2.user, e2.spec())
	_, _ = e2.svc.RunNow(ctx, e2.user, s2.ID)
	_, _ = e2.svc.RunNow(ctx, e2.user, s2.ID)
	if len(e2.db.convs) != 2 {
		t.Errorf("new conversation each time: %d", len(e2.db.convs))
	}
}

func TestSettleCopiesStatusAndMailsOnce(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	sp := e.spec()
	sp.NotifyEmail = true
	s, _ := e.svc.Create(ctx, e.user, sp)
	run, _ := e.svc.RunNow(ctx, e.user, s.ID)
	// Still running: nothing finishes.
	e.db.runState[run.RunID.UUID] = "running"
	_ = e.svc.Tick(ctx)
	if len(e.mail.sent) != 0 {
		t.Fatal("mailed before the run finished")
	}
	e.db.runState[run.RunID.UUID] = "done"
	_ = e.svc.Tick(ctx)
	_ = e.svc.Tick(ctx)
	if len(e.mail.sent) != 1 || !strings.Contains(e.mail.sent[0], "sam@example.com") || !strings.Contains(e.mail.sent[0], "https://ws.example.com/c/") {
		t.Errorf("mail = %v", e.mail.sent)
	}
	if got := e.db.prompts[s.ID].LastStatus; got != "done" {
		t.Errorf("last status = %s", got)
	}
	hist, _ := e.svc.History(ctx, e.user, s.ID, 5)
	if len(hist) != 1 || hist[0].Status != "done" || hist[0].EndedAt == nil {
		t.Errorf("history = %+v", hist)
	}
}
