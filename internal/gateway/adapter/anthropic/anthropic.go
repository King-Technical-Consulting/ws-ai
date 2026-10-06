// Package anthropic adapts the canonical gateway types to the Anthropic
// Messages API using the official SDK. It is the only package allowed to
// import anthropic-sdk-go.
package anthropic

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"

	"github.com/jking323/ws/internal/gateway"
)

// Adapter implements gateway.Adapter for Anthropic.
type Adapter struct {
	// HTTPClient is optional; nil uses the SDK default.
	HTTPClient *http.Client
	// Timeout per request; the SDK default is 10 minutes.
	Timeout time.Duration
}

// New returns an adapter with defaults.
func New() *Adapter { return &Adapter{Timeout: 10 * time.Minute} }

func (a *Adapter) client(p *gateway.Provider) sdk.Client {
	opts := []option.RequestOption{option.WithMaxRetries(0)} // the router owns retries
	if p.APIKey != "" {
		opts = append(opts, option.WithAPIKey(p.APIKey))
	}
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

	go func() {
		defer close(out)
		stream := client.Messages.NewStreaming(ctx, params)
		defer stream.Close()

		started := false
		emitted := false // any content token sent; after this, errors are not retryable
		// index -> tool call id, so deltas can be attributed
		toolByIndex := map[int64]string{}
		var usage gateway.Usage
		var finish gateway.FinishReason

		send := func(ev gateway.StreamEvent) bool {
			select {
			case out <- ev:
				return true
			case <-ctx.Done():
				return false
			}
		}

		for stream.Next() {
			ev := stream.Current()
			switch ev.Type {
			case "message_start":
				started = true
				u := ev.Message.Usage
				usage = gateway.Usage{
					InputTokens:      int(u.InputTokens),
					OutputTokens:     int(u.OutputTokens),
					CacheReadTokens:  int(u.CacheReadInputTokens),
					CacheWriteTokens: int(u.CacheCreationInputTokens),
				}
				if !send(gateway.StreamEvent{Type: gateway.EventStart, EndpointID: ep.ID, Model: ev.Message.Model}) {
					return
				}
			case "content_block_start":
				cb := ev.ContentBlock
				switch cb.Type {
				case "tool_use":
					toolByIndex[ev.Index] = cb.ID
					emitted = true
					if !send(gateway.StreamEvent{Type: gateway.EventToolCallStart, ToolCallID: cb.ID, ToolName: cb.Name}) {
						return
					}
				case "text":
					if cb.Text != "" {
						emitted = true
						if !send(gateway.StreamEvent{Type: gateway.EventTextDelta, Text: cb.Text}) {
							return
						}
					}
				case "thinking":
					if cb.Thinking != "" {
						if !send(gateway.StreamEvent{Type: gateway.EventReasoningDelta, Text: cb.Thinking}) {
							return
						}
					}
				}
			case "content_block_delta":
				d := ev.Delta
				switch d.Type {
				case "text_delta":
					emitted = true
					if !send(gateway.StreamEvent{Type: gateway.EventTextDelta, Text: d.Text}) {
						return
					}
				case "input_json_delta":
					id := toolByIndex[ev.Index]
					if !send(gateway.StreamEvent{Type: gateway.EventToolCallDelta, ToolCallID: id, ArgsDelta: d.PartialJSON}) {
						return
					}
				case "thinking_delta":
					if !send(gateway.StreamEvent{Type: gateway.EventReasoningDelta, Text: d.Thinking}) {
						return
					}
				case "signature_delta":
					if !send(gateway.StreamEvent{Type: gateway.EventReasoningSig, Text: d.Signature}) {
						return
					}
				}
			case "content_block_stop":
				if id, ok := toolByIndex[ev.Index]; ok {
					if !send(gateway.StreamEvent{Type: gateway.EventToolCallEnd, ToolCallID: id}) {
						return
					}
				}
			case "message_delta":
				// Output tokens arrive here cumulatively; input tokens were in message_start.
				usage.OutputTokens = int(ev.Usage.OutputTokens)
				if ev.Usage.InputTokens > 0 {
					usage.InputTokens = int(ev.Usage.InputTokens)
				}
				if ev.Usage.CacheReadInputTokens > 0 {
					usage.CacheReadTokens = int(ev.Usage.CacheReadInputTokens)
				}
				if ev.Usage.CacheCreationInputTokens > 0 {
					usage.CacheWriteTokens = int(ev.Usage.CacheCreationInputTokens)
				}
				finish = mapStop(string(ev.Delta.StopReason))
			case "message_stop":
				// handled after loop
			}
		}
		if err := stream.Err(); err != nil {
			send(gateway.ErrorEvent(normalizeErr(err), !emitted && !started || isRetryableErr(err) && !emitted))
			return
		}
		if finish == "" {
			finish = gateway.FinishStop
		}
		if !send(gateway.StreamEvent{Type: gateway.EventUsage, Usage: &usage}) {
			return
		}
		send(gateway.StreamEvent{Type: gateway.EventFinish, FinishReason: finish})
	}()
	return out, nil
}

// CountTokens implements gateway.Adapter using the remote counting endpoint.
func (a *Adapter) CountTokens(ctx context.Context, p *gateway.Provider, ep *gateway.Endpoint, req *gateway.Request) (int, bool, error) {
	params, err := buildParams(ep, req)
	if err != nil {
		return 0, false, err
	}
	cp := sdk.MessageCountTokensParams{
		Model:    params.Model,
		Messages: params.Messages,
	}
	if len(params.System) > 0 {
		cp.System = sdk.MessageCountTokensParamsSystemUnion{OfTextBlockArray: params.System}
	}
	for _, t := range params.Tools {
		if t.OfTool != nil {
			cp.Tools = append(cp.Tools, sdk.MessageCountTokensToolUnionParam{OfTool: t.OfTool})
		}
	}
	client := a.client(p)
	res, err := client.Messages.CountTokens(ctx, cp)
	if err != nil {
		return 0, false, normalizeErr(err)
	}
	return int(res.InputTokens), true, nil
}

// buildParams converts a canonical request into SDK params.
func buildParams(ep *gateway.Endpoint, req *gateway.Request) (sdk.MessageNewParams, error) {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = ep.Capabilities.MaxOutput
	}
	if maxTokens <= 0 {
		maxTokens = 16000
	}
	params := sdk.MessageNewParams{
		Model:     sdk.Model(ep.ModelName),
		MaxTokens: int64(maxTokens),
	}
	if req.System != "" {
		blk := sdk.TextBlockParam{Text: req.System}
		if req.SystemCache {
			blk.CacheControl = sdk.NewCacheControlEphemeralParam()
		}
		params.System = []sdk.TextBlockParam{blk}
	}
	if req.Temperature != nil {
		params.Temperature = param.NewOpt(*req.Temperature)
	}
	if req.TopP != nil {
		params.TopP = param.NewOpt(*req.TopP)
	}
	if len(req.Stop) > 0 {
		params.StopSequences = req.Stop
	}
	if req.Reasoning != nil && req.Reasoning.Enabled && ep.Capabilities.Reasoning {
		// Adaptive is the only forward-compatible mode on current models.
		adaptive := sdk.ThinkingConfigAdaptiveParam{}
		params.Thinking = sdk.ThinkingConfigParamUnion{OfAdaptive: &adaptive}
		if req.Reasoning.Effort != "" {
			params.OutputConfig = sdk.OutputConfigParam{Effort: sdk.OutputConfigEffort(req.Reasoning.Effort)}
		}
	}
	for _, t := range req.Tools {
		var schema sdk.ToolInputSchemaParam
		if len(t.InputSchema) > 0 {
			if err := json.Unmarshal(t.InputSchema, &schema); err != nil {
				return params, fmt.Errorf("anthropic: tool %s schema: %w", t.Name, err)
			}
		}
		tp := &sdk.ToolParam{Name: t.Name, InputSchema: schema}
		if t.Description != "" {
			tp.Description = param.NewOpt(t.Description)
		}
		params.Tools = append(params.Tools, sdk.ToolUnionParam{OfTool: tp})
	}
	if req.ToolChoice != nil && len(req.Tools) > 0 {
		switch req.ToolChoice.Mode {
		case gateway.ToolChoiceNone:
			params.ToolChoice = sdk.ToolChoiceUnionParam{OfNone: &sdk.ToolChoiceNoneParam{}}
		case gateway.ToolChoiceRequired:
			params.ToolChoice = sdk.ToolChoiceUnionParam{OfAny: &sdk.ToolChoiceAnyParam{}}
		case gateway.ToolChoiceNamed:
			params.ToolChoice = sdk.ToolChoiceParamOfTool(req.ToolChoice.Name)
		default:
			params.ToolChoice = sdk.ToolChoiceUnionParam{OfAuto: &sdk.ToolChoiceAutoParam{}}
		}
	}
	msgs, err := buildMessages(req.Messages)
	if err != nil {
		return params, err
	}
	params.Messages = msgs
	return params, nil
}

// buildMessages converts canonical messages. Tool results become user
// messages (Anthropic has no "tool" role); consecutive same-role messages
// are merged because the API requires alternation.
func buildMessages(in []gateway.Message) ([]sdk.MessageParam, error) {
	var out []sdk.MessageParam
	appendBlocks := func(role gateway.Role, blocks []sdk.ContentBlockParamUnion) {
		if len(blocks) == 0 {
			return
		}
		r := sdk.MessageParamRoleUser
		if role == gateway.RoleAssistant {
			r = sdk.MessageParamRoleAssistant
		}
		if n := len(out); n > 0 && out[n-1].Role == r {
			out[n-1].Content = append(out[n-1].Content, blocks...)
			return
		}
		out = append(out, sdk.MessageParam{Role: r, Content: blocks})
	}
	for _, m := range in {
		if m.Role == gateway.RoleSystem {
			// System text inside messages gets folded into a user turn; the
			// router should have moved it to req.System already.
			var sb strings.Builder
			for _, p := range m.Parts {
				if p.Kind == gateway.PartText {
					sb.WriteString(p.Text)
				}
			}
			if sb.Len() > 0 {
				appendBlocks(gateway.RoleUser, []sdk.ContentBlockParamUnion{sdk.NewTextBlock(sb.String())})
			}
			continue
		}
		role := m.Role
		if role == gateway.RoleTool {
			role = gateway.RoleUser
		}
		var blocks []sdk.ContentBlockParamUnion
		for _, p := range m.Parts {
			b, err := buildBlock(p)
			if err != nil {
				return nil, err
			}
			if b != nil {
				blocks = append(blocks, *b)
			}
		}
		appendBlocks(role, blocks)
	}
	return out, nil
}

func buildBlock(p gateway.Part) (*sdk.ContentBlockParamUnion, error) {
	switch p.Kind {
	case gateway.PartText:
		if p.Text == "" {
			return nil, nil
		}
		b := sdk.NewTextBlock(p.Text)
		if p.CacheHint && b.OfText != nil {
			b.OfText.CacheControl = sdk.NewCacheControlEphemeralParam()
		}
		return &b, nil
	case gateway.PartImage:
		if p.URL != "" {
			b := sdk.NewImageBlock(sdk.URLImageSourceParam{URL: p.URL})
			return &b, nil
		}
		if len(p.Data) == 0 {
			return nil, errors.New("anthropic: image part has no data or url (blob refs must be resolved before the adapter)")
		}
		b := sdk.NewImageBlockBase64(p.MIME, base64.StdEncoding.EncodeToString(p.Data))
		return &b, nil
	case gateway.PartFile:
		if p.MIME != "application/pdf" {
			// Non-PDF documents are passed as plain text if decodable.
			b := sdk.NewDocumentBlock(sdk.PlainTextSourceParam{Data: string(p.Data)})
			return &b, nil
		}
		if p.URL != "" {
			b := sdk.NewDocumentBlock(sdk.URLPDFSourceParam{URL: p.URL})
			return &b, nil
		}
		b := sdk.NewDocumentBlock(sdk.Base64PDFSourceParam{Data: base64.StdEncoding.EncodeToString(p.Data)})
		return &b, nil
	case gateway.PartToolCall:
		var input any = map[string]any{}
		if len(p.Args) > 0 {
			if err := json.Unmarshal(p.Args, &input); err != nil {
				return nil, fmt.Errorf("anthropic: tool call %s args: %w", p.ToolCallID, err)
			}
		}
		b := sdk.NewToolUseBlock(p.ToolCallID, input, p.ToolName)
		return &b, nil
	case gateway.PartToolResult:
		// Join text content; images inside tool results are rare and are
		// described rather than embedded for now.
		var sb strings.Builder
		for _, c := range p.Content {
			switch c.Kind {
			case gateway.PartText:
				sb.WriteString(c.Text)
			case gateway.PartImage:
				sb.WriteString("[image omitted]")
			}
		}
		b := sdk.NewToolResultBlock(p.ToolCallID, sb.String(), p.IsError)
		return &b, nil
	case gateway.PartReasoning:
		// Thinking blocks must be echoed back with their signature on the
		// same model. Without a signature the block is dropped.
		if p.Signature == "" {
			return nil, nil
		}
		b := sdk.ContentBlockParamUnion{OfThinking: &sdk.ThinkingBlockParam{Thinking: p.Text, Signature: p.Signature}}
		return &b, nil
	}
	return nil, nil
}

func mapStop(s string) gateway.FinishReason {
	switch s {
	case "end_turn", "stop_sequence", "pause_turn":
		return gateway.FinishStop
	case "max_tokens":
		return gateway.FinishLength
	case "tool_use":
		return gateway.FinishToolCalls
	case "refusal":
		return gateway.FinishFilter
	case "":
		return ""
	}
	return gateway.FinishStop
}

// normalizeErr unwraps SDK errors into something loggable with a status code.
func normalizeErr(err error) error {
	var apiErr *sdk.Error
	if errors.As(err, &apiErr) {
		return fmt.Errorf("anthropic: HTTP %d: %s", apiErr.StatusCode, apiErr.Error())
	}
	return fmt.Errorf("anthropic: %w", err)
}

// isRetryableErr reports whether the router may fail over.
func isRetryableErr(err error) bool {
	var apiErr *sdk.Error
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case 408, 409, 429, 500, 502, 503, 504, 529:
			return true
		}
		return false
	}
	// network errors, timeouts
	return true
}
