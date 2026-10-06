package ccjobs

import (
	"fmt"
	"time"
)

// Budget is the week's subscription pool as the router sees it (spec §6.4,
// M6). Claude's own usage is never read; Used is the number of launches
// ws knows of this week (cc_launch_counter), Cap the soft weekly cap from
// WS_CC_WEEKLY_CAP (0: count only, never downgrade), SoftPct the fill
// level from which low-value work stops going to the subscription.
type Budget struct {
	// Week is the start of the counted week, Monday 00:00 UTC.
	Week    time.Time `json:"week"`
	Used    int       `json:"used"`
	Cap     int       `json:"cap"`
	SoftPct int       `json:"soft_pct"`
}

// Budget levels.
const (
	BudgetOK   = "ok"   // below the soft threshold, or no cap
	BudgetSoft = "soft" // soft threshold reached: only clearly repo-bound work launches
	BudgetFull = "full" // cap reached: only a lane the caller asked for launches
)

// WeekStart is the Monday 00:00 UTC on or before t.
func WeekStart(t time.Time) time.Time {
	t = t.UTC()
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	back := (int(d.Weekday()) + 6) % 7 // Monday 0 … Sunday 6
	return d.AddDate(0, 0, -back)
}

// Level says how full the pool is.
func (b Budget) Level() string {
	if b.Cap <= 0 {
		return BudgetOK
	}
	if b.Used >= b.Cap {
		return BudgetFull
	}
	soft := b.SoftPct
	if soft <= 0 || soft > 100 {
		soft = 75
	}
	if b.Used*100 >= b.Cap*soft {
		return BudgetSoft
	}
	return BudgetOK
}

func (b Budget) String() string {
	if b.Cap <= 0 {
		return fmt.Sprintf("%d launches this week, no cap", b.Used)
	}
	return fmt.Sprintf("%d of %d launches this week", b.Used, b.Cap)
}

// highValue says a subscription decision is clearly repo-bound work: a
// working directory was given or several files are named. Everything
// else that the rules or the classifier sent to the subscription (long
// agentic prompts, code fences with edit verbs, classifier verdicts) is
// the low-value end that a filling pool keeps off it.
func highValue(d Decision) bool {
	return d.Features.HasCwd || d.Features.FilePaths >= 2
}

// ApplyBudget is the soft cap. A subscription decision is sent to the
// api lane instead when the pool is soft and the work is not clearly
// repo-bound, or when the pool is full; forced says the caller asked for
// the lane (a lane argument or an @claude override), which the cap never
// overrides, only notes. The returned note explains either outcome and is
// empty when nothing applied. Other lanes are untouched.
func ApplyBudget(d Decision, forced bool, b Budget) (Decision, string) {
	if d.Lane != LaneSubscription {
		return d, ""
	}
	level := b.Level()
	if level == BudgetOK {
		return d, ""
	}
	if forced {
		return d, fmt.Sprintf("weekly subscription pool %s (%s); launching anyway because the lane was asked for", level, b)
	}
	if level == BudgetSoft && highValue(d) {
		return d, ""
	}
	note := fmt.Sprintf("weekly subscription pool %s (%s); %s sent to the api lane instead", level, b, d.Rule)
	d.Lane = LaneAPI
	d.Rule = "budget:" + level
	d.Reason = fmt.Sprintf("weekly subscription pool %s (%s): was %s (%s)", level, b, LaneSubscription, d.Reason)
	return d, note
}
