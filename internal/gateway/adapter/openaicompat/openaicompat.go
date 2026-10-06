// Package openaicompat adapts the canonical gateway types to any server that
// speaks OpenAI Chat Completions: OpenAI, OpenRouter, vLLM, llama-server,
// Ollama, LM Studio, mlx-lm. It is the only package allowed to import
// openai-go.
package openaicompat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	sdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/packages/respjson"
	"github.com/openai/openai-go/v3/shared"

	"github.com/jking323/ws/internal/gateway"
)

// defaultMaxOutput is used when neither the request nor the endpoint sets one.
const defaultMaxOutput = 8192

// Adapter implements gateway.Adapter for OpenAI-compatible servers.
type Adapter struct {
	HTTPClient *http.Client
	Timeout    time.Duration
}

// New returns an adapter with defaults.
func New() *Adapter { return &Adapter{Timeout: 10 * time.Minute} }

func (a *Adapter) client(p *gateway.Provider) sdk.Client {
	opts := []option.RequestOption{option.WithMaxRetries(0)}
	key := p.APIKey
	if key == "" {
		key = "none" // local servers ignore it but the SDK wants something
	}
	opts = append(opts, option.WithAPIKey(key))
	if p.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(p.BaseURL))
	}
	for k, v := range p.Headers {
		opts = append(opts, option.WithHeader(k, v))
	}
	if a.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(a.HTTPClient))
	}
	if a.Timeout > 0 {
		opts = append(opts, option.WithRequestTimeout(a.Timeout))
	}
	return sdk.NewClient(opts...)
}

// Stream implements gateway.Adapter.
func (a *Adapter) Stream(ctx context.Context, p *gateway.Provider, ep *gateway.Endpoint, req *gateway.Request) (<-chan gateway.StreamEvent, error) {
	params, err := buildParams(ep, req)
	if err != nil {
		return nil, err
	}
	out := make(chan gateway.StreamEvent, 64)
	client := a.client(p)

	// Per-endpoint body overrides (OpenRouter provider pinning, engine knobs).
	// Applied after the SDK serializes params, so they win on conflict.
	var reqOpts []option.RequestOption
	for k, v := range ep.ExtraBody {
		reqOpts = append(reqOpts, option.WithJSONSet(k, v))
	}

	go func() {
		defer close(out)
		stream := client.Chat.Completions.NewStreaming(ctx, params, reqOpts...)
		defer stream.Close()

		send := func(ev gateway.StreamEvent) bool {
			select {
			case out <- ev:
				return true
			case <-ctx.Done():
				return false
			}
		}

		started := false
		emitted := false
		// OpenAI streams tool calls keyed by index; the id arrives only on the
		// first fragment. Track index -> id so later fragments attach.
		toolIDByIndex := map[int64]string{}
		var openTools []int64
		var usage *gateway.Usage
		var finish gateway.FinishReason

		for stream.Next() {
			chunk := stream.Current()
			if !started {
				started = true
				if !send(gateway.StreamEvent{Type: gateway.EventStart, EndpointID: ep.ID, Model: chunk.Model}) {
					return
				}
			}
			if chunk.JSON.Usage.Valid() {
				u := chunk.Usage
				cached := int(u.PromptTokensDetails.CachedTokens)
				usage = &gateway.Usage{
					// Normalize: InputTokens excludes cache reads so Pricing.Cost
					// applies cache pricing correctly.
					InputTokens:     int(u.PromptTokens) - cached,
					OutputTokens:    int(u.CompletionTokens),
					CacheReadTokens: cached,
				}
			}
			for _, ch := range chunk.Choices {
				d := ch.Delta
				if d.Content != "" {
					emitted = true
					if !send(gateway.StreamEvent{Type: gateway.EventTextDelta, Text: d.Content}) {
						return
					}
				}
				// Reasoning content is non-standard but widely emitted by
				// vLLM/llama.cpp/OpenRouter as "reasoning_content" or "reasoning".
				if r := extraString(d.JSON.ExtraFields, "reasoning_content", "reasoning"); r != "" {
					if !send(gateway.StreamEvent{Type: gateway.EventReasoningDelta, Text: r}) {
						return
					}
				}
				for _, tc := range d.ToolCalls {
					id, known := toolIDByIndex[tc.Index]
					if !known {
						id = tc.ID
						if id == "" {
							id = fmt.Sprintf("call_%d", tc.Index)
						}
						toolIDByIndex[tc.Index] = id
						openTools = append(openTools, tc.Index)
						emitted = true
						if !send(gateway.StreamEvent{Type: gateway.EventToolCallStart, ToolCallID: id, ToolName: tc.Function.Name}) {
							return
						}
					} else if tc.Function.Name != "" && tc.ID != "" && tc.ID != id {
						// Some servers (llama.cpp) reuse index 0 for a second call
						// and send a fresh id. Treat as a new call.
						if !send(gateway.StreamEvent{Type: gateway.EventToolCallEnd, ToolCallID: id}) {
							return
						}
						id = tc.ID
						toolIDByIndex[tc.Index] = id
						if !send(gateway.StreamEvent{Type: gateway.EventToolCallStart, ToolCallID: id, ToolName: tc.Function.Name}) {
							return
						}
					}
					if tc.Function.Arguments != "" {
						if !send(gateway.StreamEvent{Type: gateway.EventToolCallDelta, ToolCallID: id, ArgsDelta: tc.Function.Arguments}) {
							return
						}
					}
				}
				if ch.FinishReason != "" {
					finish = mapFinish(ch.FinishReason)
				}
			}
		}
		if err := stream.Err(); err != nil {
			send(gateway.ErrorEvent(normalizeErr(err), !emitted && isRetryableErr(err)))
			return
		}
		for _, idx := range openTools {
			if !send(gateway.StreamEvent{Type: gateway.EventToolCallEnd, ToolCallID: toolIDByIndex[idx]}) {
				return
			}
		}
		if finish == "" {
			finish = gateway.FinishStop
		}
		if usage != nil {
			if !send(gateway.StreamEvent{Type: gateway.EventUsage, Usage: usage}) {
				return
			}
		}
		send(gateway.StreamEvent{Type: gateway.EventFinish, FinishReason: finish})
	}()
	return out, nil
}

// CountTokens: OpenAI-compatible servers have no counting endpoint. Returns
// a rough estimate (4 chars per token) and exact=false.
func (a *Adapter) CountTokens(ctx context.Context, p *gateway.Provider, ep *gateway.Endpoint, req *gateway.Request) (int, bool, error) {
	n := len(req.System)
	for _, m := range req.Messages {
		for _, pt := range m.Parts {
			n += len(pt.Text) + len(pt.Args)
			for _, c := range pt.Content {
				n += len(c.Text)
			}
			if pt.Kind == gateway.PartImage {
				n += 1500 * 4 // typical image token cost
			}
		}
	}
	for _, t := range req.Tools {
		n += len(t.Name) + len(t.Description) + len(t.InputSchema)
	}
	return n / 4, false, nil
}

func buildParams(ep *gateway.Endpoint, req *gateway.Request) (sdk.ChatCompletionNewParams, error) {
	params := sdk.ChatCompletionNewParams{
		Model:         shared.ChatModel(ep.ModelName),
		StreamOptions: sdk.ChatCompletionStreamOptionsParam{IncludeUsage: param.NewOpt(true)},
	}
	// Always send a completion cap. Without one, OpenRouter (and some other
	// servers) assume the model's full output window and pre-reserve credits
	// for it, which fails with HTTP 402 on small balances.
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = ep.Capabilities.MaxOutput
	}
	if maxTokens <= 0 {
		maxTokens = defaultMaxOutput
	}
	params.MaxCompletionTokens = param.NewOpt(int64(maxTokens))
	if req.Temperature != nil {
		params.Temperature = param.NewOpt(*req.Temperature)
	}
	if req.TopP != nil {
		params.TopP = param.NewOpt(*req.TopP)
	}
	if len(req.Stop) > 0 {
		params.Stop = sdk.ChatCompletionNewParamsStopUnion{OfStringArray: req.Stop}
	}
	if req.Reasoning != nil && req.Reasoning.Enabled && ep.Capabilities.Reasoning && req.Reasoning.Effort != "" {
		params.ReasoningEffort = shared.ReasoningEffort(req.Reasoning.Effort)
	}
	// response_format only where the endpoint declares json_mode: servers
	// that do not know the field reject the whole request.
	if req.JSON != nil && ep.Capabilities.JSONMode {
		if len(req.JSON.Schema) > 0 {
			var schema map[string]any
			if err := json.Unmarshal(req.JSON.Schema, &schema); err != nil {
				return params, fmt.Errorf("openaicompat: json schema: %w", err)
			}
			name := req.JSON.Name
			if name == "" {
				name = "reply"
			}
			js := shared.ResponseFormatJSONSchemaJSONSchemaParam{Name: name, Schema: schema}
			if req.JSON.Strict {
				js.Strict = param.NewOpt(true)
			}
			params.ResponseFormat = sdk.ChatCompletionNewParamsResponseFormatUnion{OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{JSONSchema: js}}
		} else {
			params.ResponseFormat = sdk.ChatCompletionNewParamsResponseFormatUnion{OfJSONObject: &shared.ResponseFormatJSONObjectParam{}}
		}
	}
	if req.System != "" {
		params.Messages = append(params.Messages, sdk.SystemMessage(req.System))
	}
	for _, m := range req.Messages {
		msgs, err := buildMessage(m)
		if err != nil {
			return params, err
		}
		params.Messages = append(params.Messages, msgs...)
	}
	for _, t := range req.Tools {
		fn := shared.FunctionDefinitionParam{Name: t.Name}
		if t.Description != "" {
			fn.Description = param.NewOpt(t.Description)
		}
		if len(t.InputSchema) > 0 {
			var schema map[string]any
			if err := json.Unmarshal(t.InputSchema, &schema); err != nil {
				return params, fmt.Errorf("openaicompat: tool %s schema: %w", t.Name, err)
			}
			fn.Parameters = shared.FunctionParameters(schema)
		}
		params.Tools = append(params.Tools, sdk.ChatCompletionFunctionTool(fn))
	}
	if req.ToolChoice != nil && len(req.Tools) > 0 {
		switch req.ToolChoice.Mode {
		case gateway.ToolChoiceNone:
			params.ToolChoice = sdk.ChatCompletionToolChoiceOptionUnionParam{OfAuto: param.NewOpt("none")}
		case gateway.ToolChoiceRequired:
			params.ToolChoice = sdk.ChatCompletionToolChoiceOptionUnionParam{OfAuto: param.NewOpt("required")}
		case gateway.ToolChoiceNamed:
			params.ToolChoice = sdk.ToolChoiceOptionFunctionToolChoice(sdk.ChatCompletionNamedToolChoiceFunctionParam{Name: req.ToolChoice.Name})
		default:
			params.ToolChoice = sdk.ChatCompletionToolChoiceOptionUnionParam{OfAuto: param.NewOpt("auto")}
		}
	}
	return params, nil
}

// buildMessage converts one canonical message into one or more OpenAI
// messages. Tool results each become their own "tool" role message.
func buildMessage(m gateway.Message) ([]sdk.ChatCompletionMessageParamUnion, error) {
	switch m.Role {
	case gateway.RoleSystem:
		return []sdk.ChatCompletionMessageParamUnion{sdk.SystemMessage(textOf(m.Parts))}, nil

	case gateway.RoleUser, gateway.RoleTool:
		var out []sdk.ChatCompletionMessageParamUnion
		var parts []sdk.ChatCompletionContentPartUnionParam
		flush := func() {
			if len(parts) > 0 {
				out = append(out, sdk.UserMessage(parts))
				parts = nil
			}
		}
		for _, p := range m.Parts {
			switch p.Kind {
			case gateway.PartText:
				if p.Text != "" {
					parts = append(parts, sdk.TextContentPart(p.Text))
				}
			case gateway.PartImage:
				url := p.URL
				if url == "" {
					if len(p.Data) == 0 {
						return nil, errors.New("openaicompat: image part has no data or url")
					}
					url = "data:" + p.MIME + ";base64," + base64.StdEncoding.EncodeToString(p.Data)
				}
				parts = append(parts, sdk.ImageContentPart(sdk.ChatCompletionContentPartImageImageURLParam{URL: url}))
			case gateway.PartFile:
				// Most OpenAI-compatible servers can't take files; inline as text when possible.
				if strings.HasPrefix(p.MIME, "text/") || p.MIME == "application/json" {
					parts = append(parts, sdk.TextContentPart(fmt.Sprintf("<file name=%q>\n%s\n</file>", p.Name, string(p.Data))))
				} else {
					parts = append(parts, sdk.TextContentPart(fmt.Sprintf("[file %s (%s) omitted: unsupported by this model]", p.Name, p.MIME)))
				}
			case gateway.PartToolResult:
				flush()
				var sb strings.Builder
				for _, c := range p.Content {
					if c.Kind == gateway.PartText {
						sb.WriteString(c.Text)
					} else if c.Kind == gateway.PartImage {
						sb.WriteString("[image omitted]")
					}
				}
				content := sb.String()
				if p.IsError && !strings.HasPrefix(content, "Error") {
					content = "Error: " + content
				}
				out = append(out, sdk.ToolMessage(content, p.ToolCallID))
			}
		}
		flush()
		return out, nil

	case gateway.RoleAssistant:
		msg := sdk.ChatCompletionAssistantMessageParam{}
		var text strings.Builder
		for _, p := range m.Parts {
			switch p.Kind {
			case gateway.PartText:
				text.WriteString(p.Text)
			case gateway.PartToolCall:
				args := string(p.Args)
				if args == "" {
					args = "{}"
				}
				msg.ToolCalls = append(msg.ToolCalls, sdk.ChatCompletionMessageToolCallUnionParam{
					OfFunction: &sdk.ChatCompletionMessageFunctionToolCallParam{
						ID: p.ToolCallID,
						Function: sdk.ChatCompletionMessageFunctionToolCallFunctionParam{
							Name:      p.ToolName,
							Arguments: args,
						},
					},
				})
			case gateway.PartReasoning:
				// Not replayed; OpenAI-compatible servers don't accept it.
			}
		}
		if text.Len() > 0 || len(msg.ToolCalls) == 0 {
			msg.Content = sdk.ChatCompletionAssistantMessageParamContentUnion{OfString: param.NewOpt(text.String())}
		}
		return []sdk.ChatCompletionMessageParamUnion{{OfAssistant: &msg}}, nil
	}
	return nil, fmt.Errorf("openaicompat: unknown role %q", m.Role)
}

func textOf(parts []gateway.Part) string {
	var sb strings.Builder
	for _, p := range parts {
		if p.Kind == gateway.PartText {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

func mapFinish(s string) gateway.FinishReason {
	switch s {
	case "stop":
		return gateway.FinishStop
	case "length":
		return gateway.FinishLength
	case "tool_calls", "function_call":
		return gateway.FinishToolCalls
	case "content_filter":
		return gateway.FinishFilter
	}
	return gateway.FinishStop
}

// extraString pulls a string out of unknown JSON fields on the delta.
func extraString(fields map[string]respjson.Field, keys ...string) string {
	for _, k := range keys {
		if f, ok := fields[k]; ok {
			var s string
			if json.Unmarshal([]byte(f.Raw()), &s) == nil {
				return s
			}
		}
	}
	return ""
}

func normalizeErr(err error) error {
	var apiErr *sdk.Error
	if errors.As(err, &apiErr) {
		return fmt.Errorf("openaicompat: HTTP %d: %s", apiErr.StatusCode, apiErr.Message)
	}
	return fmt.Errorf("openaicompat: %w", err)
}

func isRetryableErr(err error) bool {
	var apiErr *sdk.Error
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case 408, 409, 429, 500, 502, 503, 504:
			return true
		}
		return false
	}
	return true
}
