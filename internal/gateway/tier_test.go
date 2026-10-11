package gateway

import (
	"strings"
	"testing"
)

func TestEffectiveTierFromTheModelName(t *testing.T) {
	cases := []struct {
		model string
		tier  string
		want  string
	}{
		{"qwen2.5-0.5b-instruct", "", TierSmall},
		{"Qwen/Qwen3-4B", "", TierMedium},
		{"qwen3-14b", "", TierMedium},
		{"gpt-oss-20b", "", TierMedium},
		{"qwen3.6-35b-a3b", "", TierLarge}, // the total, not the active figure
		{"gpt-oss-120b", "", TierLarge},
		{"claude-sonnet-5-5", "", TierMedium}, // no count in the name
		{"deepseek-chat-v3-0324", "", TierMedium},
		{"llama-3.1-8b", "", TierMedium},
		{"anything-0.5b", TierLarge, TierLarge}, // declared wins
	}
	for _, c := range cases {
		e := &Endpoint{ID: "p/" + c.model, ModelName: c.model, Tier: c.tier}
		if got := e.EffectiveTier(); got != c.want {
			t.Errorf("%s (tier %q) = %s, want %s", c.model, c.tier, got, c.want)
		}
	}
}

func tierRegistry() *Registry {
	reg := NewRegistry()
	reg.Replace(
		[]*Provider{{ID: "local", Kind: ProviderOpenAICompat}, {ID: "hosted", Kind: ProviderOpenAICompat, Hosted: true}},
		[]*Endpoint{
			// The policy names this one; it is down, so the fallback decides.
			{ID: "hosted/big", ProviderID: "hosted", ModelName: "big", Enabled: true, Health: Health{Status: "down"},
				Capabilities: Capabilities{ContextWindow: 100000}},
			{ID: "local/toy", ProviderID: "local", ModelName: "qwen2.5-0.5b-instruct", Enabled: true, Local: true,
				Capabilities: Capabilities{ContextWindow: 4096}},
			{ID: "local/mid", ProviderID: "local", ModelName: "qwen3-14b", Enabled: true, Local: true,
				Capabilities: Capabilities{ContextWindow: 32000}, Pricing: Pricing{InputPerM: 0.5}},
			{ID: "local/judge", ProviderID: "local", ModelName: "judge-70b", Enabled: true, Local: true, NotRoutable: true,
				Capabilities: Capabilities{ContextWindow: 32000}},
		},
	)
	return reg
}

func policyFor(t *testing.T, yaml string) []Policy {
	t.Helper()
	p, err := ParsePolicy(yaml)
	if err != nil {
		t.Fatal(err)
	}
	return []Policy{p}
}

func TestFallbackNeverDropsBelowTheRulesTier(t *testing.T) {
	r := NewRouter(tierRegistry(), policyFor(t, `
name: t
rules:
  - match: {}
    min_tier: medium
    prefer: [hosted/big]
`))
	cands, dec, err := r.Route(RouteInput{Selector: "auto", TaskClass: TaskChat})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].Endpoint.ID != "local/mid" {
		t.Errorf("candidates = %v, want only local/mid (not the toy, not the judge)", dec.Candidates)
	}
	if !strings.HasSuffix(dec.Reason, "-fallback") {
		t.Errorf("reason = %q", dec.Reason)
	}
}

func TestNoFloorMeansTheOldFallback(t *testing.T) {
	r := NewRouter(tierRegistry(), policyFor(t, `
name: t
rules:
  - match: {}
    prefer: [hosted/big]
`))
	cands, _, err := r.Route(RouteInput{Selector: "auto", TaskClass: TaskChat})
	if err != nil {
		t.Fatal(err)
	}
	// Local first, then by price: toy (free) before mid ($0.5), judge never.
	var ids []string
	for _, c := range cands {
		ids = append(ids, c.Endpoint.ID)
	}
	if strings.Join(ids, ",") != "local/toy,local/mid" {
		t.Errorf("fallback = %v", ids)
	}
}

func TestNothingMeetsTheFloorFailsWithTheReason(t *testing.T) {
	reg := tierRegistry()
	r := NewRouter(reg, policyFor(t, `
name: t
rules:
  - match: {}
    min_tier: large
    prefer: [hosted/big]
`))
	_, _, err := r.Route(RouteInput{Selector: "auto", TaskClass: TaskChat})
	if err == nil {
		t.Fatal("a toy model must not answer when the floor is large")
	}
	if !strings.Contains(err.Error(), "no model of the large tier or better is available") {
		t.Errorf("error = %v", err)
	}
}

func TestNotRoutableIsReachedByNameAndByRuleOnly(t *testing.T) {
	reg := tierRegistry()
	// Named by a rule: allowed, whatever its tier.
	named := NewRouter(reg, policyFor(t, `
name: t
rules:
  - match: { task_class: [classify] }
    min_tier: large
    prefer: [local/judge]
  - match: {}
    prefer: [local/mid]
`))
	cands, _, err := named.Route(RouteInput{Selector: "auto", TaskClass: TaskClassify})
	if err != nil || len(cands) == 0 || cands[0].Endpoint.ID != "local/judge" {
		t.Fatalf("named by a rule: %v %v", cands, err)
	}
	// Picked by id: allowed.
	if cands, _, err := named.Route(RouteInput{Selector: "local/judge", TaskClass: TaskChat}); err != nil || len(cands) != 1 {
		t.Errorf("direct pick: %v %v", cands, err)
	}
	// No policy matches: the any-endpoint list leaves it out.
	none := NewRouter(reg, nil)
	cands, _, err = none.Route(RouteInput{Selector: "auto", TaskClass: TaskChat})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cands {
		if c.Endpoint.ID == "local/judge" {
			t.Error("a not-routable endpoint entered the any-endpoint fallback")
		}
	}
}

func TestContextCeilingIgnoresNotRoutableAndBelowFloor(t *testing.T) {
	r := NewRouter(tierRegistry(), policyFor(t, `
name: t
rules:
  - match: {}
    min_tier: medium
    prefer: [hosted/big]
`))
	// hosted/big is down; of the rest only local/mid is routable and medium.
	if got := r.ContextCeiling(RouteInput{Selector: "auto", TaskClass: TaskChat}); got != 32000 {
		t.Errorf("ceiling = %d, want 32000", got)
	}
}
