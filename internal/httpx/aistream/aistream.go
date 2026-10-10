// Package aistream writes the Vercel AI SDK "UI Message Stream" protocol
// (v1) over Server-Sent Events, so @ai-sdk/react's useChat can consume a Go
// backend directly.
//
// Wire format: `data: {json}\n\n` per part, terminated by `data: [DONE]`,
// with response header `x-vercel-ai-ui-message-stream: v1`.
// Reference: https://ai-sdk.dev/docs/ai-sdk-ui/stream-protocol
package aistream

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sync"
)

// Header value that marks the protocol version.
const HeaderValue = "v1"

// Writer emits stream parts. It is safe for concurrent use.
type Writer struct {
	mu sync.Mutex
	w  io.Writer
	f  flusher
}

type flusher interface{ Flush() error }

type noopFlusher struct{}

func (noopFlusher) Flush() error { return nil }

// NewHTTP prepares the response for SSE and returns a Writer.
func NewHTTP(w http.ResponseWriter) *Writer {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	h.Set("x-vercel-ai-ui-message-stream", HeaderValue)
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	return &Writer{w: w, f: rc}
}

// New wraps any writer (for tests or buffering).
func New(w io.Writer) *Writer { return &Writer{w: w, f: noopFlusher{}} }

// Part is any stream part; it must marshal to an object with a "type" field.
type Part map[string]any

func (wr *Writer) send(p any) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	wr.mu.Lock()
	defer wr.mu.Unlock()
	if _, err := fmt.Fprintf(wr.w, "data: %s\n\n", b); err != nil {
		return err
	}
	return wr.f.Flush()
}

// Done terminates the stream.
func (wr *Writer) Done() error {
	wr.mu.Lock()
	defer wr.mu.Unlock()
	if _, err := io.WriteString(wr.w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	return wr.f.Flush()
}

// Start begins a message. messageID is the server-side id of the assistant
// message being produced.
func (wr *Writer) Start(messageID string) error {
	return wr.send(Part{"type": "start", "messageId": messageID})
}

// StartStep / FinishStep bracket one model call within a message (an agent
// loop produces several steps per message).
func (wr *Writer) StartStep() error  { return wr.send(Part{"type": "start-step"}) }
func (wr *Writer) FinishStep() error { return wr.send(Part{"type": "finish-step"}) }

// Finish ends the message. finishReason follows the AI SDK vocabulary:
// stop | length | content-filter | tool-calls | error | other.
func (wr *Writer) Finish(finishReason string) error {
	p := Part{"type": "finish"}
	if finishReason != "" {
		p["finishReason"] = finishReason
	}
	return wr.send(p)
}

// Text parts. Each text block has an id so the client can keep blocks apart.
func (wr *Writer) TextStart(id string) error { return wr.send(Part{"type": "text-start", "id": id}) }
func (wr *Writer) TextDelta(id, delta string) error {
	return wr.send(Part{"type": "text-delta", "id": id, "delta": delta})
}
func (wr *Writer) TextEnd(id string) error { return wr.send(Part{"type": "text-end", "id": id}) }

// Reasoning parts.
func (wr *Writer) ReasoningStart(id string) error {
	return wr.send(Part{"type": "reasoning-start", "id": id})
}
func (wr *Writer) ReasoningDelta(id, delta string) error {
	return wr.send(Part{"type": "reasoning-delta", "id": id, "delta": delta})
}
func (wr *Writer) ReasoningEnd(id string) error {
	return wr.send(Part{"type": "reasoning-end", "id": id})
}

// Tool parts.
func (wr *Writer) ToolInputStart(toolCallID, toolName string) error {
	return wr.send(Part{"type": "tool-input-start", "toolCallId": toolCallID, "toolName": toolName})
}
func (wr *Writer) ToolInputDelta(toolCallID, delta string) error {
	return wr.send(Part{"type": "tool-input-delta", "toolCallId": toolCallID, "inputTextDelta": delta})
}
func (wr *Writer) ToolInputAvailable(toolCallID, toolName string, input json.RawMessage) error {
	var in any
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			in = string(input)
		}
	} else {
		in = map[string]any{}
	}
	return wr.send(Part{"type": "tool-input-available", "toolCallId": toolCallID, "toolName": toolName, "input": in})
}
func (wr *Writer) ToolOutputAvailable(toolCallID string, output any) error {
	return wr.send(Part{"type": "tool-output-available", "toolCallId": toolCallID, "output": output})
}
func (wr *Writer) ToolOutputError(toolCallID, errText string) error {
	return wr.send(Part{"type": "tool-output-error", "toolCallId": toolCallID, "errorText": errText})
}
func (wr *Writer) ToolApprovalRequest(approvalID, toolCallID string) error {
	return wr.send(Part{"type": "tool-approval-request", "approvalId": approvalID, "toolCallId": toolCallID})
}

// Data sends a custom typed data part: type is "data-<name>".
func (wr *Writer) Data(name string, data any, transient bool) error {
	p := Part{"type": "data-" + name, "data": data}
	if transient {
		p["transient"] = true
	}
	return wr.send(p)
}

// Source and file parts.
func (wr *Writer) SourceURL(id, url, title string) error {
	return wr.send(Part{"type": "source-url", "sourceId": id, "url": url, "title": title})
}
func (wr *Writer) File(url, mediaType string) error {
	return wr.send(Part{"type": "file", "url": url, "mediaType": mediaType})
}

// Error sends an error part. The client shows errorText.
func (wr *Writer) Error(errText string) error {
	return wr.send(Part{"type": "error", "errorText": scrubErrorText(errText)})
}

var (
	urlInError = regexp.MustCompile(`https?://[^\s"')]+`)
	// A Go network error names the address without a scheme: "dial tcp
	// 10.0.0.5:8000", "lookup llama on 127.0.0.11:53", a bare IPv4 address,
	// a bracketed IPv6 address. A four-part dotted number in other text
	// (a version) is also replaced; that is rare in an error and the
	// loss is harmless.
	netInError = regexp.MustCompile(`dial (tcp|udp)6? [^\s:\[]+(:\d+)?|lookup \S+ on \S+|\[[0-9a-fA-F:.]+\](:\d+)?|\b\d{1,3}(\.\d{1,3}){3}(:\d+)?\b`)
)

// scrubErrorText removes addresses from an error shown to the person
// chatting. A failed model call carries the upstream address in its message
// (`Post "http://10.0.0.5:8000/v1/...": dial tcp 10.0.0.5:8000: connection
// refused`), which is an internal host to anyone but the operator. The full
// text stays in the server's logs and the ledger.
func scrubErrorText(s string) string {
	s = urlInError.ReplaceAllString(s, "<model server>")
	return netInError.ReplaceAllString(s, "<address>")
}

// Abort signals the stream was aborted.
func (wr *Writer) Abort() error { return wr.send(Part{"type": "abort"}) }
