package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jking323/ws/internal/gateway"
)

func planRuntime() *Runtime {
	return &Runtime{Tools: NewRegistry(ReadBlobTool{}, &WebFetchTool{}, AskUserTool{}, SubmitPlanTool{}, PlanStepTool{})}
}

func toolNames(defs []gateway.ToolDef) string {
	var n []string
	for _, d := range defs {
		n = append(n, d.Name)
	}
	return strings.Join(n, ",")
}

func userMsg() gateway.Message {
	return gateway.Message{Role: gateway.RoleUser, Parts: []gateway.Part{gateway.TextPart("do it")}}
}

func planResult(text string, isErr bool) gateway.Message {
	return gateway.Message{Role: gateway.RoleTool, Parts: []gateway.Part{toolResult("c1", text, isErr)}}
}

func TestPlanToolsOffUnlessPlanMode(t *testing.T) {
	rt := planRuntime()
	g := &gateway.Request{System: "base", Tools: rt.Tools.Defs(nil)}
	got := rt.planTools(g, Request{}, []gateway.Message{userMsg()})
	if n := toolNames(got); strings.Contains(n, "submit_plan") || strings.Contains(n, "plan_step") {
		t.Errorf("plan tools advertised without plan mode: %s", n)
	}
	if g.System != "base" {
		t.Errorf("system changed without plan mode: %q", g.System)
	}
}

func TestPlanToolsFirstPhaseOffersOnlySubmitPlan(t *testing.T) {
	rt := planRuntime()
	g := &gateway.Request{System: "base", Tools: rt.Tools.Defs(nil)}
	got := rt.planTools(g, Request{Plan: true}, []gateway.Message{userMsg()})
	if toolNames(got) != "submit_plan" {
		t.Fatalf("tools = %s, want only submit_plan", toolNames(got))
	}
	for _, want := range []string{"Plan mode is on", "web_fetch", "read_blob"} {
		if !strings.Contains(g.System, want) {
			t.Errorf("system lacks %q: %q", want, g.System)
		}
	}
	if strings.Contains(g.System, "plan_step:") || strings.Contains(g.System, "submit_plan:") {
		t.Error("the plan tools should not be listed as tools the plan may use")
	}
}

func TestPlanToolsAfterApproval(t *testing.T) {
	rt := planRuntime()
	hist := []gateway.Message{userMsg(), planResult(planApprovedPrefix+". Carry it out.", false)}
	g := &gateway.Request{System: "base", Tools: rt.Tools.Defs(nil)}
	got := rt.planTools(g, Request{Plan: true}, hist)
	n := toolNames(got)
	if strings.Contains(n, "submit_plan") || !strings.Contains(n, "plan_step") || !strings.Contains(n, "web_fetch") {
		t.Errorf("tools after approval = %s", n)
	}
	if g.System != "base" {
		t.Errorf("system should be untouched after approval: %q", g.System)
	}
}

func TestPlanApprovedIsPerTurn(t *testing.T) {
	approved := planResult(planApprovedPrefix+" with changes.", false)
	// A new user message after an approved plan starts a new turn that needs its own plan.
	if planApproved([]gateway.Message{userMsg(), approved, userMsg()}) {
		t.Error("an approval from an earlier turn must not carry over")
	}
	if !planApproved([]gateway.Message{userMsg(), approved}) {
		t.Error("an approval in this turn should count")
	}
	if planApproved([]gateway.Message{userMsg(), planResult("The user rejected this plan.", true)}) {
		t.Error("a rejection is not an approval")
	}
}

func TestParsePlan(t *testing.T) {
	good := json.RawMessage(`{"summary":"s","steps":[{"text":"a"},{"text":"b","tools":["web_fetch"]}]}`)
	if p, err := ParsePlan(good); err != nil || len(p.Steps) != 2 {
		t.Fatalf("good plan: %v %+v", err, p)
	}
	for name, in := range map[string]string{
		"no steps":   `{"summary":"s","steps":[]}`,
		"empty step": `{"summary":"s","steps":[{"text":"  "}]}`,
		"not json":   `nope`,
	} {
		if _, err := ParsePlan(json.RawMessage(in)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	var many []string
	for i := 0; i <= MaxPlanSteps; i++ {
		many = append(many, `{"text":"x"}`)
	}
	if _, err := ParsePlan(json.RawMessage(`{"summary":"s","steps":[` + strings.Join(many, ",") + `]}`)); err == nil {
		t.Error("more than MaxPlanSteps should be refused")
	}
}

func TestSubmitPlanIsAskByDefault(t *testing.T) {
	if (SubmitPlanTool{}).DefaultPolicy() != PolicyAsk {
		t.Fatal("submit_plan must wait for the person")
	}
	if (PlanStepTool{}).DefaultPolicy() != PolicyAuto {
		t.Fatal("plan_step must not ask")
	}
}
