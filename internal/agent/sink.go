package agent

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jking323/ws/internal/gateway"
)

// Sink receives a run's live events. The chat handler wires one to the AI
// SDK stream; the worker wires one to Postgres NOTIFY so a reconnecting
// client (or the agent monitor) can follow a run owned by another process.
type Sink interface {
	Start(messageID string)
	StartStep()
	FinishStep()
	// Model relays a gateway stream event (text, reasoning, tool-call deltas).
	Model(ev gateway.StreamEvent)
	ToolOutput(callID string, output any, isErr bool, errText string)
	ApprovalRequest(approvalID, callID string)
	Data(name string, v any, transient bool)
	Error(msg string)
	// Finish ends the message: stop | length | tool-calls | error | other.
	Finish(reason string)
}

// NopSink discards events.
type NopSink struct{}

func (NopSink) Start(string)                               {}
func (NopSink) StartStep()                                 {}
func (NopSink) FinishStep()                                {}
func (NopSink) Model(gateway.StreamEvent)                  {}
func (NopSink) ToolOutput(string, any, bool, string)       {}
func (NopSink) ApprovalRequest(string, string)             {}
func (NopSink) Data(string, any, bool)                     {}
func (NopSink) Error(string)                               {}
func (NopSink) Finish(string)                              {}

// MultiSink fans out to several sinks.
type MultiSink []Sink

func (m MultiSink) Start(id string) {
	for _, s := range m {
		s.Start(id)
	}
}
func (m MultiSink) StartStep() {
	for _, s := range m {
		s.StartStep()
	}
}
func (m MultiSink) FinishStep() {
	for _, s := range m {
		s.FinishStep()
	}
}
func (m MultiSink) Model(ev gateway.StreamEvent) {
	for _, s := range m {
		s.Model(ev)
	}
}
func (m MultiSink) ToolOutput(id string, out any, isErr bool, errText string) {
	for _, s := range m {
		s.ToolOutput(id, out, isErr, errText)
	}
}
func (m MultiSink) ApprovalRequest(a, c string) {
	for _, s := range m {
		s.ApprovalRequest(a, c)
	}
}
func (m MultiSink) Data(n string, v any, t bool) {
	for _, s := range m {
		s.Data(n, v, t)
	}
}
func (m MultiSink) Error(msg string) {
	for _, s := range m {
		s.Error(msg)
	}
}
func (m MultiSink) Finish(r string) {
	for _, s := range m {
		s.Finish(r)
	}
}

// NotifySink publishes events as JSON on a Postgres channel, one per run:
// "run_<id without dashes>". Payloads are kept under NOTIFY's 8000-byte
// limit; long text deltas are split.
type NotifySink struct {
	Pool    *pgxpool.Pool
	Channel string
	Log     *slog.Logger
}

// NotifyChannel names the channel for a run id.
func NotifyChannel(runID string) string {
	out := make([]byte, 0, len(runID)+4)
	out = append(out, "run_"...)
	for i := 0; i < len(runID); i++ {
		if runID[i] != '-' {
			out = append(out, runID[i])
		}
	}
	return string(out)
}

func (n *NotifySink) send(v map[string]any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	if len(b) > 7900 {
		// split text deltas; drop anything else oversized
		if t, ok := v["text"].(string); ok {
			half := len(t) / 2
			v1, v2 := map[string]any{}, map[string]any{}
			for k, val := range v {
				v1[k], v2[k] = val, val
			}
			v1["text"], v2["text"] = t[:half], t[half:]
			n.send(v1)
			n.send(v2)
		}
		return
	}
	if _, err := n.Pool.Exec(context.Background(), "select pg_notify($1, $2)", n.Channel, string(b)); err != nil && n.Log != nil {
		n.Log.Warn("notify sink", "err", err)
	}
}

func (n *NotifySink) Start(id string) { n.send(map[string]any{"t": "start", "message_id": id}) }
func (n *NotifySink) StartStep()      { n.send(map[string]any{"t": "start-step"}) }
func (n *NotifySink) FinishStep()     { n.send(map[string]any{"t": "finish-step"}) }
func (n *NotifySink) Model(ev gateway.StreamEvent) {
	m := map[string]any{"t": "model", "type": string(ev.Type)}
	if ev.Text != "" {
		m["text"] = ev.Text
	}
	if ev.ToolCallID != "" {
		m["tool_call_id"] = ev.ToolCallID
	}
	if ev.ToolName != "" {
		m["tool_name"] = ev.ToolName
	}
	if ev.ArgsDelta != "" {
		m["args_delta"] = ev.ArgsDelta
	}
	if ev.EndpointID != "" {
		m["endpoint"] = ev.EndpointID
		m["model"] = ev.Model
	}
	if ev.FinishReason != "" {
		m["finish_reason"] = string(ev.FinishReason)
	}
	n.send(m)
}
func (n *NotifySink) ToolOutput(id string, out any, isErr bool, errText string) {
	n.send(map[string]any{"t": "tool-output", "tool_call_id": id, "output": out, "is_error": isErr, "error": errText})
}
func (n *NotifySink) ApprovalRequest(a, c string) {
	n.send(map[string]any{"t": "approval-request", "approval_id": a, "tool_call_id": c})
}
func (n *NotifySink) Data(name string, v any, transient bool) {
	n.send(map[string]any{"t": "data", "name": name, "data": v, "transient": transient})
}
func (n *NotifySink) Error(msg string) { n.send(map[string]any{"t": "error", "text": msg}) }
func (n *NotifySink) Finish(r string)  { n.send(map[string]any{"t": "finish", "reason": r}) }
