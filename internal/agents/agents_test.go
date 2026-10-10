package agents

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jking323/ws/internal/agent"
	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
)

type fakeStore struct {
	mu       sync.Mutex
	agents   map[uuid.UUID]store.Agent
	runs     map[uuid.UUID]store.AgentRun
	convs    map[uuid.UUID]store.Conversation
	msgs     map[uuid.UUID][]store.Message
	triggers map[uuid.UUID]store.AgentTrigger
}

func newFake() *fakeStore {
	return &fakeStore{agents: map[uuid.UUID]store.Agent{}, runs: map[uuid.UUID]store.AgentRun{}, convs: map[uuid.UUID]store.Conversation{}, msgs: map[uuid.UUID][]store.Message{}, triggers: map[uuid.UUID]store.AgentTrigger{}}
}

func (f *fakeStore) GetAgent(_ context.Context, id uuid.UUID) (store.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.agents[id]
	if !ok {
		return store.Agent{}, pgx.ErrNoRows
	}
	return a, nil
}
func (f *fakeStore) TouchAgentRun(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.agents[id]
	now := time.Now()
	a.LastRunAt = &now
	f.agents[id] = a
	return nil
}
func (f *fakeStore) CountOpenRunsForAgent(_ context.Context, id uuid.NullUUID) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for _, r := range f.runs {
		if r.AgentID == id {
			switch r.Status {
			case "queued", "running", "paused_approval", "paused_steer":
				n++
			}
		}
	}
	return n, nil
}
func (f *fakeStore) GetRun(_ context.Context, id uuid.UUID) (store.AgentRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runs[id]
	if !ok {
		return store.AgentRun{}, pgx.ErrNoRows
	}
	return r, nil
}
func (f *fakeStore) CancelRun(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.runs[id]
	r.Status = "cancelled"
	f.runs[id] = r
	return nil
}
func (f *fakeStore) SetRunStatus(_ context.Context, p store.SetRunStatusParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runs[p.ID]
	if !ok {
		return pgx.ErrNoRows
	}
	r.Status = p.Status
	r.Error = p.Error
	f.runs[p.ID] = r
	return nil
}
func (f *fakeStore) CreateConversation(_ context.Context, p store.CreateConversationParams) (store.Conversation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := store.Conversation{ID: uuid.New(), ProjectID: p.ProjectID, UserID: p.UserID, Title: p.Title, Mode: p.Mode, ModelSelector: p.ModelSelector}
	f.convs[c.ID] = c
	return c, nil
}
func (f *fakeStore) SetConversationAgent(_ context.Context, p store.SetConversationAgentParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.convs[p.ID]
	c.AgentID = p.AgentID
	f.convs[p.ID] = c
	return nil
}
func (f *fakeStore) NextMessageSeq(_ context.Context, id uuid.UUID) (int32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int32(len(f.msgs[id]) + 1), nil
}
func (f *fakeStore) InsertMessage(_ context.Context, p store.InsertMessageParams) (store.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := store.Message{ID: uuid.New(), ConversationID: p.ConversationID, Seq: p.Seq, Role: p.Role, Parts: p.Parts}
	f.msgs[p.ConversationID] = append(f.msgs[p.ConversationID], m)
	return m, nil
}
func (f *fakeStore) GetTrigger(_ context.Context, id uuid.UUID) (store.AgentTrigger, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.triggers[id]
	if !ok {
		return store.AgentTrigger{}, pgx.ErrNoRows
	}
	return t, nil
}
func (f *fakeStore) CreateTrigger(_ context.Context, p store.CreateTriggerParams) (store.AgentTrigger, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := store.AgentTrigger{ID: uuid.New(), AgentID: p.AgentID, Kind: p.Kind, Name: p.Name, Spec: p.Spec, SecretHash: p.SecretHash, NextRunAt: p.NextRunAt, Enabled: true, CreatedAt: time.Now()}
	f.triggers[t.ID] = t
	return t, nil
}
func (f *fakeStore) ListDueCronTriggers(_ context.Context) ([]store.AgentTrigger, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.AgentTrigger
	for _, t := range f.triggers {
		if t.Enabled && t.Kind == KindCron && t.NextRunAt != nil && !t.NextRunAt.After(time.Now()) && f.agents[t.AgentID].Enabled {
			out = append(out, t)
		}
	}
	return out, nil
}
func (f *fakeStore) ListRepoPushTriggers(_ context.Context) ([]store.AgentTrigger, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.AgentTrigger
	for _, t := range f.triggers {
		if t.Enabled && t.Kind == KindRepoPush && f.agents[t.AgentID].Enabled {
			out = append(out, t)
		}
	}
	return out, nil
}
func (f *fakeStore) SetTriggerFired(_ context.Context, p store.SetTriggerFiredParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.triggers[p.ID]
	now := time.Now()
	t.LastRunAt, t.NextRunAt, t.LastError = &now, p.NextRunAt, p.LastError
	f.triggers[p.ID] = t
	return nil
}

// fakeRuntime records created runs as queued.
type fakeRuntime struct {
	db   *fakeStore
	last agent.StartParams
}

func (r *fakeRuntime) Create(_ context.Context, p agent.StartParams) (*store.AgentRun, error) {
	r.last = p
	req, _ := json.Marshal(p.Request)
	run := store.AgentRun{ID: uuid.New(), AgentID: p.AgentID, ConversationID: p.ConversationID, UserID: store.NullUUID(p.UserID), TriggerID: p.TriggerID, Status: "queued", Request: req, MaxSteps: int32(p.MaxSteps)}
	r.db.mu.Lock()
	r.db.runs[run.ID] = run
	r.db.mu.Unlock()
	return &run, nil
}

func newService(t *testing.T) (*Service, *fakeStore, *fakeRuntime, store.Agent) {
	t.Helper()
	db := newFake()
	rt := &fakeRuntime{db: db}
	mp, _ := json.Marshal(ModelPolicy{Selector: "cheap", TaskClass: gateway.TaskChat})
	tp, _ := json.Marshal(map[string]agent.Policy{"bash": agent.PolicyAsk})
	ag := store.Agent{ID: uuid.New(), OwnerID: uuid.New(), ProjectID: store.NullUUID(uuid.New()), Name: "Night watch", Goal: "Check the server logs and report anomalies.",
		ModelPolicy: mp, ToolPolicies: tp, ToolAllowlist: []string{"web_fetch"}, MaxSteps: 20, Enabled: true}
	db.agents[ag.ID] = ag
	var enq []uuid.UUID
	svc := &Service{DB: db, Runtime: rt, Enqueue: func(_ context.Context, id uuid.UUID) error { enq = append(enq, id); return nil }}
	return svc, db, rt, ag
}

func TestStartCreatesConversationAndRun(t *testing.T) {
	svc, db, rt, ag := newService(t)
	run, err := svc.Start(context.Background(), StartParams{AgentID: ag.ID, Label: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "queued" || run.AgentID.UUID != ag.ID || run.UserID.UUID != ag.OwnerID {
		t.Errorf("run = %+v", run)
	}
	conv := db.convs[run.ConversationID]
	if conv.ProjectID != ag.ProjectID.UUID || conv.Mode != "agent" || conv.AgentID.UUID != ag.ID || !strings.HasPrefix(conv.Title, "Night watch · manual") {
		t.Errorf("conversation = %+v", conv)
	}
	msgs := db.msgs[run.ConversationID]
	if len(msgs) != 1 || msgs[0].Role != "user" || !strings.Contains(string(msgs[0].Parts), "server logs") {
		t.Errorf("first message = %+v", msgs)
	}
	req := rt.last.Request
	if req.Selector != "cheap" || req.TaskClass != gateway.TaskChat || len(req.ToolAllow) != 1 || !strings.Contains(req.System, "standing goal") || rt.last.MaxSteps != 20 || rt.last.Policies["bash"] != agent.PolicyAsk {
		t.Errorf("start params = %+v", rt.last)
	}
	if db.agents[ag.ID].LastRunAt == nil {
		t.Error("last_run_at not set")
	}
	// A second start while the first is open is refused.
	if _, err := svc.Start(context.Background(), StartParams{AgentID: ag.ID}); !errors.Is(err, ErrBusy) {
		t.Errorf("second start: %v", err)
	}
	// Disabled agents do not run.
	a := db.agents[ag.ID]
	a.Enabled = false
	db.agents[ag.ID] = a
	svc.DB.CancelRun(context.Background(), run.ID)
	if _, err := svc.Start(context.Background(), StartParams{AgentID: ag.ID}); !errors.Is(err, ErrDisabled) {
		t.Errorf("disabled start: %v", err)
	}
}

func TestSteerContinuesConversation(t *testing.T) {
	svc, db, _, ag := newService(t)
	ctx := context.Background()
	run, _ := svc.Start(ctx, StartParams{AgentID: ag.ID})
	if _, err := svc.Steer(ctx, run.ID, "Also check disk space.", ag.OwnerID); !errors.Is(err, ErrBusy) {
		t.Errorf("steer while open: %v", err)
	}
	r := db.runs[run.ID]
	r.Status = "done"
	db.runs[run.ID] = r
	next, err := svc.Steer(ctx, run.ID, "Also check disk space.", ag.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	if next.ConversationID != run.ConversationID || next.ID == run.ID {
		t.Errorf("steer run = %+v", next)
	}
	if msgs := db.msgs[run.ConversationID]; len(msgs) != 2 || !strings.Contains(string(msgs[1].Parts), "disk space") {
		t.Errorf("messages = %+v", msgs)
	}
	if _, err := svc.Steer(ctx, next.ID, "   ", ag.OwnerID); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty steer: %v", err)
	}
}

func TestCronTriggerTick(t *testing.T) {
	svc, db, _, ag := newService(t)
	ctx := context.Background()
	if _, _, err := svc.NewTrigger(ctx, ag.ID, KindCron, "bad", json.RawMessage(`{"expr":"every day"}`)); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad cron: %v", err)
	}
	trig, secret, err := svc.NewTrigger(ctx, ag.ID, KindCron, "ten past", json.RawMessage(`{"expr":"*/10 * * * *","input":"Look at the last ten minutes."}`))
	if err != nil || secret != "" || trig.NextRunAt == nil || trig.NextRunAt.Before(time.Now()) {
		t.Fatalf("trigger = %+v secret=%q err=%v", trig, secret, err)
	}
	// Not due yet: nothing starts.
	if err := svc.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(db.runs) != 0 {
		t.Fatal("ran before due")
	}
	// Make it due.
	past := time.Now().Add(-time.Minute)
	tr := db.triggers[trig.ID]
	tr.NextRunAt = &past
	db.triggers[trig.ID] = tr
	if err := svc.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(db.runs) != 1 {
		t.Fatalf("runs after tick = %d", len(db.runs))
	}
	var run store.AgentRun
	for _, r := range db.runs {
		run = r
	}
	if run.TriggerID.UUID != trig.ID || !strings.Contains(string(db.msgs[run.ConversationID][0].Parts), "last ten minutes") {
		t.Errorf("cron run = %+v", run)
	}
	tr = db.triggers[trig.ID]
	if tr.LastRunAt == nil || tr.NextRunAt == nil || !tr.NextRunAt.After(time.Now()) || tr.LastError != nil {
		t.Errorf("trigger after tick = %+v", tr)
	}
	// Due again while the run is still open: skipped, recorded, rescheduled.
	tr.NextRunAt = &past
	db.triggers[trig.ID] = tr
	_ = svc.Tick(ctx)
	tr = db.triggers[trig.ID]
	if len(db.runs) != 1 || tr.LastError == nil || !strings.Contains(*tr.LastError, "already open") || !tr.NextRunAt.After(time.Now()) {
		t.Errorf("busy tick: runs=%d trigger=%+v", len(db.runs), tr)
	}
}

func TestWebhookFire(t *testing.T) {
	svc, db, _, ag := newService(t)
	ctx := context.Background()
	trig, secret, err := svc.NewTrigger(ctx, ag.ID, KindWebhook, "deploys", nil)
	if err != nil || len(secret) != 48 || len(trig.SecretHash) != 32 {
		t.Fatalf("webhook trigger = %+v secret=%q err=%v", trig, secret, err)
	}
	if _, err := svc.Fire(ctx, trig.ID, "wrong", "x"); !errors.Is(err, ErrSecret) {
		t.Errorf("wrong secret: %v", err)
	}
	if _, err := svc.Fire(ctx, uuid.New(), secret, "x"); !errors.Is(err, ErrSecret) {
		t.Errorf("unknown trigger: %v", err)
	}
	run, err := svc.Fire(ctx, trig.ID, secret, `{"deploy":"v12"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(db.msgs[run.ConversationID][0].Parts), "Webhook payload") || run.TriggerID.UUID != trig.ID {
		t.Errorf("webhook run = %+v", run)
	}
	// A disabled trigger is as good as missing.
	tr := db.triggers[trig.ID]
	tr.Enabled = false
	db.triggers[trig.ID] = tr
	if _, err := svc.Fire(ctx, trig.ID, secret, ""); !errors.Is(err, ErrSecret) {
		t.Errorf("disabled trigger: %v", err)
	}
}

func signed(secret, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

const pushBody = `{"ref":"refs/heads/main","repository":{"full_name":"acme/app"},"pusher":{"name":"ann"},"commits":[{"id":"abc1234","message":"fix","author":{"name":"ann"}}]}`

func TestRepoPushNeedsTheSignature(t *testing.T) {
	svc, _, _, ag := newService(t)
	ctx := context.Background()
	spec, _ := json.Marshal(RepoPushSpec{Repo: "acme/app"})
	trig, secret, err := svc.NewTrigger(ctx, ag.ID, KindRepoPush, "pushes", spec)
	if err != nil {
		t.Fatal(err)
	}
	// The URL secret alone is not enough for a GitHub delivery.
	if _, err := svc.FireHook(ctx, FireParams{TriggerID: trig.ID, Secret: secret, Body: pushBody, GitHubEvent: "push"}); !errors.Is(err, ErrNoSignature) {
		t.Errorf("no signature: %v", err)
	}
	if _, err := svc.FireHook(ctx, FireParams{TriggerID: trig.ID, Secret: secret, Body: pushBody, GitHubEvent: "push", Signature: signed("other", pushBody)}); !errors.Is(err, ErrSecret) {
		t.Errorf("bad signature: %v", err)
	}
	run, err := svc.FireHook(ctx, FireParams{TriggerID: trig.ID, Secret: secret, Body: pushBody, GitHubEvent: "push", Signature: signed(secret, pushBody)})
	if err != nil || run.TriggerID.UUID != trig.ID {
		t.Fatalf("signed delivery: %v %+v", err, run)
	}
}

func TestAppDeliveryFansOutByRepository(t *testing.T) {
	svc, db, _, ag := newService(t)
	ctx := context.Background()
	mk := func(repo string, branches []string) store.AgentTrigger {
		spec, _ := json.Marshal(RepoPushSpec{Repo: repo, Branches: branches})
		trig, _, err := svc.NewTrigger(ctx, ag.ID, KindRepoPush, repo, spec)
		if err != nil {
			t.Fatal(err)
		}
		return *trig
	}
	match := mk("ACME/app", nil)      // case does not matter
	mk("acme/other", nil)             // another repository
	mk("acme/app", []string{"rel/*"}) // this repository, another branch
	mk("", nil)                       // no repo: never on the App hook
	runs, err := svc.AppDelivery(ctx, "push", pushBody)
	if err != nil || len(runs) != 1 || runs[0].TriggerID.UUID != match.ID {
		t.Fatalf("runs = %+v err = %v", runs, err)
	}
	if !strings.Contains(string(db.msgs[runs[0].ConversationID][0].Parts), "GitHub push to acme/app") {
		t.Errorf("input = %s", db.msgs[runs[0].ConversationID][0].Parts)
	}
	if _, err := svc.AppDelivery(ctx, "push", `{"zen":"ping"}`); !errors.Is(err, ErrIgnored) {
		t.Errorf("ping: %v", err)
	}
	if _, err := svc.AppDelivery(ctx, "push", strings.Replace(pushBody, "acme/app", "nobody/here", 1)); !errors.Is(err, ErrIgnored) {
		t.Errorf("unknown repository: %v", err)
	}
}

func TestSystemPrompt(t *testing.T) {
	p := SystemPrompt(store.Agent{Name: "Bot", Goal: "Tidy the inbox."})
	if !strings.Contains(p, "You are Bot") || !strings.Contains(p, "Tidy the inbox.") || !strings.Contains(p, "ask_user") {
		t.Errorf("prompt = %q", p)
	}
	p = SystemPrompt(store.Agent{Name: "Bot", SystemPrompt: "Custom."})
	if !strings.HasPrefix(p, "Custom.") || strings.Contains(p, "standing goal") {
		t.Errorf("custom prompt = %q", p)
	}
}
