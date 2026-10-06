package openaicompat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jking323/ws/internal/gateway"
)

// fixture mimics vLLM / llama-server: text, a tool call split over three
// fragments (id+name first, then argument pieces), usage in a trailing
// chunk with empty choices, then [DONE].
const fixture = `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"qwen3-coder","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"qwen3-coder","choices":[{"index":0,"delta":{"content":"Checking"},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"qwen3-coder","choices":[{"index":0,"delta":{"reasoning_content":"user wants weather"},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"qwen3-coder","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"qwen3-coder","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"qwen3-coder","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":" \"Paris\"}"}}]},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"qwen3-coder","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"qwen3-coder","choices":[],"usage":{"prompt_tokens":120,"completion_tokens":18,"total_tokens":138,"prompt_tokens_details":{"cached_tokens":100}}}

data: [DONE]

`

func serve(t *testing.T, body string, status int, capture *[]byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if capture != nil {
			b, _ := io.ReadAll(r.Body)
			*capture = b
		}
		if status != 200 {
			w.WriteHeader(status)
			fmt.Fprint(w, body)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, body)
	}))
}

func TestStreamToolCallReassembly(t *testing.T) {
	var sent []byte
	srv := serve(t, fixture, 200, &sent)
	defer srv.Close()

	a := New()
	p := &gateway.Provider{ID: "local", Kind: gateway.ProviderOpenAICompat, BaseURL: srv.URL + "/v1"}
	ep := &gateway.Endpoint{ID: "local/qwen", ModelName: "qwen3-coder", Capabilities: gateway.Capabilities{Tools: true}}
	req := &gateway.Request{
		System:   "sys",
		Messages: []gateway.Message{{Role: gateway.RoleUser, Parts: []gateway.Part{gateway.TextPart("Weather in Paris?")}}},
		Tools:    []gateway.ToolDef{{Name: "get_weather", InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)}},
	}
	ch, err := a.Stream(context.Background(), p, ep, req)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := gateway.Accumulate(ch)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if resp.Text() != "Checking" {
		t.Errorf("text = %q", resp.Text())
	}
	var reasoning string
	for _, p := range resp.Parts {
		if p.Kind == gateway.PartReasoning {
			reasoning += p.Text
		}
	}
	if reasoning != "user wants weather" {
		t.Errorf("reasoning = %q", reasoning)
	}
	calls := resp.ToolCalls()
	if len(calls) != 1 || calls[0].ToolCallID != "call_abc" || calls[0].ToolName != "get_weather" {
		t.Fatalf("calls = %+v", calls)
	}
	if string(calls[0].Args) != `{"city": "Paris"}` {
		t.Errorf("args = %s", calls[0].Args)
	}
	if resp.FinishReason != gateway.FinishToolCalls {
		t.Errorf("finish = %s", resp.FinishReason)
	}
	want := gateway.Usage{InputTokens: 20, OutputTokens: 18, CacheReadTokens: 100}
	if resp.Usage != want {
		t.Errorf("usage = %+v, want %+v (input must exclude cached)", resp.Usage, want)
	}

	var body map[string]any
	if err := json.Unmarshal(sent, &body); err != nil {
		t.Fatal(err)
	}
	so := body["stream_options"].(map[string]any)
	if so["include_usage"] != true {
		t.Error("include_usage not requested")
	}
	msgs := body["messages"].([]any)
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Errorf("first message should be system, got %v", msgs[0])
	}
}

func TestAssistantToolCallRoundTrip(t *testing.T) {
	out, err := buildMessage(gateway.Message{Role: gateway.RoleAssistant, Parts: []gateway.Part{
		gateway.TextPart("calling"),
		{Kind: gateway.PartToolCall, ToolCallID: "c1", ToolName: "f", Args: json.RawMessage(`{"a":1}`)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out[0])
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if m["role"] != "assistant" || m["content"] != "calling" {
		t.Errorf("assistant = %s", b)
	}
	tcs := m["tool_calls"].([]any)
	fn := tcs[0].(map[string]any)["function"].(map[string]any)
	if fn["arguments"] != `{"a":1}` {
		t.Errorf("arguments = %v", fn["arguments"])
	}

	// tool result becomes a tool-role message
	out, err = buildMessage(gateway.Message{Role: gateway.RoleTool, Parts: []gateway.Part{
		{Kind: gateway.PartToolResult, ToolCallID: "c1", Content: []gateway.Part{gateway.TextPart("42")}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(out[0])
	_ = json.Unmarshal(b, &m)
	if m["role"] != "tool" || m["tool_call_id"] != "c1" || m["content"] != "42" {
		t.Errorf("tool msg = %s", b)
	}
}

func TestNonRetryable400(t *testing.T) {
	srv := serve(t, `{"error":{"message":"bad request","type":"invalid_request_error"}}`, 400, nil)
	defer srv.Close()
	a := New()
	ch, err := a.Stream(context.Background(), &gateway.Provider{BaseURL: srv.URL}, &gateway.Endpoint{ModelName: "m"},
		&gateway.Request{Messages: []gateway.Message{{Role: gateway.RoleUser, Parts: []gateway.Part{gateway.TextPart("x")}}}})
	if err != nil {
		t.Fatal(err)
	}
	for ev := range ch {
		if ev.Type == gateway.EventError && ev.Retryable {
			t.Error("400 must not be retryable")
		}
	}
}

func TestExtraBodyMergedIntoRequest(t *testing.T) {
	var sent []byte
	srv := serve(t, "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", 200, &sent)
	defer srv.Close()
	a := New()
	p := &gateway.Provider{ID: "openrouter", Kind: gateway.ProviderOpenAICompat, BaseURL: srv.URL}
	ep := &gateway.Endpoint{
		ID: "openrouter/openai/gpt-oss-120b@cerebras", ProviderID: "openrouter", ModelName: "openai/gpt-oss-120b",
		Capabilities: gateway.Capabilities{MaxOutput: 1000},
		ExtraBody:    gateway.RouteExtraBody("cerebras"),
	}
	ch, err := a.Stream(context.Background(), p, ep, &gateway.Request{Messages: []gateway.Message{{Role: gateway.RoleUser, Parts: []gateway.Part{gateway.TextPart("hi")}}}})
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	var body map[string]any
	if err := json.Unmarshal(sent, &body); err != nil {
		t.Fatalf("body: %v: %s", err, sent)
	}
	prov, _ := body["provider"].(map[string]any)
	if prov == nil || prov["allow_fallbacks"] != false {
		t.Fatalf("provider block missing or wrong: %s", sent)
	}
	if order, _ := prov["order"].([]any); len(order) != 1 || order[0] != "cerebras" {
		t.Errorf("order = %v", prov["order"])
	}
	if body["model"] != "openai/gpt-oss-120b" || body["max_completion_tokens"] != float64(1000) {
		t.Errorf("params lost: model=%v max=%v", body["model"], body["max_completion_tokens"])
	}
}

// TestResponseFormat: a JSON request becomes response_format on endpoints
// that declare json_mode (json_schema with a schema, json_object without)
// and is left out elsewhere, since unknown fields fail whole requests.
func TestResponseFormat(t *testing.T) {
	stream := "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"{}\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	send := func(t *testing.T, jsonMode bool, f *gateway.JSONFormat) map[string]any {
		t.Helper()
		var sent []byte
		srv := serve(t, stream, 200, &sent)
		defer srv.Close()
		p := &gateway.Provider{ID: "p", Kind: gateway.ProviderOpenAICompat, BaseURL: srv.URL}
		ep := &gateway.Endpoint{ID: "p/m", ProviderID: "p", ModelName: "m", Capabilities: gateway.Capabilities{MaxOutput: 100, JSONMode: jsonMode}}
		req := &gateway.Request{Messages: []gateway.Message{{Role: gateway.RoleUser, Parts: []gateway.Part{gateway.TextPart("hi")}}}, JSON: f}
		ch, err := New().Stream(context.Background(), p, ep, req)
		if err != nil {
			t.Fatal(err)
		}
		for range ch {
		}
		var body map[string]any
		if err := json.Unmarshal(sent, &body); err != nil {
			t.Fatalf("body: %v: %s", err, sent)
		}
		return body
	}
	schema := json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`)
	body := send(t, true, &gateway.JSONFormat{Name: "verdict", Schema: schema, Strict: true})
	rf, _ := body["response_format"].(map[string]any)
	if rf == nil || rf["type"] != "json_schema" {
		t.Fatalf("response_format = %v", body["response_format"])
	}
	js, _ := rf["json_schema"].(map[string]any)
	if js["name"] != "verdict" || js["strict"] != true {
		t.Errorf("json_schema = %v", js)
	}
	if sch, _ := js["schema"].(map[string]any); sch["additionalProperties"] != false {
		t.Errorf("schema not forwarded: %v", js["schema"])
	}
	body = send(t, true, &gateway.JSONFormat{})
	if rf, _ := body["response_format"].(map[string]any); rf == nil || rf["type"] != "json_object" {
		t.Errorf("no schema must be json_object: %v", body["response_format"])
	}
	body = send(t, false, &gateway.JSONFormat{Schema: schema})
	if _, ok := body["response_format"]; ok {
		t.Errorf("endpoint without json_mode must not get response_format: %v", body["response_format"])
	}
	body = send(t, true, nil)
	if _, ok := body["response_format"]; ok {
		t.Errorf("no JSON request must not get response_format: %v", body["response_format"])
	}
}
