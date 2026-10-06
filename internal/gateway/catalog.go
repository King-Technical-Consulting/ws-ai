package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CatalogModel is a model a provider offers, normalized so the admin UI can
// turn it into an Endpoint with one click.
type CatalogModel struct {
	ID           string       `json:"id"`   // provider's model name, e.g. "anthropic/claude-sonnet-4.5"
	Name         string       `json:"name"` // display name
	Description  string       `json:"description,omitempty"`
	Capabilities Capabilities `json:"capabilities"`
	Pricing      Pricing      `json:"pricing"`
	// Added reports whether an endpoint for this model already exists.
	Added bool `json:"added"`
	// EndpointID is the id the endpoint has or would get.
	EndpointID string `json:"endpoint_id"`
}

// OpenRouterCatalog fetches https://openrouter.ai/api/v1/models and maps it
// to CatalogModels. Prices there are USD per token as strings.
func OpenRouterCatalog(ctx context.Context, p *Provider, client *http.Client) ([]CatalogModel, error) {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	base := strings.TrimRight(p.BaseURL, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models", nil)
	if err != nil {
		return nil, err
	}
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openrouter catalog: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("openrouter catalog: HTTP %d", resp.StatusCode)
	}
	var body struct {
		Data []struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			Description   string `json:"description"`
			ContextLength int    `json:"context_length"`
			Architecture  struct {
				InputModalities []string `json:"input_modalities"`
				Modality        string   `json:"modality"`
			} `json:"architecture"`
			Pricing struct {
				Prompt          string `json:"prompt"`
				Completion      string `json:"completion"`
				InputCacheRead  string `json:"input_cache_read"`
				InputCacheWrite string `json:"input_cache_write"`
			} `json:"pricing"`
			TopProvider struct {
				MaxCompletionTokens *int `json:"max_completion_tokens"`
				ContextLength       *int `json:"context_length"`
			} `json:"top_provider"`
			SupportedParameters []string `json:"supported_parameters"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("openrouter catalog: decode: %w", err)
	}
	perM := func(s string) float64 {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || f < 0 {
			return 0
		}
		return f * 1_000_000
	}
	out := make([]CatalogModel, 0, len(body.Data))
	for _, m := range body.Data {
		if m.ID == "" {
			continue
		}
		caps := Capabilities{ContextWindow: m.ContextLength}
		if m.TopProvider.ContextLength != nil && *m.TopProvider.ContextLength > 0 {
			caps.ContextWindow = *m.TopProvider.ContextLength
		}
		if m.TopProvider.MaxCompletionTokens != nil {
			caps.MaxOutput = *m.TopProvider.MaxCompletionTokens
		}
		for _, mod := range m.Architecture.InputModalities {
			if mod == "image" {
				caps.Vision = true
			}
		}
		if caps.ContextWindow == 0 && strings.Contains(m.Architecture.Modality, "image") {
			caps.Vision = true
		}
		for _, sp := range m.SupportedParameters {
			switch sp {
			case "tools", "tool_choice":
				caps.Tools = true
			case "response_format", "structured_outputs":
				caps.JSONMode = true
			case "reasoning", "include_reasoning":
				caps.Reasoning = true
			}
		}
		pr := Pricing{InputPerM: perM(m.Pricing.Prompt), OutputPerM: perM(m.Pricing.Completion)}
		if m.Pricing.InputCacheRead != "" {
			pr.CacheReadPerM = perM(m.Pricing.InputCacheRead)
			pr.CacheWritePerM = perM(m.Pricing.InputCacheWrite)
			if pr.CacheReadPerM > 0 {
				caps.PromptCache = true
			}
		}
		if strings.Contains(m.ID, "embed") {
			caps.Embeddings = true
		}
		name := m.Name
		if name == "" {
			name = m.ID
		}
		out = append(out, CatalogModel{
			ID: m.ID, Name: name, Description: trimDesc(m.Description), Capabilities: caps, Pricing: pr,
			EndpointID: p.ID + "/" + m.ID,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func trimDesc(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, ".\n"); i > 40 {
		s = s[:i+1]
	}
	if len(s) > 220 {
		s = s[:217] + "..."
	}
	return s
}

// CatalogRoute is one upstream serving a model behind an aggregator
// (OpenRouter calls these "endpoints"). Adding a route creates a separate
// ws endpoint pinned to that upstream, so one model can exist as several
// routes with their own prices and health.
type CatalogRoute struct {
	Provider     string       `json:"provider"` // display name, e.g. "Cerebras"
	Tag          string       `json:"tag"`      // routing slug accepted in provider.order, e.g. "cerebras/fp16"
	Capabilities Capabilities `json:"capabilities"`
	Pricing      Pricing      `json:"pricing"`
	Quantization string       `json:"quantization,omitempty"`
	Uptime30m    float64      `json:"uptime_30m,omitempty"`
	Status       string       `json:"status,omitempty"`
	Added        bool         `json:"added"`
	EndpointID   string       `json:"endpoint_id"`
}

// RouteEndpointID is the ws endpoint id for a model pinned to one route:
// "<provider>/<model>@<tag>" with the tag's slashes flattened.
func RouteEndpointID(providerID, model, tag string) string {
	return providerID + "/" + model + "@" + strings.ReplaceAll(tag, "/", "-")
}

// RouteExtraBody is the request-body override that pins an OpenRouter
// request to one upstream, with no fallback to others.
func RouteExtraBody(tag string) map[string]any {
	return map[string]any{"provider": map[string]any{"order": []any{tag}, "allow_fallbacks": false}}
}

// openRouterRouteJSON is one entry of /models/{author}/{slug}/endpoints.
type openRouterRouteJSON struct {
	ProviderName        string   `json:"provider_name"`
	Tag                 string   `json:"tag"`
	ContextLength       int      `json:"context_length"`
	MaxCompletionTokens *int     `json:"max_completion_tokens"`
	Quantization        string   `json:"quantization"`
	Status              any      `json:"status"`
	SupportsToolChoice  bool     `json:"supports_tool_choice"`
	SupportedParameters []string `json:"supported_parameters"`
	Uptime30m           *float64 `json:"uptime_last_30m"`
	Pricing             struct {
		Prompt          string `json:"prompt"`
		Completion      string `json:"completion"`
		InputCacheRead  string `json:"input_cache_read"`
		InputCacheWrite string `json:"input_cache_write"`
	} `json:"pricing"`
}

// OpenRouterRoutes fetches /models/{author}/{slug}/endpoints for one model.
func OpenRouterRoutes(ctx context.Context, p *Provider, client *http.Client, model string) ([]CatalogRoute, error) {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	if strings.Count(model, "/") != 1 {
		return nil, fmt.Errorf("openrouter routes: model must be author/slug, got %q", model)
	}
	base := strings.TrimRight(p.BaseURL, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models/"+model+"/endpoints", nil)
	if err != nil {
		return nil, err
	}
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openrouter routes: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return nil, fmt.Errorf("openrouter routes: model %q not found", model)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("openrouter routes: HTTP %d", resp.StatusCode)
	}
	var body struct {
		Data struct {
			Endpoints []openRouterRouteJSON `json:"endpoints"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("openrouter routes: decode: %w", err)
	}
	return mapOpenRouterRoutes(p.ID, model, body.Data.Endpoints), nil
}

func mapOpenRouterRoutes(providerID, model string, in []openRouterRouteJSON) []CatalogRoute {
	perM := func(s string) float64 {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || f < 0 {
			return 0
		}
		return f * 1_000_000
	}
	seen := map[string]bool{}
	out := make([]CatalogRoute, 0, len(in))
	for _, e := range in {
		if e.Tag == "" || seen[e.Tag] {
			continue
		}
		seen[e.Tag] = true
		caps := Capabilities{ContextWindow: e.ContextLength, Tools: e.SupportsToolChoice}
		if e.MaxCompletionTokens != nil {
			caps.MaxOutput = *e.MaxCompletionTokens
		}
		for _, sp := range e.SupportedParameters {
			switch sp {
			case "tools", "tool_choice":
				caps.Tools = true
			case "response_format", "structured_outputs":
				caps.JSONMode = true
			case "reasoning", "include_reasoning":
				caps.Reasoning = true
			}
		}
		pr := Pricing{InputPerM: perM(e.Pricing.Prompt), OutputPerM: perM(e.Pricing.Completion)}
		if e.Pricing.InputCacheRead != "" {
			pr.CacheReadPerM = perM(e.Pricing.InputCacheRead)
			pr.CacheWritePerM = perM(e.Pricing.InputCacheWrite)
			caps.PromptCache = pr.CacheReadPerM > 0
		}
		r := CatalogRoute{
			Provider: e.ProviderName, Tag: e.Tag, Capabilities: caps, Pricing: pr, Quantization: e.Quantization,
			EndpointID: RouteEndpointID(providerID, model, e.Tag),
		}
		if e.Uptime30m != nil {
			r.Uptime30m = *e.Uptime30m
		}
		if s, ok := e.Status.(string); ok {
			r.Status = s
		}
		out = append(out, r)
	}
	// Cheapest output price first; ties by input price.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Pricing.OutputPerM != out[j].Pricing.OutputPerM {
			return out[i].Pricing.OutputPerM < out[j].Pricing.OutputPerM
		}
		return out[i].Pricing.InputPerM < out[j].Pricing.InputPerM
	})
	return out
}
