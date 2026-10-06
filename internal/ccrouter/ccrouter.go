// Package ccrouter is the task router's dispatcher: the spawn_job tool on
// the ws agent runtime (docs/CLAUDE_CODE_JOBS.md §6.4, spec M3). It
// decides a lane with the deterministic rules in internal/ccjobs, logs
// the decision, and either launches the official claude in a tmux window
// through the same launcher the wsj CLI uses (claude-subscription lane)
// or starts an ordinary ws agent run under a routing alias (api,
// openrouter, local lanes).
//
// Nothing here reads Claude Code's output or touches a credential: the
// subscription lane returns a tmux handle, and the other lanes run on the
// gateway, which holds API keys only.
package ccrouter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/agent"
	"github.com/jking323/ws/internal/ccjobs"
	"github.com/jking323/ws/internal/ccweb"
	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
)

// ToolName is the tool's name as the model sees it.
const ToolName = "spawn_job"

// StartupGrace is how long a subscription launch is watched for an
// immediate exit (the not-logged-in case) before the handle is returned.
const StartupGrace = 3 * time.Second

// DefaultAliases maps the non-subscription lanes to gateway selectors
// (config/policies/*.yaml aliases). Overridden by WS_CC_ALIASES.
var DefaultAliases = map[string]string{
	ccjobs.LaneAPI:        "best",
	ccjobs.LaneOpenRouter: "cheap",
	ccjobs.LaneLocal:      "local",
}

// RunTools is what a dispatched ws run may use. Deliberately no ask_user
// (nobody is watching a background run) and no spawn_job (no recursion).
var RunTools = []string{"web_fetch", "read_blob", "create_artifact", "update_artifact"}

// Tool is the spawn_job tool.
type Tool struct {
	// DB records decisions; nil skips the log (tests).
	DB *store.DB
	// Runtime creates the ws runs for the non-subscription lanes.
	Runtime *agent.Runtime
	// Enqueue hands a created run to the worker. nil drives it in this
	// process in the background.
	Enqueue func(ctx context.Context, runID uuid.UUID) error
	// Targets loads the launcher targets for the subscription lane. nil
	// means the lane is not available from this host.
	Targets func() (*ccjobs.Config, error)
	// Dispatch gates real launches. While false every call is a dry run,
	// which is the shipped default until a host has tmux, ssh keys and a
	// logged-in claude for its targets.
	Dispatch bool
	// IsOwner says whether the calling user may use the subscription lane
	// (spec §2: single user; nobody else's request may reach it). nil
	// refuses every subscription launch.
	IsOwner func(ctx context.Context, userID uuid.UUID) (bool, error)
	// Aliases maps lanes to selectors; nil uses DefaultAliases.
	Aliases map[string]string
	// Classifier decides the prompts the rules call ambiguous (M4). nil
	// keeps the rules' default lane for them.
	Classifier *Classifier
	// Jobs records subscription launches for the web tab (M5); nil skips it.
	Jobs *ccweb.Registry
	// Budget reads the week's launch pool (M6). nil means no cap: the
	// decision stands as the rules and classifier made it.
	Budget func(ctx context.Context) (ccjobs.Budget, error)
	Log    *slog.Logger
}

// ParseAliases reads "lane=alias,lane=alias" (WS_CC_ALIASES) on top of
// the defaults.
func ParseAliases(s string) map[string]string {
	out := map[string]string{}
	for k, v := range DefaultAliases {
		out[k] = v
	}
	for _, kv := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if ok && k != "" && v != "" {
			out[k] = v
		}
	}
	return out
}

func (t *Tool) Def() gateway.ToolDef {
	return gateway.ToolDef{
		Name: ToolName,
		Description: "Dispatch a task to the lane that suits it: a Claude Code session on the subscription (repo-shaped, multi-file, agentic work), " +
			"a hosted API model (hard reasoning), or a cheap/local model (simple, bulk or sensitive text). " +
			"Omit lane to let the router decide from the prompt. Returns the decision and a handle (tmux window or ws run), never the task's output. " +
			"Set dry_run to see the decision without starting anything.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
"prompt":{"type":"string","description":"The task, written for the model that will run it. Everything it needs must be in here or in cwd."},
"lane":{"type":"string","enum":["claude-subscription","api","openrouter","local"],"description":"Force a lane. Omit to route by rules."},
"cwd":{"type":"string","description":"Working directory on the target for repo work (e.g. ~/code/app). Its presence alone routes to the subscription lane."},
"target":{"type":"string","description":"Launcher target name for the subscription lane. Omit to pick by cwd, else the default target."},
"model":{"type":"string","description":"Model hint: claude --model for the subscription lane, a gateway selector for the others."},
"dry_run":{"type":"boolean","description":"Only decide and log; start nothing."}
},"required":["prompt"],"additionalProperties":false}`),
	}
}

// DefaultPolicy asks before a real launch; dry runs need no approval.
func (t *Tool) DefaultPolicy() agent.Policy {
	if t.Dispatch {
		return agent.PolicyAsk
	}
	return agent.PolicyAuto
}

func (t *Tool) Idempotent() bool { return false }

// Input is the tool's arguments.
type Input struct {
	Prompt string `json:"prompt"`
	Lane   string `json:"lane"`
	Cwd    string `json:"cwd"`
	Target string `json:"target"`
	Model  string `json:"model"`
	DryRun bool   `json:"dry_run"`
}

// Output is what the model and the UI get back.
type Output struct {
	Decision   ccjobs.Decision `json:"decision"`
	Target     string          `json:"target,omitempty"`
	Selector   string          `json:"selector,omitempty"`
	DryRun     bool            `json:"dry_run"`
	Dispatched bool            `json:"dispatched"`
	// Handle is the tmux handle for the subscription lane.
	Handle *ccjobs.Handle `json:"handle,omitempty"`
	// RunID and ConversationID identify the ws run for the other lanes.
	RunID          *uuid.UUID `json:"run_id,omitempty"`
	ConversationID *uuid.UUID `json:"conversation_id,omitempty"`
	DecisionID     *uuid.UUID `json:"decision_id,omitempty"`
	Note           string     `json:"note,omitempty"`
}

var validLanes = map[string]bool{ccjobs.LaneSubscription: true, ccjobs.LaneAPI: true, ccjobs.LaneOpenRouter: true, ccjobs.LaneLocal: true}

func (t *Tool) Call(ctx context.Context, tc agent.ToolCtx, args json.RawMessage) (agent.Result, error) {
	var in Input
	if err := json.Unmarshal(args, &in); err != nil {
		return agent.ErrorResult(err), nil
	}
	if strings.TrimSpace(in.Prompt) == "" {
		return agent.ErrorResult(errors.New("prompt is required")), nil
	}
	if in.Lane != "" && !validLanes[in.Lane] {
		return agent.ErrorResult(fmt.Errorf("unknown lane %q", in.Lane)), nil
	}
	out, err := t.Run(ctx, tc, in)
	if err != nil {
		return agent.ErrorResult(err), nil
	}
	return agent.JSONResult(out), nil
}

// Run decides, logs and dispatches. It is the tool minus the JSON
// plumbing so a CLI or a test can call it directly.
func (t *Tool) Run(ctx context.Context, tc agent.ToolCtx, in Input) (*Output, error) {
	var d ccjobs.Decision
	if in.Lane != "" {
		d = ccjobs.Decision{Lane: in.Lane, Rule: "requested", Reason: "lane given by the caller", Features: ccjobs.Extract(ccjobs.RouteInput{Prompt: in.Prompt, Cwd: in.Cwd})}
		// The policy's sensitive rule applies to a forced lane too: the
		// lane argument comes from the calling model, which may have been
		// steered by text it read.
		if d.Features.Sensitive && !ccjobs.SensitiveAllowed(in.Lane) {
			d.Lane, d.Rule, d.Reason = ccjobs.LaneLocal, "sensitive", "prompt mentions secrets or personal data; requested lane "+in.Lane+" refused, never leaves the box"
		}
	} else {
		d = ccjobs.Route(ccjobs.RouteInput{Prompt: in.Prompt, Cwd: in.Cwd})
	}
	out := &Output{DryRun: in.DryRun || !t.Dispatch}
	// Stage 2: only an ambiguous prompt is shown to the small model, and
	// only its verdict can move the lane; any failure keeps the rules.
	if d.Ambiguous && t.Classifier != nil {
		meta := gateway.Metadata{}
		if tc.UserID != uuid.Nil {
			meta.UserID = tc.UserID.String()
		}
		if tc.ConversationID != uuid.Nil {
			meta.ConversationID = tc.ConversationID.String()
		}
		if v, err := t.Classifier.Classify(ctx, in.Prompt, meta); err != nil {
			out.Note = joinNote(out.Note, "classifier failed, rules decision kept: "+err.Error())
			if t.Log != nil {
				t.Log.Warn("spawn_job classifier", "err", err)
			}
		} else {
			d = ccjobs.RouteClassified(d.Features, v)
		}
	}
	// Stage 3: the weekly soft cap (spec M6). Low-value subscription work
	// goes to the api lane as the pool fills; a lane the caller asked for
	// is noted, never overridden. A failed read keeps the decision.
	if d.Lane == ccjobs.LaneSubscription && t.Budget != nil {
		if b, err := t.Budget(ctx); err != nil {
			out.Note = joinNote(out.Note, "launch budget unavailable, decision kept: "+err.Error())
		} else {
			var note string
			d, note = ccjobs.ApplyBudget(d, in.Lane != "" || d.Rule == "override", b)
			out.Note = joinNote(out.Note, note)
		}
	}
	out.Decision = d
	if !t.Dispatch && !in.DryRun {
		out.Note = joinNote(out.Note, "dispatch is disabled on this host (WS_CC_DISPATCH); decision logged only")
	}

	aliases := t.Aliases
	if aliases == nil {
		aliases = DefaultAliases
	}
	var cfg *ccjobs.Config
	switch d.Lane {
	case ccjobs.LaneSubscription:
		// Single user: only the owner may reach the subscription, whoever
		// approves the tool call. Checked on every call, dry run included,
		// so a non-owner learns it here rather than at launch.
		if t.IsOwner == nil {
			return nil, errors.New("subscription lane refused: no owner check is configured on this host")
		}
		ok, err := t.IsOwner(ctx, tc.UserID)
		if err != nil {
			return nil, fmt.Errorf("subscription lane refused: owner check: %w", err)
		}
		if !ok {
			return nil, errors.New("subscription lane refused: only the owner's requests may start a Claude Code session")
		}
		if t.Targets == nil {
			out.Note = joinNote(out.Note, "no launcher targets configured on this host; the subscription lane cannot launch from here")
		} else {
			c, err := t.Targets()
			if err != nil {
				out.Note = joinNote(out.Note, "launcher targets: "+err.Error())
			} else {
				cfg = c
				out.Target = in.Target
				if out.Target == "" {
					out.Target = cfg.ChooseTarget(in.Cwd)
				} else if _, ok := cfg.Targets[out.Target]; !ok {
					return nil, fmt.Errorf("unknown target %q (configured: %v)", out.Target, cfg.Names())
				}
			}
		}
	default:
		out.Selector = in.Model
		if out.Selector == "" || d.Features.Sensitive {
			// A model hint could name any endpoint, OpenRouter included;
			// sensitive prompts always take the lane's own alias.
			if in.Model != "" && d.Features.Sensitive {
				out.Note = joinNote(out.Note, "model hint ignored for sensitive content; using the lane alias")
			}
			out.Selector = aliases[d.Lane]
		}
	}

	if !out.DryRun {
		var err error
		switch d.Lane {
		case ccjobs.LaneSubscription:
			if cfg == nil {
				return nil, errors.New("subscription lane unavailable: " + out.Note)
			}
			err = t.launch(ctx, tc, cfg, in, out)
		default:
			err = t.startRun(ctx, tc, in, out)
		}
		if err != nil {
			t.record(ctx, tc, in, out)
			return nil, err
		}
		out.Dispatched = true
	}
	t.record(ctx, tc, in, out)
	return out, nil
}

// launch starts the official claude in a tmux window on the chosen
// target and watches it for StartupGrace. The handle is returned either
// way; an immediate exit is reported in the note so the user attaches.
func (t *Tool) launch(ctx context.Context, tc agent.ToolCtx, cfg *ccjobs.Config, in Input, out *Output) error {
	l, err := cfg.Launcher(out.Target)
	if err != nil {
		return err
	}
	job := ccjobs.Job{Lane: ccjobs.LaneSubscription, Target: out.Target, Cwd: in.Cwd, Model: in.Model, Prompt: in.Prompt}
	h, err := l.Launch(ctx, job)
	if err != nil {
		return err
	}
	out.Handle = &h
	dead := false
	if err := l.CheckStartup(ctx, h, StartupGrace); err != nil {
		var ex *ccjobs.ExitedError
		if errors.As(err, &ex) {
			dead = true
			out.Note = joinNote(out.Note, err.Error())
		} else {
			out.Note = joinNote(out.Note, "startup check failed: "+err.Error())
		}
	}
	// The web tab lists the handle (never the prompt); a failed write is
	// a note, not a failed launch, since the window exists either way.
	if t.Jobs != nil {
		job.Prompt = ""
		if err := t.Jobs.Launched(context.WithoutCancel(ctx), tc.UserID, h, job, dead); err != nil {
			out.Note = joinNote(out.Note, "job tab not updated: "+err.Error())
		}
	}
	out.Note = joinNote(out.Note, fmt.Sprintf("attach with: wsj attach --on %s %s", h.Target, h.ID))
	return nil
}

// startRun creates a conversation in the caller's project, posts the
// prompt as its first message and queues a run under the lane's
// selector. The run's output lands in that conversation; the caller only
// gets the ids.
func (t *Tool) startRun(ctx context.Context, tc agent.ToolCtx, in Input, out *Output) error {
	if t.DB == nil || t.Runtime == nil {
		return errors.New("ws runs are not available from this tool instance")
	}
	parent, err := t.DB.GetConversation(ctx, tc.ConversationID)
	if err != nil {
		return fmt.Errorf("conversation: %w", err)
	}
	conv, err := t.DB.CreateConversation(ctx, store.CreateConversationParams{
		ProjectID: parent.ProjectID, UserID: tc.UserID, Title: "job (" + out.Decision.Lane + "): " + titleFor(in.Prompt),
		Mode: "chat", ModelSelector: out.Selector, Settings: json.RawMessage(`{}`),
	})
	if err != nil {
		return fmt.Errorf("create conversation: %w", err)
	}
	parts, _ := json.Marshal([]gateway.Part{gateway.TextPart(in.Prompt)})
	seq, err := t.DB.NextMessageSeq(ctx, conv.ID)
	if err != nil {
		return err
	}
	if _, err := t.DB.InsertMessage(ctx, store.InsertMessageParams{ConversationID: conv.ID, Seq: int64(seq), Role: "user", Parts: parts}); err != nil {
		return fmt.Errorf("insert message: %w", err)
	}
	run, err := t.Runtime.Create(ctx, agent.StartParams{
		ConversationID: conv.ID, UserID: tc.UserID,
		Request: agent.Request{Selector: out.Selector, TaskClass: gateway.TaskChat, ToolAllow: RunTools},
	})
	if err != nil {
		return fmt.Errorf("create run: %w", err)
	}
	out.RunID, out.ConversationID = &run.ID, &conv.ID
	bg := context.WithoutCancel(ctx)
	if t.Enqueue != nil {
		if err := t.Enqueue(bg, run.ID); err != nil {
			return fmt.Errorf("enqueue run: %w", err)
		}
	} else {
		go func() {
			if err := t.Runtime.Drive(bg, run.ID, nil); err != nil && !errors.Is(err, agent.ErrPaused) && t.Log != nil {
				t.Log.Warn("spawn_job run", "run", run.ID, "err", err)
			}
		}()
	}
	out.Note = joinNote(out.Note, "the task runs in conversation "+conv.ID.String())
	return nil
}

// record writes the decision row. The prompt is stored as a sha256 only.
func (t *Tool) record(ctx context.Context, tc agent.ToolCtx, in Input, out *Output) {
	if t.DB == nil {
		return
	}
	features, _ := json.Marshal(out.Decision.Features)
	p := store.InsertCCRouteDecisionParams{
		UserID:         uuid.NullUUID{UUID: tc.UserID, Valid: tc.UserID != uuid.Nil},
		ConversationID: uuid.NullUUID{UUID: tc.ConversationID, Valid: tc.ConversationID != uuid.Nil},
		PromptSha256:   ccjobs.PromptSHA256(in.Prompt),
		Features:       features,
		Lane:           out.Decision.Lane,
		Rule:           out.Decision.Rule,
		Reason:         out.Decision.Reason,
		DryRun:         out.DryRun,
	}
	if out.Target != "" {
		p.Target = &out.Target
	}
	if m := firstNonEmpty(in.Model, out.Selector); m != "" {
		p.Model = &m
	}
	if out.Handle != nil {
		p.JobID = &out.Handle.ID
	}
	if out.RunID != nil {
		p.RunID = uuid.NullUUID{UUID: *out.RunID, Valid: true}
	}
	row, err := t.DB.InsertCCRouteDecision(context.WithoutCancel(ctx), p)
	if err != nil {
		if t.Log != nil {
			t.Log.Warn("cc_route_decisions insert", "err", err)
		}
		return
	}
	out.DecisionID = &row.ID
}

// titleFor is the prompt's first words, cut at a rune boundary so a
// multibyte character at the cut never yields invalid UTF-8.
func titleFor(prompt string) string {
	title := strings.Join(strings.Fields(prompt), " ")
	const max = 60
	if utf8.RuneCountInString(title) <= max {
		return title
	}
	runes := []rune(title)
	return string(runes[:max-3]) + "..."
}

func joinNote(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "; " + b
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
