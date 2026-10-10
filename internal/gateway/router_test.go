package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

func TestRouteBlamesTheImageBeforeTheKey(t *testing.T) {
	p, _ := ParsePolicy(testPolicy)
	// The shared registry's anthropic is not hosted; here it takes a key.
	reg := testRegistry()
	provs := []*Provider{{ID: "anthropic", Kind: ProviderAnthropic, Hosted: true, APIKey: "server-key"}, {ID: "local", Kind: ProviderOpenAICompat}}
	reg.Replace(provs, reg.Endpoints())
	r := NewRouter(reg, []Policy{p})
	// "cheap" names local/small (no vision) and anthropic/haiku (vision,
	// hosted). A member with no key for anthropic sends an image: no route,
	// and the error names the image first, then the key.
	noKeys := &KeyAccess{Own: map[string]bool{}}
	_, _, err := r.Route(RouteInput{Selector: "cheap", TaskClass: TaskChat, Required: Capabilities{Vision: true}, Keys: noKeys})
	if err == nil || !strings.Contains(err.Error(), "needs a model that can read images: needs your own API key") {
		t.Fatalf("err = %v", err)
	}
	// Without the image the same person routes to the local model.
	if cands, _, err := r.Route(RouteInput{Selector: "cheap", TaskClass: TaskChat, Keys: noKeys}); err != nil || cands[0].Endpoint.ID != "local/small" {
		t.Fatalf("without the image: %v %v", cands, err)
	}
	// A text-only direct pick says so itself.
	if _, _, err := r.Route(RouteInput{Selector: "local/small", Required: Capabilities{Vision: true}}); err == nil || !strings.Contains(err.Error(), "no vision") {
		t.Fatalf("direct: %v", err)
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

// An alias or auto drops the per-endpoint reasons, so a prompt too long for
// every endpoint must still say it is the size that left no route.
func TestRouteNoRouteBlamesSizeOnlyWhenSizeIsTheCause(t *testing.T) {
	p, _ := ParsePolicy(testPolicy)
	r := NewRouter(testRegistry(), []Policy{p})
	_, _, err := r.Route(RouteInput{Selector: "auto", TaskClass: TaskChat, EstTokensIn: 50_000_000})
	if err == nil || !strings.Contains(err.Error(), "context too small") {
		t.Errorf("a prompt larger than every window: err = %v, want it to say context too small", err)
	}
	// A request nothing could serve at any size is not blamed on its size.
	_, _, err = r.Route(RouteInput{Selector: "auto", TaskClass: TaskChat, Required: Capabilities{Embeddings: true}, EstTokensIn: 50_000_000})
	if err == nil || strings.Contains(err.Error(), "context too small") {
		t.Errorf("an unservable capability: err = %v, want no size blame", err)
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

// ContextCeiling is the window compaction has to fit a request into: the
// endpoint's own for a direct pick, the largest eligible one for an alias
// or rule, and nothing a key the person lacks could unlock.
func TestContextCeiling(t *testing.T) {
	p, err := ParsePolicy(testPolicy)
	if err != nil {
		t.Fatal(err)
	}
	r := NewRouter(testRegistry(), []Policy{p})
	cases := []struct {
		name string
		in   RouteInput
		want int
	}{
		{"direct", RouteInput{Selector: "local/small"}, 32_000},
		{"direct down", RouteInput{Selector: "local/down"}, 0},
		{"alias of small and haiku", RouteInput{Selector: "cheap"}, 200_000},
		{"auto prefers opus", RouteInput{Selector: "auto", TaskClass: TaskChat}, 1_000_000},
		{"summarize rule, down skipped", RouteInput{Selector: "auto", TaskClass: TaskSummarize}, 200_000},
		{"budget downgrade: local only", RouteInput{Selector: "auto", TaskClass: TaskChat, Downgrade: true}, 32_000},
		{"a size that fits nothing is ignored", RouteInput{Selector: "cheap", EstTokensIn: 5_000_000}, 200_000},
	}
	for _, c := range cases {
		if got := r.ContextCeiling(c.in); got != c.want {
			t.Errorf("%s: ceiling = %d, want %d", c.name, got, c.want)
		}
	}
}

// A rule that matches on a selector gives that name meaning: it routes
// with the rule's rank, and Selectors lists it beside the aliases so the
// API, Settings and the MCP server offer it.
func TestSelectorRuleIsANameWithARank(t *testing.T) {
	p, err := ParsePolicy(`
name: t
rules:
  - match: { selector: [fast] }
    rank: throughput
    prefer: [anthropic/haiku, local/small]
  - match: {}
    prefer: [anthropic/opus]
aliases:
  cheap: [local/small]
`)
	if err != nil {
		t.Fatal(err)
	}
	reg := testRegistry()
	r := NewRouter(reg, []Policy{p})
	cands, dec, err := r.Route(RouteInput{Selector: "fast", TaskClass: TaskChat})
	if err != nil || len(cands) != 2 || dec.Reason != "rule+throughput" {
		t.Fatalf("fast: cands=%v reason=%q err=%v", dec.Candidates, dec.Reason, err)
	}
	sel := p.Selectors()
	if _, ok := sel["fast"]; !ok || len(sel["fast"]) != 2 {
		t.Errorf("Selectors lacks fast: %v", sel)
	}
	if _, ok := sel["cheap"]; !ok {
		t.Errorf("Selectors lacks the alias: %v", sel)
	}
	if r.ContextCeiling(RouteInput{Selector: "fast", TaskClass: TaskChat}) != 200_000 {
		t.Errorf("fast ceiling should be haiku's window")
	}
}

// With no media engine to go to, the router says that instead of blaming a
// missing key; with one that is enabled, a missing key is still the reason.
func TestRouteMediaBlame(t *testing.T) {
	img := Capabilities{Media: &MediaCaps{Engine: "comfyui", Image: true}}
	vid := Capabilities{Media: &MediaCaps{Engine: "fal", Video: true}}
	providers := []*Provider{{ID: "comfyui", Kind: ProviderOpenAICompat}, {ID: "fal", Kind: ProviderOpenAICompat, Hosted: true}}
	cases := []struct {
		name string
		eps  []*Endpoint
		need *MediaCaps
		kind string
		off  bool // the error is a *NoMediaError with Disabled set
		none bool // the error is a *NoMediaError
		want string
	}{
		{"no media endpoint at all", nil, &MediaCaps{Image: true}, "image", false, true, "no image endpoint is configured"},
		{"only a video endpoint for an image", []*Endpoint{{ID: "fal/v", ProviderID: "fal", Enabled: true, Capabilities: vid}}, &MediaCaps{Image: true}, "image", false, true, "no image endpoint is configured"},
		{"the image endpoint is disabled", []*Endpoint{{ID: "comfyui/sdxl", ProviderID: "comfyui", Capabilities: img}}, &MediaCaps{Image: true}, "image", true, true, "every image endpoint is disabled"},
		{"no video endpoint", []*Endpoint{{ID: "comfyui/sdxl", ProviderID: "comfyui", Enabled: true, Capabilities: img}}, &MediaCaps{Video: true}, "video", false, true, "no video endpoint is configured"},
		{"an enabled endpoint that needs a key", []*Endpoint{{ID: "fal/img", ProviderID: "fal", Enabled: true, Capabilities: img}}, &MediaCaps{Image: true}, "image", false, false, "needs your own API key"},
		{"the only image endpoint is down", []*Endpoint{
			{ID: "comfyui/sdxl", ProviderID: "comfyui", Enabled: true, Capabilities: img, Health: Health{Status: "down", Error: "connection refused"}},
			{ID: "fal/img", ProviderID: "fal", Enabled: true, Capabilities: img}}, &MediaCaps{Image: true}, "image", false, true,
			"the image endpoint comfyui/sdxl is down: connection refused; the owner checks the server it points at (COMFYUI_URL) and Admin, endpoints"},
		{"a down hosted endpoint is not the person's to fix", []*Endpoint{{ID: "fal/img", ProviderID: "fal", Enabled: true, Capabilities: img, Health: Health{Status: "down"}}}, &MediaCaps{Image: true}, "image", false, false, "no endpoint satisfies selector"},
		{"every image endpoint is down", []*Endpoint{
			{ID: "comfyui/sdxl", ProviderID: "comfyui", Enabled: true, Capabilities: img, Health: Health{Status: "down"}},
			{ID: "comfyui/flux", ProviderID: "comfyui", Enabled: true, Capabilities: img, Health: Health{Status: "down"}}}, &MediaCaps{Image: true}, "image", false, true,
			"every image endpoint is down (comfyui/flux, comfyui/sdxl)"},
	}
	for _, c := range cases {
		reg := NewRegistry()
		reg.Replace(providers, c.eps)
		r := NewRouter(reg, nil)
		_, _, err := r.Route(RouteInput{Selector: "auto", TaskClass: TaskImage, Required: Capabilities{Media: c.need},
			Keys: &KeyAccess{Shared: false, Own: map[string]bool{}}})
		if err == nil {
			t.Fatalf("%s: routed", c.name)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q lacks %q", c.name, err, c.want)
		}
		var nm *NoMediaError
		if got := errors.As(err, &nm); got != c.none {
			t.Errorf("%s: NoMediaError = %v, want %v (%v)", c.name, got, c.none, err)
		} else if got && (nm.Kind != c.kind || nm.Disabled != c.off) {
			t.Errorf("%s: %+v", c.name, nm)
		}
		if c.none && strings.Contains(err.Error(), "API key") {
			t.Errorf("%s: blames a key: %q", c.name, err)
		}
		if !c.none && strings.Contains(err.Error(), " is down") {
			t.Errorf("%s: says an endpoint is down: %q", c.name, err)
		}
	}
	// A degraded endpoint is still routed to.
	reg := NewRegistry()
	reg.Replace(providers, []*Endpoint{{ID: "comfyui/sdxl", ProviderID: "comfyui", Enabled: true, Capabilities: img, Health: Health{Status: "degraded"}}})
	if _, _, err := NewRouter(reg, nil).Route(RouteInput{Selector: "auto", TaskClass: TaskImage, Required: Capabilities{Media: &MediaCaps{Image: true}}}); err != nil {
		t.Errorf("degraded endpoint: %v", err)
	}
}

// The routing error prints the media capabilities by value, not as a pointer.
func TestCapabilitiesStringShowsMediaByValue(t *testing.T) {
	got := fmt.Sprintf("%+v", Capabilities{Media: &MediaCaps{Image: true}})
	if strings.Contains(got, "0x") {
		t.Errorf("pointer in %q", got)
	}
	if !strings.Contains(got, "Media:{Engine: Image:true") {
		t.Errorf("media not printed by value: %q", got)
	}
	if got := fmt.Sprintf("%+v", Capabilities{Tools: true}); !strings.Contains(got, "Tools:true") || !strings.Contains(got, "Media:<nil>") {
		t.Errorf("text caps: %q", got)
	}
}
