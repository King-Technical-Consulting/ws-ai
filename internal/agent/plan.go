package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jking323/ws/internal/gateway"
)

// Plan mode (M13 cut 2). A run with Request.Plan set offers the model one
// tool, submit_plan, until a plan has been approved in the current turn.
// submit_plan's policy is ask, so M3's approval pauses the run before any
// other tool runs. Approve lets the full tool set through; reject ends the
// turn; an edited plan comes back as the approval note and is the plan the
// model is told to follow. plan_step lets the model tick steps off for the
// checklist the client derives from the submit_plan call.
const (
	ToolSubmitPlan = "submit_plan"
	ToolPlanStep   = "plan_step"
)

// MaxPlanSteps bounds a plan so the card and the checklist stay readable.
const MaxPlanSteps = 20

// SubmitPlanTool is the plan itself: the call's arguments are the plan, the
// approval is the person's decision on it.
type SubmitPlanTool struct{}

func (SubmitPlanTool) Def() gateway.ToolDef {
	return gateway.ToolDef{
		Name:        ToolSubmitPlan,
		Description: "Submit the plan for this request before doing anything else. List the steps you will take in order and the tools each step needs. The person approves, edits or rejects it; nothing else runs until they do.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string","description":"One sentence: what this plan achieves."},"steps":{"type":"array","minItems":1,"maxItems":20,"items":{"type":"object","properties":{"text":{"type":"string","description":"What this step does."},"tools":{"type":"array","items":{"type":"string"},"description":"Names of the tools this step will call, if any."}},"required":["text"],"additionalProperties":false}}},"required":["summary","steps"],"additionalProperties":false}`),
	}
}
func (SubmitPlanTool) DefaultPolicy() Policy { return PolicyAsk }
func (SubmitPlanTool) Idempotent() bool      { return true }
func (SubmitPlanTool) Call(ctx context.Context, tc ToolCtx, args json.RawMessage) (Result, error) {
	if _, err := ParsePlan(args); err != nil {
		return ErrorResult(err), nil
	}
	return Result{Text: "The plan was approved. Carry it out now, in order, and call plan_step as each step finishes."}, nil
}

// PlanStepTool marks a plan step done or skipped. It changes nothing but the
// client's checklist.
type PlanStepTool struct{}

func (PlanStepTool) Def() gateway.ToolDef {
	return gateway.ToolDef{
		Name:        ToolPlanStep,
		Description: "Mark a step of the approved plan as done or skipped. Steps are numbered from 1 in the order the plan lists them.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"step":{"type":"integer","minimum":1},"status":{"type":"string","enum":["done","skipped"]}},"required":["step","status"],"additionalProperties":false}`),
	}
}
func (PlanStepTool) DefaultPolicy() Policy { return PolicyAuto }
func (PlanStepTool) Idempotent() bool      { return true }
func (PlanStepTool) Call(ctx context.Context, tc ToolCtx, args json.RawMessage) (Result, error) {
	var in struct {
		Step   int    `json:"step"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(args, &in); err != nil || in.Step < 1 || (in.Status != "done" && in.Status != "skipped") {
		return ErrorResult(fmt.Errorf("step must be a number from 1 and status done or skipped")), nil
	}
	return Result{Text: fmt.Sprintf("Step %d marked %s.", in.Step, in.Status)}, nil
}

// Plan is the argument of submit_plan.
type Plan struct {
	Summary string     `json:"summary"`
	Steps   []PlanStep `json:"steps"`
}

// PlanStep is one line of a plan.
type PlanStep struct {
	Text  string   `json:"text"`
	Tools []string `json:"tools,omitempty"`
}

// ParsePlan validates submit_plan's arguments.
func ParsePlan(args json.RawMessage) (*Plan, error) {
	var p Plan
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, fmt.Errorf("the plan is not valid JSON: %v", err)
	}
	if len(p.Steps) == 0 {
		return nil, fmt.Errorf("a plan needs at least one step")
	}
	if len(p.Steps) > MaxPlanSteps {
		return nil, fmt.Errorf("a plan has at most %d steps", MaxPlanSteps)
	}
	for i, s := range p.Steps {
		if strings.TrimSpace(s.Text) == "" {
			return nil, fmt.Errorf("step %d has no text", i+1)
		}
	}
	return &p, nil
}

func isPlanTool(name string) bool { return name == ToolSubmitPlan || name == ToolPlanStep }

// planApproved reports whether the turn in progress (everything after the
// last user message) already holds a successful submit_plan result.
func planApproved(history []gateway.Message) bool {
	for i := len(history) - 1; i >= 0; i-- {
		m := history[i]
		if m.Role == gateway.RoleUser {
			return false
		}
		if m.Role != gateway.RoleTool {
			continue
		}
		for _, p := range m.Parts {
			if p.Kind != gateway.PartToolResult || p.IsError {
				continue
			}
			if strings.HasPrefix(resultText(p), planApprovedPrefix) {
				return true
			}
		}
	}
	return false
}

const planApprovedPrefix = "The plan was approved"

func resultText(p gateway.Part) string {
	var b strings.Builder
	for _, c := range p.Content {
		if c.Kind == gateway.PartText {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// planSystem is appended to the system prompt while the model has to plan.
// It names the tools the plan may use, since the model is offered only
// submit_plan in this phase.
func planSystem(defs []gateway.ToolDef) string {
	var b strings.Builder
	b.WriteString("\n\nPlan mode is on. Do not do the work yet. First call submit_plan with a one-sentence summary and the numbered steps you will take, naming the tools each step needs. The person reviews it before anything runs. If the request needs no tools and no steps, just answer it.")
	var names []string
	for _, d := range defs {
		if !isPlanTool(d.Name) {
			names = append(names, d.Name+": "+firstSentence(d.Description))
		}
	}
	if len(names) > 0 {
		b.WriteString(" After approval these tools are available:\n- " + strings.Join(names, "\n- "))
	}
	return b.String()
}

func firstSentence(s string) string {
	if i := strings.Index(s, ". "); i > 0 {
		return s[:i+1]
	}
	return s
}
