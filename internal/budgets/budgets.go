// Package budgets sends the warning mails for spending budgets (PLAN.md
// M7): at 80 percent of a limit, and again at the limit. The gateway's
// budget middleware (internal/gateway/budget.go) is what blocks or
// downgrades; this package only tells people before that happens.
package budgets

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
)

// Mailer sends one message; auth.Mailer satisfies it.
type Mailer interface {
	Send(ctx context.Context, to, subject, text, html string) error
}

// Store is what the warning sweep reads and writes; *store.DB implements it.
type Store interface {
	ListAllBudgets(ctx context.Context) ([]store.Budget, error)
	ListUsers(ctx context.Context) ([]store.User, error)
	ListAllAgents(ctx context.Context) ([]store.Agent, error)
	GetUserByID(ctx context.Context, id uuid.UUID) (store.User, error)
	GetAgent(ctx context.Context, id uuid.UUID) (store.Agent, error)
	GetBudgetWarning(ctx context.Context, p store.GetBudgetWarningParams) (store.BudgetWarning, error)
	UpsertBudgetWarning(ctx context.Context, p store.UpsertBudgetWarningParams) error
}

// Service runs the sweep.
type Service struct {
	DB    Store
	Spend gateway.BudgetStore // the ledger sums, as the middleware reads them
	Mail  Mailer
	// PublicURL is the app origin for the links in the mail.
	PublicURL string
	Log       *slog.Logger
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Thresholds are the percentages a warning is sent at, in order.
var Thresholds = []int{80, 100}

// target is one (scope, id) a budget applies to, with the limit it is under.
type target struct {
	scope   string
	id      uuid.NullUUID
	limit   store.Budget
	emails  []string // who is told
	subject string   // what the budget is, for the mail
	link    string
}

// Warn is the hourly sweep: for every budget, and for every member or agent
// a default budget covers, compare spend with the limit and mail the
// people concerned once per threshold per window. Errors on one target are
// logged and the sweep continues; the returned error is only a listing
// failure.
func (s *Service) Warn(ctx context.Context) error {
	if s.Mail == nil {
		return nil
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	targets, err := s.targets(ctx)
	if err != nil {
		return err
	}
	for _, t := range targets {
		if err := s.check(ctx, t, now); err != nil {
			s.log().Warn("budget warn", "scope", t.scope, "id", t.id.UUID, "err", err)
		}
	}
	return nil
}

func (s *Service) log() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}

// targets expands the budget rows into the members, agents and keys they
// apply to. A default row (no scope_id) covers every member or agent that
// has no row of its own for that period.
func (s *Service) targets(ctx context.Context) ([]target, error) {
	rows, err := s.DB.ListAllBudgets(ctx)
	if err != nil {
		return nil, err
	}
	owners, err := s.ownerEmails(ctx)
	if err != nil {
		return nil, err
	}
	var users []store.User
	var agents []store.Agent
	own := map[string]bool{} // scope:id:period that has its own row
	for _, b := range rows {
		if b.ScopeID.Valid {
			own[b.Scope+":"+b.ScopeID.UUID.String()+":"+b.Period] = true
		}
	}
	var out []target
	for _, b := range rows {
		switch {
		case b.Scope == "global":
			out = append(out, target{scope: "global", limit: b, emails: owners, subject: "the shared budget", link: s.PublicURL + "/admin"})
		case b.Scope == "api_key" && b.ScopeID.Valid:
			// A key belongs to a person; the ledger row names the key, not
			// the person, so the owner is told.
			out = append(out, target{scope: "api_key", id: b.ScopeID, limit: b, emails: owners, subject: "an API key's budget", link: s.PublicURL + "/admin"})
		case b.Scope == "user" && b.ScopeID.Valid:
			u, err := s.DB.GetUserByID(ctx, b.ScopeID.UUID)
			if err != nil {
				continue
			}
			out = append(out, userTarget(u, b, s.PublicURL))
		case b.Scope == "user":
			if users == nil {
				if users, err = s.DB.ListUsers(ctx); err != nil {
					return nil, err
				}
			}
			for _, u := range users {
				if u.DisabledAt != nil || own["user:"+u.ID.String()+":"+b.Period] {
					continue
				}
				out = append(out, userTarget(u, b, s.PublicURL))
			}
		case b.Scope == "agent" && b.ScopeID.Valid:
			a, err := s.DB.GetAgent(ctx, b.ScopeID.UUID)
			if err != nil {
				continue
			}
			out = append(out, s.agentTarget(ctx, a, b))
		case b.Scope == "agent":
			if agents == nil {
				if agents, err = s.DB.ListAllAgents(ctx); err != nil {
					return nil, err
				}
			}
			for _, a := range agents {
				if own["agent:"+a.ID.String()+":"+b.Period] {
					continue
				}
				out = append(out, s.agentTarget(ctx, a, b))
			}
		}
	}
	return out, nil
}

func userTarget(u store.User, b store.Budget, base string) target {
	return target{scope: "user", id: uuid.NullUUID{UUID: u.ID, Valid: true}, limit: b, emails: []string{u.Email}, subject: "your budget", link: base + "/settings"}
}

func (s *Service) agentTarget(ctx context.Context, a store.Agent, b store.Budget) target {
	t := target{scope: "agent", id: uuid.NullUUID{UUID: a.ID, Valid: true}, limit: b, subject: fmt.Sprintf("the budget of the agent %q", a.Name), link: s.PublicURL + "/agents/" + a.ID.String()}
	if u, err := s.DB.GetUserByID(ctx, a.OwnerID); err == nil {
		t.emails = []string{u.Email}
	}
	return t
}

func (s *Service) ownerEmails(ctx context.Context) ([]string, error) {
	users, err := s.DB.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, u := range users {
		if u.Role == "owner" && u.DisabledAt == nil {
			out = append(out, u.Email)
		}
	}
	return out, nil
}

// check compares one target's spend with its limit and sends what is due.
func (s *Service) check(ctx context.Context, t target, now time.Time) error {
	if t.limit.LimitUsd <= 0 || len(t.emails) == 0 {
		return nil
	}
	id := ""
	if t.id.Valid {
		id = t.id.UUID.String()
	}
	spent, err := s.Spend.Spent(ctx, t.scope, id, gateway.PeriodStart(t.limit.Period, now))
	if err != nil {
		return err
	}
	pct := int(spent / t.limit.LimitUsd * 100)
	for _, th := range Thresholds {
		if pct < th {
			break
		}
		fresh, err := s.alreadySent(ctx, t, th, now)
		if err != nil {
			return err
		}
		if fresh {
			continue
		}
		if err := s.send(ctx, t, th, spent); err != nil {
			return err
		}
		if err := s.DB.UpsertBudgetWarning(ctx, store.UpsertBudgetWarningParams{Scope: t.scope, ScopeID: t.id, Period: t.limit.Period, Pct: int32(th), SpentUsd: spent, LimitUsd: t.limit.LimitUsd}); err != nil {
			return err
		}
	}
	return nil
}

// alreadySent is true when this threshold was mailed within the budget's
// window (a total budget is mailed once).
func (s *Service) alreadySent(ctx context.Context, t target, th int, now time.Time) (bool, error) {
	w, err := s.DB.GetBudgetWarning(ctx, store.GetBudgetWarningParams{Scope: t.scope, ScopeID: t.id, Period: t.limit.Period, Pct: int32(th)})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	start := gateway.PeriodStart(t.limit.Period, now)
	if start.IsZero() {
		return true, nil
	}
	return w.SentAt.After(start), nil
}

func (s *Service) send(ctx context.Context, t target, th int, spent float64) error {
	subject, text := Message(t.subject, t.limit, th, spent, t.link)
	var errs []string
	for _, to := range t.emails {
		if err := s.Mail.Send(ctx, to, subject, text, ""); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// Message is the mail for one threshold: what is spent of what, and what
// happens at the limit.
func Message(what string, b store.Budget, th int, spent float64, link string) (subject, text string) {
	window := "per " + b.Period
	if b.Period == "total" {
		window = "in total"
	}
	atLimit := "requests fall back to local models"
	if b.OnExceed == "block" {
		atLimit = "requests are refused"
	}
	if th >= 100 {
		subject = fmt.Sprintf("ws: %s is spent", what)
		text = fmt.Sprintf("%s is spent: $%.2f of $%.2f %s. Until the window moves on, %s.\n\n%s\n", capital(what), spent, b.LimitUsd, window, atLimit, link)
		return
	}
	subject = fmt.Sprintf("ws: %s is %d%% spent", what, th)
	text = fmt.Sprintf("%s is %d%% spent: $%.2f of $%.2f %s. At the limit, %s.\n\n%s\n", capital(what), th, spent, b.LimitUsd, window, atLimit, link)
	return
}

func capital(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
