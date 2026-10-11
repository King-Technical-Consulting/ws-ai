package gateway

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// verdictAdapter answers verdict calls (task class route) with a scripted
// reply and every other request with the endpoint it reached.
type verdictAdapter struct {
	mu      sync.Mutex
	reply   string
	block   time.Duration
	calls   int
	lastReq *Request
}

func (a *verdictAdapter) Stream(ctx context.Context, p *Provider, ep *Endpoint, req *Request) (<-chan StreamEvent, error) {
	ch := make(chan StreamEvent, 8)
	isVerdict := req.Metadata.TaskClass == TaskRoute
	if isVerdict {
		a.mu.Lock()
		a.calls++
		a.lastReq = req
		a.mu.Unlock()
	}
	go func() {
		defer close(ch)
		text := "chat from " + ep.ID
		if isVerdict {
			if a.block > 0 {
				select {
				case <-time.After(a.block):
				case <-ctx.Done():
					ch <- ErrorEvent(ctx.Err(), false)
					return
				}
			}
			a.mu.Lock()
			text = a.reply
			a.mu.Unlock()
		}
		ch <- StreamEvent{Type: EventStart, EndpointID: ep.ID, Model: ep.ModelName}
		ch <- StreamEvent{Type: EventTextDelta, Text: text}
		ch <- StreamEvent{Type: EventUsage, Usage: &Usage{InputTokens: 100, OutputTokens: 20}}
		ch <- StreamEvent{Type: EventFinish, FinishReason: FinishStop}
	}()
	return ch, nil
}

func (a *verdictAdapter) CountTokens(context.Context, *Provider, *Endpoint, *Request) (int, bool, error) {
	return 0, false, nil
}

func (a *verdictAdapter) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

const verdictPolicy = `
name: v
rules:
  - match: { task_class: [route] }
    prefer: [local/judge]
  - match: { task_class: [classify] }
    prefer: [local/judge]
  - match: { verdict: [simple-chat] }
    prefer: [local/cheap, hosted/big]
  - match: { task_class: [summarize] }
    prefer: [local/cheap]
  - match: { task_class: [code] }
    require: { tools: true }
    prefer: [hosted/big]
  - match: { task_class: [vision] }
    require: { vision: true }
    prefer: [hosted/big]
  - match: {}
    min_tier: medium
    prefer: [hosted/big, local/cheap]
`

func verdictGateway(t *testing.T, policy string, cfg VerdictConfig) (*Gateway, *verdictAdapter, *memRecorder) {
	t.Helper()
	reg := NewRegistry()
	reg.Replace(
		[]*Provider{{ID: "local", Kind: ProviderOpenAICompat}, {ID: "hosted", Kind: ProviderAnthropic}},
		[]*Endpoint{
			{ID: "hosted/big", ProviderID: "hosted", ModelName: "big", Enabled: true, Tier: TierLarge,
				Capabilities: Capabilities{ContextWindow: 200000, Tools: true, Vision: true}, Pricing: Pricing{InputPerM: 3, OutputPerM: 15}},
			{ID: "local/cheap", ProviderID: "local", ModelName: "cheap-8b", Enabled: true, Local: true, Tier: TierSmall,
				Capabilities: Capabilities{ContextWindow: 8000}},
			{ID: "local/judge", ProviderID: "local", ModelName: "judge-20b", Enabled: true, Local: true, NotRoutable: true,
				Capabilities: Capabilities{ContextWindow: 8000}},
		},
	)
	p, err := ParsePolicy(policy)
	if err != nil {
		t.Fatal(err)
	}
	rec := &memRecorder{recs: make(chan UsageRecord, 64)}
	g := New(reg, NewRouter(reg, []Policy{p}), rec, nil)
	ad := &verdictAdapter{}
	g.RegisterAdapter(ProviderAnthropic, ad)
	g.RegisterAdapter(ProviderOpenAICompat, ad)
	g.EnableVerdicts(cfg)
	return g, ad, rec
}

func chatReq(text string) *Request {
	return &Request{Model: "auto", Messages: []Message{{Role: RoleUser, Parts: []Part{TextPart(text)}}},
		Metadata: Metadata{UserID: "u1", ConversationID: "c1", TaskClass: TaskChat}}
}

func prepare(t *testing.T, g *Gateway, req *Request) ([]Candidate, Decision) {
	t.Helper()
	c, d, err := g.Prepare(context.Background(), req)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	return c, d
}

func TestVerdictClassesMapToTheRouteAndTheDecision(t *testing.T) {
	cases := []struct {
		reply  string
		class  string
		chosen string
	}{
		{`{"class":"simple-chat","needs":{},"sensitive":false,"reason":"hi"}`, "simple-chat", "local/cheap"},
		{`{"class":"hard-chat","needs":{},"sensitive":false,"reason":"x"}`, "hard-chat", "hosted/big"},
		{`{"class":"code","needs":{"tools":true},"sensitive":false,"reason":"x"}`, "code", "hosted/big"},
		{"```json\n" + `{"class":"summarize","needs":{},"sensitive":false,"reason":"x"}` + "\n```", "summarize", "local/cheap"},
		{`{"class":"vision","needs":{"vision":true},"sensitive":false,"reason":"x"}`, "vision", "hosted/big"},
		{`{"class":"long-context","needs":{"long_context":true},"sensitive":false,"reason":"x"}`, "long-context", "hosted/big"},
	}
	for _, c := range cases {
		g, ad, _ := verdictGateway(t, verdictPolicy, VerdictConfig{})
		ad.reply = c.reply
		cands, dec := prepare(t, g, chatReq("hello there"))
		if dec.Verdict != c.class || dec.VerdictSource != "llm" {
			t.Errorf("%s: verdict %q source %q", c.class, dec.Verdict, dec.VerdictSource)
		}
		if got := cands[0].Endpoint.ID; got != c.chosen {
			t.Errorf("%s: routed to %s, want %s", c.class, got, c.chosen)
		}
	}
}

func TestVerdictOffIsToday(t *testing.T) {
	g, ad, _ := verdictGateway(t, verdictPolicy, VerdictConfig{})
	g.verdicts = nil
	cands, dec := prepare(t, g, chatReq("hello"))
	if ad.callCount() != 0 || dec.Verdict != "" || dec.VerdictSource != "" {
		t.Errorf("calls %d decision %+v", ad.callCount(), dec)
	}
	if cands[0].Endpoint.ID != "hosted/big" {
		t.Errorf("routed to %s", cands[0].Endpoint.ID)
	}
}

func TestVerdictFallsThroughOnBadAnswers(t *testing.T) {
	for name, reply := range map[string]string{
		"not json":      "I think this is easy",
		"unknown class": `{"class":"chitchat","needs":{},"sensitive":false,"reason":""}`,
	} {
		g, ad, _ := verdictGateway(t, verdictPolicy, VerdictConfig{})
		ad.reply = reply
		cands, dec := prepare(t, g, chatReq("hello"))
		if dec.Verdict != "" || !strings.HasPrefix(dec.VerdictSource, "fallthrough") {
			t.Errorf("%s: %+v", name, dec)
		}
		if cands[0].Endpoint.ID != "hosted/big" {
			t.Errorf("%s: routing changed: %s", name, cands[0].Endpoint.ID)
		}
	}
}

func TestVerdictDeadline(t *testing.T) {
	g, ad, _ := verdictGateway(t, verdictPolicy, VerdictConfig{Deadline: 50 * time.Millisecond})
	ad.reply = `{"class":"simple-chat","needs":{},"sensitive":false,"reason":""}`
	ad.block = 2 * time.Second
	start := time.Now()
	cands, dec := prepare(t, g, chatReq("hello"))
	if time.Since(start) > time.Second {
		t.Errorf("the turn waited %v for a verdict", time.Since(start))
	}
	if dec.VerdictSource != "fallthrough:deadline" || cands[0].Endpoint.ID != "hosted/big" {
		t.Errorf("decision %+v chosen %s", dec, cands[0].Endpoint.ID)
	}
}

func TestVerdictSkips(t *testing.T) {
	cases := []struct {
		name   string
		req    func() *Request
		policy string
		want   string
	}{
		{"sensitive text", func() *Request { return chatReq("my password is hunter2, is that fine?") }, verdictPolicy, "skipped:sensitive"},
		{"code mode", func() *Request { r := chatReq("fix the race"); r.Metadata.TaskClass = TaskCode; return r }, verdictPolicy, "skipped:code"},
		{"external caller", func() *Request { r := chatReq("hello"); r.Metadata.External = true; return r }, verdictPolicy, "skipped:external"},
		{"caller-set class", func() *Request { r := chatReq("hello"); r.Metadata.TaskClass = TaskSummarize; return r }, verdictPolicy, "skipped:class"},
		{"empty", func() *Request { return chatReq("   ") }, verdictPolicy, "skipped:empty"},
		{"local-only rule", func() *Request { return chatReq("hello") }, strings.Replace(verdictPolicy, "  - match: {}\n    min_tier: medium", "  - match: {}\n    local_only: true\n    min_tier: medium", 1), "skipped:local_only"},
	}
	for _, c := range cases {
		g, ad, _ := verdictGateway(t, c.policy, VerdictConfig{})
		ad.reply = `{"class":"hard-chat","needs":{},"sensitive":false,"reason":""}`
		_, dec, err := g.Prepare(context.Background(), c.req())
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if ad.callCount() != 0 {
			t.Errorf("%s: %d classifier calls", c.name, ad.callCount())
		}
		if dec.VerdictSource != c.want {
			t.Errorf("%s: source %q, want %q", c.name, dec.VerdictSource, c.want)
		}
	}
}

func TestVerdictBudgetDowngradeSkips(t *testing.T) {
	g, ad, _ := verdictGateway(t, verdictPolicy, VerdictConfig{})
	g.Use(func(_ context.Context, _ *Request, in *RouteInput) error { in.Downgrade = true; return nil })
	ad.reply = `{"class":"hard-chat","needs":{},"sensitive":false,"reason":""}`
	_, dec := prepare(t, g, chatReq("hello"))
	if ad.callCount() != 0 || dec.VerdictSource != "skipped:local_only" {
		t.Errorf("calls %d source %q", ad.callCount(), dec.VerdictSource)
	}
}

func TestSensitiveVerdictForcesLocalAndNeverLoosens(t *testing.T) {
	g, ad, _ := verdictGateway(t, verdictPolicy, VerdictConfig{})
	ad.reply = `{"class":"hard-chat","needs":{},"sensitive":true,"reason":"health"}`
	cands, dec := prepare(t, g, chatReq("what should I do about the lump?"))
	for _, c := range cands {
		if !c.Endpoint.Local {
			t.Errorf("a sensitive turn reached hosted %s", c.Endpoint.ID)
		}
	}
	// No local endpoint is large, so the hard-chat floor finds nothing and
	// the turn is routed as without a verdict, still local only.
	if dec.Verdict != "" || dec.VerdictSource != "fallthrough:no-route" {
		t.Errorf("decision %+v", dec)
	}
	// A sensitive simple-chat keeps its verdict and goes local.
	g2, ad2, _ := verdictGateway(t, verdictPolicy, VerdictConfig{})
	ad2.reply = `{"class":"simple-chat","needs":{},"sensitive":true,"reason":"health"}`
	cands, dec = prepare(t, g2, chatReq("is this normal?"))
	if dec.Verdict != "simple-chat" || !cands[0].Endpoint.Local {
		t.Errorf("decision %+v first %s", dec, cands[0].Endpoint.ID)
	}
	// A verdict that says "not sensitive" does not undo the rules' check:
	// that text never reached the classifier (TestVerdictSkips).
}

func TestVerdictNeverRemovesARequirement(t *testing.T) {
	g, ad, _ := verdictGateway(t, verdictPolicy, VerdictConfig{})
	ad.reply = `{"class":"simple-chat","needs":{"tools":false,"vision":false},"sensitive":false,"reason":""}`
	req := chatReq("hi")
	req.Tools = []ToolDef{{Name: "web_fetch"}}
	cands, _ := prepare(t, g, req)
	for _, c := range cands {
		if !c.Endpoint.Capabilities.Tools {
			t.Errorf("a turn with tools reached %s, which has none", c.Endpoint.ID)
		}
	}
}

func TestVerdictFloorThatFindsNothingIsRetriedWithoutIt(t *testing.T) {
	// Only a small endpoint exists; hard-chat wants large. The turn must
	// still be answered, as it would be without the classifier.
	ad := &verdictAdapter{reply: `{"class":"hard-chat","needs":{},"sensitive":false,"reason":""}`}
	reg := NewRegistry()
	reg.Replace(
		[]*Provider{{ID: "local", Kind: ProviderOpenAICompat}},
		[]*Endpoint{
			{ID: "local/cheap", ProviderID: "local", ModelName: "cheap-8b", Enabled: true, Local: true, Tier: TierSmall, Capabilities: Capabilities{ContextWindow: 8000}},
			{ID: "local/judge", ProviderID: "local", ModelName: "judge-20b", Enabled: true, Local: true, NotRoutable: true, Capabilities: Capabilities{ContextWindow: 8000}},
		},
	)
	p, _ := ParsePolicy(verdictPolicy)
	g2 := New(reg, NewRouter(reg, []Policy{p}), &memRecorder{recs: make(chan UsageRecord, 8)}, nil)
	g2.RegisterAdapter(ProviderOpenAICompat, ad)
	g2.EnableVerdicts(VerdictConfig{})
	cands, dec := prepare(t, g2, chatReq("a hard question"))
	if len(cands) == 0 || cands[0].Endpoint.ID != "local/cheap" {
		t.Fatalf("candidates = %v", cands)
	}
	if dec.Verdict != "" || dec.VerdictSource != "fallthrough:no-route" {
		t.Errorf("decision %+v", dec)
	}
}

func TestVerdictRecursionGuard(t *testing.T) {
	g, ad, _ := verdictGateway(t, verdictPolicy, VerdictConfig{})
	ad.reply = `{"class":"simple-chat","needs":{},"sensitive":false,"reason":""}`
	prepare(t, g, chatReq("hello"))
	if n := ad.callCount(); n != 1 {
		t.Fatalf("one turn made %d classifier calls", n)
	}
	ad.mu.Lock()
	lr := ad.lastReq
	ad.mu.Unlock()
	if lr.Metadata.TaskClass != TaskRoute || !lr.Metadata.NoVerdict {
		t.Errorf("the verdict request must carry the guards: %+v", lr.Metadata)
	}
	// A request that carries either guard is never classified.
	for _, md := range []Metadata{{TaskClass: TaskRoute}, {TaskClass: TaskClassify}, {TaskClass: TaskChat, NoVerdict: true}} {
		r := chatReq("hello")
		r.Metadata = md
		before := ad.callCount()
		_, _, _ = g.Prepare(context.Background(), r)
		if ad.callCount() != before {
			t.Errorf("%+v was classified", md)
		}
	}
}

func TestVerdictSendsOnlyTheLastMessageClipped(t *testing.T) {
	g, ad, _ := verdictGateway(t, verdictPolicy, VerdictConfig{})
	ad.reply = `{"class":"simple-chat","needs":{},"sensitive":false,"reason":""}`
	req := chatReq(strings.Repeat("A", 5000) + " THE END")
	req.System = "SYSTEM SECRET INSTRUCTIONS"
	req.Messages = append([]Message{{Role: RoleUser, Parts: []Part{TextPart("EARLIER TURN")}}, {Role: RoleAssistant, Parts: []Part{TextPart("EARLIER REPLY")}}}, req.Messages...)
	prepare(t, g, req)
	ad.mu.Lock()
	lr := ad.lastReq
	ad.mu.Unlock()
	sent := lr.System + " " + lr.Messages[0].Parts[0].Text
	for _, banned := range []string{"SYSTEM SECRET", "EARLIER TURN", "EARLIER REPLY"} {
		if strings.Contains(sent, banned) {
			t.Errorf("the classifier saw %q", banned)
		}
	}
	if len(lr.Messages[0].Parts[0].Text) > verdictInputChars+300 || !strings.Contains(lr.Messages[0].Parts[0].Text, "THE END") {
		t.Errorf("the clipped input is %d bytes and lost the tail", len(lr.Messages[0].Parts[0].Text))
	}
}

func TestVerdictCache(t *testing.T) {
	g, ad, _ := verdictGateway(t, verdictPolicy, VerdictConfig{ReuseTurns: 3})
	ad.reply = `{"class":"hard-chat","needs":{},"sensitive":false,"reason":""}`
	var sources []string
	for i := 0; i < 3; i++ {
		_, dec := prepare(t, g, chatReq("next question"))
		sources = append(sources, dec.VerdictSource)
	}
	if strings.Join(sources, ",") != "llm,cache,cache" || ad.callCount() != 1 {
		t.Errorf("sources %v calls %d", sources, ad.callCount())
	}
	// A long message re-classifies.
	if _, dec := prepare(t, g, chatReq(strings.Repeat("long ", 400))); dec.VerdictSource != "llm" {
		t.Errorf("a long message used %q", dec.VerdictSource)
	}
	// A new image re-classifies.
	img := chatReq("look")
	img.Messages[0].Parts = append(img.Messages[0].Parts, Part{Kind: PartImage, MIME: "image/png", Data: []byte("x")})
	if _, dec := prepare(t, g, img); dec.VerdictSource != "llm" {
		t.Errorf("a new image used %q", dec.VerdictSource)
	}
	// No conversation id: no cache.
	g2, ad2, _ := verdictGateway(t, verdictPolicy, VerdictConfig{})
	ad2.reply = `{"class":"hard-chat","needs":{},"sensitive":false,"reason":""}`
	for i := 0; i < 2; i++ {
		r := chatReq("hi")
		r.Metadata.ConversationID = ""
		prepare(t, g2, r)
	}
	if ad2.callCount() != 2 {
		t.Errorf("without a conversation id each turn is classified: %d calls", ad2.callCount())
	}
}

func TestVerdictDailyCap(t *testing.T) {
	spent := 0.50
	g, ad, _ := verdictGateway(t, verdictPolicy, VerdictConfig{
		MaxUSDDay:  func(context.Context) float64 { return 1.00 },
		SpentToday: func(context.Context) (float64, error) { return spent, nil },
	})
	ad.reply = `{"class":"hard-chat","needs":{},"sensitive":false,"reason":""}`
	if _, dec := prepare(t, g, chatReq("one")); dec.VerdictSource != "llm" {
		t.Fatalf("under the cap: %q", dec.VerdictSource)
	}
	g.verdicts.mu.Lock()
	g.verdicts.spend, g.verdicts.spendSet, g.verdicts.spendAt = 1.00, true, time.Now()
	g.verdicts.mu.Unlock()
	r := chatReq("two")
	r.Metadata.ConversationID = "other"
	before := ad.callCount()
	_, dec := prepare(t, g, r)
	if dec.VerdictSource != "skipped:budget" || ad.callCount() != before {
		t.Errorf("at the cap: source %q, %d new calls", dec.VerdictSource, ad.callCount()-before)
	}
}

func TestVerdictCallIsOnTheLedgerUnderItsOwnClass(t *testing.T) {
	g, ad, rec := verdictGateway(t, verdictPolicy, VerdictConfig{})
	ad.reply = `{"class":"simple-chat","needs":{},"sensitive":false,"reason":""}`
	prepare(t, g, chatReq("hello"))
	select {
	case r := <-rec.recs:
		if r.Metadata.TaskClass != TaskRoute || r.EndpointID != "local/judge" {
			t.Errorf("ledger row = %+v", r)
		}
	case <-time.After(time.Second):
		t.Fatal("the verdict call was not recorded")
	}
}

func TestParseVerdict(t *testing.T) {
	v, err := ParseVerdict("Sure! {\"class\":\"code\",\"needs\":{\"tools\":true},\"sensitive\":false,\"reason\":\"" + strings.Repeat("x", 300) + "\"} done")
	if err != nil || v.Class != "code" || !v.Needs.Tools || len(v.Reason) != 200 {
		t.Errorf("%+v %v", v, err)
	}
	for _, bad := range []string{"", "no json here", `{"class":""}`, `{"class":"other"}`, `{"class":`} {
		if _, err := ParseVerdict(bad); err == nil {
			t.Errorf("%q should not parse", bad)
		}
	}
}

func TestClipHeadTailKeepsRunes(t *testing.T) {
	s := strings.Repeat("é", 2000)
	out := clipHeadTail(s, 1000)
	if len(out) > 1100 || !strings.Contains(out, "[...]") {
		t.Errorf("len %d", len(out))
	}
	for _, r := range out {
		if r == '�' {
			t.Fatal("split a rune")
		}
	}
}

// The shipped policy and endpoint seed, loaded as the server loads them:
// every verdict class must resolve to at least one candidate on a box that
// has the local models and an OpenRouter key.
func TestShippedPolicyResolvesEveryVerdictClass(t *testing.T) {
	seed, err := LoadSeed("../../config/endpoints.yaml")
	if err != nil {
		t.Fatal(err)
	}
	pol, err := LoadPolicyDir("../../config/policies")
	if err != nil {
		t.Fatal(err)
	}
	provs, eps := seed.Resolve(func(string) string { return "http://set.invalid" })
	reg := NewRegistry()
	reg.Replace(provs, eps)
	r := NewRouter(reg, pol)
	for _, class := range []string{VerdictSimpleChat, VerdictHardChat, VerdictCode, VerdictSummarize, VerdictVision, VerdictLongContext} {
		in := RouteInput{Selector: "auto", TaskClass: TaskChat, Required: Capabilities{Tools: class == VerdictCode}}
		Verdict{Class: class, Needs: VerdictNeeds{Vision: class == VerdictVision}}.Apply(&in)
		cands, dec, err := r.Route(in)
		if err != nil || len(cands) == 0 {
			t.Errorf("%s: no route: %v (decision %+v)", class, err, dec)
		}
	}
	// And the verdict call itself routes to a cheap endpoint.
	cands, _, err := r.Route(RouteInput{Selector: "auto", TaskClass: TaskRoute})
	if err != nil || len(cands) == 0 {
		t.Fatalf("the verdict call has no route: %v", err)
	}
	for _, c := range cands {
		if !c.Endpoint.Local && c.Endpoint.Pricing.OutputPerM > 2 {
			t.Errorf("the verdict call may reach %s at $%v/M out", c.Endpoint.ID, c.Endpoint.Pricing.OutputPerM)
		}
	}
}
