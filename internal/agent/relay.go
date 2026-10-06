package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jking323/ws/internal/gateway"
)

// RunStatus is the subset of the runs table Relay needs to detect a run
// that finished without a final NOTIFY (worker crash between the last
// event and the status write).
type RunStatus func(ctx context.Context, runID uuid.UUID) (status string, err error)

// Relay subscribes to a run's NOTIFY channel and replays the events the
// worker's NotifySink publishes into sink, so a run driven by the worker
// streams to the browser through serve. It returns when the run finishes,
// errors, pauses, or ctx ends. ready is closed once LISTEN is active, so
// the caller can enqueue the run without racing the first event.
func Relay(ctx context.Context, pool *pgxpool.Pool, runID uuid.UUID, sink Sink, status RunStatus, ready chan<- struct{}) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	ch := NotifyChannel(runID.String())
	if _, err := conn.Exec(ctx, "listen "+ch); err != nil {
		return fmt.Errorf("relay: listen: %w", err)
	}
	if ready != nil {
		close(ready)
	}
	deadline := time.Now().Add(10 * time.Minute) // no first event: job never ran
	started := false
	for {
		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		n, err := conn.Conn().WaitForNotification(wctx)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("relay: wait: %w", err)
			}
			// Poll the row: the worker may have finished without a NOTIFY, or
			// may never have started.
			if status != nil {
				st, serr := status(ctx, runID)
				if serr == nil {
					switch st {
					case "done":
						sink.Finish("stop")
						return nil
					case "failed", "cancelled":
						sink.Error("run " + st)
						sink.Finish("error")
						return nil
					case "paused_approval", "paused_steer":
						if started {
							sink.Finish("other")
							return nil
						}
					}
				}
			}
			if !started && time.Now().After(deadline) {
				sink.Error("the worker did not pick up this run; is `ws worker` running?")
				sink.Finish("error")
				return errors.New("relay: run never started")
			}
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(n.Payload), &ev); err != nil {
			continue
		}
		started = true
		if done := dispatch(sink, ev); done {
			return nil
		}
	}
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

// dispatch replays one NotifySink payload; true means the run is over.
func dispatch(sink Sink, ev map[string]any) bool {
	switch str(ev, "t") {
	case "start":
		sink.Start(str(ev, "message_id"))
	case "start-step":
		sink.StartStep()
	case "finish-step":
		sink.FinishStep()
	case "model":
		sink.Model(gateway.StreamEvent{
			Type:         gateway.EventType(str(ev, "type")),
			Text:         str(ev, "text"),
			ToolCallID:   str(ev, "tool_call_id"),
			ToolName:     str(ev, "tool_name"),
			ArgsDelta:    str(ev, "args_delta"),
			EndpointID:   str(ev, "endpoint"),
			Model:        str(ev, "model"),
			FinishReason: gateway.FinishReason(str(ev, "finish_reason")),
		})
	case "tool-output":
		isErr, _ := ev["is_error"].(bool)
		sink.ToolOutput(str(ev, "tool_call_id"), ev["output"], isErr, str(ev, "error"))
	case "approval-request":
		sink.ApprovalRequest(str(ev, "approval_id"), str(ev, "tool_call_id"))
	case "data":
		transient, _ := ev["transient"].(bool)
		sink.Data(str(ev, "name"), ev["data"], transient)
	case "error":
		sink.Error(str(ev, "text"))
	case "finish":
		sink.Finish(str(ev, "reason"))
		return true
	}
	return false
}
