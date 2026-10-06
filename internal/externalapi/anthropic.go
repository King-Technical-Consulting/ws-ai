package externalapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jking323/ws/internal/gateway"
)

// ---- inbound shapes (Anthropic Messages) ----

type anRequest struct {
	Model         string          `json:"model"`
	MaxTokens     int             `json:"max_tokens"`
	System        json.RawMessage `json:"system"`
	Messages      []anMessage     `json:"messages"`
	Tools         []anTool        `json:"tools"`
	ToolChoice    *anToolChoice   `json:"tool_choice"`
	Stream        bool            `json:"stream"`
	Temperature   *float64        `json:"temperature"`
	TopP          *float64        `json:"top_p"`
	StopSequences []string        `json:"stop_sequences"`
	Thinking      *struct {
		Type         string `json:"type"`
		BudgetTokens int    `json:"budget_tokens"`
	} `json:"thinking"`
	OutputConfig *struct {
		Effort string `json:"effort"`
	} `json:"output_config"`
	Metadata *struct {
		UserID string `json:"user_id"`
	} `json:"metadata"`
}

type anMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

type anBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Signature string          `json:"signature"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
	Source    *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	} `json:"source"`
	Title        string          `json:"title"`
	CacheControl json.RawMessage `json:"cache_control"`
}

// ToCanonical converts an Anthropic request into the gateway's request.
func (in *anRequest) ToCanonical() (*gateway.Request, error) {
	req := &gateway.Request{Model: in.Model, MaxTokens: in.MaxTokens, Temperature: in.Temperature, TopP: in.TopP, Stop: in.StopSequences}
	if len(in.System) > 0 {
		var s string
		if json.Unmarshal(in.System, &s) == nil {
			req.System = s
		} else {
			var blocks []anBlock
			if err := json.Unmarshal(in.System, &blocks); err != nil {
				return nil, errors.New("system must be a string or an array of text blocks")
			}
			var parts []string
			for _, b := range blocks {
				if b.Type == "text" {
					parts = append(parts, b.Text)
					if len(b.CacheControl) > 0 {
						req.SystemCache = true
					}
				}
			}
			req.System = strings.Join(parts, "\n\n")
		}
		if req.System != "" {
			req.SystemCache = true
		}
	}
	if in.Thinking != nil && in.Thinking.Type != "disabled" {
		req.Reasoning = &gateway.ReasoningConfig{Enabled: true, BudgetTokens: in.Thinking.BudgetTokens}
		if in.OutputConfig != nil {
			req.Reasoning.Effort = in.OutputConfig.Effort
		}
	}
	for _, m := range in.Messages {
		role := gateway.Role(m.Role)
		if role != gateway.RoleUser && role != gateway.RoleAssistant {
			return nil, fmt.Errorf("unknown role %q", m.Role)
		}
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			req.Messages = append(req.Messages, gateway.Message{Role: role, Parts: []gateway.Part{gateway.TextPart(s)}})
			continue
		}
		var blocks []anBlock
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			return nil, errors.New("message content must be a string or an array of blocks")
		}
		var userParts, toolParts []gateway.Part
		for _, b := range blocks {
			switch b.Type {
			case "text":
				p := gateway.TextPart(b.Text)
				p.CacheHint = len(b.CacheControl) > 0
				userParts = append(userParts, p)
			case "thinking":
				userParts = append(userParts, gateway.Part{Kind: gateway.PartReasoning, Text: b.Thinking, Signature: b.Signature})
			case "redacted_thinking":
				// dropped: cannot be replayed through another provider
			case "image", "document":
				p := gateway.Part{Kind: gateway.PartImage, Name: b.Title}
				if b.Type == "document" {
					p.Kind = gateway.PartFile
				}
				if b.Source != nil {
					p.MIME = b.Source.MediaType
					switch b.Source.Type {
					case "base64":
						data, err := base64.StdEncoding.DecodeString(b.Source.Data)
						if err != nil {
							return nil, fmt.Errorf("bad base64 in %s block", b.Type)
						}
						p.Data = data
					case "url":
						p.URL = b.Source.URL
					case "text":
						p.Data = []byte(b.Source.Data)
						if p.MIME == "" {
							p.MIME = "text/plain"
						}
					}
				}
				userParts = append(userParts, p)
			case "tool_use":
				args := b.Input
				if len(args) == 0 {
					args = json.RawMessage("{}")
				}
				userParts = append(userParts, gateway.Part{Kind: gateway.PartToolCall, ToolCallID: b.ID, ToolName: b.Name, Args: args})
			case "tool_result":
				tr := gateway.Part{Kind: gateway.PartToolResult, ToolCallID: b.ToolUseID, IsError: b.IsError}
				var cs string
				if json.Unmarshal(b.Content, &cs) == nil {
					tr.Content = []gateway.Part{gateway.TextPart(cs)}
				} else {
					var inner []anBlock
					_ = json.Unmarshal(b.Content, &inner)
					for _, ib := range inner {
						if ib.Type == "text" {
							tr.Content = append(tr.Content, gateway.TextPart(ib.Text))
						}
					}
				}
				toolParts = append(toolParts, tr)
			}
		}
		// Anthropic puts tool results in user turns; canonical form uses a
		// tool role. Keep ordering: results first, then any user text.
		if len(toolParts) > 0 {
			req.Messages = append(req.Messages, gateway.Message{Role: gateway.RoleTool, Parts: toolParts})
		}
		if len(userParts) > 0 {
			req.Messages = append(req.Messages, gateway.Message{Role: role, Parts: userParts})
		}
	}
	for _, t := range in.Tools {
		if t.Type != "" && t.Type != "custom" {
			continue // server tools (web_search, etc.) are not available through the gateway
		}
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		req.Tools = append(req.Tools, gateway.ToolDef{Name: t.Name, Description: t.Description, InputSchema: schema})
	}
	if in.ToolChoice != nil && len(req.Tools) > 0 {
		switch in.ToolChoice.Type {
		case "none":
			req.ToolChoice = &gateway.ToolChoice{Mode: gateway.ToolChoiceNone}
		case "any":
			req.ToolChoice = &gateway.ToolChoice{Mode: gateway.ToolChoiceRequired}
		case "tool":
			req.ToolChoice = &gateway.ToolChoice{Mode: gateway.ToolChoiceNamed, Name: in.ToolChoice.Name}
		default:
			req.ToolChoice = &gateway.ToolChoice{Mode: gateway.ToolChoiceAuto}
		}
	}
	if len(req.Messages) == 0 {
		return nil, errors.New("messages is required")
	}
	return req, nil
}

// ---- handlers ----

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	raw, err := readRaw(r)
	if err != nil {
		anError(w, 400, "invalid_request_error", "read body: "+err.Error())
		return
	}
	var head struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		anError(w, 400, "invalid_request_error", "invalid JSON: "+err.Error())
		return
	}
	p := s.Principal(r.Context())
	sel, tc := s.resolveSelector(head.Model, p)
	md := s.metadata(r, p, tc)

	// Route first (budgets, policy), then pick the path by where it landed.
	cands, dec, err := s.GW.Prepare(r.Context(), probeFor(raw, sel, md))
	if err != nil {
		anFail(w, err)
		return
	}
	if isAnthropic(cands) {
		s.passthrough(w, r, raw, "/v1/messages", head.Stream, cands, dec, md)
		return
	}

	// Translate path: an OpenAI-compatible endpoint behind the canonical
	// types. Unknown betas and block types are dropped, never an error.
	var in anRequest
	if err := json.Unmarshal(raw, &in); err != nil {
		anError(w, 400, "invalid_request_error", "invalid JSON: "+err.Error())
		return
	}
	req, err := in.ToCanonical()
	if err != nil {
		anError(w, 400, "invalid_request_error", err.Error())
		return
	}
	req.Model = sel
	req.Metadata = md
	ch := s.GW.StreamCandidates(r.Context(), req, cands, dec)
	id := newID("msg_")

	if !in.Stream {
		resp, err := gateway.Accumulate(ch)
		if err != nil && resp != nil && len(resp.Parts) == 0 {
			anError(w, statusFor(err), "api_error", err.Error())
			return
		}
		writeJSON(w, 200, anMessageJSON(id, in.Model, resp))
		return
	}

	sse := newSSE(w)
	model := in.Model
	var usage gateway.Usage
	var finish gateway.FinishReason
	idx := -1
	open := "" // text | thinking | tool
	closeBlock := func() {
		if open != "" {
			sse.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
			open = ""
		}
	}
	openBlock := func(kind string, block map[string]any) {
		closeBlock()
		idx++
		open = kind
		sse.event("content_block_start", map[string]any{"type": "content_block_start", "index": idx, "content_block": block})
	}
	started := false
	start := func() {
		if started {
			return
		}
		started = true
		sse.event("message_start", map[string]any{"type": "message_start", "message": map[string]any{
			"id": id, "type": "message", "role": "assistant", "model": model, "content": []any{},
			"stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		}})
	}
	for ev := range ch {
		switch ev.Type {
		case gateway.EventStart:
			if ev.Model != "" {
				model = ev.Model
			}
			start()
		case gateway.EventTextDelta:
			start()
			if open != "text" {
				openBlock("text", map[string]any{"type": "text", "text": ""})
			}
			sse.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": idx, "delta": map[string]any{"type": "text_delta", "text": ev.Text}})
		case gateway.EventReasoningDelta:
			start()
			if open != "thinking" {
				openBlock("thinking", map[string]any{"type": "thinking", "thinking": "", "signature": ""})
			}
			sse.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": idx, "delta": map[string]any{"type": "thinking_delta", "thinking": ev.Text}})
		case gateway.EventReasoningSig:
			if open == "thinking" {
				sse.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": idx, "delta": map[string]any{"type": "signature_delta", "signature": ev.Text}})
			}
		case gateway.EventToolCallStart:
			start()
			openBlock("tool", map[string]any{"type": "tool_use", "id": ev.ToolCallID, "name": ev.ToolName, "input": map[string]any{}})
		case gateway.EventToolCallDelta:
			if open == "tool" {
				sse.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": idx, "delta": map[string]any{"type": "input_json_delta", "partial_json": ev.ArgsDelta}})
			}
		case gateway.EventToolCallEnd:
			if open == "tool" {
				closeBlock()
			}
		case gateway.EventUsage:
			if ev.Usage != nil {
				usage.Add(*ev.Usage)
			}
		case gateway.EventFinish:
			finish = ev.FinishReason
		case gateway.EventError:
			start()
			closeBlock()
			sse.event("error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": ev.ErrText}})
			return
		}
	}
	start()
	closeBlock()
	sse.event("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": anStop(finish), "stop_sequence": nil},
		"usage": anUsage(usage)})
	sse.event("message_stop", map[string]any{"type": "message_stop"})
}

func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	raw, err := readRaw(r)
	if err != nil {
		anError(w, 400, "invalid_request_error", "read body: "+err.Error())
		return
	}
	var in anRequest
	if err := json.Unmarshal(raw, &in); err != nil {
		anError(w, 400, "invalid_request_error", "invalid JSON")
		return
	}
	p := s.Principal(r.Context())
	sel, tc := s.resolveSelector(in.Model, p)
	md := s.metadata(r, p, tc)
	// A native Anthropic endpoint counts exactly; the proxy relays the
	// answer and records nothing (counting is free). Anything else gets
	// the local estimate.
	if cands, dec, err := s.GW.Prepare(r.Context(), probeFor(raw, sel, md)); err == nil && isAnthropic(cands) {
		s.passthrough(w, r, raw, "/v1/messages/count_tokens", false, cands, dec, md)
		return
	}
	req, err := in.ToCanonical()
	if err != nil {
		anError(w, 400, "invalid_request_error", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"input_tokens": estimateTokens(req)})
}

func anMessageJSON(id, model string, resp *gateway.Response) map[string]any {
	var content []any
	for _, p := range resp.Parts {
		switch p.Kind {
		case gateway.PartText:
			content = append(content, map[string]any{"type": "text", "text": p.Text})
		case gateway.PartReasoning:
			content = append(content, map[string]any{"type": "thinking", "thinking": p.Text, "signature": p.Signature})
		case gateway.PartToolCall:
			var input any = map[string]any{}
			_ = json.Unmarshal(p.Args, &input)
			content = append(content, map[string]any{"type": "tool_use", "id": p.ToolCallID, "name": p.ToolName, "input": input})
		}
	}
	if content == nil {
		content = []any{}
	}
	if resp.Model != "" {
		model = resp.Model
	}
	return map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": model, "content": content,
		"stop_reason": anStop(resp.FinishReason), "stop_sequence": nil, "usage": anUsage(resp.Usage),
	}
}

func anUsage(u gateway.Usage) map[string]any {
	return map[string]any{
		"input_tokens": u.InputTokens, "output_tokens": u.OutputTokens,
		"cache_read_input_tokens": u.CacheReadTokens, "cache_creation_input_tokens": u.CacheWriteTokens,
	}
}

func anStop(f gateway.FinishReason) string {
	switch f {
	case gateway.FinishToolCalls:
		return "tool_use"
	case gateway.FinishLength:
		return "max_tokens"
	case gateway.FinishFilter:
		return "refusal"
	}
	return "end_turn"
}

func anError(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{"type": "error", "error": map[string]any{"type": typ, "message": msg}})
}
