package training

import (
	"bytes"
	"encoding/json"
	"hash/fnv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
)

// Filters say which conversations and turns a dataset takes (PLAN M10:
// task class, model, rating threshold, consent). Consent is not a filter:
// only opted-in users' conversations are ever read.
type Filters struct {
	// Modes are conversation modes (chat, code, design, agent); empty is
	// every mode.
	Modes []string `json:"modes,omitempty"`
	// Models keeps assistant turns whose model or endpoint id is listed
	// (a distillation set: the frontier model's answers); empty is any.
	Models []string `json:"models,omitempty"`
	// MinRating is the rating an assistant turn needs: 1 keeps upvoted
	// turns only, 0 (the default) keeps upvoted and unrated turns, -1
	// keeps everything including downvoted.
	MinRating int `json:"min_rating"`
	// Since drops conversations not updated since then; zero is forever.
	Since time.Time `json:"since,omitempty"`
	// HoldoutPct of conversations (by id, so a conversation is never on
	// both sides) go to the eval file; default 10.
	HoldoutPct int `json:"holdout_pct"`
	// MaxExamples caps the train file; default 5000. MaxConversations
	// caps the conversations read; default 2000.
	MaxExamples      int `json:"max_examples"`
	MaxConversations int `json:"max_conversations"`
	// MaxPrefix caps the turns before the answer; default 40.
	MaxPrefix int `json:"max_prefix"`
}

func (f *Filters) defaults() {
	if f.HoldoutPct < 0 || f.HoldoutPct > 50 {
		f.HoldoutPct = 10
	}
	if f.MaxExamples <= 0 {
		f.MaxExamples = 5000
	}
	if f.MaxConversations <= 0 {
		f.MaxConversations = 2000
	}
	if f.MaxPrefix <= 0 {
		f.MaxPrefix = 40
	}
	if f.MinRating < -1 || f.MinRating > 1 {
		f.MinRating = 0
	}
}

// ChatMessage is one line's turn in the chat-format JSONL most trainers
// (TRL, Unsloth, Axolotl, MLX-LM) read: OpenAI's shape with tool calls.
type ChatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// ToolCall is an assistant's call in OpenAI's shape.
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Example is one training line: the turns up to and including one
// assistant answer, plus where it came from.
type Example struct {
	Messages []ChatMessage `json:"messages"`
	Meta     ExampleMeta   `json:"meta"`
}

// ExampleMeta says where an example came from.
type ExampleMeta struct {
	ConversationID string `json:"conversation_id"`
	MessageID      string `json:"message_id"`
	Mode           string `json:"mode,omitempty"`
	Model          string `json:"model,omitempty"`
	Rating         int    `json:"rating,omitempty"`
}

// convert turns stored gateway parts into chat-format messages. A tool
// result row becomes one tool message per result; images and files
// become a short placeholder (a text-only trainer cannot use them).
func convert(role string, parts []gateway.Part) []ChatMessage {
	switch role {
	case "tool":
		var out []ChatMessage
		for _, p := range parts {
			if p.Kind != gateway.PartToolResult {
				continue
			}
			var b strings.Builder
			for _, c := range p.Content {
				if c.Kind == gateway.PartText {
					b.WriteString(c.Text)
				}
			}
			out = append(out, ChatMessage{Role: "tool", ToolCallID: p.ToolCallID, Content: b.String()})
		}
		return out
	default:
		m := ChatMessage{Role: role}
		var text strings.Builder
		for _, p := range parts {
			switch p.Kind {
			case gateway.PartText:
				text.WriteString(p.Text)
			case gateway.PartImage:
				text.WriteString("[image]")
			case gateway.PartFile:
				if p.Name != "" {
					text.WriteString("[file " + p.Name + "]")
				} else {
					text.WriteString("[file]")
				}
			case gateway.PartToolCall:
				var tc ToolCall
				tc.ID, tc.Type = p.ToolCallID, "function"
				tc.Function.Name = p.ToolName
				if len(p.Args) > 0 {
					tc.Function.Arguments = string(p.Args)
				} else {
					tc.Function.Arguments = "{}"
				}
				m.ToolCalls = append(m.ToolCalls, tc)
			}
			// reasoning is dropped: it is the model's scratch, not a target
		}
		m.Content = text.String()
		if m.Content == "" && len(m.ToolCalls) == 0 {
			return nil
		}
		return []ChatMessage{m}
	}
}

// Rating is the best score a message got (any rater; the owner's rating
// wins ties by being the one that is there).
type ratings map[uuid.UUID]int

// Examples builds the examples of one conversation: one per assistant
// turn that passes the filters, with the turns before it as the prompt.
func Examples(conv store.Conversation, msgs []store.Message, rt ratings, f Filters) []Example {
	f.defaults()
	var out []Example
	var prefix []ChatMessage
	// Byte position of each stored message's turns in prefix, so an
	// example can take the last MaxPrefix stored turns.
	var starts []int
	for _, m := range msgs {
		var parts []gateway.Part
		if err := json.Unmarshal(m.Parts, &parts); err != nil || len(parts) == 0 {
			continue
		}
		turns := convert(m.Role, parts)
		if len(turns) == 0 {
			continue
		}
		if m.Role == "assistant" && len(prefix) > 0 {
			model := ""
			if m.Model != nil {
				model = *m.Model
			}
			ep := ""
			if m.EndpointID != nil {
				ep = *m.EndpointID
			}
			score, rated := rt[m.ID]
			keep := true
			switch f.MinRating {
			case 1:
				keep = rated && score >= 1
			case 0:
				keep = !rated || score >= 0
			}
			if keep && len(f.Models) > 0 && !containsFold(f.Models, model) && !containsFold(f.Models, ep) {
				keep = false
			}
			if keep {
				from := 0
				if n := len(starts); n > f.MaxPrefix {
					from = starts[n-f.MaxPrefix]
				}
				ex := Example{Meta: ExampleMeta{ConversationID: conv.ID.String(), MessageID: m.ID.String(), Mode: conv.Mode, Model: model, Rating: score}}
				ex.Messages = append(append([]ChatMessage{}, prefix[from:]...), turns...)
				out = append(out, ex)
			}
		}
		starts = append(starts, len(prefix))
		prefix = append(prefix, turns...)
	}
	return out
}

// Holdout reports whether a conversation belongs to the eval split: a
// stable hash of its id against the percentage, so re-exports keep the
// same split and no conversation sits on both sides.
func Holdout(convID uuid.UUID, pct int) bool {
	if pct <= 0 {
		return false
	}
	h := fnv.New32a()
	h.Write(convID[:])
	return int(h.Sum32()%100) < pct
}

// JSONL encodes examples one per line.
func JSONL(examples []Example) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	for _, e := range examples {
		_ = enc.Encode(e)
	}
	return b.Bytes()
}

// ParseJSONL reads examples back (the eval gate reads the eval file).
func ParseJSONL(b []byte) ([]Example, error) {
	var out []Example
	dec := json.NewDecoder(bytes.NewReader(b))
	for dec.More() {
		var e Example
		if err := dec.Decode(&e); err != nil {
			return out, err
		}
		out = append(out, e)
	}
	return out, nil
}

func containsFold(list []string, s string) bool {
	for _, l := range list {
		if strings.EqualFold(strings.TrimSpace(l), s) {
			return true
		}
	}
	return false
}
