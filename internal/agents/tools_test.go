package agents

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/agent"
	"github.com/jking323/ws/internal/store"
)

func TestRepoPushTrigger(t *testing.T) {
	svc, db, _, ag := newService(t)
	ctx := context.Background()
	if _, _, err := svc.NewTrigger(ctx, ag.ID, KindRepoPush, "ci", json.RawMessage(`{"repo":"nonsense"}`)); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad repo: %v", err)
	}
	if _, _, err := svc.NewTrigger(ctx, ag.ID, KindRepoPush, "ci", json.RawMessage(`{"branches":["["]}`)); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad pattern: %v", err)
	}
	trig, secret, err := svc.NewTrigger(ctx, ag.ID, KindRepoPush, "ci", json.RawMessage(`{"repo":"acme/ws","branches":["main"],"input":"Review the push."}`))
	if err != nil || len(secret) != 48 {
		t.Fatalf("trigger = %+v err=%v", trig, err)
	}
	var spec RepoPushSpec
	_ = json.Unmarshal(trig.Spec, &spec)
	if len(spec.Events) != 1 || spec.Events[0] != "push" {
		t.Errorf("events default to push: %+v", spec)
	}
	// A ping, another branch, or a plain POST without an event is ignored
	// without a run; a wrong secret is still a 404-shaped error.
	if _, err := svc.FireHook(ctx, FireParams{TriggerID: trig.ID, Secret: secret, GitHubEvent: "ping", Body: `{"zen":"x"}`, Signature: signed(secret, `{"zen":"x"}`)}); !errors.Is(err, ErrIgnored) {
		t.Errorf("ping: %v", err)
	}
	dev := strings.Replace(pushPayload, "refs/heads/main", "refs/heads/dev", 1)
	if _, err := svc.FireHook(ctx, FireParams{TriggerID: trig.ID, Secret: secret, GitHubEvent: "push", Body: dev, Signature: signed(secret, dev)}); !errors.Is(err, ErrIgnored) {
		t.Errorf("other branch: %v", err)
	}
	if _, err := svc.FireHook(ctx, FireParams{TriggerID: trig.ID, Secret: secret, Body: pushPayload, Signature: signed(secret, pushPayload)}); !errors.Is(err, ErrIgnored) {
		t.Errorf("no event header: %v", err)
	}
	if _, err := svc.FireHook(ctx, FireParams{TriggerID: trig.ID, Secret: "nope", GitHubEvent: "push", Body: pushPayload}); !errors.Is(err, ErrSecret) {
		t.Errorf("wrong secret: %v", err)
	}
	if len(db.runs) != 0 {
		t.Fatalf("ignored deliveries must not start runs: %d", len(db.runs))
	}
	run, err := svc.FireHook(ctx, FireParams{TriggerID: trig.ID, Secret: secret, GitHubEvent: "push", Body: pushPayload, Signature: signed(secret, pushPayload)})
	if err != nil {
		t.Fatal(err)
	}
	first := string(db.msgs[run.ConversationID][0].Parts)
	if !strings.Contains(first, "Review the push.") || !strings.Contains(first, "GitHub push to acme/ws on main") || run.TriggerID.UUID != trig.ID {
		t.Errorf("run input = %s", first)
	}
	if conv := db.convs[run.ConversationID]; !strings.Contains(conv.Title, "github") {
		t.Errorf("conversation title = %q", conv.Title)
	}
	if tr := db.triggers[trig.ID]; tr.LastRunAt == nil || tr.LastError != nil {
		t.Errorf("trigger after firing = %+v", tr)
	}
}

func TestMemoryTools(t *testing.T) {
	db := newMemStore()
	gw := &fakeGW{}
	mem := &Memory{DB: db, GW: gw}
	ag := store.Agent{ID: uuid.New(), OwnerID: uuid.New(), Name: "Bot", Goal: "Watch the server."}
	db.agents[ag.ID] = ag
	run := store.AgentRun{ID: uuid.New(), AgentID: store.NullUUID(ag.ID), ConversationID: uuid.New(), Status: "running"}
	db.runs[run.ID] = run
	chat := store.AgentRun{ID: uuid.New(), ConversationID: uuid.New(), Status: "running"}
	db.runs[chat.ID] = chat
	ctx := context.Background()
	remember, recall := NewRememberTool(mem), NewRecallTool(mem)
	if remember.Def().Name != ToolRemember || recall.Def().Name != ToolRecall || !remember.Idempotent() {
		t.Error("tool definitions")
	}

	// A chat turn has no agent, so no memory.
	res, err := remember.Call(ctx, agent.ToolCtx{RunID: chat.ID}, json.RawMessage(`{"content":"x"}`))
	if err != nil || !res.IsError || !strings.Contains(res.Text, "chat turn") {
		t.Errorf("chat remember = %+v err=%v", res, err)
	}

	res, err = remember.Call(ctx, agent.ToolCtx{RunID: run.ID}, json.RawMessage(`{"content":"The server is called atlas.","kind":"fact","importance":0.9}`))
	if err != nil || res.IsError || !strings.Contains(res.Text, "Remembered as a fact (importance 0.90)") {
		t.Fatalf("remember = %+v err=%v", res, err)
	}
	res, _ = remember.Call(ctx, agent.ToolCtx{RunID: run.ID}, json.RawMessage(`{"content":"Reports go to the owner on Mondays.","kind":"preference"}`))
	if res.IsError {
		t.Fatalf("remember preference = %+v", res)
	}
	if _, err := remember.Call(ctx, agent.ToolCtx{RunID: run.ID}, json.RawMessage(`{"content":"  "}`)); err != nil {
		t.Fatal(err)
	}
	res, _ = remember.Call(ctx, agent.ToolCtx{RunID: run.ID}, json.RawMessage(`{"content":"x","kind":"secret"}`))
	if !res.IsError {
		t.Error("unknown kind should fail")
	}
	if len(db.mems) != 2 {
		t.Fatalf("memories = %d", len(db.mems))
	}
	for _, m := range db.mems {
		if m.Embedding == nil || m.SourceRunID.UUID != run.ID {
			t.Errorf("memory = %+v", m)
		}
		if m.Kind == MemoryPreference && m.Importance != 0.7 {
			t.Errorf("default importance = %v", m.Importance)
		}
	}
	// The same statement again upserts rather than duplicating.
	_, _ = remember.Call(ctx, agent.ToolCtx{RunID: run.ID}, json.RawMessage(`{"content":"the server is called ATLAS.","importance":0.5}`))
	if len(db.mems) != 2 {
		t.Errorf("duplicate content should upsert: %d", len(db.mems))
	}

	res, err = recall.Call(ctx, agent.ToolCtx{RunID: run.ID}, json.RawMessage(`{"query":"what is the server called?","k":1}`))
	if err != nil || res.IsError || !strings.Contains(res.Text, "1 memory, nearest first") || !strings.Contains(res.Text, "[fact] The server is called atlas.") || strings.Contains(res.Text, "Mondays") {
		t.Errorf("recall = %+v err=%v", res, err)
	}
	if len(db.touched) != 1 {
		t.Errorf("recall should mark memories used: %v", db.touched)
	}
	res, _ = recall.Call(ctx, agent.ToolCtx{RunID: run.ID}, json.RawMessage(`{"query":""}`))
	if !res.IsError {
		t.Error("empty query should fail")
	}
	// Without an embedding model, recall falls back to importance.
	gw.embedErr = errors.New("no embedder")
	res, _ = recall.Call(ctx, agent.ToolCtx{RunID: run.ID}, json.RawMessage(`{"query":"anything"}`))
	if res.IsError || !strings.Contains(res.Text, "2 memories") {
		t.Errorf("importance fallback = %+v", res)
	}
	// Memory turned off for the agent turns both tools off.
	ag.MemoryConfig = json.RawMessage(`{"disabled":true}`)
	db.agents[ag.ID] = ag
	res, _ = recall.Call(ctx, agent.ToolCtx{RunID: run.ID}, json.RawMessage(`{"query":"x"}`))
	if !res.IsError || !strings.Contains(res.Text, "turned off") {
		t.Errorf("disabled = %+v", res)
	}
}
