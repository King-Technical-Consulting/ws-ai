package externalapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jking323/ws/internal/gateway"
)

// Passthrough: when a /v1/messages request routes to a native Anthropic
// endpoint, the body is proxied byte-for-byte instead of translated. Only
// the model field is rewritten; anthropic-version and anthropic-beta go
// through; the client's bearer is swapped for the provider key; the
// response (status, headers, SSE pings, error bodies, retry-after,
// x-should-retry) is relayed unchanged. The stream is teed to read usage
// for the ledger, so routing, budgets and the ledger still apply
// (docs/PLAN.md, "Claude Code integration").

// AnthropicVersionDefault is sent upstream when the client gave none.
const AnthropicVersionDefault = "2023-06-01"

// MaxBody caps a proxied request or non-streaming response body.
const MaxBody = 64 << 20

// forwardedRequestHeaders go from the client to Anthropic verbatim.
var forwardedRequestHeaders = []string{"anthropic-version", "anthropic-beta", "accept", "user-agent"}

// droppedResponseHeaders are not relayed: hop-by-hop, framing that the
// proxy re-does, and anything that would bind the client to the
// upstream host.
var droppedResponseHeaders = map[string]bool{
	"Content-Length": true, "Content-Encoding": true, "Transfer-Encoding": true, "Connection": true,
	"Keep-Alive": true, "Set-Cookie": true, "Alt-Svc": true, "Via": true,
}

// isAnthropic reports whether the first candidate is a native Anthropic
// endpoint, which is what selects the passthrough.
func isAnthropic(cands []gateway.Candidate) bool {
	return len(cands) > 0 && cands[0].Provider != nil && cands[0].Provider.Kind == gateway.ProviderAnthropic
}

// retryableStatus mirrors the Anthropic adapter: these fail over to the
// next candidate when nothing has been sent to the client yet.
func retryableStatus(code int) bool {
	return code == 408 || code == 409 || code == 429 || code >= 500
}

func (s *Server) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// passthrough proxies raw to the Anthropic candidates in order and relays
// the first non-retryable response. path is "/v1/messages" or
// "/v1/messages/count_tokens"; only the former is recorded in the ledger.
func (s *Server) passthrough(w http.ResponseWriter, r *http.Request, raw []byte, path string, stream bool, cands []gateway.Candidate, dec gateway.Decision, md gateway.Metadata) {
	ctx := r.Context()
	start := time.Now()
	record := path == "/v1/messages"
	var lastErr error
	tried := 0
	for i, c := range cands {
		if c.Provider == nil || c.Provider.Kind != gateway.ProviderAnthropic {
			continue
		}
		last := true
		for _, n := range cands[i+1:] {
			if n.Provider != nil && n.Provider.Kind == gateway.ProviderAnthropic {
				last = false
				break
			}
		}
		tried++
		dec.Tried = append(dec.Tried, c.Endpoint.ID)
		body, err := rewriteModel(raw, c.Endpoint.ModelName)
		if err != nil {
			anError(w, 400, "invalid_request_error", "invalid JSON: "+err.Error())
			return
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.Provider.BaseURL, "/")+path, bytes.NewReader(body))
		if err != nil {
			lastErr = err
			continue
		}
		req.URL.RawQuery = r.URL.RawQuery // ?beta=true and friends
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", c.Provider.APIKey)
		req.Header.Set("anthropic-version", AnthropicVersionDefault)
		for _, h := range forwardedRequestHeaders {
			if v := r.Header.Get(h); v != "" {
				req.Header.Set(h, v)
			}
		}
		for k, v := range c.Provider.Headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return // client went away
			}
			s.logger().Warn("passthrough: upstream unreachable", "endpoint", c.Endpoint.ID, "err", err)
			continue
		}
		if retryableStatus(resp.StatusCode) && !last {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			lastErr = fmt.Errorf("upstream %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
			s.logger().Warn("passthrough: retryable status, failing over", "endpoint", c.Endpoint.ID, "status", resp.StatusCode)
			continue
		}
		dec.Chosen = c.Endpoint.ID
		rec := gateway.UsageRecord{Metadata: md, EndpointID: c.Endpoint.ID, Model: c.Endpoint.ModelName, Decision: dec}
		s.relay(w, resp, stream, &rec, start)
		resp.Body.Close()
		if record {
			rec.Latency = time.Since(start)
			rec.CostUSD = c.Endpoint.Pricing.Cost(rec.Usage)
			s.GW.Record(ctx, rec)
		}
		return
	}
	if lastErr == nil {
		lastErr = gateway.ErrNoRoute
	}
	err := fmt.Errorf("gateway: all %d candidates failed: %w", tried, lastErr)
	if record {
		s.GW.Record(ctx, gateway.UsageRecord{Metadata: md, Decision: dec, Latency: time.Since(start), FinishReason: gateway.FinishError, Err: err.Error()})
	}
	w.Header().Set("x-should-retry", "true")
	anError(w, http.StatusBadGateway, "api_error", err.Error())
}

// rewriteModel sets the body's model to the endpoint's model name and
// leaves every other field's bytes untouched.
func rewriteModel(raw []byte, model string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	m, _ := json.Marshal(model)
	obj["model"] = m
	return json.Marshal(obj)
}

// relay copies the upstream response to the client as it arrives. For an
// SSE stream it watches message_start, message_delta and the first content
// delta to fill rec; for a JSON body it reads usage and stop_reason from
// the message. An upstream error status is relayed as is and recorded.
func (s *Server) relay(w http.ResponseWriter, resp *http.Response, stream bool, rec *gateway.UsageRecord, start time.Time) {
	h := w.Header()
	for k, vs := range resp.Header {
		if droppedResponseHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			h.Add(k, v)
		}
	}
	if stream && resp.StatusCode == http.StatusOK {
		h.Set("Cache-Control", "no-cache, no-transform")
		h.Set("X-Accel-Buffering", "no")
	}
	w.WriteHeader(resp.StatusCode)
	rc := http.NewResponseController(w)

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, MaxBody))
		_, _ = w.Write(b)
		_ = rc.Flush()
		rec.FinishReason = gateway.FinishError
		rec.Err = fmt.Sprintf("upstream %d: %s", resp.StatusCode, clipText(string(b), 500))
		return
	}
	if !stream || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		b, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody))
		_, _ = w.Write(b)
		_ = rc.Flush()
		rec.TTFT = time.Since(start)
		if err != nil {
			rec.FinishReason, rec.Err = gateway.FinishError, "upstream read: "+err.Error()
			return
		}
		var msg anUpstreamMessage
		if json.Unmarshal(b, &msg) == nil {
			msg.Usage.into(&rec.Usage, true)
			rec.FinishReason = finishFor(msg.StopReason)
		}
		return
	}

	// SSE: write every line through unchanged (pings included), flush per
	// event, and read the data lines we care about on the way.
	br := bufio.NewReaderSize(resp.Body, 64<<10)
	first := true
	var stop string
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			_, _ = w.Write(line)
			if bytes.HasPrefix(line, []byte("data:")) {
				data := bytes.TrimSpace(line[5:])
				var ev anUpstreamEvent
				if json.Unmarshal(data, &ev) == nil {
					switch ev.Type {
					case "message_start":
						if ev.Message != nil {
							ev.Message.Usage.into(&rec.Usage, true)
						}
					case "content_block_delta", "content_block_start":
						if first {
							first = false
							rec.TTFT = time.Since(start)
						}
					case "message_delta":
						ev.Usage.into(&rec.Usage, false)
						if ev.Delta != nil && ev.Delta.StopReason != "" {
							stop = ev.Delta.StopReason
						}
					case "error":
						rec.Err = clipText(string(data), 500)
					}
				}
			}
			if len(bytes.TrimSpace(line)) == 0 {
				_ = rc.Flush() // end of an event
			}
		}
		if err != nil {
			_ = rc.Flush()
			if !errors.Is(err, io.EOF) {
				rec.FinishReason, rec.Err = gateway.FinishError, "upstream stream: "+err.Error()
				return
			}
			break
		}
	}
	if rec.Err != "" {
		rec.FinishReason = gateway.FinishError
		return
	}
	rec.FinishReason = finishFor(stop)
}

// Upstream shapes, only the fields the ledger needs.
type anUpstreamUsage struct {
	InputTokens         *int `json:"input_tokens"`
	OutputTokens        *int `json:"output_tokens"`
	CacheReadTokens     *int `json:"cache_read_input_tokens"`
	CacheCreationTokens *int `json:"cache_creation_input_tokens"`
}

// into applies the usage. message_start carries input and cache counts;
// message_delta carries the cumulative output count and, on newer API
// versions, the input counts again. Counts replace, never add.
func (u *anUpstreamUsage) into(dst *gateway.Usage, initial bool) {
	if u == nil {
		return
	}
	if u.InputTokens != nil && (initial || *u.InputTokens > 0) {
		dst.InputTokens = *u.InputTokens
	}
	if u.OutputTokens != nil {
		dst.OutputTokens = *u.OutputTokens
	}
	if u.CacheReadTokens != nil && (initial || *u.CacheReadTokens > 0) {
		dst.CacheReadTokens = *u.CacheReadTokens
	}
	if u.CacheCreationTokens != nil && (initial || *u.CacheCreationTokens > 0) {
		dst.CacheWriteTokens = *u.CacheCreationTokens
	}
}

type anUpstreamMessage struct {
	StopReason string           `json:"stop_reason"`
	Usage      *anUpstreamUsage `json:"usage"`
}

type anUpstreamEvent struct {
	Type    string             `json:"type"`
	Message *anUpstreamMessage `json:"message"`
	Usage   *anUpstreamUsage   `json:"usage"`
	Delta   *struct {
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
}

func finishFor(stop string) gateway.FinishReason {
	switch stop {
	case "tool_use":
		return gateway.FinishToolCalls
	case "max_tokens":
		return gateway.FinishLength
	case "refusal":
		return gateway.FinishFilter
	case "":
		return ""
	}
	return gateway.FinishStop
}

func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// readRaw reads a bounded request body.
func readRaw(r *http.Request) ([]byte, error) {
	r.Body = http.MaxBytesReader(nil, r.Body, MaxBody)
	return io.ReadAll(r.Body)
}

// probeFor builds the request the router sees for a raw Anthropic body:
// the selector, the metadata, and enough of the body for capability
// matching (tools, images). The lenient decode never rejects a body the
// passthrough could serve.
func probeFor(raw []byte, sel string, md gateway.Metadata) *gateway.Request {
	var in anRequest
	_ = json.Unmarshal(raw, &in)
	if req, err := in.ToCanonical(); err == nil {
		req.Model, req.Metadata = sel, md
		return req
	}
	req := &gateway.Request{Model: sel, Metadata: md}
	if len(in.Tools) > 0 {
		req.Tools = []gateway.ToolDef{{Name: "probe"}}
	}
	return req
}

// anFail writes one of our own errors in Anthropic shape, with
// x-should-retry set the way Claude Code expects.
func anFail(w http.ResponseWriter, err error) {
	status := statusFor(err)
	switch status {
	case http.StatusPaymentRequired:
		w.Header().Set("x-should-retry", "false")
		anError(w, status, "rate_limit_error", err.Error())
	case http.StatusServiceUnavailable:
		w.Header().Set("x-should-retry", "true")
		anError(w, status, "overloaded_error", err.Error())
	default:
		anError(w, status, "api_error", err.Error())
	}
}

var _ = context.Background
