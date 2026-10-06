package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jking323/ws/internal/agent"
	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
)

// Tool names on the agent runtime.
const (
	ToolRemember = "remember"
	ToolRecall   = "recall"
)

// MemoryTool is remember or recall on the agent runtime. Both work only in
// an agent's run (the run names the agent whose memory it is); a chat
// turn gets an error result. remember writes one memory through the same
// path reflection uses; recall searches by meaning.
type MemoryTool struct {
	Mem *Memory
	// Recall makes this the recall tool; otherwise it is remember.
	Recall bool
}

// NewRememberTool wires remember.
func NewRememberTool(m *Memory) *MemoryTool { return &MemoryTool{Mem: m} }

// NewRecallTool wires recall.
func NewRecallTool(m *Memory) *MemoryTool { return &MemoryTool{Mem: m, Recall: true} }

// Def implements agent.Tool.
func (t *MemoryTool) Def() gateway.ToolDef {
	if t.Recall {
		return gateway.ToolDef{
			Name:        ToolRecall,
			Description: "Search this agent's long-term memory: facts, preferences and episodes kept by earlier runs and by remember. Use it when the task mentions something you may have seen before (a name, a repository, a decision), before asking the user. Returns the nearest memories with their kind and importance.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"What to look for, as a sentence or a few keywords."},"k":{"type":"integer","minimum":1,"maximum":50,"description":"How many memories to return (default 8)."}},"required":["query"],"additionalProperties":false}`),
		}
	}
	return gateway.ToolDef{
		Name:        ToolRemember,
		Description: "Keep something for this agent's future runs: a stable fact about the environment or the task (an id, a URL, a number, what exists where), a preference about how the owner wants things done, or a short episode. One statement per call, specific and self-contained; do not store secrets, transient detail, or what the goal already says. A summary of each run is kept automatically, so use this for what a summary would miss.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"content":{"type":"string","description":"The statement to keep, at most 1000 characters."},"kind":{"type":"string","enum":["fact","preference","episode"],"description":"fact (default), preference or episode."},"importance":{"type":"number","minimum":0,"maximum":1,"description":"How much future runs should weigh it (default 0.7)."}},"required":["content"],"additionalProperties":false}`),
	}
}

// DefaultPolicy implements agent.Tool.
func (t *MemoryTool) DefaultPolicy() agent.Policy { return agent.PolicyAuto }

// Idempotent implements agent.Tool: both are safe to repeat (remember
// upserts by content).
func (t *MemoryTool) Idempotent() bool { return true }

// Call implements agent.Tool.
func (t *MemoryTool) Call(ctx context.Context, tc agent.ToolCtx, args json.RawMessage) (agent.Result, error) {
	if t.Mem == nil {
		return agent.ErrorResult(errors.New("memory is not configured")), nil
	}
	run, err := t.Mem.DB.GetRun(ctx, tc.RunID)
	if err != nil || !run.AgentID.Valid {
		return agent.ErrorResult(errors.New("only an agent's run has long-term memory; this is a chat turn")), nil
	}
	ag, err := t.Mem.DB.GetAgent(ctx, run.AgentID.UUID)
	if err != nil {
		return agent.ErrorResult(fmt.Errorf("agent: %w", err)), nil
	}
	if memoryConfig(ag).Disabled {
		return agent.ErrorResult(errors.New("memory is turned off for this agent")), nil
	}
	if t.Recall {
		var in struct {
			Query string `json:"query"`
			K     int    `json:"k"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return agent.ErrorResult(err), nil
		}
		if strings.TrimSpace(in.Query) == "" {
			return agent.ErrorResult(errors.New("query is required")), nil
		}
		mem, err := t.Mem.Search(ctx, ag, in.Query, in.K)
		if err != nil {
			return agent.ErrorResult(err), nil
		}
		if len(mem) == 0 {
			return agent.Result{Text: "Nothing remembered matches that.", Data: map[string]any{"memories": []any{}}}, nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%d memor%s, nearest first:\n", len(mem), plural(len(mem), "y", "ies"))
		items := make([]map[string]any, 0, len(mem))
		for _, r := range mem {
			fmt.Fprintf(&b, "- [%s] %s\n", r.Kind, r.Content)
			items = append(items, map[string]any{"id": r.ID, "kind": r.Kind, "content": r.Content, "importance": r.Importance, "distance": r.Distance})
		}
		return agent.Result{Text: b.String(), Data: map[string]any{"memories": items}}, nil
	}
	var in struct {
		Content    string  `json:"content"`
		Kind       string  `json:"kind"`
		Importance float32 `json:"importance"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return agent.ErrorResult(err), nil
	}
	row, err := t.Mem.Remember(ctx, ag, in.Kind, in.Content, in.Importance, store.NullUUID(run.ID))
	if err != nil {
		return agent.ErrorResult(err), nil
	}
	embedded := row.Embedding != nil
	text := fmt.Sprintf("Remembered as a %s (importance %.2f).", row.Kind, row.Importance)
	if !embedded {
		text += " No embedding model answered, so it is found by importance, not by meaning."
	}
	return agent.Result{Text: text, Data: map[string]any{"id": row.ID, "kind": row.Kind, "content": row.Content, "importance": row.Importance, "embedded": embedded}}, nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
