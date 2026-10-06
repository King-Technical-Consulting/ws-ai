package externalapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jking323/ws/internal/gateway"
)

// upstreamSSE is a recorded Anthropic stream with a ping, usage on
// message_start and message_delta, and a tool_use stop.
const upstreamSSE = "event: message_start\n" +
	"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_up\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-b\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":42,\"output_tokens\":1,\"cache_read_input_tokens\":10,\"cache_creation_input_tokens\":5}}}\n\n" +
	"event: ping\n" +
	"data: {\"type\": \"ping\"}\n\n" +
	"event: content_block_start\n" +
	"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
	"event: content_block_delta\n" +
	"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi \\u00e9\"}}\n\n" +
	"event: content_block_stop\n" +
	"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
	"event: message_delta\n" +
	"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":17}}\n\n" +
	"event: message_stop\n" +
	"data: {\"type\":\"message_stop\"}\n\n"

// upstream is a fake Anthropic API: it records each request and answers
// by the model it was asked for.
type upstream struct {
	mu   sync.Mutex
	reqs []recordedReq
}

type recordedReq struct {
	path, query string
	header      http.Header
	body        map[string]any
}

func (u *upstream) handler(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	u.mu.Lock()
	u.reqs = append(u.reqs, recordedReq{path: r.URL.Path, query: r.URL.RawQuery, header: r.Header.Clone(), body: body})
	u.mu.Unlock()
	w.Header().Set("request-id", "req_up")
	model, _ := body["model"].(string)
	switch {
	case r.URL.Path == "/v1/messages/count_tokens":
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"input_tokens":123}`)
	case model == "claude-a": // the first candidate is always overloaded
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("retry-after", "7")
		w.Header().Set("x-should-retry", "true")
		w.WriteHeader(529)
		fmt.Fprint(w, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	case model == "claude-bad":
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-should-retry", "false")
		w.WriteHeader(400)
		fmt.Fprint(w, `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: required"}}`)
	case body["stream"] == true:
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, upstreamSSE)
	default:
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"msg_up","type":"message","role":"assistant","model":"claude-b","content":[{"type":"text","text":"Hello"}],"stop_reason":"end_turn","usage":{"input_tokens":8,"output_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`)
	}
}

func (u *upstream) last() recordedReq {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.reqs[len(u.reqs)-1]
}

type memRecorder struct{ recs chan gateway.UsageRecord }

func (m *memRecorder) Record(_ context.Context, rec gateway.UsageRecord) { m.recs <- rec }

func (m *memRecorder) next(t *testing.T) gateway.UsageRecord {
	t.Helper()
	select {
	case r := <-m.recs:
		return r
	case <-time.After(3 * time.Second):
		t.Fatal("no ledger record")
		return gateway.UsageRecord{}
	}
}

var testUser = uuid.New()

// newPassthroughServer: two Anthropic endpoints (a always 529s, b works)
// behind one fake upstream, plus a local openai_compat endpoint for the
// translate path. Aliases: "claude" prefers a then b; "cheap" is local;
// "bad" names the endpoint whose upstream answers 400.
func newPassthroughServer(t *testing.T) (http.Handler, *upstream, *memRecorder) {
	t.Helper()
	up := &upstream{}
	srv := httptest.NewServer(http.HandlerFunc(up.handler))
	t.Cleanup(srv.Close)
	reg := gateway.NewRegistry()
	reg.Replace(
		[]*gateway.Provider{
			{ID: "anthropic", Kind: gateway.ProviderAnthropic, BaseURL: srv.URL, APIKey: "sk-provider", Headers: map[string]string{"x-provider-extra": "1"}},
			{ID: "local", Kind: gateway.ProviderOpenAICompat},
		},
		[]*gateway.Endpoint{
			{ID: "anthropic/a", ProviderID: "anthropic", ModelName: "claude-a", Enabled: true, Capabilities: gateway.Capabilities{ContextWindow: 200000, Tools: true, Vision: true}, Pricing: gateway.Pricing{InputPerM: 3, OutputPerM: 15, CacheReadPerM: 0.3, CacheWritePerM: 3.75}},
			{ID: "anthropic/b", ProviderID: "anthropic", ModelName: "claude-b", DisplayName: "Claude B", Enabled: true, Capabilities: gateway.Capabilities{ContextWindow: 200000, Tools: true, Vision: true}, Pricing: gateway.Pricing{InputPerM: 3, OutputPerM: 15, CacheReadPerM: 0.3, CacheWritePerM: 3.75}},
			{ID: "anthropic/bad", ProviderID: "anthropic", ModelName: "claude-bad", Enabled: true, Capabilities: gateway.Capabilities{ContextWindow: 200000, Tools: true}},
			{ID: "local/m", ProviderID: "local", ModelName: "m", Enabled: true, Local: true, Capabilities: gateway.Capabilities{ContextWindow: 32000, Tools: true}},
		},
	)
	pol, err := gateway.ParsePolicy("name: t\nrules:\n  - match: {}\n    prefer: [anthropic/a, anthropic/b]\naliases:\n  claude: [anthropic/a, anthropic/b]\n  only-a: [anthropic/a]\n  bad: [anthropic/bad]\n  cheap: [local/m]\n")
	if err != nil {
		t.Fatal(err)
	}
	rec := &memRecorder{recs: make(chan gateway.UsageRecord, 8)}
	gw := gateway.New(reg, gateway.NewRouter(reg, []gateway.Policy{pol}), rec, nil)
	gw.RegisterAdapter(gateway.ProviderOpenAICompat, &fakeAdapter{})
	s := &Server{GW: gw, Principal: func(context.Context) *Principal {
		return &Principal{UserID: testUser, APIKeyID: uuid.NullUUID{UUID: uuid.New(), Valid: true}, DefaultSelector: "auto"}
	}}
	r := chi.NewRouter()
	s.Routes(r)
	return r, up, rec
}

func post(h http.Handler, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer ws_secret")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestPassthroughStreaming(t *testing.T) {
	h, up, rec := newPassthroughServer(t)
	body := `{"model":"claude","max_tokens":64,"stream":true,"system":[{"type":"text","text":"be terse","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":"u"},"unknown_future_field":{"keep":"me"}}`
	w := post(h, "/v1/messages?beta=true", body, map[string]string{
		"anthropic-version": "2023-06-01", "anthropic-beta": "interleaved-thinking-2025-05-14,context-1m-2025-08-07", SessionHeader: "sess-123",
	})
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d headers %v body %s", w.Code, w.Header(), w.Body.String())
	}
	// Byte-for-byte: the upstream stream, pings included, nothing re-synthesised.
	if w.Body.String() != upstreamSSE {
		t.Errorf("stream altered:\n%s", w.Body.String())
	}
	if w.Header().Get("request-id") != "req_up" {
		t.Errorf("upstream headers not relayed: %v", w.Header())
	}
	// The first candidate (a) was overloaded and failed over to b; both saw the request.
	if len(up.reqs) != 2 || up.reqs[0].body["model"] != "claude-a" || up.last().body["model"] != "claude-b" {
		t.Fatalf("upstream requests = %+v", up.reqs)
	}
	last := up.last()
	if last.path != "/v1/messages" || last.query != "beta=true" {
		t.Errorf("upstream path = %s?%s", last.path, last.query)
	}
	if last.header.Get("x-api-key") != "sk-provider" || last.header.Get("Authorization") != "" {
		t.Errorf("credential swap wrong: %v", last.header)
	}
	if last.header.Get("anthropic-beta") != "interleaved-thinking-2025-05-14,context-1m-2025-08-07" || last.header.Get("anthropic-version") != "2023-06-01" || last.header.Get("x-provider-extra") != "1" {
		t.Errorf("request headers not forwarded: %v", last.header)
	}
	if last.header.Get(SessionHeader) != "" {
		t.Errorf("session id must not leak upstream")
	}
	// Body untouched except model: system array with cache_control, metadata and an unknown field survive.
	sys, _ := json.Marshal(last.body["system"])
	if !strings.Contains(string(sys), `"cache_control":{"type":"ephemeral"}`) || last.body["unknown_future_field"] == nil || last.body["max_tokens"] != float64(64) {
		t.Errorf("body not verbatim: %v", last.body)
	}
	// Ledger: usage from message_start and message_delta, cost from pricing,
	// session id and tool_use stop.
	r := rec.next(t)
	if r.EndpointID != "anthropic/b" || r.Usage != (gateway.Usage{InputTokens: 42, OutputTokens: 17, CacheReadTokens: 10, CacheWriteTokens: 5}) || r.FinishReason != gateway.FinishToolCalls {
		t.Errorf("record = %+v", r)
	}
	if r.Metadata.SessionID != "sess-123" || r.Metadata.UserID != testUser.String() || !r.Metadata.External {
		t.Errorf("metadata = %+v", r.Metadata)
	}
	if r.CostUSD <= 0 || r.TTFT <= 0 || len(r.Decision.Tried) != 2 || r.Decision.Chosen != "anthropic/b" {
		t.Errorf("record accounting = %+v", r)
	}
}

func TestPassthroughNonStreaming(t *testing.T) {
	h, up, rec := newPassthroughServer(t)
	w := post(h, "/v1/messages", `{"model":"anthropic/b","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"msg_up"`) {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	if len(up.reqs) != 1 || up.last().header.Get("anthropic-version") != AnthropicVersionDefault {
		t.Errorf("default anthropic-version missing: %+v", up.reqs)
	}
	r := rec.next(t)
	if r.Usage.InputTokens != 8 || r.Usage.OutputTokens != 2 || r.FinishReason != gateway.FinishStop || r.Metadata.SessionID != "" {
		t.Errorf("record = %+v", r)
	}
}

func TestPassthroughRelaysUpstreamErrors(t *testing.T) {
	h, _, rec := newPassthroughServer(t)
	// A 400 from Anthropic comes back as Anthropic wrote it, with its headers.
	w := post(h, "/v1/messages", `{"model":"bad","messages":[]}`, nil)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "max_tokens: required") || w.Header().Get("x-should-retry") != "false" {
		t.Errorf("400 not relayed: %d %s %v", w.Code, w.Body.String(), w.Header())
	}
	if r := rec.next(t); r.FinishReason != gateway.FinishError || !strings.Contains(r.Err, "upstream 400") {
		t.Errorf("error record = %+v", r)
	}
	// A retryable status with no other candidate is relayed too, retry-after included.
	w = post(h, "/v1/messages", `{"model":"only-a","messages":[]}`, nil)
	if w.Code != 529 || w.Header().Get("retry-after") != "7" || !strings.Contains(w.Body.String(), "Overloaded") {
		t.Errorf("529 not relayed: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	rec.next(t)
}

func TestTranslatePathStillUsedForLocal(t *testing.T) {
	h, up, _ := newPassthroughServer(t)
	w := post(h, "/v1/messages", `{"model":"cheap","max_tokens":5,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"text":"Hello "`) || !strings.Contains(w.Body.String(), `"text":"there"`) || !strings.Contains(w.Body.String(), "message_stop") {
		t.Fatalf("translate path broken: %d %s", w.Code, w.Body.String())
	}
	if len(up.reqs) != 0 {
		t.Errorf("local request reached the Anthropic upstream")
	}
}

func TestCountTokensPassthrough(t *testing.T) {
	h, up, rec := newPassthroughServer(t)
	w := post(h, "/v1/messages/count_tokens", `{"model":"anthropic/b","messages":[{"role":"user","content":"hi"}]}`, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"input_tokens":123`) || up.last().path != "/v1/messages/count_tokens" {
		t.Errorf("count_tokens not proxied: %d %s", w.Code, w.Body.String())
	}
	select {
	case r := <-rec.recs:
		t.Errorf("counting must not write a ledger row: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}
	// Local endpoints keep the estimate.
	w = post(h, "/v1/messages/count_tokens", `{"model":"cheap","messages":[{"role":"user","content":"hello world"}]}`, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"input_tokens":`) || len(up.reqs) != 1 {
		t.Errorf("estimate path: %d %s", w.Code, w.Body.String())
	}
}

func TestModelsAnthropicShape(t *testing.T) {
	h, _, _ := newPassthroughServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("anthropic-version", "2023-06-01")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var out struct {
		Data []struct {
			Type, ID, DisplayName string `json:"-"`
		} `json:"-"`
		Raw json.RawMessage `json:"-"`
	}
	_ = out
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil || m["has_more"] != false || m["first_id"] != "auto" {
		t.Fatalf("anthropic list = %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"display_name":"Claude B"`) || !strings.Contains(w.Body.String(), `"type":"model"`) {
		t.Errorf("anthropic list = %s", w.Body.String())
	}
	// Without the header the OpenAI shape is unchanged.
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if !strings.Contains(w.Body.String(), `"object":"list"`) {
		t.Errorf("openai list = %s", w.Body.String())
	}
}

func TestRewriteModelAndUsage(t *testing.T) {
	b, err := rewriteModel([]byte(`{"model":"x","a":{"b":[1,2]},"s":"\u00e9"}`), "claude-b")
	if err != nil || !strings.Contains(string(b), `"model":"claude-b"`) || !strings.Contains(string(b), `"a":{"b":[1,2]}`) {
		t.Errorf("rewrite = %s %v", b, err)
	}
	if _, err := rewriteModel([]byte(`[1]`), "m"); err == nil {
		t.Error("array accepted")
	}
	var u gateway.Usage
	one, zero := 1, 0
	(&anUpstreamUsage{InputTokens: &one, OutputTokens: &one, CacheReadTokens: &one}).into(&u, true)
	(&anUpstreamUsage{InputTokens: &zero, OutputTokens: &[]int{9}[0]}).into(&u, false)
	if u != (gateway.Usage{InputTokens: 1, OutputTokens: 9, CacheReadTokens: 1}) {
		t.Errorf("usage merge = %+v", u)
	}
	if finishFor("end_turn") != gateway.FinishStop || finishFor("max_tokens") != gateway.FinishLength || finishFor("") != "" {
		t.Error("finishFor")
	}
}
