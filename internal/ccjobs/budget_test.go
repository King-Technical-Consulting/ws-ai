package ccjobs

import (
	"strings"
	"testing"
	"time"
)

func TestWeekStart(t *testing.T) {
	cases := map[string]string{
		"2026-10-05T16:00:00Z":      "2026-10-05T00:00:00Z", // a Monday
		"2026-10-11T23:59:59Z":      "2026-10-05T00:00:00Z", // the Sunday after
		"2026-10-12T00:00:00Z":      "2026-10-12T00:00:00Z", // next Monday
		"2026-10-07T03:00:00-05:00": "2026-10-05T00:00:00Z", // local time, UTC week
	}
	for in, want := range cases {
		ti, _ := time.Parse(time.RFC3339, in)
		if got := WeekStart(ti).Format(time.RFC3339); got != want {
			t.Errorf("WeekStart(%s) = %s want %s", in, got, want)
		}
	}
}

func TestBudgetLevel(t *testing.T) {
	cases := []struct {
		b    Budget
		want string
	}{
		{Budget{Used: 999}, BudgetOK},                        // no cap: count only
		{Budget{Used: 0, Cap: 40, SoftPct: 75}, BudgetOK},    // empty
		{Budget{Used: 29, Cap: 40, SoftPct: 75}, BudgetOK},   // just under 75%
		{Budget{Used: 30, Cap: 40, SoftPct: 75}, BudgetSoft}, // exactly 75%
		{Budget{Used: 39, Cap: 40, SoftPct: 75}, BudgetSoft}, // under the cap
		{Budget{Used: 40, Cap: 40, SoftPct: 75}, BudgetFull}, // at the cap
		{Budget{Used: 41, Cap: 40, SoftPct: 75}, BudgetFull}, // over
		{Budget{Used: 30, Cap: 40}, BudgetSoft},              // SoftPct 0 means 75
		{Budget{Used: 20, Cap: 40, SoftPct: 50}, BudgetSoft}, // tuned threshold
		{Budget{Used: 20, Cap: 40, SoftPct: 101}, BudgetOK},  // bad value means 75; 50% is under it
	}
	for _, c := range cases {
		if got := c.b.Level(); got != c.want {
			t.Errorf("%+v: level %s want %s", c.b, got, c.want)
		}
	}
}

// TestApplyBudget: the soft cap keeps low-value work off the subscription
// as the week fills, never touches other lanes, and never overrides a
// lane the caller asked for.
func TestApplyBudget(t *testing.T) {
	sub := func(rule string, f Features) Decision {
		return Decision{Lane: LaneSubscription, Rule: rule, Reason: "r", Features: f}
	}
	cwd := Features{HasCwd: true}
	paths := Features{FilePaths: 3}
	long := Features{Chars: 2000, Agentic: 2}
	ok := Budget{Used: 1, Cap: 40, SoftPct: 75}
	soft := Budget{Used: 31, Cap: 40, SoftPct: 75}
	full := Budget{Used: 40, Cap: 40, SoftPct: 75}
	none := Budget{Used: 500}

	cases := []struct {
		name   string
		d      Decision
		forced bool
		b      Budget
		lane   string
		rule   string
		noted  string // substring the note must carry; "" means no note
	}{
		{"no cap", sub("repo", long), false, none, LaneSubscription, "repo", ""},
		{"ok pool", sub("classifier:code", long), false, ok, LaneSubscription, "classifier:code", ""},
		{"soft keeps cwd work", sub("repo", cwd), false, soft, LaneSubscription, "repo", ""},
		{"soft keeps multi-file work", sub("repo", paths), false, soft, LaneSubscription, "repo", ""},
		{"soft downgrades long agentic", sub("repo", long), false, soft, LaneAPI, "budget:soft", "sent to the api lane"},
		{"soft downgrades classifier", sub("classifier:code", long), false, soft, LaneAPI, "budget:soft", "sent to the api lane"},
		{"full downgrades cwd work", sub("repo", cwd), false, full, LaneAPI, "budget:full", "pool full"},
		{"full keeps forced", sub("requested", cwd), true, full, LaneSubscription, "requested", "launching anyway"},
		{"soft keeps override", sub("override", long), true, soft, LaneSubscription, "override", "launching anyway"},
		{"other lanes untouched", Decision{Lane: LaneAPI, Rule: "reasoning"}, false, full, LaneAPI, "reasoning", ""},
		{"local untouched", Decision{Lane: LaneLocal, Rule: "sensitive"}, false, full, LaneLocal, "sensitive", ""},
	}
	for _, c := range cases {
		got, note := ApplyBudget(c.d, c.forced, c.b)
		if got.Lane != c.lane || got.Rule != c.rule {
			t.Errorf("%s: lane %s rule %s, want %s %s", c.name, got.Lane, got.Rule, c.lane, c.rule)
		}
		if c.noted == "" && note != "" || c.noted != "" && !strings.Contains(note, c.noted) {
			t.Errorf("%s: note %q want %q", c.name, note, c.noted)
		}
		if got.Lane != c.d.Lane && !strings.Contains(got.Reason, "was claude-subscription (r)") {
			t.Errorf("%s: downgraded reason must keep the original: %q", c.name, got.Reason)
		}
		if got.Features != c.d.Features {
			t.Errorf("%s: features changed", c.name)
		}
	}
}
