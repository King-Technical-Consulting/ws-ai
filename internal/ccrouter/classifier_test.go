package ccrouter

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/agent"
	"github.com/jking323/ws/internal/ccjobs"
	"github.com/jking323/ws/internal/gateway"
)

// fakeCompleter scripts the classifier's replies and records requests.
type fakeCompleter struct {
	replies []string
	err     error
	reqs    []*gateway.Request
}

func (f *fakeCompleter) Complete(_ context.Context, req *gateway.Request) (*gateway.Response, error) {
	f.reqs = append(f.reqs, req)
	if f.err != nil {
		return nil, f.err
	}
	reply := f.replies[0]
	if len(f.replies) > 1 {
		f.replies = f.replies[1:]
	}
	return &gateway.Response{Parts: []gateway.Part{gateway.TextPart(reply)}}, nil
}

// ambiguous is a prompt no rule matches (medium prose, no verbs the
// rules know), so the classifier is consulted.
const ambiguous = "Tell me about the history of the Hanseatic League and its trade routes across the Baltic, and how the cities organised themselves over the centuries of the league's existence. I am curious about Lübeck, Hamburg and Bergen in particular, and about how the kontors in London and Novgorod were governed, and when and how it all faded away."

func TestClassifierCallAndCache(t *testing.T) {
	fc := &fakeCompleter{replies: []string{"Sure! ```json\n{\"task_type\":\"Analysis\",\"difficulty\":\"medium\",\"needs_tools\":false,\"long_context\":false,\"sensitive\":false,\"latency_critical\":false}\n```"}}
	c := &Classifier{GW: fc, CacheSize: 2}
	meta := gateway.Metadata{UserID: "u1", ConversationID: "c1"}
	v, err := c.Classify(context.Background(), ambiguous, meta)
	if err != nil || v.TaskType != "analysis" || v.Difficulty != "medium" {
		t.Fatalf("v = %+v, err = %v", v, err)
	}
	req := fc.reqs[0]
	if req.Metadata.TaskClass != gateway.TaskClassify || req.Model != "auto" || req.MaxTokens == 0 || !strings.Contains(req.Messages[0].Parts[0].Text, "Hanseatic") {
		t.Errorf("request = %+v", req)
	}
	// The call is attributed to the caller so the ledger and user budgets see it.
	if req.Metadata.UserID != "u1" || req.Metadata.ConversationID != "c1" {
		t.Errorf("metadata = %+v", req.Metadata)
	}
	// Structured output is requested, as a strict schema that matches the
	// parser's enums, so a json_mode endpoint cannot answer off-shape.
	if req.JSON == nil || req.JSON.Name != "classification" || !req.JSON.Strict {
		t.Fatalf("json format = %+v", req.JSON)
	}
	var schema struct {
		Props map[string]struct {
			Type string   `json:"type"`
			Enum []string `json:"enum"`
		} `json:"properties"`
		Required []string `json:"required"`
		Extra    *bool    `json:"additionalProperties"`
	}
	if err := json.Unmarshal(req.JSON.Schema, &schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	if len(schema.Required) != 6 || len(schema.Props) != 6 || schema.Extra == nil || *schema.Extra {
		t.Errorf("schema shape = %+v", schema)
	}
	if got := strings.Join(schema.Props["task_type"].Enum, ","); got != "code,writing,analysis,chat,data,other" {
		t.Errorf("task_type enum = %s", got)
	}
	if got := strings.Join(schema.Props["difficulty"].Enum, ","); got != "easy,medium,hard" {
		t.Errorf("difficulty enum = %s", got)
	}
	// Second call for the same prompt is served from the cache.
	if _, err := c.Classify(context.Background(), ambiguous, meta); err != nil || c.Calls != 1 || len(fc.reqs) != 1 {
		t.Errorf("cache miss: calls=%d reqs=%d err=%v", c.Calls, len(fc.reqs), err)
	}
	// The cache is bounded: three distinct prompts with size 2 evict the first.
	fc.replies = []string{`{"task_type":"chat","difficulty":"easy"}`}
	_, _ = c.Classify(context.Background(), "b", meta)
	_, _ = c.Classify(context.Background(), "c", meta)
	_, _ = c.Classify(context.Background(), ambiguous, meta)
	if c.Calls != 4 {
		t.Errorf("eviction: calls=%d", c.Calls)
	}
	// A long prompt is clipped head and tail, never dropped.
	long := strings.Repeat("x", 4000) + "MIDDLE" + strings.Repeat("y", 4000)
	_, _ = c.Classify(context.Background(), long, meta)
	sent := fc.reqs[len(fc.reqs)-1].Messages[0].Parts[0].Text
	if len(sent) > 6200 || strings.Contains(sent, "MIDDLE") || !strings.Contains(sent, "omitted") || !strings.HasSuffix(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(sent), "JSON:")), "y") {
		t.Errorf("clip: len=%d", len(sent))
	}
}

func TestClipRuneSafe(t *testing.T) {
	// Two-byte runes: a byte cut at 2/3 of max would split one.
	s := strings.Repeat("é", 50) + strings.Repeat("ü", 50)
	got := clip(s, 30)
	if !utf8.ValidString(got) || !strings.HasPrefix(got, strings.Repeat("é", 20)) || !strings.HasSuffix(got, strings.Repeat("ü", 10)) || !strings.Contains(got, "70 characters omitted") {
		t.Errorf("clip = %q", got)
	}
	if got := clip("short", 30); got != "short" {
		t.Errorf("clip short = %q", got)
	}
}

func TestParseClassification(t *testing.T) {
	for _, bad := range []string{"", "no json here", `{"task_type":"magic","difficulty":"easy"}`, `{"task_type":"code","difficulty":"trivial"}`, `{"task_type":"code"`} {
		if _, err := ParseClassification(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	v, err := ParseClassification(`Here you go: {"task_type":"CODE","difficulty":"hard","needs_tools":true} done.`)
	if err != nil || v.TaskType != "code" || !v.NeedsTools || v.Difficulty != "hard" {
		t.Errorf("v = %+v err = %v", v, err)
	}
}

// TestToolUsesClassifier: an ambiguous prompt is classified and routed by
// the verdict; a classifier failure keeps the rules' decision; a prompt
// the rules settle never reaches the classifier.
func TestToolUsesClassifier(t *testing.T) {
	fc := &fakeCompleter{replies: []string{`{"task_type":"code","difficulty":"hard","needs_tools":true,"long_context":false,"sensitive":false,"latency_critical":false}`}}
	tl := &Tool{Classifier: &Classifier{GW: fc}, IsOwner: func(context.Context, uuid.UUID) (bool, error) { return true, nil }}
	call := func(args string) (Output, agent.Result) {
		t.Helper()
		res, err := tl.Call(context.Background(), agent.ToolCtx{}, json.RawMessage(args))
		if err != nil {
			t.Fatal(err)
		}
		if res.IsError {
			return Output{}, res
		}
		return *res.Data.(*Output), res
	}
	out, _ := call(`{"prompt":` + quote(ambiguous) + `}`)
	if out.Decision.Lane != ccjobs.LaneSubscription || out.Decision.Rule != "classifier:code" || out.Decision.Ambiguous || out.Decision.Features.Classification == nil {
		t.Errorf("classified = %+v", out.Decision)
	}
	if len(fc.reqs) != 1 {
		t.Fatalf("classifier calls = %d", len(fc.reqs))
	}
	// The tool passes the caller's ids through for ledger attribution.
	uid, cid := uuid.New(), uuid.New()
	fc.replies = []string{`{"task_type":"chat","difficulty":"easy"}`}
	if _, err := tl.Call(context.Background(), agent.ToolCtx{UserID: uid, ConversationID: cid}, json.RawMessage(`{"prompt":`+quote(ambiguous+" Second variant.")+`}`)); err != nil {
		t.Fatal(err)
	}
	if m := fc.reqs[len(fc.reqs)-1].Metadata; m.UserID != uid.String() || m.ConversationID != cid.String() {
		t.Errorf("attribution = %+v", m)
	}
	// Settled by the rules: no call (still the two calls from above).
	out, _ = call(`{"prompt":"summarize: the meeting moved to Thursday"}`)
	if out.Decision.Rule != "simple" || len(fc.reqs) != 2 {
		t.Errorf("rules-settled prompt reached the classifier: %+v calls=%d", out.Decision, len(fc.reqs))
	}
	// Failure: the rules' decision stands, flagged ambiguous, with the error in the note.
	fc.err = errors.New("endpoint down")
	out, _ = call(`{"prompt":` + quote(ambiguous+" Also mention Lübeck.") + `}`)
	if out.Decision.Rule != "default" || !out.Decision.Ambiguous || !strings.Contains(out.Note, "classifier failed") || !strings.Contains(out.Note, "endpoint down") {
		t.Errorf("fallback = %+v note=%q", out.Decision, out.Note)
	}
	// No classifier at all: plain rules, no note about it.
	tl.Classifier = nil
	out, _ = call(`{"prompt":` + quote(ambiguous) + `}`)
	if out.Decision.Rule != "default" || strings.Contains(out.Note, "classifier") {
		t.Errorf("no classifier = %+v note=%q", out.Decision, out.Note)
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
