package gateway

import (
	"encoding/json"
	"testing"
)

const routesFixture = `{"data":{"id":"openai/gpt-oss-120b","endpoints":[
 {"provider_name":"Groq","tag":"groq","context_length":131072,"max_completion_tokens":65536,"quantization":"unknown","status":0,"supports_tool_choice":true,"supported_parameters":["reasoning","tools","tool_choice","max_tokens"],"uptime_last_30m":99.1,"pricing":{"prompt":"0.00000015","completion":"0.0000006","input_cache_read":"","input_cache_write":""}},
 {"provider_name":"Cerebras","tag":"cerebras/fp16","context_length":131072,"max_completion_tokens":40960,"quantization":"fp16","status":0,"supports_tool_choice":true,"supported_parameters":["reasoning","include_reasoning","max_tokens"],"uptime_last_30m":99.99,"pricing":{"prompt":"0.00000035","completion":"0.00000075","input_cache_read":"","input_cache_write":""}},
 {"provider_name":"BaseTen","tag":"baseten/fp4","context_length":128072,"max_completion_tokens":115264,"quantization":"fp4","supports_tool_choice":false,"supported_parameters":["max_tokens"],"pricing":{"prompt":"0.0000001","completion":"0.0000005"}},
 {"provider_name":"BaseTen","tag":"baseten/fp4","context_length":128072,"max_completion_tokens":115264,"quantization":"fp4","supports_tool_choice":false,"supported_parameters":["max_tokens"],"pricing":{"prompt":"0.0000001","completion":"0.0000005"}}
]}}`

func TestMapOpenRouterRoutes(t *testing.T) {
	var body struct {
		Data struct {
			Endpoints []openRouterRouteJSON `json:"endpoints"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(routesFixture), &body); err != nil {
		t.Fatal(err)
	}
	routes := mapOpenRouterRoutes("openrouter", "openai/gpt-oss-120b", body.Data.Endpoints)
	if len(routes) != 3 {
		t.Fatalf("want 3 routes (duplicate tag collapsed), got %d", len(routes))
	}
	// Sorted by output price: baseten 0.50, groq 0.60, cerebras 0.75.
	if routes[0].Tag != "baseten/fp4" || routes[1].Tag != "groq" || routes[2].Tag != "cerebras/fp16" {
		t.Errorf("order = %s, %s, %s", routes[0].Tag, routes[1].Tag, routes[2].Tag)
	}
	c := routes[2]
	if c.Provider != "Cerebras" || c.EndpointID != "openrouter/openai/gpt-oss-120b@cerebras-fp16" {
		t.Errorf("cerebras route = %+v", c)
	}
	if c.Pricing.InputPerM != 0.35 || c.Pricing.OutputPerM != 0.75 {
		t.Errorf("pricing per M = %+v", c.Pricing)
	}
	if !c.Capabilities.Tools || !c.Capabilities.Reasoning || c.Capabilities.MaxOutput != 40960 || c.Capabilities.ContextWindow != 131072 {
		t.Errorf("caps = %+v", c.Capabilities)
	}
	if routes[0].Capabilities.Tools {
		t.Errorf("baseten should not report tools")
	}
}

func TestRouteExtraBodyAndLabel(t *testing.T) {
	ep := &Endpoint{ID: "x", ExtraBody: RouteExtraBody("cerebras/fp16")}
	if ep.Route() != "cerebras/fp16" {
		t.Errorf("Route() = %q", ep.Route())
	}
	b, _ := json.Marshal(ep.ExtraBody)
	if string(b) != `{"provider":{"allow_fallbacks":false,"order":["cerebras/fp16"]}}` {
		t.Errorf("extra_body = %s", b)
	}
	// Round trip through JSON (as stored in Postgres) keeps the label.
	var back map[string]any
	_ = json.Unmarshal(b, &back)
	if (&Endpoint{ExtraBody: back}).Route() != "cerebras/fp16" {
		t.Errorf("Route() after round trip = %q", (&Endpoint{ExtraBody: back}).Route())
	}
	if (&Endpoint{}).Route() != "" {
		t.Errorf("empty endpoint should have no route")
	}
}
