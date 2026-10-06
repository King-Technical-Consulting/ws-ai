package ccrouter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jking323/ws/internal/ccjobs"
	"github.com/jking323/ws/internal/gateway"
)

// Completer is the one gateway call the classifier makes.
// *gateway.Gateway satisfies it.
type Completer interface {
	Complete(ctx context.Context, req *gateway.Request) (*gateway.Response, error)
}

// Classifier asks a small model to classify a prompt the rules found
// ambiguous (spec §6.4 stage 2, M4). The request carries task class
// `classify`, so the policy picks a local or cheap hosted endpoint; the
// gateway holds no subscription credential, so it can never be one. The
// verdict is cached by prompt hash, and any failure (transport, timeout,
// unparsable answer) is reported to the caller, who keeps the rules'
// decision.
type Classifier struct {
	GW Completer
	// Timeout bounds one call (default 20 s).
	Timeout time.Duration
	// MaxPromptChars caps what the model sees (default 6000: head and tail).
	MaxPromptChars int
	// CacheSize bounds the verdict cache (default 1024 entries).
	CacheSize int

	mu    sync.Mutex
	cache map[string]ccjobs.Classification
	order []string
	// Calls counts gateway calls, for tests and the decision note.
	Calls int
}

// classifySystem is the whole instruction. It asks for strict JSON and
// nothing else; the parser tolerates a code fence or a sentence around it.
const classifySystem = `You classify a task so a router can pick where it runs. Read the task and answer with one JSON object and nothing else:
{"task_type":"code|writing|analysis|chat|data|other","difficulty":"easy|medium|hard","needs_tools":bool,"long_context":bool,"sensitive":bool,"latency_critical":bool}
task_type: code = writing, changing, debugging or running software; writing = prose to produce or edit; analysis = reasoning, comparing, deciding, explaining why; chat = conversation or a quick question; data = transforming, extracting or summarizing given data.
difficulty: easy = one short step a small model does well; hard = needs careful multi-step reasoning or expert judgement; else medium.
needs_tools: true if doing it well needs to read or edit files, run commands or browse a repository.
long_context: true if the task involves a large body of text or many files.
sensitive: true if it contains or asks about secrets, credentials, personal, medical or financial details, or anything marked private.
latency_critical: true if the user clearly wants an instant reply (autocomplete, a one-word answer).`

// classificationSchema is the verdict's JSON Schema, the strict subset
// (every property required, no extras) so a structured-output server
// can enforce it. It mirrors ccjobs.Classification and ParseClassification.
const classificationSchema = `{"type":"object","additionalProperties":false,
"properties":{
"task_type":{"type":"string","enum":["code","writing","analysis","chat","data","other"]},
"difficulty":{"type":"string","enum":["easy","medium","hard"]},
"needs_tools":{"type":"boolean"},
"long_context":{"type":"boolean"},
"sensitive":{"type":"boolean"},
"latency_critical":{"type":"boolean"}},
"required":["task_type","difficulty","needs_tools","long_context","sensitive","latency_critical"]}`

// Classify returns the verdict for prompt, from the cache when seen before.
// meta carries the caller's user and conversation ids so the call's spend
// lands on the ledger against them and user budgets see it; the task
// class is set here.
func (c *Classifier) Classify(ctx context.Context, prompt string, meta gateway.Metadata) (ccjobs.Classification, error) {
	if c == nil || c.GW == nil {
		return ccjobs.Classification{}, errors.New("no classifier configured")
	}
	key := ccjobs.PromptSHA256(prompt)
	c.mu.Lock()
	if v, ok := c.cache[key]; ok {
		c.mu.Unlock()
		return v, nil
	}
	c.mu.Unlock()

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	maxChars := c.MaxPromptChars
	if maxChars <= 0 {
		maxChars = 6000
	}
	zero := 0.0
	c.mu.Lock()
	c.Calls++
	c.mu.Unlock()
	meta.TaskClass = gateway.TaskClassify
	r, err := c.GW.Complete(cctx, &gateway.Request{
		Model:       "auto",
		System:      classifySystem,
		Messages:    []gateway.Message{{Role: gateway.RoleUser, Parts: []gateway.Part{gateway.TextPart("Task:\n" + clip(prompt, maxChars) + "\n\nJSON:")}}},
		MaxTokens:   120,
		Temperature: &zero,
		// Structured output where the endpoint supports it (Cerebras, the
		// local servers); elsewhere the prompt alone asks for JSON and
		// ParseClassification tolerates fences and chatter.
		JSON:     &gateway.JSONFormat{Name: "classification", Schema: json.RawMessage(classificationSchema), Strict: true},
		Metadata: meta,
	})
	if err != nil {
		return ccjobs.Classification{}, fmt.Errorf("classifier call: %w", err)
	}
	v, err := ParseClassification(r.Text())
	if err != nil {
		return ccjobs.Classification{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	size := c.CacheSize
	if size <= 0 {
		size = 1024
	}
	if c.cache == nil {
		c.cache = map[string]ccjobs.Classification{}
	}
	for len(c.order) >= size {
		delete(c.cache, c.order[0])
		c.order = c.order[1:]
	}
	c.cache[key] = v
	c.order = append(c.order, key)
	return v, nil
}

// ParseClassification finds the JSON object in a model reply and checks
// its enums. A reply with no object, or with values outside the enums,
// is an error so the caller falls back to the rules.
func ParseClassification(s string) (ccjobs.Classification, error) {
	start, end := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return ccjobs.Classification{}, errors.New("classifier reply has no JSON object")
	}
	var v ccjobs.Classification
	if err := json.Unmarshal([]byte(s[start:end+1]), &v); err != nil {
		return ccjobs.Classification{}, fmt.Errorf("classifier reply: %w", err)
	}
	v.TaskType, v.Difficulty = strings.ToLower(strings.TrimSpace(v.TaskType)), strings.ToLower(strings.TrimSpace(v.Difficulty))
	switch v.TaskType {
	case "code", "writing", "analysis", "chat", "data", "other":
	default:
		return ccjobs.Classification{}, fmt.Errorf("classifier reply: bad task_type %q", v.TaskType)
	}
	switch v.Difficulty {
	case "easy", "medium", "hard":
	default:
		return ccjobs.Classification{}, fmt.Errorf("classifier reply: bad difficulty %q", v.Difficulty)
	}
	return v, nil
}

// clip keeps the head and tail of a long prompt so the classifier sees
// both the ask and the material it refers to. max counts characters
// (runes), and the cut lands on rune boundaries, so a multibyte character
// is never split into invalid UTF-8.
func clip(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	head := max * 2 / 3
	tail := max - head
	return string(runes[:head]) + "\n[... " + fmt.Sprint(len(runes)-max) + " characters omitted ...]\n" + string(runes[len(runes)-tail:])
}
