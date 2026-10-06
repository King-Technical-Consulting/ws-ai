package gateway

import (
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

	// Providers without a key are dropped by Resolve; with a key they stay.
	provs, eps := sf.Resolve(func(string) string { return "" })
	if len(provs) != 0 || len(eps) != 0 {
		t.Errorf("keyless hosted provider should be skipped, got %d/%d", len(provs), len(eps))
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
			if m.Engine == "" || !(m.Image || m.ImageEdit || m.Video || m.ImageToVideo) {
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
