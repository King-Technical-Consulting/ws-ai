package compaction

import (
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
