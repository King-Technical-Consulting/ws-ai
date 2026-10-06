package gateway

import (
	"context"
	"errors"
	"testing"
)

func testRegistry() *Registry {
	reg := NewRegistry()
	reg.Replace(
		[]*Provider{
			{ID: "anthropic", Kind: ProviderAnthropic},
			{ID: "local", Kind: ProviderOpenAICompat},
		},
		[]*Endpoint{
			{ID: "anthropic/opus", ProviderID: "anthropic", ModelName: "opus", Enabled: true,
				Capabilities: Capabilities{ContextWindow: 1_000_000, Tools: true, Vision: true}, Pricing: Pricing{InputPerM: 4, OutputPerM: 20}},
			{ID: "anthropic/haiku", ProviderID: "anthropic", ModelName: "haiku", Enabled: true,
				Capabilities: Capabilities{ContextWindow: 200_000, Tools: true, Vision: true}, Pricing: Pricing{InputPerM: 1, OutputPerM: 5}},
			{ID: "local/small", ProviderID: "local", ModelName: "small", Enabled: true, Local: true,
				Capabilities: Capabilities{ContextWindow: 32_000, Tools: true}},
			{ID: "local/down", ProviderID: "local", ModelName: "down", Enabled: true, Local: true,
				Capabilities: Capabilities{ContextWindow: 32_000}, Health: Health{Status: "down"}},
		},
	)
	return reg
}

const testPolicy = `
name: test
rules:
  - match: { task_class: [summarize] }
    prefer: [local/down, local/small, anthropic/haiku]
  - match: { task_class: [vision] }
    require: { vision: true }
    prefer: [local/small, anthropic/haiku, anthropic/opus]
  - match: {}
    prefer: [anthropic/opus, local/small]
aliases:
  cheap: [local/small, anthropic/haiku]
`

func TestRouteByTaskClassSkipsDown(t *testing.T) {
	p, err := ParsePolicy(testPolicy)
	if err != nil {
		t.Fatal(err)
	}
	r := NewRouter(testRegistry(), []Policy{p})
	cands, dec, err := r.Route(RouteInput{Selector: "auto", TaskClass: TaskSummarize})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 2 || cands[0].Endpoint.ID != "local/small" || cands[1].Endpoint.ID != "anthropic/haiku" {
		t.Errorf("candidates = %v", dec.Candidates)
	}
	if dec.Policy != "test" || dec.Rule != 0 {
		t.Errorf("decision = %+v", dec)
	}
}

func TestRouteRequireFiltersCapabilities(t *testing.T) {
	p, _ := ParsePolicy(testPolicy)
	r := NewRouter(testRegistry(), []Policy{p})
	cands, _, err := r.Route(RouteInput{Selector: "auto", TaskClass: TaskVision})
	if err != nil {
		t.Fatal(err)
	}
	// local/small has no vision; must be skipped
	if cands[0].Endpoint.ID != "anthropic/haiku" {
		t.Errorf("first = %s", cands[0].Endpoint.ID)
	}
}

func TestRouteAliasAndDirect(t *testing.T) {
	p, _ := ParsePolicy(testPolicy)
	r := NewRouter(testRegistry(), []Policy{p})
	cands, dec, err := r.Route(RouteInput{Selector: "cheap", TaskClass: TaskChat})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Reason != "alias" || cands[0].Endpoint.ID != "local/small" {
		t.Errorf("alias route = %+v", dec)
	}
	cands, dec, err = r.Route(RouteInput{Selector: "anthropic/haiku"})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Reason != "direct" || len(cands) != 1 {
		t.Errorf("direct route = %+v", dec)
	}
	if _, _, err := r.Route(RouteInput{Selector: "local/down"}); err == nil {
		t.Error("direct route to a down endpoint should fail")
	}
}

func TestRouteDowngradeToLocal(t *testing.T) {
	p, _ := ParsePolicy(testPolicy)
	r := NewRouter(testRegistry(), []Policy{p})
	cands, _, err := r.Route(RouteInput{Selector: "auto", TaskClass: TaskChat, Downgrade: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cands {
		if !c.Endpoint.Local {
			t.Errorf("non-local candidate under downgrade: %s", c.Endpoint.ID)
		}
	}
}

func TestRouteContextTooSmall(t *testing.T) {
	p, _ := ParsePolicy(testPolicy)
	r := NewRouter(testRegistry(), []Policy{p})
	cands, _, err := r.Route(RouteInput{Selector: "auto", TaskClass: TaskChat, EstTokensIn: 100_000})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cands {
		if c.Endpoint.ID == "local/small" {
			t.Error("32k endpoint should be excluded for 100k input")
		}
	}
}

// fakeAdapter scripts per-endpoint behaviour for failover tests.
type fakeAdapter struct {
	fail map[string]bool // endpoint model -> fail retryably before tokens
}

func (f *fakeAdapter) Stream(ctx context.Context, p *Provider, ep *Endpoint, req *Request) (<-chan StreamEvent, error) {
	ch := make(chan StreamEvent, 8)
	go func() {
		defer close(ch)
		if f.fail[ep.ModelName] {
			ch <- ErrorEvent(errors.New("overloaded"), true)
			return
		}
		ch <- StreamEvent{Type: EventStart, EndpointID: ep.ID, Model: ep.ModelName}
		ch <- StreamEvent{Type: EventTextDelta, Text: "hi from " + ep.ModelName}
		ch <- StreamEvent{Type: EventUsage, Usage: &Usage{InputTokens: 10, OutputTokens: 3}}
		ch <- StreamEvent{Type: EventFinish, FinishReason: FinishStop}
	}()
	return ch, nil
}

func (f *fakeAdapter) CountTokens(context.Context, *Provider, *Endpoint, *Request) (int, bool, error) {
	return 0, false, nil
}

type memRecorder struct{ recs chan UsageRecord }

func (m *memRecorder) Record(_ context.Context, r UsageRecord) { m.recs <- r }

func TestGatewayFailsOverBeforeFirstToken(t *testing.T) {
	p, _ := ParsePolicy(testPolicy)
	reg := testRegistry()
	rec := &memRecorder{recs: make(chan UsageRecord, 4)}
	g := New(reg, NewRouter(reg, []Policy{p}), rec, nil)
	fa := &fakeAdapter{fail: map[string]bool{"opus": true}}
	g.RegisterAdapter(ProviderAnthropic, fa)
	g.RegisterAdapter(ProviderOpenAICompat, fa)

	resp, err := g.Complete(context.Background(), &Request{Model: "auto",
		Messages: []Message{{Role: RoleUser, Parts: []Part{TextPart("x")}}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text() != "hi from small" {
		t.Errorf("text = %q (should have failed over from opus to local/small)", resp.Text())
	}
	r := <-rec.recs
	if r.EndpointID != "local/small" || len(r.Decision.Tried) != 2 || r.Usage.OutputTokens != 3 {
		t.Errorf("ledger = %+v", r)
	}
}

// A rule written for the GPU fleet names endpoints this box doesn't have.
// Instead of "no model available", the router falls back to any enabled
// endpoint that meets the rule's requirements, local first then cheapest.
func TestRouteRuleFallbackWhenPreferredAbsent(t *testing.T) {
	p, err := ParsePolicy(`
name: fleet
rules:
  - match: { task_class: [code] }
    require: { tools: true }
    deny: [anthropic/opus]
    prefer: [missing/opus, local-vllm/coder]
`)
	if err != nil {
		t.Fatal(err)
	}
	r := NewRouter(testRegistry(), []Policy{p})
	cands, dec, err := r.Route(RouteInput{Selector: "auto", TaskClass: TaskCode, Required: Capabilities{Tools: true}})
	if err != nil {
		t.Fatalf("expected fallback, got %v", err)
	}
	if dec.Reason != "rule-fallback" {
		t.Errorf("reason = %q", dec.Reason)
	}
	// local/small (local, tools) first, then haiku; opus denied by the rule;
	// local/down is down.
	if len(cands) != 2 || cands[0].Endpoint.ID != "local/small" || cands[1].Endpoint.ID != "anthropic/haiku" {
		t.Errorf("candidates = %v", dec.Candidates)
	}
	// Requirements still apply in the fallback.
	_, _, err = r.Route(RouteInput{Selector: "auto", TaskClass: TaskCode, Required: Capabilities{Vision: true, Tools: true}, EstTokensIn: 500_000})
	if err == nil {
		t.Errorf("fallback must still honor capability and context requirements")
	}
}

// A media endpoint is invisible to text routing and the only thing a media
// task can route to; the fallback-all path keeps the two apart as well.
func TestRouteMediaEndpointsAreSeparate(t *testing.T) {
	reg := testRegistry()
	provs, eps := reg.Providers(), reg.Endpoints()
	provs = append(provs, &Provider{ID: "openai", Kind: ProviderOpenAICompat})
	eps = append(eps, &Endpoint{ID: "openai/gpt-image-1", ProviderID: "openai", ModelName: "gpt-image-1", Enabled: true,
		Capabilities: Capabilities{Media: &MediaCaps{Engine: "openai_images", Image: true}}, Pricing: Pricing{PerImage: 0.04}})
	reg.Replace(provs, eps)
	r := NewRouter(reg, nil)

	cands, _, err := r.Route(RouteInput{Selector: "auto", TaskClass: TaskChat})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cands {
		if c.Endpoint.Capabilities.IsMedia() {
			t.Errorf("text route picked media endpoint %s", c.Endpoint.ID)
		}
	}
	req := &Request{Model: "auto", Metadata: Metadata{TaskClass: TaskImage}}
	cands, dec, err := r.Route(RouteInput{Selector: "auto", TaskClass: TaskImage, Required: RequiredCapabilities(req)})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].Endpoint.ID != "openai/gpt-image-1" {
		t.Errorf("image candidates = %v", dec.Candidates)
	}
	// Direct selection of a media endpoint by a text request is refused too.
	if _, _, err := r.Route(RouteInput{Selector: "openai/gpt-image-1", TaskClass: TaskChat}); err == nil {
		t.Error("direct text route to a media endpoint should fail")
	}
	// Video needs a video-capable media endpoint; an image one won't do.
	vreq := &Request{Model: "auto", Metadata: Metadata{TaskClass: TaskVideo}}
	if _, _, err := r.Route(RouteInput{Selector: "auto", TaskClass: TaskVideo, Required: RequiredCapabilities(vreq)}); err == nil {
		t.Error("video route should fail with only an image endpoint")
	}
}

// embedAdapter is a fakeAdapter that also embeds.
type embedAdapter struct {
	fakeAdapter
	fail map[string]bool
}

func (e *embedAdapter) Embed(_ context.Context, _ *Provider, ep *Endpoint, req *EmbedRequest) (*EmbedResponse, error) {
	if e.fail[ep.ModelName] {
		return nil, errors.New("embed down")
	}
	out := &EmbedResponse{Model: ep.ModelName, Usage: Usage{InputTokens: 7}}
	for range req.Inputs {
		out.Vectors = append(out.Vectors, []float32{1, 0})
	}
	return out, nil
}

// Embed routes only to embedding endpoints, fails over, and lands in the
// ledger under task class embed.
func TestGatewayEmbedRoutesAndFailsOver(t *testing.T) {
	reg := testRegistry()
	provs, eps := reg.Providers(), reg.Endpoints()
	eps = append(eps,
		&Endpoint{ID: "local/embed-a", ProviderID: "local", ModelName: "embed-a", Enabled: true, Local: true, Capabilities: Capabilities{Embeddings: true}},
		&Endpoint{ID: "local/embed-b", ProviderID: "local", ModelName: "embed-b", Enabled: true, Local: true, Capabilities: Capabilities{Embeddings: true}},
	)
	reg.Replace(provs, eps)
	p, _ := ParsePolicy("name: t\nrules:\n  - match: { task_class: [embed] }\n    prefer: [local/embed-a, local/embed-b]\n")
	rec := &memRecorder{recs: make(chan UsageRecord, 4)}
	g := New(reg, NewRouter(reg, []Policy{p}), rec, nil)
	ad := &embedAdapter{fail: map[string]bool{"embed-a": true}}
	g.RegisterAdapter(ProviderOpenAICompat, ad)
	g.RegisterAdapter(ProviderAnthropic, &fakeAdapter{}) // cannot embed

	resp, err := g.Embed(context.Background(), &EmbedRequest{Model: "auto", Inputs: []string{"a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.EndpointID != "local/embed-b" || len(resp.Vectors) != 2 {
		t.Errorf("resp = %+v", resp)
	}
	r := <-rec.recs
	if r.EndpointID != "local/embed-b" || r.Metadata.TaskClass != TaskEmbed || len(r.Decision.Tried) != 2 || r.Usage.InputTokens != 7 {
		t.Errorf("ledger = %+v", r)
	}
	// A chat endpoint named directly is refused: it is not an embedding model.
	if _, err := g.Embed(context.Background(), &EmbedRequest{Model: "anthropic/haiku", Inputs: []string{"a"}}); err == nil {
		t.Error("direct embed on a chat endpoint should fail")
	}
	// No inputs is a no-op.
	if resp, err := g.Embed(context.Background(), &EmbedRequest{Model: "auto"}); err != nil || len(resp.Vectors) != 0 {
		t.Errorf("empty = %+v %v", resp, err)
	}
}
