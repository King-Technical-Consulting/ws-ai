package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Verdict routing (docs/design/llm-routing.md): for selector "auto" a small
// model reads the turn and says what it needs, and that moves the task
// class and the tier floor the router sees. The call lives in
// Gateway.Prepare, not in Route, so Route stays a pure function of its
// input. Every failure falls through to routing exactly as without it.

// Verdict classes.
const (
	VerdictSimpleChat  = "simple-chat"
	VerdictHardChat    = "hard-chat"
	VerdictCode        = "code"
	VerdictSummarize   = "summarize"
	VerdictVision      = "vision"
	VerdictLongContext = "long-context"
)

// VerdictNeeds are the capabilities the model says the turn needs. They are
// only ever added to the request's own requirements.
type VerdictNeeds struct {
	Tools       bool `json:"tools"`
	Vision      bool `json:"vision"`
	LongContext bool `json:"long_context"`
}

// Verdict is the model's reading of one turn.
type Verdict struct {
	Class     string       `json:"class"`
	Needs     VerdictNeeds `json:"needs"`
	Sensitive bool         `json:"sensitive"`
	Reason    string       `json:"reason"`
}

// longContextWindow is the context window a "long-context" verdict asks for.
const longContextWindow = 64000

// ParseVerdict reads the model's answer, tolerating a code fence or a
// sentence around the JSON object. An unknown class is an error, which
// means the verdict is not used.
func ParseVerdict(text string) (Verdict, error) {
	text = strings.TrimSpace(text)
	i, j := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if i < 0 || j <= i {
		return Verdict{}, errors.New("no JSON object in the verdict")
	}
	var v Verdict
	if err := json.Unmarshal([]byte(text[i:j+1]), &v); err != nil {
		return Verdict{}, fmt.Errorf("verdict is not valid JSON: %w", err)
	}
	switch v.Class {
	case VerdictSimpleChat, VerdictHardChat, VerdictCode, VerdictSummarize, VerdictVision, VerdictLongContext:
	default:
		return Verdict{}, fmt.Errorf("unknown verdict class %q", v.Class)
	}
	if len(v.Reason) > 200 {
		v.Reason = v.Reason[:200]
	}
	return v, nil
}

// verdictRoute is the task class and tier a verdict class maps to.
func verdictRoute(class string) (TaskClass, string) {
	switch class {
	case VerdictSimpleChat:
		return TaskChat, TierSmall
	case VerdictHardChat:
		return TaskChat, TierLarge
	case VerdictCode:
		return TaskCode, TierLarge
	case VerdictSummarize:
		return TaskSummarize, TierSmall
	case VerdictVision:
		return TaskVision, TierMedium
	case VerdictLongContext:
		return TaskChat, TierMedium
	}
	return TaskChat, ""
}

// Apply folds a verdict into the routing input. Capability needs are OR-ed
// in (a verdict adds a requirement, never removes one); a sensitive verdict
// forces local candidates. Only a chat turn is reclassified: a caller that
// set a class (summarize, vision, image...) keeps it, and a verdict that
// maps to chat leaves a code turn alone.
func (v Verdict) Apply(in *RouteInput) {
	task, tier := verdictRoute(v.Class)
	in.VerdictClass = v.Class
	in.MinTier = tier
	if in.TaskClass == TaskChat {
		in.TaskClass = task
	}
	if v.Needs.Tools {
		in.Required.Tools = true
	}
	if v.Needs.Vision {
		in.Required.Vision = true
	}
	if v.Needs.LongContext || v.Class == VerdictLongContext {
		if in.Required.ContextWindow < longContextWindow {
			in.Required.ContextWindow = longContextWindow
		}
	}
	if v.Sensitive {
		in.ForceLocal = true
	}
}

// VerdictConfig configures the Verdicter.
type VerdictConfig struct {
	// Deadline bounds the verdict call (default 2 s).
	Deadline time.Duration
	// ReuseTurns is how many further turns of a conversation reuse its
	// verdict (default 4).
	ReuseTurns int
	// MaxUSDDay returns today's spend ceiling; <= 0 means no ceiling.
	MaxUSDDay func(ctx context.Context) float64
	// SpentToday returns what verdict calls have cost since UTC midnight.
	SpentToday func(ctx context.Context) (float64, error)
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Verdicter asks for and caches verdicts. One per gateway.
type Verdicter struct {
	gw  *Gateway
	cfg VerdictConfig

	mu    sync.Mutex
	cache map[string]*verdictEntry
	order []string

	spendAt  time.Time
	spend    float64
	spendSet bool
}

type verdictEntry struct {
	v       Verdict
	turns   int // turns served from this verdict, the first included
	images  int
	hadTool bool
}

const verdictCacheSize = 512

// EnableVerdicts turns verdict routing on for this gateway.
func (g *Gateway) EnableVerdicts(cfg VerdictConfig) *Verdicter {
	if cfg.Deadline <= 0 {
		cfg.Deadline = 2 * time.Second
	}
	if cfg.ReuseTurns <= 0 {
		cfg.ReuseTurns = 4
	}
	v := &Verdicter{gw: g, cfg: cfg, cache: map[string]*verdictEntry{}}
	g.verdicts = v
	return v
}

func (v *Verdicter) now() time.Time {
	if v.cfg.Now != nil {
		return v.cfg.Now()
	}
	return time.Now()
}

// verdictSensitive is a cheap rules-based check run before any hosted call:
// text that looks like it carries secrets or personal data is never sent to
// the classifier. It mirrors the router's sensitive patterns in
// internal/ccjobs (which the gateway cannot import).
var verdictSensitive = regexp.MustCompile(`(?i)(\b(password|passwd|secret|api[ _-]?key|private[ _-]?key|access[ _-]?token|bearer|credentials?|ssn|social security|credit card|confidential|do not share|medical|diagnosis)\b|(^|[\s"'(])\.env\b|BEGIN (RSA|OPENSSH|EC) PRIVATE KEY)`)

const verdictInputChars = 1500

const verdictSystem = `You route a chat turn. Reply with one JSON object only: {"class":"simple-chat|hard-chat|code|summarize|vision|long-context","needs":{"tools":bool,"vision":bool,"long_context":bool},"sensitive":bool,"reason":"one line"}.
class: simple-chat = a greeting or short factual question; hard-chat = multi-step reasoning, analysis or long writing; code = write, read or debug software; summarize = condense given text; vision = the turn depends on an attached image; long-context = depends on a large pasted document.
sensitive: true for secrets, credentials, personal, medical or financial details.
reason: under 120 characters.`

// lastUserText is the text of the last user message and the counts the
// classifier is told.
func lastUserText(req *Request) (text string, images int) {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		if m.Role != RoleUser {
			continue
		}
		var b strings.Builder
		for _, p := range m.Parts {
			switch p.Kind {
			case PartText:
				b.WriteString(p.Text)
				b.WriteString("\n")
			case PartImage:
				images++
			}
		}
		return strings.TrimSpace(b.String()), images
	}
	return "", 0
}

// clipHeadTail keeps the start and the end of s within n bytes, on rune
// boundaries.
func clipHeadTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	h := n / 2
	t := n - h
	for h > 0 && !utf8.RuneStart(s[h]) {
		h--
	}
	k := len(s) - t
	for k < len(s) && !utf8.RuneStart(s[k]) {
		k++
	}
	return s[:h] + "\n[...]\n" + s[k:]
}

func verdictInput(req *Request, text string, images int) string {
	return fmt.Sprintf("Last user message:\n%s\n\nmessages in the conversation: %d\ntools attached: %s\nimages attached: %d",
		clipHeadTail(text, verdictInputChars), len(req.Messages), yesNo(len(req.Tools) > 0), images)
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// spentToday is today's verdict spend, refreshed from the ledger at most
// once a minute and kept current in between by add.
func (v *Verdicter) spentToday(ctx context.Context) (float64, bool) {
	if v.cfg.SpentToday == nil {
		return 0, true
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.now()
	if !v.spendSet || now.Sub(v.spendAt) > time.Minute {
		s, err := v.cfg.SpentToday(ctx)
		if err != nil {
			return v.spend, v.spendSet // keep the last figure; unknown is not zero
		}
		v.spend, v.spendAt, v.spendSet = s, now, true
	}
	return v.spend, true
}

// Decide returns the verdict for a request, or why there is none. rule is
// the rule that would apply without a verdict. It never returns an error:
// every failure is a source string and a nil verdict.
func (v *Verdicter) Decide(ctx context.Context, req *Request, in RouteInput, rule Rule) (*Verdict, string) {
	switch {
	case req.Metadata.NoVerdict || in.TaskClass == TaskRoute || in.TaskClass == TaskClassify:
		return nil, "skipped:recursion"
	case in.TaskClass == TaskCode:
		return nil, "skipped:code"
	case in.TaskClass != TaskChat:
		return nil, "skipped:class"
	case in.External:
		return nil, "skipped:external"
	case in.AgentID != "":
		return nil, "skipped:agent" // background work: the owner picked the model
	case in.Downgrade || rule.LocalOnly:
		return nil, "skipped:local_only"
	}
	text, images := lastUserText(req)
	if strings.TrimSpace(text) == "" && images == 0 {
		return nil, "skipped:empty"
	}
	if verdictSensitive.MatchString(text) {
		return nil, "skipped:sensitive"
	}
	conv := req.Metadata.ConversationID
	if hit, ok := v.fromCache(conv, req, text, images); ok {
		return hit, "cache"
	}
	if v.cfg.MaxUSDDay != nil {
		if cap := v.cfg.MaxUSDDay(ctx); cap > 0 {
			if spent, _ := v.spentToday(ctx); spent >= cap {
				return nil, "skipped:budget"
			}
		}
	}
	cctx, cancel := context.WithTimeout(ctx, v.cfg.Deadline)
	defer cancel()
	vreq := &Request{
		Model:       "auto",
		System:      verdictSystem,
		Messages:    []Message{{Role: RoleUser, Parts: []Part{TextPart(verdictInput(req, text, images))}}},
		MaxTokens:   120,
		Temperature: ptrFloat(0),
		JSON:        &JSONFormat{},
		Metadata: Metadata{
			UserID: req.Metadata.UserID, ConversationID: conv,
			TaskClass: TaskRoute, NoVerdict: true,
		},
	}
	resp, err := v.gw.Complete(cctx, vreq)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || cctx.Err() != nil {
			return nil, "fallthrough:deadline"
		}
		return nil, "fallthrough:call"
	}
	verdict, err := ParseVerdict(resp.Text())
	if err != nil {
		return nil, "fallthrough:parse"
	}
	v.addSpend(v.gw.cost(resp))
	v.store(conv, verdict, req, images)
	return &verdict, "llm"
}

func ptrFloat(f float64) *float64 { return &f }

func (v *Verdicter) addSpend(c float64) {
	if c <= 0 {
		return
	}
	v.mu.Lock()
	v.spend += c
	v.mu.Unlock()
}

// fromCache reuses a conversation's verdict for the next few turns unless
// the turn changed shape: a long message, a new image, a tool set that
// appeared, or a long message after a simple-chat verdict.
func (v *Verdicter) fromCache(conv string, req *Request, text string, images int) (*Verdict, bool) {
	if conv == "" {
		return nil, false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	e, ok := v.cache[conv]
	if !ok {
		return nil, false
	}
	if e.turns > v.cfg.ReuseTurns || len(text) > verdictInputChars || images > e.images ||
		(len(req.Tools) > 0 && !e.hadTool) || (e.v.Class == VerdictSimpleChat && len(text) > 400) {
		return nil, false
	}
	e.turns++
	out := e.v
	return &out, true
}

func (v *Verdicter) store(conv string, verdict Verdict, req *Request, images int) {
	if conv == "" {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, ok := v.cache[conv]; !ok {
		v.order = append(v.order, conv)
		if len(v.order) > verdictCacheSize {
			delete(v.cache, v.order[0])
			v.order = v.order[1:]
		}
	}
	v.cache[conv] = &verdictEntry{v: verdict, turns: 1, images: images, hadTool: len(req.Tools) > 0}
}

// cost is the priced cost of a finished response, 0 when unknown.
func (g *Gateway) cost(resp *Response) float64 {
	if g.Registry == nil || resp == nil {
		return 0
	}
	if ep, ok := g.Registry.Endpoint(resp.EndpointID); ok {
		return ep.Pricing.Cost(resp.Usage)
	}
	return 0
}

// Stats is what Admin shows beside the daily cap.
func (v *Verdicter) SpentTodayUSD(ctx context.Context) float64 {
	s, _ := v.spentToday(ctx)
	return s
}

func logVerdict(log *slog.Logger, class, source, chosen string, ms int64, conv string) {
	log.Info("route verdict", "class", class, "source", source, "ms", ms, "conv", conv, "chosen", chosen)
}
