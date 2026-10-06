package ccjobs

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Router v1 (docs/CLAUDE_CODE_JOBS.md §6.4, spec M3): deterministic rules
// only. The router decides *where* a task runs; it never sees Claude
// Code's output, and nothing here can resolve to a subscription credential
// because ws holds none. M4 adds a small-model classifier for the prompts
// these rules call ambiguous.

// Features are what the rules see. They are logged with every decision
// (cc_route_decisions.features) so a routing can be explained later
// without the prompt, which is never stored.
type Features struct {
	Chars      int  `json:"chars"`
	Words      int  `json:"words"`
	Lines      int  `json:"lines"`
	CodeFences int  `json:"code_fences"`
	FilePaths  int  `json:"file_paths"`
	HasCwd     bool `json:"has_cwd"`
	// Override is the explicit lane the prompt asked for (@claude, @api,
	// @openrouter, @local), or "".
	Override  string `json:"override,omitempty"`
	Sensitive bool   `json:"sensitive"`
	// Agentic counts verbs that imply editing, running or debugging code.
	Agentic int `json:"agentic"`
	// Reasoning counts words that imply analysis or design rather than edits.
	Reasoning int `json:"reasoning"`
	// Simple counts words that mark bulk or one-shot text work.
	Simple int `json:"simple"`
	// LatencyCritical is set by the caller (interactive chat, a UI hint).
	LatencyCritical bool `json:"latency_critical,omitempty"`
	// Classification is the small-model verdict (spec M4), present only
	// when the rules were ambiguous and the classifier answered.
	Classification *Classification `json:"classification,omitempty"`
}

// Classification is the JSON a small model returns for an ambiguous
// prompt (spec §6.4 stage 2). The model only ever sees the prompt text;
// it is routed by task class `classify`, which the policy resolves to a
// local or cheap API endpoint, never to a subscription.
type Classification struct {
	// TaskType: code | writing | analysis | chat | data | other.
	TaskType string `json:"task_type"`
	// Difficulty: easy | medium | hard.
	Difficulty      string `json:"difficulty"`
	NeedsTools      bool   `json:"needs_tools"`
	LongContext     bool   `json:"long_context"`
	Sensitive       bool   `json:"sensitive"`
	LatencyCritical bool   `json:"latency_critical"`
}

// RouteClassified applies the spec's policy to a classifier verdict for a
// prompt the rules found ambiguous. The rules' own features still count:
// their sensitive detection is never overridden by the model saying no.
//
//  1. Sensitive (rules or model): local.
//  2. Code work that needs tools, or long-context code work: claude-subscription.
//  3. Hard, or analysis: api.
//  4. Latency-critical or easy: openrouter.
//  5. Otherwise: api.
func RouteClassified(f Features, c Classification) Decision {
	cc := c
	f.Classification = &cc
	d := Decision{Features: f}
	switch {
	case f.Sensitive || c.Sensitive:
		d.Lane, d.Rule, d.Reason = LaneLocal, "classifier:sensitive", "classifier or rules flagged secrets or personal data; never leaves the box"
	case c.TaskType == "code" && (c.NeedsTools || c.LongContext):
		d.Lane, d.Rule, d.Reason = LaneSubscription, "classifier:code", "classifier: code work that needs tools or a large context"
	case c.Difficulty == "hard" || c.TaskType == "analysis":
		d.Lane, d.Rule, d.Reason = LaneAPI, "classifier:hard", "classifier: hard or analytical task"
	case c.LatencyCritical || c.Difficulty == "easy":
		d.Lane, d.Rule, d.Reason = LaneOpenRouter, "classifier:simple", "classifier: easy or latency-critical task"
	default:
		d.Lane, d.Rule, d.Reason = LaneAPI, "classifier:default", "classifier: medium task, hosted model"
	}
	return d
}

// Decision is the router's output: a lane and why.
type Decision struct {
	Lane     string   `json:"lane"`
	Reason   string   `json:"reason"`
	Rule     string   `json:"rule"`
	Features Features `json:"features"`
	// Ambiguous is true when no rule fired and the default lane was used;
	// M4 sends exactly these prompts to the classifier.
	Ambiguous bool `json:"ambiguous"`
}

// RouteInput is what the router is given. Cwd and Target are set by the
// caller (spawn_job arguments); the prompt text supplies the rest.
type RouteInput struct {
	Prompt          string
	Cwd             string
	LatencyCritical bool
}

// Thresholds that the rules use. Exported so tests and a future config
// knob can name them; the defaults come from the spec's wording ("long",
// "simple/bulk") and should be tuned against the decision log.
const (
	LongPromptChars  = 1500
	ShortPromptChars = 240
)

var (
	// Overrides: a lane name after @ anywhere in the prompt, as its own word.
	overrideRe = regexp.MustCompile(`(^|\s)@(claude|api|openrouter|local)\b`)
	// A path-ish token: at least one slash with word characters around it,
	// or a bare filename with a code-ish extension.
	filePathRe  = regexp.MustCompile(`(^|[\s"'(\x60])((\.{0,2}/)?[\w.-]+(/[\w.-]+)+|[\w-]+\.(go|ts|tsx|js|jsx|py|rs|java|kt|rb|sql|yaml|yml|toml|json|md|sh|css|html|c|h|cpp|cs|swift|proto))\b`)
	codeFenceRe = regexp.MustCompile("(?m)^\\s*```")
	wordRe      = regexp.MustCompile(`[A-Za-z][A-Za-z'-]*`)

	sensitiveRe = regexp.MustCompile(`(?i)(\b(password|passwd|secret|api[ _-]?key|private[ _-]?key|access[ _-]?token|bearer|credentials?|ssn|social security|credit card|confidential|do not share|medical|diagnosis)\b|(^|[\s"'(])\.env\b|BEGIN (RSA|OPENSSH|EC) PRIVATE KEY)`)

	agenticWords = map[string]bool{
		"refactor": true, "implement": true, "fix": true, "debug": true, "build": true, "compile": true,
		"test": true, "tests": true, "migrate": true, "migration": true, "rename": true, "add": true,
		"remove": true, "delete": true, "update": true, "upgrade": true, "install": true, "run": true,
		"deploy": true, "lint": true, "typecheck": true, "commit": true, "push": true, "branch": true,
		"pr": true, "merge": true, "rebase": true, "endpoint": true, "handler": true, "function": true,
		"module": true, "package": true, "repo": true, "repository": true, "codebase": true, "ci": true,
		"failing": true, "stack": true, "trace": true, "error": true, "bug": true, "patch": true,
	}
	reasoningWords = map[string]bool{
		"prove": true, "proof": true, "analyze": true, "analyse": true, "analysis": true, "compare": true,
		"tradeoff": true, "tradeoffs": true, "trade-offs": true, "design": true, "architecture": true,
		"strategy": true, "plan": true, "evaluate": true, "assess": true, "critique": true, "review": true,
		"reason": true, "derive": true, "theorem": true, "optimal": true, "complexity": true, "why": true,
		"explain": true, "estimate": true, "model": true, "research": true, "survey": true,
	}
	simpleWords = map[string]bool{
		"summarize": true, "summarise": true, "summary": true, "tldr": true, "translate": true,
		"rewrite": true, "rephrase": true, "paraphrase": true, "shorten": true, "expand": true,
		"title": true, "caption": true, "tag": true, "tags": true, "classify": true, "label": true,
		"extract": true, "list": true, "bullet": true, "bullets": true, "format": true, "convert": true,
		"spellcheck": true, "grammar": true, "proofread": true, "tweet": true, "email": true, "reply": true,
	}
)

// Extract computes the features of a prompt.
func Extract(in RouteInput) Features {
	p := in.Prompt
	f := Features{
		Chars:           utf8.RuneCountInString(p),
		Lines:           strings.Count(strings.TrimRight(p, "\n"), "\n") + 1,
		CodeFences:      len(codeFenceRe.FindAllStringIndex(p, -1)) / 2,
		FilePaths:       len(filePathRe.FindAllStringIndex(p, -1)),
		HasCwd:          strings.TrimSpace(in.Cwd) != "",
		Sensitive:       sensitiveRe.MatchString(p),
		LatencyCritical: in.LatencyCritical,
	}
	if m := overrideRe.FindStringSubmatch(p); m != nil {
		f.Override = m[2]
	}
	for _, w := range wordRe.FindAllString(strings.ToLower(p), -1) {
		f.Words++
		if agenticWords[w] {
			f.Agentic++
		}
		if reasoningWords[w] {
			f.Reasoning++
		}
		if simpleWords[w] {
			f.Simple++
		}
	}
	return f
}

var overrideLanes = map[string]string{"claude": LaneSubscription, "api": LaneAPI, "openrouter": LaneOpenRouter, "local": LaneLocal}

// SensitiveAllowed reports whether a lane may carry sensitive content:
// the spec's policy rule 1 allows local or api only, never OpenRouter
// (free models may retain prompts) and not the subscription lane.
func SensitiveAllowed(lane string) bool { return lane == LaneLocal || lane == LaneAPI }

// Route applies the policy in the spec's priority order and returns the
// lane with the rule that chose it.
//
//  1. Sensitive content goes to local or api only. It wins over an
//     override and over every rule below: @openrouter or @claude on a
//     prompt with a secret in it is downgraded to local.
//  2. An explicit override (@claude, @api, @openrouter, @local).
//  3. Repo-shaped, long or tool-heavy work: claude-subscription.
//  4. Hard reasoning without a repo: api.
//  5. Simple, bulk or latency-critical: openrouter.
//  6. Otherwise ambiguous: api, flagged for the M4 classifier.
func Route(in RouteInput) Decision {
	f := Extract(in)
	d := Decision{Features: f}
	switch {
	case f.Sensitive && (f.Override == "" || !SensitiveAllowed(overrideLanes[f.Override])):
		d.Lane, d.Rule, d.Reason = LaneLocal, "sensitive", "prompt mentions secrets or personal data; never leaves the box"
		if f.Override != "" {
			d.Reason += " (@" + f.Override + " ignored)"
		}
	case f.Override != "":
		d.Lane = overrideLanes[f.Override]
		d.Rule, d.Reason = "override", "prompt asks for @"+f.Override
	case repoShaped(f):
		d.Lane, d.Rule = LaneSubscription, "repo"
		d.Reason = repoReason(f)
	case f.Reasoning > 0 && f.Agentic == 0:
		d.Lane, d.Rule, d.Reason = LaneAPI, "reasoning", "analysis or design question with no repository"
	case f.LatencyCritical || (f.Chars <= ShortPromptChars && f.CodeFences == 0 && f.FilePaths == 0 && (f.Simple > 0 || f.Agentic == 0)):
		d.Lane, d.Rule = LaneOpenRouter, "simple"
		if f.LatencyCritical {
			d.Reason = "latency-critical"
		} else {
			d.Reason = "short one-shot text task"
		}
	default:
		d.Lane, d.Rule, d.Reason, d.Ambiguous = LaneAPI, "default", "no rule matched; default lane (classifier in M4)", true
	}
	return d
}

func repoShaped(f Features) bool {
	switch {
	case f.HasCwd:
		return true
	case f.FilePaths >= 2:
		return true
	case f.CodeFences >= 1 && f.Agentic >= 1:
		return true
	case f.Chars >= LongPromptChars && f.Agentic >= 1:
		return true
	case f.Agentic >= 3:
		return true
	}
	return false
}

func repoReason(f Features) string {
	switch {
	case f.HasCwd:
		return "a working directory was given"
	case f.FilePaths >= 2:
		return "names several files"
	case f.CodeFences >= 1:
		return "contains code and asks for changes"
	case f.Chars >= LongPromptChars:
		return "long prompt with code-editing verbs"
	}
	return "several code-editing verbs"
}

// PromptSHA256 is the only form of the prompt that is ever stored.
func PromptSHA256(prompt string) string {
	sum := sha256.Sum256([]byte(prompt))
	return hex.EncodeToString(sum[:])
}

// Repos are optional directories (per target) that a job's cwd may fall
// under; see Target.Repos. ChooseTarget picks the target whose DefaultDir
// or one of its Repos contains cwd, else the default target. "~" is
// compared literally, since the target's home is not known here.
func (c *Config) ChooseTarget(cwd string) string {
	cwd = strings.TrimRight(strings.TrimSpace(cwd), "/")
	if cwd == "" {
		return c.Default
	}
	best, bestLen := c.Default, -1
	for _, name := range c.Names() {
		t := c.Targets[name]
		for _, root := range append([]string{t.DefaultDir}, t.Repos...) {
			root = strings.TrimRight(strings.TrimSpace(root), "/")
			if root == "" {
				continue
			}
			if (cwd == root || strings.HasPrefix(cwd, root+"/")) && len(root) > bestLen {
				best, bestLen = name, len(root)
			}
		}
	}
	return best
}
