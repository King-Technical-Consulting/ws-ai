package training

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
	"github.com/jking323/ws/internal/store/blob"
)

func TestRemoteRunnerDrivesTheTrainerBox(t *testing.T) {
	ts := &trainerServer{state: "idle"}
	srv := httptest.NewServer(ts.handler(t))
	t.Cleanup(srv.Close)
	r := &RemoteRunner{URL: srv.URL + "/v1/", Key: func() string { return "rk-test" }, Client: srv.Client(), Poll: 5 * time.Millisecond}
	var fracs []float64
	adapter, log, err := r.Run(context.Background(), RunSpec{JobID: uuid.New(), BaseModel: "Qwen/Qwen3-8B", AdapterName: "chat-v1", Config: FinetuneConfig{Epochs: 1, Target: TargetRemote}, Train: []byte("{}\n")}, func(f float64, _ string) { fracs = append(fracs, f) })
	if err != nil {
		t.Fatal(err)
	}
	if string(adapter) != "adapter-tar" || !strings.HasPrefix(log, "trainer at 127.0.0.1:") || fracs[0] != 0 || fracs[len(fracs)-1] != 1 {
		t.Errorf("adapter=%q log=%q fracs=%v", adapter, log, fracs)
	}
	if ts.got["adapter_name"] != "chat-v1" {
		t.Errorf("trainer got %+v", ts.got)
	}
	// A box that refuses the key fails the run with the trainer's answer.
	r.Key = func() string { return "wrong" }
	if _, _, err := r.Run(context.Background(), RunSpec{JobID: uuid.New()}, nil); err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Errorf("wrong key: %v", err)
	}
	// No URL: no runner.
	if _, _, err := (&RemoteRunner{}).Run(context.Background(), RunSpec{}, nil); !errors.Is(err, ErrNoRunner) {
		t.Errorf("no url: %v", err)
	}
	// The targets list puts it between local and rental, and it is the
	// default when there is no local image.
	both := &Targets{Remote: r, Rental: &fakeRunner{}, RemoteLabel: r.Host()}
	if l := both.List(); len(l) != 2 || l[0].ID != TargetRemote || !strings.Contains(l[0].Label, "the trainer at 127.0.0.1") {
		t.Errorf("list = %+v", l)
	}
	if got, _ := both.Pick(""); got != r {
		t.Error("default should be the remote box")
	}
	if _, err := (&Targets{Local: &fakeRunner{}}).Pick(TargetRemote); !errors.Is(err, ErrInvalid) {
		t.Errorf("missing remote: %v", err)
	}
}

func TestRoutesWriteTheAdapterPolicy(t *testing.T) {
	db := newFake()
	svc := newService(t, db)
	ctx := context.Background()
	reloads := 0
	svc.ReloadPolicies = func(context.Context) error { reloads++; return nil }
	svc.BasePrefer = func(tc string) []string {
		if tc == "chat" {
			return []string{"local/qwen", "anthropic/sonnet"}
		}
		return nil
	}
	eps := map[string]bool{"local/qwen": true, "lora/chat-v1": true, "lora/chat-v2": true, "anthropic/opus": true}
	svc.Endpoint = func(id string) (*gateway.Endpoint, bool) {
		if eps[id] {
			return &gateway.Endpoint{ID: id}, true
		}
		return nil, false
	}
	if err := svc.AddRoute(ctx, "Chat Class!", "lora/chat-v1"); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad class: %v", err)
	}
	if err := svc.AddRoute(ctx, "chat", "lora/nope"); !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown endpoint: %v", err)
	}
	if err := svc.AddRoute(ctx, "chat", "lora/chat-v1"); err != nil {
		t.Fatal(err)
	}
	row := db.policies[AdapterPolicy]
	if row.Priority != adapterPolicyPriority || !row.Enabled || reloads != 1 {
		t.Fatalf("policy row = %+v reloads=%d", row, reloads)
	}
	p, err := gateway.ParsePolicy(row.Yaml)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Rules) != 1 || len(p.Rules[0].Match.TaskClass) != 1 || p.Rules[0].Match.TaskClass[0] != "chat" || strings.Join(p.Rules[0].Prefer, ",") != "lora/chat-v1,local/qwen,anthropic/sonnet" {
		t.Errorf("rule = %+v", p.Rules)
	}
	if !strings.HasPrefix(row.Yaml, "# Written by the Training page") || strings.Contains(row.Yaml, "deny:") || strings.Contains(row.Yaml, "require:") {
		t.Errorf("yaml should be the header and only the keys set:\n%s", row.Yaml)
	}
	// A second adapter for the same class goes first; a distillation
	// route for another class is its own rule; the base list never
	// repeats a routed endpoint.
	_ = svc.AddRoute(ctx, "chat", "lora/chat-v2")
	svc.BasePrefer = func(tc string) []string { return []string{"anthropic/opus", "local/qwen"} }
	if err := svc.AddRoute(ctx, "code", "anthropic/opus"); err != nil {
		t.Fatal(err)
	}
	p, _ = gateway.ParsePolicy(db.policies[AdapterPolicy].Yaml)
	if len(p.Rules) != 2 || strings.Join(p.Rules[0].Prefer, ",") != "lora/chat-v2,lora/chat-v1,anthropic/opus,local/qwen" || strings.Join(p.Rules[1].Prefer, ",") != "anthropic/opus,local/qwen" {
		t.Errorf("rules = %+v", p.Rules)
	}
	routes, _ := svc.Routes(ctx)
	if len(routes) != 3 || routes[0].TaskClass != "code" || routes[1].Endpoint != "lora/chat-v2" {
		t.Errorf("routes = %+v", routes)
	}
	// The router honours it over a default policy.
	def, _ := gateway.ParsePolicy("name: default\nrules:\n  - match: {}\n    prefer: [local/qwen]\n")
	prefs, _ := gateway.PreferredBy([]gateway.Policy{p, def}, gateway.RouteInput{Selector: "auto", TaskClass: gateway.TaskChat}, nil)
	if len(prefs) == 0 || prefs[0] != "lora/chat-v2" {
		t.Errorf("router prefers %v", prefs)
	}
	// Removing every route to an endpoint (an adapter unpromoted) keeps
	// the other rules; the last route deletes the policy.
	if err := svc.RemoveRoute(ctx, "", "lora/chat-v2"); err != nil {
		t.Fatal(err)
	}
	p, _ = gateway.ParsePolicy(db.policies[AdapterPolicy].Yaml)
	if len(p.Rules) != 2 || p.Rules[0].Prefer[0] != "lora/chat-v1" {
		t.Errorf("after remove = %+v", p.Rules)
	}
	_ = svc.RemoveRoute(ctx, "chat", "lora/chat-v1")
	_ = svc.RemoveRoute(ctx, "code", "anthropic/opus")
	if _, ok := db.policies[AdapterPolicy]; ok {
		t.Error("the policy should be gone with its last route")
	}
	if routes, _ := svc.Routes(ctx); len(routes) != 0 {
		t.Errorf("routes = %+v", routes)
	}
	// A removal that changes nothing writes nothing.
	before := reloads
	_ = svc.RemoveRoute(ctx, "", "lora/chat-v1")
	if reloads != before {
		t.Error("no-op removal reloaded")
	}
}

func TestPromoteIntoATaskClassAndPreview(t *testing.T) {
	db := newFake()
	svc := newService(t, db)
	ctx := context.Background()
	svc.BasePrefer = func(string) []string { return []string{"local/qwen"} }
	ep := "lora/chat-v1"
	db.eps[ep] = false
	ad, _ := db.CreateAdapter(ctx, store.CreateAdapterParams{Name: "chat-v1", BaseModel: "m", BlobKey: "k", EndpointID: &ep})
	if err := svc.Promote(ctx, ad.ID, true, "chat"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Promote(ctx, ad.ID, true, "Not a class"); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad class: %v", err)
	}
	// A bad class is refused before anything changes.
	_ = svc.Unpromote(ctx, ad.ID)
	if err := svc.Promote(ctx, ad.ID, true, "no class!"); !errors.Is(err, ErrInvalid) || db.eps[ep] {
		t.Errorf("bad class should enable nothing: %v eps=%v", err, db.eps)
	}
	_ = svc.Promote(ctx, ad.ID, true, "chat")
	if routes, _ := svc.Routes(ctx); len(routes) != 1 || routes[0].Endpoint != ep || routes[0].TaskClass != "chat" || !db.eps[ep] {
		t.Errorf("after promote: routes=%+v eps=%v", routes, db.eps)
	}
	if err := svc.Unpromote(ctx, ad.ID); err != nil {
		t.Fatal(err)
	}
	if routes, _ := svc.Routes(ctx); len(routes) != 0 || db.eps[ep] {
		t.Errorf("after unpromote: routes=%+v eps=%v", routes, db.eps)
	}
	// Promoting without a class touches no policy; deleting the adapter
	// takes its routes out.
	_ = svc.Promote(ctx, ad.ID, true, "")
	if _, ok := db.policies[AdapterPolicy]; ok {
		t.Error("promote without a class wrote a policy")
	}
	_ = svc.addRoute(ctx, "chat", ep)
	if err := svc.DeleteAdapter(ctx, ad.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := db.policies[AdapterPolicy]; ok {
		t.Error("delete left the route")
	}

	// Preview: the first n examples of the train file.
	owner := uuid.New()
	ds, _ := db.CreateDataset(ctx, store.CreateDatasetParams{OwnerID: owner, Name: "d", Filters: []byte("{}")})
	if _, err := svc.Preview(ctx, ds.ID, false, 3); !errors.Is(err, ErrNotReady) {
		t.Errorf("preview before ready: %v", err)
	}
	var exs []Example
	for i := 0; i < 8; i++ {
		exs = append(exs, Example{Messages: []ChatMessage{{Role: "user", Content: "q"}, {Role: "assistant", Content: "a"}}, Meta: ExampleMeta{Mode: "chat"}})
	}
	key, _ := blob.PutBytes(ctx, svc.Blobs, JSONL(exs))
	_ = db.FinishDataset(ctx, store.FinishDatasetParams{ID: ds.ID, BlobKey: &key, Examples: 8})
	got, err := svc.Preview(ctx, ds.ID, false, 3)
	if err != nil || len(got) != 3 || got[0].Messages[1].Content != "a" || got[0].Meta.Mode != "chat" {
		t.Errorf("preview = %+v %v", got, err)
	}
	if got, _ := svc.Preview(ctx, ds.ID, false, 0); len(got) != 5 {
		t.Errorf("default n = %d", len(got))
	}
	if _, err := svc.Preview(ctx, ds.ID, true, 3); !errors.Is(err, ErrNotReady) {
		t.Errorf("no eval file: %v", err)
	}
}
