package externalapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jking323/ws/internal/gateway"
)

// fakeAdapter echoes a scripted reply: text, then a tool call.
type fakeAdapter struct{ fail bool }

func (f *fakeAdapter) Stream(ctx context.Context, p *gateway.Provider, ep *gateway.Endpoint, req *gateway.Request) (<-chan gateway.StreamEvent, error) {
	ch := make(chan gateway.StreamEvent, 16)
	go func() {
		defer close(ch)
		if f.fail {
			ch <- gateway.ErrorEvent(errors.New("down"), true)
			return
		}
		ch <- gateway.StreamEvent{Type: gateway.EventStart, EndpointID: ep.ID, Model: ep.ModelName}
		ch <- gateway.StreamEvent{Type: gateway.EventTextDelta, Text: "Hello "}
		ch <- gateway.StreamEvent{Type: gateway.EventTextDelta, Text: "there"}
		if len(req.Tools) > 0 {
			ch <- gateway.StreamEvent{Type: gateway.EventToolCallStart, ToolCallID: "call_1", ToolName: req.Tools[0].Name}
			ch <- gateway.StreamEvent{Type: gateway.EventToolCallDelta, ToolCallID: "call_1", ArgsDelta: `{"q":`}
			ch <- gateway.StreamEvent{Type: gateway.EventToolCallDelta, ToolCallID: "call_1", ArgsDelta: `"x"}`}
			ch <- gateway.StreamEvent{Type: gateway.EventToolCallEnd, ToolCallID: "call_1"}
			ch <- gateway.StreamEvent{Type: gateway.EventUsage, Usage: &gateway.Usage{InputTokens: 10, OutputTokens: 5}}
			ch <- gateway.StreamEvent{Type: gateway.EventFinish, FinishReason: gateway.FinishToolCalls}
			return
		}
		ch <- gateway.StreamEvent{Type: gateway.EventUsage, Usage: &gateway.Usage{InputTokens: 10, OutputTokens: 2, CacheReadTokens: 3}}
		ch <- gateway.StreamEvent{Type: gateway.EventFinish, FinishReason: gateway.FinishStop}
	}()
	return ch, nil
}

func (f *fakeAdapter) CountTokens(context.Context, *gateway.Provider, *gateway.Endpoint, *gateway.Request) (int, bool, error) {
	return 0, false, nil
}

func newTestServer(t *testing.T) http.Handler {
	t.Helper()
	reg := gateway.NewRegistry()
	reg.Replace(
		[]*gateway.Provider{{ID: "local", Kind: gateway.ProviderOpenAICompat}},
		[]*gateway.Endpoint{{ID: "local/m", ProviderID: "local", ModelName: "m", Enabled: true, Local: true, Capabilities: gateway.Capabilities{ContextWindow: 32000, Tools: true}}},
	)
	pol, _ := gateway.ParsePolicy("name: t\nrules:\n  - match: {selector: [fast]}\n    prefer: [local/m]\n  - match: {}\n    prefer: [local/m]\naliases:\n  cheap: [local/m]\n")
	gw := gateway.New(reg, gateway.NewRouter(reg, []gateway.Policy{pol}), nil, nil)
	gw.RegisterAdapter(gateway.ProviderOpenAICompat, &fakeAdapter{})
	s := &Server{GW: gw, Principal: func(context.Context) *Principal {
		return &Principal{UserID: uuid.New(), DefaultSelector: "auto"}
	}}
	r := chi.NewRouter()
	s.Routes(r)
	return r
}

func TestOpenAIToCanonical(t *testing.T) {
	var in oaRequest
	err := json.Unmarshal([]byte(`{
	  "model":"cheap",
	  "messages":[
	    {"role":"system","content":"be terse"},
	    {"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGk="}}]},
	    {"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]},
	    {"role":"tool","tool_call_id":"c1","content":"42"}
	  ],
	  "tools":[{"type":"function","function":{"name":"f","description":"d","parameters":{"type":"object"}}}],
	  "tool_choice":"auto","max_completion_tokens":100,"stop":["END"]
	}`), &in)
	if err != nil {
		t.Fatal(err)
	}
	req, err := in.ToCanonical()
	if err != nil {
		t.Fatal(err)
	}
	if req.System != "be terse" || !req.SystemCache || req.MaxTokens != 100 || len(req.Stop) != 1 {
		t.Errorf("header fields wrong: %+v", req)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %d", len(req.Messages))
	}
	if req.Messages[0].Parts[1].Kind != gateway.PartImage || string(req.Messages[0].Parts[1].Data) != "hi" {
		t.Errorf("image part wrong: %+v", req.Messages[0].Parts[1])
	}
	if req.Messages[1].Parts[0].Kind != gateway.PartToolCall || req.Messages[2].Role != gateway.RoleTool {
		t.Errorf("tool round trip wrong")
	}
	if req.ToolChoice == nil || req.ToolChoice.Mode != gateway.ToolChoiceAuto {
		t.Errorf("tool choice = %+v", req.ToolChoice)
	}
}

func TestAnthropicToCanonical(t *testing.T) {
	var in anRequest
	err := json.Unmarshal([]byte(`{
	  "model":"claude-sonnet-5-5","max_tokens":512,
	  "system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],
	  "messages":[
	    {"role":"user","content":"hi"},
	    {"role":"assistant","content":[{"type":"text","text":"calling"},{"type":"tool_use","id":"t1","name":"f","input":{"a":1}}]},
	    {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"42"},{"type":"text","text":"and?"}]}
	  ],
	  "tools":[{"name":"f","description":"d","input_schema":{"type":"object"}},{"type":"web_search_20260209","name":"web_search"}],
	  "tool_choice":{"type":"any"},
	  "thinking":{"type":"adaptive"}
	}`), &in)
	if err != nil {
		t.Fatal(err)
	}
	req, err := in.ToCanonical()
	if err != nil {
		t.Fatal(err)
	}
	if req.System != "sys" || !req.SystemCache || req.MaxTokens != 512 {
		t.Errorf("header wrong: %+v", req)
	}
	// user, assistant, tool(result), user(and?)
	if len(req.Messages) != 4 || req.Messages[2].Role != gateway.RoleTool || req.Messages[3].Parts[0].Text != "and?" {
		t.Errorf("messages wrong: %+v", req.Messages)
	}
	if len(req.Tools) != 1 {
		t.Errorf("server tools must be dropped; got %d", len(req.Tools))
	}
	if req.ToolChoice.Mode != gateway.ToolChoiceRequired || req.Reasoning == nil || !req.Reasoning.Enabled {
		t.Errorf("choice/thinking wrong")
	}
}

func TestOpenAIStreaming(t *testing.T) {
	h := newTestServer(t)
	body := `{"model":"cheap","stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d ct %s: %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.HasSuffix(strings.TrimSpace(out), "data: [DONE]") {
		t.Error("stream must end with [DONE]")
	}
	var sawName, sawArgs, sawFinish, sawUsage bool
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var c struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage map[string]any `json:"usage"`
		}
		if err := json.Unmarshal([]byte(line[6:]), &c); err != nil {
			t.Fatalf("bad chunk %s: %v", line, err)
		}
		if c.Usage != nil {
			sawUsage = true
		}
		for _, ch := range c.Choices {
			for _, tc := range ch.Delta.ToolCalls {
				if tc.Function.Name == "f" && tc.ID == "call_1" {
					sawName = true
				}
				if tc.Function.Arguments != "" {
					sawArgs = true
				}
			}
			if ch.FinishReason != nil && *ch.FinishReason == "tool_calls" {
				sawFinish = true
			}
		}
	}
	if !sawName || !sawArgs || !sawFinish || !sawUsage {
		t.Errorf("stream incomplete: name=%v args=%v finish=%v usage=%v\n%s", sawName, sawArgs, sawFinish, sawUsage, out)
	}
}

func TestOpenAINonStreaming(t *testing.T) {
	h := newTestServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Choices []struct {
			Message      struct{ Content string } `json:"message"`
			FinishReason string                   `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Choices[0].Message.Content != "Hello there" || resp.Choices[0].FinishReason != "stop" || resp.Usage.PromptTokens != 13 {
		t.Errorf("resp = %s", rec.Body.String())
	}
}

func TestAnthropicStreaming(t *testing.T) {
	h := newTestServer(t)
	body := `{"model":"auto","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"f","input_schema":{"type":"object"}}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	for _, want := range []string{
		"event: message_start", `"delta":{"text":"Hello ","type":"text_delta"}`,
		`"content_block":{"id":"call_1","input":{},"name":"f","type":"tool_use"}`,
		`"delta":{"partial_json":"{\"q\":","type":"input_json_delta"}`,
		`"stop_reason":"tool_use"`, "event: message_stop",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// blocks must be closed in order: text stop before tool start
	ts := strings.Index(out, `{"index":0,"type":"content_block_stop"}`)
	tu := strings.Index(out, `"type":"tool_use"`)
	if ts < 0 || tu < 0 || ts > tu {
		t.Error("text block not closed before tool block opened")
	}
}

func TestAnthropicNonStreaming(t *testing.T) {
	h := newTestServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"auto","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)))
	var resp struct {
		Content    []struct{ Type, Text string } `json:"content"`
		StopReason string                        `json:"stop_reason"`
		Usage      struct {
			CacheRead int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Content) != 1 || resp.Content[0].Text != "Hello there" || resp.StopReason != "end_turn" || resp.Usage.CacheRead != 3 {
		t.Errorf("resp = %s", rec.Body.String())
	}
}

func TestModelsAndCountTokens(t *testing.T) {
	h := newTestServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	// The aliases, the selectors rules match on ("fast"), then the endpoints.
	if b := rec.Body.String(); !strings.Contains(b, `"id":"cheap"`) || !strings.Contains(b, `"id":"fast"`) || !strings.Contains(b, `"id":"local/m"`) || strings.Index(b, `"id":"fast"`) > strings.Index(b, `"id":"local/m"`) {
		t.Errorf("models = %s", b)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"hello world, twelve chars"}]}`)))
	if !strings.Contains(rec.Body.String(), `"input_tokens":`) {
		t.Errorf("count = %s", rec.Body.String())
	}
}
