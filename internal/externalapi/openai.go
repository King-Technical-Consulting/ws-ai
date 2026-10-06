package externalapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/jking323/ws/internal/gateway"
)

// ---- inbound shapes (OpenAI Chat Completions) ----

type oaRequest struct {
	Model         string          `json:"model"`
	Messages      []oaMessage     `json:"messages"`
	Tools         []oaTool        `json:"tools"`
	ToolChoice    json.RawMessage `json:"tool_choice"`
	Stream        bool            `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	MaxTokens           int             `json:"max_tokens"`
	MaxCompletionTokens int             `json:"max_completion_tokens"`
	Temperature         *float64        `json:"temperature"`
	TopP                *float64        `json:"top_p"`
	Stop                json.RawMessage `json:"stop"`
	ReasoningEffort     string          `json:"reasoning_effort"`
	N                   int             `json:"n"`
}

type oaMessage struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`
	Name             string          `json:"name"`
	ToolCalls        []oaToolCall    `json:"tool_calls"`
	ToolCallID       string          `json:"tool_call_id"`
	ReasoningContent string          `json:"reasoning_content"`
}

type oaToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type oaPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url"`
	File *struct {
		FileData string `json:"file_data"`
		Filename string `json:"filename"`
	} `json:"file"`
}

// ToCanonical converts an OpenAI request into the gateway's request.
func (in *oaRequest) ToCanonical() (*gateway.Request, error) {
	req := &gateway.Request{Model: in.Model, Temperature: in.Temperature, TopP: in.TopP}
	if in.MaxCompletionTokens > 0 {
		req.MaxTokens = in.MaxCompletionTokens
	} else if in.MaxTokens > 0 {
		req.MaxTokens = in.MaxTokens
	}
	if len(in.Stop) > 0 {
		var one string
		var many []string
		if json.Unmarshal(in.Stop, &one) == nil && one != "" {
			req.Stop = []string{one}
		} else if json.Unmarshal(in.Stop, &many) == nil {
			req.Stop = many
		}
	}
	if in.ReasoningEffort != "" {
		req.Reasoning = &gateway.ReasoningConfig{Enabled: true, Effort: in.ReasoningEffort}
	}
	var system []string
	for _, m := range in.Messages {
		switch m.Role {
		case "system", "developer":
			system = append(system, oaTextOf(m.Content))
		case "user":
			parts, err := oaUserParts(m.Content)
			if err != nil {
				return nil, err
			}
			req.Messages = append(req.Messages, gateway.Message{Role: gateway.RoleUser, Parts: parts})
		case "assistant":
			var parts []gateway.Part
			if m.ReasoningContent != "" {
				parts = append(parts, gateway.Part{Kind: gateway.PartReasoning, Text: m.ReasoningContent})
			}
			if t := oaTextOf(m.Content); t != "" {
				parts = append(parts, gateway.TextPart(t))
			}
			for _, tc := range m.ToolCalls {
				args := tc.Function.Arguments
				if strings.TrimSpace(args) == "" {
					args = "{}"
				}
				parts = append(parts, gateway.Part{Kind: gateway.PartToolCall, ToolCallID: tc.ID, ToolName: tc.Function.Name, Args: json.RawMessage(args)})
			}
			if len(parts) > 0 {
				req.Messages = append(req.Messages, gateway.Message{Role: gateway.RoleAssistant, Parts: parts})
			}
		case "tool":
			req.Messages = append(req.Messages, gateway.Message{Role: gateway.RoleTool, Parts: []gateway.Part{{
				Kind: gateway.PartToolResult, ToolCallID: m.ToolCallID, Content: []gateway.Part{gateway.TextPart(oaTextOf(m.Content))},
			}}})
		default:
			return nil, fmt.Errorf("unknown role %q", m.Role)
		}
	}
	req.System = strings.Join(system, "\n\n")
	req.SystemCache = req.System != ""
	for _, t := range in.Tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		schema := t.Function.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		req.Tools = append(req.Tools, gateway.ToolDef{Name: t.Function.Name, Description: t.Function.Description, InputSchema: schema})
	}
	if len(in.ToolChoice) > 0 && len(req.Tools) > 0 {
		var s string
		if json.Unmarshal(in.ToolChoice, &s) == nil {
			switch s {
			case "none":
				req.ToolChoice = &gateway.ToolChoice{Mode: gateway.ToolChoiceNone}
			case "required":
				req.ToolChoice = &gateway.ToolChoice{Mode: gateway.ToolChoiceRequired}
			default:
				req.ToolChoice = &gateway.ToolChoice{Mode: gateway.ToolChoiceAuto}
			}
		} else {
			var named struct {
				Function struct{ Name string } `json:"function"`
			}
			if json.Unmarshal(in.ToolChoice, &named) == nil && named.Function.Name != "" {
				req.ToolChoice = &gateway.ToolChoice{Mode: gateway.ToolChoiceNamed, Name: named.Function.Name}
			}
		}
	}
	if len(req.Messages) == 0 {
		return nil, errors.New("messages is required")
	}
	return req, nil
}

func oaTextOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []oaPart
	if json.Unmarshal(raw, &parts) == nil {
		var sb strings.Builder
		for _, p := range parts {
			if p.Type == "text" {
				sb.WriteString(p.Text)
			}
		}
		return sb.String()
	}
	return ""
}

func oaUserParts(raw json.RawMessage) ([]gateway.Part, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []gateway.Part{gateway.TextPart(s)}, nil
	}
	var parts []oaPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("content must be a string or an array of parts")
	}
	var out []gateway.Part
	for _, p := range parts {
		switch p.Type {
		case "text":
			out = append(out, gateway.TextPart(p.Text))
		case "image_url":
			if p.ImageURL == nil {
				continue
			}
			part, err := partFromURL(p.ImageURL.URL, "", true)
			if err != nil {
				return nil, err
			}
			out = append(out, part)
		case "file":
			if p.File == nil {
				continue
			}
			part, err := partFromURL(p.File.FileData, p.File.Filename, false)
			if err != nil {
				return nil, err
			}
			out = append(out, part)
		}
	}
	return out, nil
}

// partFromURL handles data: URLs and remote URLs.
func partFromURL(u, name string, image bool) (gateway.Part, error) {
	p := gateway.Part{Kind: gateway.PartFile, Name: name}
	if image {
		p.Kind = gateway.PartImage
	}
	if strings.HasPrefix(u, "data:") {
		comma := strings.Index(u, ",")
		if comma < 0 {
			return p, errors.New("bad data url")
		}
		meta := u[5:comma]
		p.MIME = strings.Split(meta, ";")[0]
		if !strings.Contains(meta, ";base64") {
			return p, errors.New("only base64 data urls are supported")
		}
		b, err := base64.StdEncoding.DecodeString(u[comma+1:])
		if err != nil {
			return p, fmt.Errorf("bad data url: %w", err)
		}
		p.Data = b
		return p, nil
	}
	p.URL = u
	if p.MIME == "" && image {
		p.MIME = "image/*"
	}
	return p, nil
}

// ---- handlers ----

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	now := time.Now().Unix()
	var out []model
	seen := map[string]bool{}
	for _, pol := range s.GW.Router.Policies() {
		for a := range pol.Aliases {
			if !seen[a] {
				seen[a] = true
				out = append(out, model{ID: a, Object: "model", Created: now, OwnedBy: "ws"})
			}
		}
	}
	if !seen["auto"] {
		out = append([]model{{ID: "auto", Object: "model", Created: now, OwnedBy: "ws"}}, out...)
	}
	for _, ep := range s.GW.Registry.Endpoints() {
		if ep.Enabled && !ep.Capabilities.Embeddings {
			out = append(out, model{ID: ep.ID, Object: "model", Created: now, OwnedBy: ep.ProviderID})
		}
	}
	// An Anthropic SDK client (Claude Code with gateway model discovery on)
	// identifies itself with anthropic-version and expects that API's
	// list shape; everyone else gets the OpenAI shape.
	if r.Header.Get("anthropic-version") != "" {
		type anModel struct {
			Type        string `json:"type"`
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
			CreatedAt   string `json:"created_at"`
		}
		created := time.Now().UTC().Format(time.RFC3339)
		data := make([]anModel, 0, len(out))
		for _, m := range out {
			name := m.ID
			if ep, ok := s.GW.Registry.Endpoint(m.ID); ok && ep.DisplayName != "" {
				name = ep.DisplayName
			}
			data = append(data, anModel{Type: "model", ID: m.ID, DisplayName: name, CreatedAt: created})
		}
		first, last := "", ""
		if len(data) > 0 {
			first, last = data[0].ID, data[len(data)-1].ID
		}
		writeJSON(w, 200, map[string]any{"data": data, "has_more": false, "first_id": first, "last_id": last})
		return
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": out})
}

func (s *Server) handleModel(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sel, _ := s.resolveSelector(id, s.Principal(r.Context()))
	if r.Header.Get("anthropic-version") != "" {
		writeJSON(w, 200, map[string]any{"type": "model", "id": id, "display_name": id, "created_at": time.Now().UTC().Format(time.RFC3339), "resolved": sel})
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "object": "model", "created": time.Now().Unix(), "owned_by": "ws", "resolved": sel})
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	var in oaRequest
	if err := readJSON(r, &in); err != nil {
		oaError(w, 400, "invalid_request_error", "invalid JSON: "+err.Error())
		return
	}
	req, err := in.ToCanonical()
	if err != nil {
		oaError(w, 400, "invalid_request_error", err.Error())
		return
	}
	p := s.Principal(r.Context())
	sel, tc := s.resolveSelector(in.Model, p)
	req.Model = sel
	req.Metadata = s.metadata(r, p, tc)

	ch, err := s.GW.Stream(r.Context(), req)
	if err != nil {
		oaError(w, statusFor(err), "server_error", err.Error())
		return
	}
	id := newID("chatcmpl-")
	created := time.Now().Unix()

	if !in.Stream {
		resp, err := gateway.Accumulate(ch)
		if err != nil && resp != nil && len(resp.Parts) == 0 {
			oaError(w, statusFor(err), "server_error", err.Error())
			return
		}
		writeJSON(w, 200, oaCompletion(id, created, resp))
		return
	}

	sse := newSSE(w)
	model := in.Model
	chunk := func(delta map[string]any, finish any) {
		sse.event("", map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		})
	}
	first := true
	toolIndex := map[string]int{}
	var usage gateway.Usage
	var finish gateway.FinishReason
	for ev := range ch {
		switch ev.Type {
		case gateway.EventStart:
			if ev.Model != "" {
				model = ev.Model
			}
			chunk(map[string]any{"role": "assistant", "content": ""}, nil)
			first = false
		case gateway.EventTextDelta:
			if first {
				chunk(map[string]any{"role": "assistant", "content": ""}, nil)
				first = false
			}
			chunk(map[string]any{"content": ev.Text}, nil)
		case gateway.EventReasoningDelta:
			chunk(map[string]any{"reasoning_content": ev.Text}, nil)
		case gateway.EventToolCallStart:
			idx := len(toolIndex)
			toolIndex[ev.ToolCallID] = idx
			chunk(map[string]any{"tool_calls": []any{map[string]any{"index": idx, "id": ev.ToolCallID, "type": "function", "function": map[string]any{"name": ev.ToolName, "arguments": ""}}}}, nil)
		case gateway.EventToolCallDelta:
			idx := toolIndex[ev.ToolCallID]
			chunk(map[string]any{"tool_calls": []any{map[string]any{"index": idx, "function": map[string]any{"arguments": ev.ArgsDelta}}}}, nil)
		case gateway.EventUsage:
			if ev.Usage != nil {
				usage.Add(*ev.Usage)
			}
		case gateway.EventFinish:
			finish = ev.FinishReason
		case gateway.EventError:
			sse.event("", map[string]any{"error": map[string]any{"message": ev.ErrText, "type": "server_error"}})
			sse.raw("data: [DONE]\n\n")
			return
		}
	}
	chunk(map[string]any{}, oaFinish(finish))
	sse.event("", map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": created, "model": model, "choices": []any{},
		"usage": oaUsage(usage),
	})
	sse.raw("data: [DONE]\n\n")
}

func oaCompletion(id string, created int64, resp *gateway.Response) map[string]any {
	msg := map[string]any{"role": "assistant", "content": nil}
	if t := resp.Text(); t != "" {
		msg["content"] = t
	}
	var reasoning string
	var calls []any
	for _, p := range resp.Parts {
		switch p.Kind {
		case gateway.PartReasoning:
			reasoning += p.Text
		case gateway.PartToolCall:
			calls = append(calls, map[string]any{"id": p.ToolCallID, "type": "function", "function": map[string]any{"name": p.ToolName, "arguments": string(p.Args)}})
		}
	}
	if reasoning != "" {
		msg["reasoning_content"] = reasoning
	}
	if len(calls) > 0 {
		msg["tool_calls"] = calls
	}
	return map[string]any{
		"id": id, "object": "chat.completion", "created": created, "model": resp.Model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": oaFinish(resp.FinishReason)}},
		"usage":   oaUsage(resp.Usage),
		"ws":      map[string]any{"endpoint": resp.EndpointID},
	}
}

func oaUsage(u gateway.Usage) map[string]any {
	prompt := u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens
	return map[string]any{
		"prompt_tokens": prompt, "completion_tokens": u.OutputTokens, "total_tokens": prompt + u.OutputTokens,
		"prompt_tokens_details": map[string]any{"cached_tokens": u.CacheReadTokens},
	}
}

func oaFinish(f gateway.FinishReason) string {
	switch f {
	case gateway.FinishLength:
		return "length"
	case gateway.FinishToolCalls:
		return "tool_calls"
	case gateway.FinishFilter:
		return "content_filter"
	}
	return "stop"
}

func oaError(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg, "type": typ, "code": nil, "param": nil}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
