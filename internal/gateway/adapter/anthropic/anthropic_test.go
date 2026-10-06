package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jking323/ws/internal/gateway"
)

// fixture is a recorded Anthropic SSE stream: text, then a tool call.
const fixture = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"usage":{"input_tokens":42,"output_tokens":1,"cache_read_input_tokens":10,"cache_creation_input_tokens":5}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Let me check "}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"the weather."}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\": \"Par"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"is\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":27}}

event: message_stop
data: {"type":"message_stop"}

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

func TestStreamTextAndToolCall(t *testing.T) {
	var sent []byte
	srv := serve(t, fixture, 200, &sent)
	defer srv.Close()

	a := New()
	p := &gateway.Provider{ID: "anthropic", Kind: gateway.ProviderAnthropic, BaseURL: srv.URL, APIKey: "test"}
	ep := &gateway.Endpoint{ID: "anthropic/opus", ModelName: "claude-opus-5-5", Capabilities: gateway.Capabilities{Tools: true, MaxOutput: 1024}}
	req := &gateway.Request{
		System:      "You are terse.",
		SystemCache: true,
		Messages: []gateway.Message{
			{Role: gateway.RoleUser, Parts: []gateway.Part{gateway.TextPart("Weather in Paris?")}},
		},
		Tools: []gateway.ToolDef{{Name: "get_weather", Description: "Get weather", InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`)}},
	}
	ch, err := a.Stream(context.Background(), p, ep, req)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := gateway.Accumulate(ch)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if got := resp.Text(); got != "Let me check the weather." {
		t.Errorf("text = %q", got)
	}
	calls := resp.ToolCalls()
	if len(calls) != 1 || calls[0].ToolName != "get_weather" || calls[0].ToolCallID != "toolu_1" {
		t.Fatalf("tool calls = %+v", calls)
	}
	if string(calls[0].Args) != `{"city": "Paris"}` {
		t.Errorf("args = %s", calls[0].Args)
	}
	if resp.FinishReason != gateway.FinishToolCalls {
		t.Errorf("finish = %s", resp.FinishReason)
	}
	want := gateway.Usage{InputTokens: 42, OutputTokens: 27, CacheReadTokens: 10, CacheWriteTokens: 5}
	if resp.Usage != want {
		t.Errorf("usage = %+v, want %+v", resp.Usage, want)
	}

	// Request shape: system cache_control, tool schema, max_tokens from endpoint.
	var body map[string]any
	if err := json.Unmarshal(sent, &body); err != nil {
		t.Fatal(err)
	}
	if body["stream"] != true {
		t.Error("stream not set")
	}
	if body["max_tokens"].(float64) != 1024 {
		t.Errorf("max_tokens = %v", body["max_tokens"])
	}
	sys := body["system"].([]any)[0].(map[string]any)
	if sys["cache_control"] == nil {
		t.Error("system cache_control missing")
	}
	tools := body["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "get_weather" {
		t.Errorf("tools = %v", tools)
	}
}

func TestRetryableErrorBeforeTokens(t *testing.T) {
	srv := serve(t, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, 529, nil)
	defer srv.Close()
	a := New()
	p := &gateway.Provider{BaseURL: srv.URL, APIKey: "test"}
	ep := &gateway.Endpoint{ID: "x", ModelName: "m"}
	ch, err := a.Stream(context.Background(), p, ep, &gateway.Request{Messages: []gateway.Message{{Role: gateway.RoleUser, Parts: []gateway.Part{gateway.TextPart("hi")}}}})
	if err != nil {
		t.Fatal(err)
	}
	var sawRetryable bool
	for ev := range ch {
		if ev.Type == gateway.EventError {
			sawRetryable = ev.Retryable
			if !strings.Contains(ev.ErrText, "529") {
				t.Errorf("err = %s", ev.ErrText)
			}
		}
	}
	if !sawRetryable {
		t.Error("expected retryable error")
	}
}

func TestBuildMessagesMergesAndMapsToolRole(t *testing.T) {
	msgs, err := buildMessages([]gateway.Message{
		{Role: gateway.RoleUser, Parts: []gateway.Part{gateway.TextPart("a")}},
		{Role: gateway.RoleAssistant, Parts: []gateway.Part{{Kind: gateway.PartToolCall, ToolCallID: "t1", ToolName: "f", Args: json.RawMessage(`{"x":1}`)}}},
		{Role: gateway.RoleTool, Parts: []gateway.Part{{Kind: gateway.PartToolResult, ToolCallID: "t1", Content: []gateway.Part{gateway.TextPart("ok")}}}},
		{Role: gateway.RoleUser, Parts: []gateway.Part{gateway.TextPart("b")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// user, assistant, user(tool_result + "b" merged)
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3", len(msgs))
	}
	if len(msgs[2].Content) != 2 {
		t.Errorf("tool result and following user text should merge; got %d blocks", len(msgs[2].Content))
	}
}
