package compaction

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jking323/ws/internal/gateway"
)

func msg(role gateway.Role, seq int64, text string) gateway.Message {
	return gateway.Message{Role: role, Seq: seq, Parts: []gateway.Part{gateway.TextPart(text)}}
}

func TestApplyBlockReplacesCoveredTurns(t *testing.T) {
	msgs := []gateway.Message{
		msg(gateway.RoleUser, 1, "a"), msg(gateway.RoleAssistant, 2, "b"),
		msg(gateway.RoleUser, 3, "c"), msg(gateway.RoleAssistant, 4, "d"),
		msg(gateway.RoleUser, 5, "e"),
	}
	out := applyBlock(msgs, 4, Block{Summary: "earlier stuff", Decisions: []string{"use Go"}})
	if len(out) != 3 {
		t.Fatalf("got %d messages, want 3 (summary pair + latest user)", len(out))
	}
	if out[0].Role != gateway.RoleUser || !strings.Contains(out[0].Parts[0].Text, "earlier stuff") || !strings.Contains(out[0].Parts[0].Text, "use Go") {
		t.Errorf("summary message wrong: %+v", out[0])
	}
	if !out[0].Parts[0].CacheHint {
		t.Error("summary should carry a cache hint")
	}
	if out[2].Parts[0].Text != "e" {
		t.Errorf("latest user turn lost: %+v", out[2])
	}
}

func TestTruncateKeepsRecentAndToolPairs(t *testing.T) {
	big := strings.Repeat("x", 4000) // ~1000 tokens each
	var msgs []gateway.Message
	for i := int64(1); i <= 20; i++ {
		role := gateway.RoleUser
		if i%2 == 0 {
			role = gateway.RoleAssistant
		}
		msgs = append(msgs, msg(role, i, big))
	}
	// an assistant tool call followed by its result in the droppable region
	msgs[5] = gateway.Message{Role: gateway.RoleAssistant, Seq: 6, Parts: []gateway.Part{{Kind: gateway.PartToolCall, ToolCallID: "c", ToolName: "f", Args: []byte(`{}`)}}}
	msgs[6] = gateway.Message{Role: gateway.RoleTool, Seq: 7, Parts: []gateway.Part{{Kind: gateway.PartToolResult, ToolCallID: "c", Content: []gateway.Part{gateway.TextPart(big)}}}}

	req := &gateway.Request{Messages: msgs}
	out := truncateToBudget(msgs, req, 6000, 4)
	if len(out) >= len(msgs) {
		t.Fatal("nothing truncated")
	}
	// first real message after the note pair must not be an orphaned tool result
	if out[2].Role == gateway.RoleTool {
		t.Error("truncation left an orphaned tool result at the front")
	}
	if !strings.Contains(out[0].Parts[0].Text, "omitted") {
		t.Errorf("missing omission note: %+v", out[0])
	}
	// the last 4 originals survive
	for i := 0; i < 4; i++ {
		if out[len(out)-1-i].Seq != msgs[len(msgs)-1-i].Seq {
			t.Errorf("recent message %d not preserved", i)
		}
	}
}

func TestEstimateTokens(t *testing.T) {
	req := &gateway.Request{System: strings.Repeat("s", 400), Messages: []gateway.Message{msg(gateway.RoleUser, 1, strings.Repeat("u", 400))}}
	if n := EstimateTokens(req); n < 195 || n > 210 {
		t.Errorf("estimate = %d, want ~200", n)
	}
}

func TestEstimateTokensByContent(t *testing.T) {
	est := func(s string) int {
		return EstimateTokens(&gateway.Request{Messages: []gateway.Message{msg(gateway.RoleUser, 1, s)}})
	}
	// Prose stays near 4 characters per token (the old estimate).
	prose := strings.Repeat("The quick brown fox jumps over the lazy dog, again. ", 400)
	if n, old := est(prose), len(prose)/4; n < old*8/10 || n > old*13/10 {
		t.Errorf("prose estimate %d, want near %d", n, old)
	}
	// Dense digits run about 2 characters per token: 21,000 five-digit
	// numbers measured 63,207 tokens in 126,000 characters.
	var sb strings.Builder
	for i := 0; i < 21000; i++ {
		fmt.Fprintf(&sb, "%05d ", (i*7919)%100000)
	}
	if n := est(sb.String()); n < 52000 || n > 75000 {
		t.Errorf("digit estimate %d, want near 63000 (chars/4 gave %d)", n, sb.Len()/4)
	}
	// CJK is far denser than 4 bytes per token.
	if n, old := est(strings.Repeat("漢字", 3000)), len(strings.Repeat("漢字", 3000))/4; n <= old/2 {
		t.Errorf("cjk estimate %d, want well over %d bytes/4", n, old/2)
	}
}

// A request's budget is the global ceiling lowered to what its endpoints
// can take: a conversation bound for a 32k slot is cut to fit it, one that
// can reach a 200k model keeps the global number.
func TestBudgetForFollowsTheEndpointWindow(t *testing.T) {
	reg := gateway.NewRegistry()
	reg.Replace(
		[]*gateway.Provider{{ID: "local", Kind: gateway.ProviderOpenAICompat}, {ID: "big", Kind: gateway.ProviderOpenAICompat}},
		[]*gateway.Endpoint{
			{ID: "local/small", ProviderID: "local", ModelName: "small", Enabled: true, Local: true, Capabilities: gateway.Capabilities{ContextWindow: 32_000}},
			{ID: "big/model", ProviderID: "big", ModelName: "model", Enabled: true, Capabilities: gateway.Capabilities{ContextWindow: 200_000}},
		},
	)
	pol, err := gateway.ParsePolicy("name: t\nrules:\n  - match: {}\n    prefer: [big/model, local/small]\naliases:\n  local: [local/small]\n")
	if err != nil {
		t.Fatal(err)
	}
	gw := gateway.New(reg, gateway.NewRouter(reg, []gateway.Policy{pol}), nil, nil)
	c := &Compactor{GW: gw}
	in := func(sel string) *gateway.RouteInput {
		return &gateway.RouteInput{Selector: sel, TaskClass: gateway.TaskChat}
	}
	if got := c.budgetFor(&gateway.Request{}, in("local/small")); got != 32_000-replyReserve {
		t.Errorf("direct 32k slot: budget %d", got)
	}
	if got := c.budgetFor(&gateway.Request{}, in("local")); got != 32_000-replyReserve {
		t.Errorf("local alias: budget %d", got)
	}
	if got := c.budgetFor(&gateway.Request{MaxTokens: 1000}, in("local")); got != 31_000 {
		t.Errorf("max_tokens is the reserve: budget %d", got)
	}
	if got := c.budgetFor(&gateway.Request{}, in("auto")); got != 60_000 {
		t.Errorf("a 200k model keeps the global budget: %d", got)
	}
	if got := (&Compactor{}).budgetFor(&gateway.Request{}, in("local")); got != 60_000 {
		t.Errorf("no gateway: global budget, got %d", got)
	}
}
