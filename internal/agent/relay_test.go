package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jking323/ws/internal/gateway"
)

// recSink records what a relay replays.
type recSink struct{ log []string }

func (r *recSink) Start(id string)   { r.log = append(r.log, "start:"+id) }
func (r *recSink) StartStep()        { r.log = append(r.log, "step") }
func (r *recSink) FinishStep()       { r.log = append(r.log, "finish-step") }
func (r *recSink) Model(ev gateway.StreamEvent) {
	r.log = append(r.log, "model:"+string(ev.Type)+":"+ev.Text+ev.ToolName+ev.ArgsDelta+ev.EndpointID+string(ev.FinishReason))
}
func (r *recSink) ToolOutput(id string, out any, isErr bool, errText string) {
	b, _ := json.Marshal(out)
	r.log = append(r.log, "tool:"+id+":"+string(b)+":"+errText)
}
func (r *recSink) ApprovalRequest(a, c string) { r.log = append(r.log, "approval:"+a+":"+c) }
func (r *recSink) Data(name string, v any, t bool) {
	r.log = append(r.log, "data:"+name)
}
func (r *recSink) Error(msg string)  { r.log = append(r.log, "error:"+msg) }
func (r *recSink) Finish(rs string) { r.log = append(r.log, "finish:"+rs) }

// The relay must reproduce exactly what NotifySink encoded. Run the real
// encoder's payload shapes through dispatch.
func TestRelayDispatchRoundTrip(t *testing.T) {
	payloads := []map[string]any{
		{"t": "start", "message_id": "m1"},
		{"t": "start-step"},
		{"t": "model", "type": "start", "endpoint": "ep/x", "model": "x"},
		{"t": "model", "type": "text_delta", "text": "hello"},
		{"t": "model", "type": "tool_call_start", "tool_call_id": "c1", "tool_name": "bash"},
		{"t": "model", "type": "tool_call_delta", "tool_call_id": "c1", "args_delta": `{"command":"ls"}`},
		{"t": "model", "type": "tool_call_end", "tool_call_id": "c1"},
		{"t": "model", "type": "finish", "finish_reason": "tool_calls"},
		{"t": "approval-request", "approval_id": "a1", "tool_call_id": "c1"},
		{"t": "tool-output", "tool_call_id": "c1", "output": map[string]any{"exit_code": 0}, "is_error": false, "error": ""},
		{"t": "data", "name": "sandbox-exec", "data": map[string]any{"command": "ls"}, "transient": true},
		{"t": "finish-step"},
		{"t": "error", "text": "boom"},
		{"t": "finish", "reason": "stop"},
	}
	s := &recSink{}
	for i, p := range payloads {
		// Through JSON, as pg_notify delivers it.
		b, _ := json.Marshal(p)
		var ev map[string]any
		_ = json.Unmarshal(b, &ev)
		done := dispatch(s, ev)
		if done != (i == len(payloads)-1) {
			t.Errorf("payload %d: done=%v", i, done)
		}
	}
	want := []string{
		"start:m1", "step",
		"model:start:ep/x", "model:text_delta:hello", "model:tool_call_start:bash",
		`model:tool_call_delta:{"command":"ls"}`, "model:tool_call_end:", "model:finish:tool_calls",
		"approval:a1:c1", `tool:c1:{"exit_code":0}:`, "data:sandbox-exec", "finish-step", "error:boom", "finish:stop",
	}
	if strings.Join(s.log, "|") != strings.Join(want, "|") {
		t.Errorf("relay log:\n got %s\nwant %s", strings.Join(s.log, "|"), strings.Join(want, "|"))
	}
}
