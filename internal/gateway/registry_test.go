package gateway

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The seed uses snake_case keys; every struct field must carry a yaml tag or
// the value silently parses as zero (pricing and context windows did).
func TestSeedParsesSnakeCaseFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "endpoints.yaml")
	err := os.WriteFile(path, []byte(`
providers:
  - id: or
    kind: openai_compat
    name: OpenRouter
    base_url: https://openrouter.ai/api/v1
    api_key_env: TEST_OR_KEY
endpoints:
  - id: or/m
    provider: or
    model: vendor/m
    display_name: M
    capabilities: { context_window: 128000, max_output: 16000, tools: true, vision: false, json_mode: true, prompt_cache: true }
    pricing: { input_per_m: 0.30, output_per_m: 0.88, cache_read_per_m: 0.05 }
    throughput_class: medium
    latency_class: normal
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	sf, err := LoadSeed(path)
	if err != nil {
		t.Fatal(err)
	}
	e := sf.Endpoints[0]
	if e.Capabilities.ContextWindow != 128000 || e.Capabilities.MaxOutput != 16000 || !e.Capabilities.JSONMode || !e.Capabilities.PromptCache {
		t.Errorf("capabilities not parsed: %+v", e.Capabilities)
	}
	if e.Pricing.InputPerM != 0.30 || e.Pricing.OutputPerM != 0.88 || e.Pricing.CacheReadPerM != 0.05 {
		t.Errorf("pricing not parsed: %+v", e.Pricing)
	}
	cost := e.Pricing.Cost(Usage{InputTokens: 534, OutputTokens: 15})
	if cost < 0.00017 || cost > 0.00018 {
		t.Errorf("cost = %f, want ~0.000173", cost)
	}

	// A hosted provider without a server key stays, keyless and Hosted, so a
	// person's own key can reach it; with a key it carries the key.
	provs, eps := sf.Resolve(func(string) string { return "" })
	if len(provs) != 1 || len(eps) != 1 || provs[0].APIKey != "" || !provs[0].Hosted {
		t.Errorf("keyless hosted provider should stay without a key, got %d/%d %+v", len(provs), len(eps), provs)
	}
	provs, eps = sf.Resolve(func(k string) string { return "sk-test" })
	if len(provs) != 1 || len(eps) != 1 || provs[0].APIKey != "sk-test" {
		t.Errorf("resolve with key: %d providers %d endpoints", len(provs), len(eps))
	}
}

// Shipped config must parse and reference only providers it defines, with
// non-zero pricing on every hosted endpoint.
func TestShippedEndpointsSeed(t *testing.T) {
	sf, err := LoadSeed(filepath.Join("..", "..", "config", "endpoints.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	local := map[string]bool{}
	for _, p := range sf.Providers {
		local[p.ID] = p.BaseURLEnv != ""
	}
	for _, e := range sf.Endpoints {
		if m := e.Capabilities.Media; m != nil {
			// Media endpoints: an engine, something they can make, and a
			// per-output price when hosted.
			if m.Engine == "" || !(m.Image || m.ImageEdit || m.Video || m.ImageToVideo || m.Upscale) {
				t.Errorf("%s: media endpoint needs an engine and a kind: %+v", e.ID, m)
			}
			if !local[e.Provider] && e.Pricing.PerImage == 0 && e.Pricing.PerSecond == 0 {
				t.Errorf("%s: hosted media endpoint has no per_image or per_second price", e.ID)
			}
			continue
		}
		if e.Capabilities.ContextWindow == 0 {
			t.Errorf("%s: context_window missing", e.ID)
		}
		if !local[e.Provider] && !e.Capabilities.Embeddings && e.Pricing.InputPerM == 0 {
			t.Errorf("%s: hosted endpoint has no pricing", e.ID)
		}
	}
}

// Enabled and Health change while the router, the handlers and json.Marshal
// read endpoints without a lock. Run under -race this failed when they were
// written in place.
func TestRegistryUpdatesDoNotRace(t *testing.T) {
	reg := testRegistry()
	p, _ := ParsePolicy(testPolicy)
	r := NewRouter(reg, []Policy{p})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 300; i++ {
			reg.SetEnabled("local/small", i%2 == 0)
			reg.SetHealth("local/small", "healthy", i, 0, "")
		}
	}()
	for i := 0; i < 300; i++ {
		_, _, _ = r.Route(RouteInput{Selector: "auto", TaskClass: TaskSummarize})
		if _, err := json.Marshal(reg.Endpoints()); err != nil {
			t.Fatal(err)
		}
	}
	<-done
}

func TestSetEnabledIsASnapshotForHolders(t *testing.T) {
	reg := testRegistry()
	before, _ := reg.Endpoint("local/small")
	if !reg.SetEnabled("local/small", false) {
		t.Fatal("endpoint not found")
	}
	after, _ := reg.Endpoint("local/small")
	if !before.Enabled {
		t.Error("a pointer taken earlier changed under its holder")
	}
	if after.Enabled {
		t.Error("a fresh lookup does not see the change")
	}
	if reg.SetEnabled("nope", true) {
		t.Error("SetEnabled invented an endpoint")
	}
	// Health survives a toggle, and a toggle survives a health update.
	reg.SetHealth("local/small", "degraded", 40, 0.1, "slow")
	got, _ := reg.Endpoint("local/small")
	if got.Enabled || got.Health.Status != "degraded" {
		t.Errorf("fields lost: %+v", got)
	}
}
