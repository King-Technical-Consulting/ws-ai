// Package gateway defines the provider-neutral request, message, and stream
// types that every other package speaks. Adapters translate these to and
// from Anthropic Messages and OpenAI-compatible Chat Completions. Nothing
// above the adapter layer may import a provider SDK.
//
// All types are JSON-serializable so agent checkpoints can store them and
// a run can resume on a different model or provider.
package gateway

import (
	"context"
	"encoding/json"
	"time"
)

// Role is a message author.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// PartKind discriminates the Part union.
type PartKind string

const (
	PartText       PartKind = "text"
	PartImage      PartKind = "image"
	PartFile       PartKind = "file" // PDF or other document
	PartToolCall   PartKind = "tool_call"
	PartToolResult PartKind = "tool_result"
	PartReasoning  PartKind = "reasoning"
)

// Part is one block of a message. It is a flat tagged union: Kind decides
// which fields are meaningful. Flat beats interfaces for JSON round-trips.
type Part struct {
	Kind PartKind `json:"kind"`

	// Text, Reasoning.
	Text string `json:"text,omitempty"`

	// Image, File. Exactly one of Data, URL, or BlobKey is set.
	MIME    string `json:"mime,omitempty"`
	Data    []byte `json:"data,omitempty"` // base64 in JSON
	URL     string `json:"url,omitempty"`
	BlobKey string `json:"blob_key,omitempty"` // internal blob store reference
	Name    string `json:"name,omitempty"`     // file name for documents

	// ToolCall. Args is the raw JSON object the model produced.
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolName   string          `json:"tool_name,omitempty"`
	Args       json.RawMessage `json:"args,omitempty"`

	// ToolResult. Content is usually one Text part, may include images.
	Content []Part `json:"content,omitempty"`
	IsError bool   `json:"is_error,omitempty"`

	// Reasoning. Signature is provider-opaque (Anthropic thinking signature)
	// and must be echoed back on the next turn when present.
	Signature string `json:"signature,omitempty"`

	// CacheHint marks this part as the end of a stable prefix. Adapters that
	// support prompt caching place a cache breakpoint here.
	CacheHint bool `json:"cache_hint,omitempty"`
}

// TextPart is a convenience constructor.
func TextPart(s string) Part { return Part{Kind: PartText, Text: s} }

// Message is one turn. Seq is the persisted message sequence when the
// message came from a conversation; middleware (compaction) uses it.
type Message struct {
	Role  Role   `json:"role"`
	Parts []Part `json:"parts"`
	Seq   int64  `json:"seq,omitempty"`
}

// ToolDef describes a tool the model may call.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"` // JSON Schema object
}

// ToolChoiceMode controls whether the model must call a tool.
type ToolChoiceMode string

const (
	ToolChoiceAuto     ToolChoiceMode = "auto"
	ToolChoiceNone     ToolChoiceMode = "none"
	ToolChoiceRequired ToolChoiceMode = "required"
	ToolChoiceNamed    ToolChoiceMode = "named"
)

// ToolChoice is the tool selection constraint.
type ToolChoice struct {
	Mode ToolChoiceMode `json:"mode"`
	Name string         `json:"name,omitempty"` // for ToolChoiceNamed
}

// ReasoningConfig enables extended thinking / reasoning where supported.
type ReasoningConfig struct {
	Enabled      bool   `json:"enabled"`
	BudgetTokens int    `json:"budget_tokens,omitempty"` // Anthropic
	Effort       string `json:"effort,omitempty"`        // OpenAI: low|medium|high
}

// JSONFormat is the shape of a JSON reply. Without a Schema it is plain
// JSON mode (response_format json_object); with one it is a structured
// output (json_schema), strict when Strict is set and the server honours it.
type JSONFormat struct {
	// Name labels the schema (a-z, A-Z, 0-9, _ and -; required with a schema).
	Name string `json:"name,omitempty"`
	// Schema is a JSON Schema object.
	Schema json.RawMessage `json:"schema,omitempty"`
	Strict bool            `json:"strict,omitempty"`
}

// TaskClass is what the request is for. The router uses it to pick a model.
type TaskClass string

const (
	TaskChat      TaskClass = "chat"
	TaskCode      TaskClass = "code"
	TaskSummarize TaskClass = "summarize"
	TaskTitle     TaskClass = "title"
	TaskEmbed     TaskClass = "embed"
	TaskVision    TaskClass = "vision"
	TaskClassify  TaskClass = "classify"
	TaskReflect   TaskClass = "reflect" // agent memory extraction
	// Media task classes route to endpoints with Capabilities.Media
	// (internal/media), never to a text model.
	TaskImage TaskClass = "image"
	TaskVideo TaskClass = "video"
	// TaskRental marks the per-hour ledger rows of a rented GPU
	// (internal/fleet/rental); it never routes.
	TaskRental TaskClass = "rental"
)

// Metadata travels with a request for routing, budgets, and the ledger.
type Metadata struct {
	UserID         string `json:"user_id,omitempty"`
	ConversationID string `json:"conversation_id,omitempty"`
	AgentID        string `json:"agent_id,omitempty"`
	AgentRunID     string `json:"agent_run_id,omitempty"`
	APIKeyID       string `json:"api_key_id,omitempty"`
	// SessionID is the client's own session id (Claude Code sends
	// x-claude-code-session-id) so one coding session's spend can be
	// grouped in the ledger.
	SessionID string    `json:"session_id,omitempty"`
	TaskClass TaskClass `json:"task_class,omitempty"`
	// External is true when the request came through /v1 from an outside
	// client that owns its own context; compaction is skipped.
	External bool `json:"external,omitempty"`
}

// Request is a canonical chat completion request.
type Request struct {
	// Model is a selector: an endpoint ID, "provider/model", or a policy
	// alias such as "auto", "code", "cheap". The router resolves it.
	Model string `json:"model"`

	System   string    `json:"system,omitempty"`
	Messages []Message `json:"messages"`

	Tools      []ToolDef   `json:"tools,omitempty"`
	ToolChoice *ToolChoice `json:"tool_choice,omitempty"`

	MaxTokens   int              `json:"max_tokens,omitempty"`
	Temperature *float64         `json:"temperature,omitempty"`
	TopP        *float64         `json:"top_p,omitempty"`
	Stop        []string         `json:"stop,omitempty"`
	Reasoning   *ReasoningConfig `json:"reasoning,omitempty"`

	// JSON asks for a JSON reply (OpenAI-style response_format). A hint,
	// not a routing requirement: an adapter sends it only when the
	// endpoint declares json_mode, and callers still parse defensively.
	JSON *JSONFormat `json:"json,omitempty"`

	// SystemCache asks adapters to put a cache breakpoint after the system
	// prompt. Message-level breakpoints use Part.CacheHint.
	SystemCache bool `json:"system_cache,omitempty"`

	// Media, on a media task class, says what the job needs of a media
	// endpoint (an edit needs image_edit, image to video needs
	// image_to_video); nil means the task class's default (image, video).
	Media *MediaCaps `json:"media,omitempty"`

	Metadata Metadata `json:"metadata"`
}

// Usage is token accounting from the provider.
type Usage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
}

// Add accumulates usage.
func (u *Usage) Add(o Usage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.CacheReadTokens += o.CacheReadTokens
	u.CacheWriteTokens += o.CacheWriteTokens
}

// Pricing is USD per million tokens. Zero for local models. Media
// endpoints price per output instead: PerImage for image models and
// PerSecond of output for video models (a media engine that also reports
// token usage, as OpenAI's image models do, is priced by tokens when the
// token rates are set, which follows quality and size exactly).
type Pricing struct {
	InputPerM      float64 `json:"input_per_m" yaml:"input_per_m"`
	OutputPerM     float64 `json:"output_per_m" yaml:"output_per_m"`
	CacheReadPerM  float64 `json:"cache_read_per_m,omitempty" yaml:"cache_read_per_m"`
	CacheWritePerM float64 `json:"cache_write_per_m,omitempty" yaml:"cache_write_per_m"`
	PerImage       float64 `json:"per_image,omitempty" yaml:"per_image"`
	PerSecond      float64 `json:"per_second,omitempty" yaml:"per_second"`
}

// MediaCost prices a media job: images at PerImage, seconds of video at
// PerSecond.
func (p Pricing) MediaCost(images int, seconds float64) float64 {
	return float64(images)*p.PerImage + seconds*p.PerSecond
}

// Cost returns the USD cost of a usage record under this pricing.
func (p Pricing) Cost(u Usage) float64 {
	const m = 1_000_000.0
	// Anthropic reports cache tokens separately from input tokens; OpenAI
	// folds cached tokens into input. Adapters normalize to "InputTokens
	// excludes cache reads/writes" so this formula holds for both.
	return float64(u.InputTokens)/m*p.InputPerM +
		float64(u.OutputTokens)/m*p.OutputPerM +
		float64(u.CacheReadTokens)/m*p.CacheReadPerM +
		float64(u.CacheWriteTokens)/m*p.CacheWritePerM
}

// FinishReason is why generation stopped.
type FinishReason string

const (
	FinishStop      FinishReason = "stop"
	FinishLength    FinishReason = "length"
	FinishToolCalls FinishReason = "tool_calls"
	FinishFilter    FinishReason = "content_filter"
	FinishError     FinishReason = "error"
)

// EventType discriminates StreamEvent.
type EventType string

const (
	EventStart          EventType = "start"
	EventTextDelta      EventType = "text_delta"
	EventReasoningDelta EventType = "reasoning_delta"
	EventReasoningSig   EventType = "reasoning_signature"
	EventToolCallStart  EventType = "tool_call_start"
	EventToolCallDelta  EventType = "tool_call_delta"
	EventToolCallEnd    EventType = "tool_call_end"
	EventUsage          EventType = "usage"
	EventFinish         EventType = "finish"
	EventError          EventType = "error"
)

// StreamEvent is one increment of a streamed response.
type StreamEvent struct {
	Type EventType `json:"type"`

	// Start.
	EndpointID string `json:"endpoint_id,omitempty"`
	Model      string `json:"model,omitempty"`

	// TextDelta, ReasoningDelta, ReasoningSig (Text holds the signature).
	Text string `json:"text,omitempty"`

	// ToolCall*. Delta carries a fragment of the JSON arguments.
	ToolCallID string `json:"tool_call_id,omitempty"`
	ToolName   string `json:"tool_name,omitempty"`
	ArgsDelta  string `json:"args_delta,omitempty"`

	// Usage. May arrive more than once; consumers should Add.
	Usage *Usage `json:"usage,omitempty"`

	// Finish.
	FinishReason FinishReason `json:"finish_reason,omitempty"`

	// Error. Retryable means no tokens were emitted yet and the router may
	// fail over to the next candidate.
	Err       error  `json:"-"`
	ErrText   string `json:"error,omitempty"`
	Retryable bool   `json:"retryable,omitempty"`
}

// ErrorEvent builds an error event.
func ErrorEvent(err error, retryable bool) StreamEvent {
	return StreamEvent{Type: EventError, Err: err, ErrText: err.Error(), Retryable: retryable}
}

// Response is a fully accumulated reply.
type Response struct {
	EndpointID   string        `json:"endpoint_id"`
	Model        string        `json:"model"`
	Parts        []Part        `json:"parts"`
	Usage        Usage         `json:"usage"`
	FinishReason FinishReason  `json:"finish_reason"`
	Latency      time.Duration `json:"latency_ns"`
}

// ToolCalls returns the tool call parts of the response.
func (r *Response) ToolCalls() []Part {
	var out []Part
	for _, p := range r.Parts {
		if p.Kind == PartToolCall {
			out = append(out, p)
		}
	}
	return out
}

// Text concatenates text parts.
func (r *Response) Text() string {
	var s string
	for _, p := range r.Parts {
		if p.Kind == PartText {
			s += p.Text
		}
	}
	return s
}

// Message converts the response into an assistant message for the next turn.
func (r *Response) Message() Message {
	return Message{Role: RoleAssistant, Parts: r.Parts}
}

// Accumulate drains a stream into a Response. It returns the first error
// event as an error, along with whatever was accumulated.
func Accumulate(ch <-chan StreamEvent) (*Response, error) {
	start := time.Now()
	resp := &Response{}
	var text, reasoning, sig string
	type tc struct {
		id, name string
		args     []byte
	}
	var calls []*tc
	byID := map[string]*tc{}

	flushText := func() {
		if text != "" {
			resp.Parts = append(resp.Parts, Part{Kind: PartText, Text: text})
			text = ""
		}
	}
	flushReasoning := func() {
		if reasoning != "" || sig != "" {
			resp.Parts = append(resp.Parts, Part{Kind: PartReasoning, Text: reasoning, Signature: sig})
			reasoning, sig = "", ""
		}
	}

	var firstErr error
	for ev := range ch {
		switch ev.Type {
		case EventStart:
			resp.EndpointID, resp.Model = ev.EndpointID, ev.Model
		case EventTextDelta:
			flushReasoning()
			text += ev.Text
		case EventReasoningDelta:
			flushText()
			reasoning += ev.Text
		case EventReasoningSig:
			sig = ev.Text
		case EventToolCallStart:
			flushText()
			flushReasoning()
			c := &tc{id: ev.ToolCallID, name: ev.ToolName}
			calls = append(calls, c)
			byID[c.id] = c
		case EventToolCallDelta:
			if c, ok := byID[ev.ToolCallID]; ok {
				c.args = append(c.args, ev.ArgsDelta...)
			}
		case EventToolCallEnd:
			// nothing; args are complete
		case EventUsage:
			if ev.Usage != nil {
				resp.Usage.Add(*ev.Usage)
			}
		case EventFinish:
			resp.FinishReason = ev.FinishReason
		case EventError:
			if firstErr == nil {
				firstErr = ev.Err
			}
		}
	}
	flushReasoning()
	flushText()
	for _, c := range calls {
		args := c.args
		if len(args) == 0 {
			args = []byte("{}")
		}
		resp.Parts = append(resp.Parts, Part{Kind: PartToolCall, ToolCallID: c.id, ToolName: c.name, Args: json.RawMessage(args)})
	}
	if resp.FinishReason == "" && len(calls) > 0 {
		resp.FinishReason = FinishToolCalls
	}
	resp.Latency = time.Since(start)
	return resp, firstErr
}

// Capabilities describe what an endpoint can do. Engines rarely report these
// so they live in config.
type Capabilities struct {
	ContextWindow int  `json:"context_window" yaml:"context_window"`
	MaxOutput     int  `json:"max_output" yaml:"max_output"`
	Tools         bool `json:"tools" yaml:"tools"`
	Vision        bool `json:"vision" yaml:"vision"`
	JSONMode      bool `json:"json_mode" yaml:"json_mode"`
	Reasoning     bool `json:"reasoning" yaml:"reasoning"`
	PromptCache   bool `json:"prompt_cache" yaml:"prompt_cache"`
	Embeddings    bool `json:"embeddings" yaml:"embeddings"`
	// Media marks an image or video endpoint (internal/media). Text
	// requests never route to one and media requests only route to one.
	Media *MediaCaps `json:"media,omitempty" yaml:"media"`
}

// MediaCaps describes a media endpoint: which engine drives it and what it
// can make. Declared in config like the rest; engines rarely report it.
type MediaCaps struct {
	// Engine names the adapter in internal/media: openai_images,
	// openai_videos, fal, comfyui or google.
	Engine       string `json:"engine" yaml:"engine"`
	Image        bool   `json:"image,omitempty" yaml:"image"`                   // text to image
	ImageEdit    bool   `json:"image_edit,omitempty" yaml:"image_edit"`         // image plus prompt (and mask) to image
	Video        bool   `json:"video,omitempty" yaml:"video"`                   // text to video
	ImageToVideo bool   `json:"image_to_video,omitempty" yaml:"image_to_video"` // image plus prompt to video
	Upscale      bool   `json:"upscale,omitempty" yaml:"upscale"`               // image to a larger image
	// Sizes lists the accepted "WxH" (or engine keywords such as "auto");
	// empty means the engine's default only.
	Sizes []string `json:"sizes,omitempty" yaml:"sizes"`
	// MaxImages caps the images per job; 0 means 1.
	MaxImages int `json:"max_images,omitempty" yaml:"max_images"`
	// MaxSeconds caps a video job's length; 0 means the engine's default.
	MaxSeconds int `json:"max_seconds,omitempty" yaml:"max_seconds"`
	// Seconds lists the video lengths the engine accepts (Sora takes 4, 8
	// or 12); empty means any length up to MaxSeconds. The first is the
	// default.
	Seconds []int `json:"seconds,omitempty" yaml:"seconds"`
}

// IsMedia reports whether the endpoint is an image or video endpoint.
func (c Capabilities) IsMedia() bool { return c.Media != nil }

// ProviderKind selects the adapter.
type ProviderKind string

const (
	ProviderAnthropic    ProviderKind = "anthropic"
	ProviderOpenAICompat ProviderKind = "openai_compat"
)

// Provider is an API surface: a base URL plus credentials.
type Provider struct {
	ID      string            `json:"id"`
	Kind    ProviderKind      `json:"kind"`
	Name    string            `json:"name"`
	BaseURL string            `json:"base_url"`
	APIKey  string            `json:"-"`
	Headers map[string]string `json:"headers,omitempty"`
}

// Endpoint is one model on one provider.
type Endpoint struct {
	ID              string       `json:"id"`
	ProviderID      string       `json:"provider_id"`
	ModelName       string       `json:"model_name"` // what the provider expects
	DisplayName     string       `json:"display_name"`
	Capabilities    Capabilities `json:"capabilities"`
	Pricing         Pricing      `json:"pricing"`
	ThroughputClass string       `json:"throughput_class"` // low | medium | high
	LatencyClass    string       `json:"latency_class"`    // fast | normal | slow
	Enabled         bool         `json:"enabled"`
	// Local marks on-prem endpoints; the router treats them as free.
	Local bool `json:"local"`
	// ExtraBody is merged into every request body sent to this endpoint
	// (OpenAI-compatible adapters only). It is how one model gets several
	// routes: an OpenRouter endpoint pinned to one upstream provider carries
	// {"provider": {"order": ["cerebras"], "allow_fallbacks": false}}.
	ExtraBody map[string]any `json:"extra_body,omitempty"`
	// Health is runtime state maintained by the health checker.
	Health Health `json:"health"`
}

// Route returns a short label for the upstream route pinned in ExtraBody
// ("via cerebras"), or "" when the provider picks.
func (e *Endpoint) Route() string {
	prov, ok := e.ExtraBody["provider"].(map[string]any)
	if !ok {
		return ""
	}
	if order, ok := prov["order"].([]any); ok && len(order) > 0 {
		if s, ok := order[0].(string); ok {
			return s
		}
	}
	if only, ok := prov["only"].([]any); ok && len(only) > 0 {
		if s, ok := only[0].(string); ok {
			return s
		}
	}
	return ""
}

// Adapter speaks one provider protocol.
type Adapter interface {
	// Stream starts a completion and returns events. The channel is closed
	// after EventFinish or EventError. Stream must not block on the caller
	// beyond the channel.
	Stream(ctx context.Context, p *Provider, ep *Endpoint, req *Request) (<-chan StreamEvent, error)
	// CountTokens estimates input tokens. Adapters may call a remote
	// counting API or return an estimate; the bool reports which.
	CountTokens(ctx context.Context, p *Provider, ep *Endpoint, req *Request) (n int, exact bool, err error)
}
