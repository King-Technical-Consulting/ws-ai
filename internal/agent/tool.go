// Package agent is the durable step runtime shared by chat turns, coding
// sessions (M4) and long-lived agents (M7). A run is a sequence of steps;
// each step is one model call followed by the tool calls it requested.
// Every step is checkpointed so a run can resume on another process or
// another model.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store/blob"
)

// Policy decides whether a tool runs without asking.
type Policy string

// Tool policies.
const (
	PolicyAuto Policy = "auto"
	PolicyAsk  Policy = "ask"
	PolicyDeny Policy = "deny"
)

// ToolCtx is what a tool gets besides its arguments.
type ToolCtx struct {
	RunID          uuid.UUID
	ConversationID uuid.UUID
	UserID         uuid.UUID
	MessageID      uuid.NullUUID // the assistant message that made the call
	Blobs          blob.Store
	// Emit sends a transient data part to the UI (progress, logs).
	Emit func(name string, data any)
}

// Result is a tool's output. Text goes back to the model; Data is a
// structured copy for the UI (shown as the tool part's output).
type Result struct {
	Text    string
	Data    any
	IsError bool
}

// Tool is a callable the model may invoke.
type Tool interface {
	Def() gateway.ToolDef
	// Default policy when the agent/conversation doesn't override it.
	DefaultPolicy() Policy
	// Idempotent tools may be re-run after a crash; others refuse to re-run
	// a call that already started (the runtime records a started marker).
	Idempotent() bool
	Call(ctx context.Context, tc ToolCtx, args json.RawMessage) (Result, error)
}

// Registry holds the tools available to a run.
type Registry struct {
	tools map[string]Tool
}

// NewRegistry builds a registry.
func NewRegistry(tools ...Tool) *Registry {
	r := &Registry{tools: map[string]Tool{}}
	for _, t := range tools {
		r.Add(t)
	}
	return r
}

// Add registers a tool.
func (r *Registry) Add(t Tool) { r.tools[t.Def().Name] = t }

// Get looks up a tool.
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Defs returns tool definitions, filtered by an allowlist (empty = all),
// sorted by name so the prompt prefix stays cache-stable.
func (r *Registry) Defs(allow []string) []gateway.ToolDef {
	allowed := map[string]bool{}
	for _, a := range allow {
		allowed[a] = true
	}
	var out []gateway.ToolDef
	for name, t := range r.tools {
		if len(allow) > 0 && !allowed[name] {
			continue
		}
		out = append(out, t.Def())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Policies resolves the effective policy per tool: overrides win, then the
// tool's default.
func (r *Registry) Policies(overrides map[string]Policy) map[string]Policy {
	out := map[string]Policy{}
	for name, t := range r.tools {
		out[name] = t.DefaultPolicy()
		if p, ok := overrides[name]; ok && p != "" {
			out[name] = p
		}
	}
	return out
}

// ---- helpers for tool authors ----

// ErrorResult wraps an error for the model.
func ErrorResult(err error) Result {
	return Result{Text: "Error: " + err.Error(), IsError: true}
}

// JSONResult returns v as both text and data.
func JSONResult(v any) Result {
	b, err := json.Marshal(v)
	if err != nil {
		return ErrorResult(err)
	}
	return Result{Text: string(b), Data: v}
}

// Truncate caps text for the model with a note.
func Truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("\n\n[... truncated %d more characters]", len(s)-max)
}

// MaxInlineResult is the size above which tool output is externalized to
// the blob store and replaced with a stub plus a read_blob reference.
const MaxInlineResult = 24 * 1024

// externalize stores big output and returns the stub the model sees.
func externalize(ctx context.Context, blobs blob.Store, name, text string) (string, bool) {
	if blobs == nil || len(text) <= MaxInlineResult {
		return text, false
	}
	key, err := blob.PutBytes(ctx, blobs, []byte(text))
	if err != nil {
		return Truncate(text, MaxInlineResult), false
	}
	head := text
	if len(head) > 2048 {
		head = head[:2048]
	}
	// Keep the start so the model can decide whether it needs the rest.
	return fmt.Sprintf("[%s output is %d bytes; stored as blob %s. First 2 KB follow. Use read_blob with that key and an offset to read more.]\n\n%s",
		name, len(text), key, strings.TrimRight(head, "\n")), true
}
