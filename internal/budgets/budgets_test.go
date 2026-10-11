package budgets

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
)

type fakeStore struct {
	budgets  []store.Budget
	users    []store.User
	agents   []store.Agent
	warnings map[string]store.BudgetWarning
}

func (f *fakeStore) ListAllBudgets(context.Context) ([]store.Budget, error) { return f.budgets, nil }
func (f *fakeStore) ListUsers(context.Context) ([]store.User, error)       { return f.users, nil }
func (f *fakeStore) ListAllAgents(context.Context) ([]store.Agent, error)   { return f.agents, nil }
func (f *fakeStore) GetUserByID(_ context.Context, id uuid.UUID) (store.User, error) {
	for _, u := range f.users {
		if u.ID == id {
			return u, nil
		}
	}
	return store.User{}, pgx.ErrNoRows
}
func (f *fakeStore) GetAgent(_ context.Context, id uuid.UUID) (store.Agent, error) {
	for _, a := range f.agents {
		if a.ID == id {
			return a, nil
		}
	}
	return store.Agent{}, pgx.ErrNoRows
}
func warnKey(scope string, id uuid.NullUUID, period string, pct int32) string {
	return scope + ":" + id.UUID.String() + ":" + period + ":" + string(rune('0'+pct/10))
}
func (f *fakeStore) GetBudgetWarning(_ context.Context, p store.GetBudgetWarningParams) (store.BudgetWarning, error) {
	w, ok := f.warnings[warnKey(p.Scope, p.ScopeID, p.Period, p.Pct)]
	if !ok {
		return store.BudgetWarning{}, pgx.ErrNoRows
	}
	return w, nil
}
func (f *fakeStore) UpsertBudgetWarning(_ context.Context, p store.UpsertBudgetWarningParams) error {
	if f.warnings == nil {
		f.warnings = map[string]store.BudgetWarning{}
	}
	f.warnings[warnKey(p.Scope, p.ScopeID, p.Period, p.Pct)] = store.BudgetWarning{Scope: p.Scope, ScopeID: p.ScopeID, Period: p.Period, Pct: p.Pct, SentAt: time.Now()}
	return nil
}

// fakeSpend is the ledger sum per scope:id; Limits is unused by the sweep.
type fakeSpend map[string]float64

func (f fakeSpend) Spent(_ context.Context, scope, id string, _ time.Time) (float64, error) {
	return f[scope+":"+id], nil
}
func (f fakeSpend) Limits(context.Context, string, string) ([]gateway.BudgetLimit, error) {
	return nil, nil
}

type mail struct{ to, subject, text string }
type fakeMail struct{ sent []mail }

func (m *fakeMail) Send(_ context.Context, to, subject, text, _ string) error {
	m.sent = append(m.sent, mail{to, subject, text})
	return nil
}

func TestWarnMailsTheMemberOnceAtEightyAndAgainAtTheLimit(t *testing.T) {
	owner := store.User{ID: uuid.New(), Email: "owner@example.com", Role: "owner"}
	alice := store.User{ID: uuid.New(), Email: "alice@example.com", Role: "member"}
	st := &fakeStore{
		users:   []store.User{owner, alice},
		budgets: []store.Budget{{Scope: "user", Period: "month", LimitUsd: 10, OnExceed: "block"}}, // the default for every member
	}
	spend := fakeSpend{"user:" + alice.ID.String(): 8.5}
	m := &fakeMail{}
	svc := &Service{DB: st, Spend: spend, Mail: m, PublicURL: "https://app.example.com"}

	if err := svc.Warn(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m.sent) != 1 || m.sent[0].to != alice.Email {
		t.Fatalf("expected one mail to alice at 85%%, got %+v", m.sent)
	}
	if !strings.Contains(m.sent[0].text, "$8.50 of $10.00 per month") || !strings.Contains(m.sent[0].text, "requests are refused") {
		t.Errorf("mail text: %q", m.sent[0].text)
	}
	// The same sweep an hour later sends nothing new.
	if err := svc.Warn(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(m.sent) != 1 {
		t.Fatalf("a second sweep mailed again: %+v", m.sent)
	}
	// Over the limit: the at-the-limit mail, once.
	spend["user:"+alice.ID.String()] = 11
	_ = svc.Warn(context.Background())
	_ = svc.Warn(context.Background())
	if len(m.sent) != 2 || !strings.Contains(m.sent[1].subject, "is spent") {
		t.Fatalf("expected the at-the-limit mail once, got %+v", m.sent)
	}
	// The owner, under the same default, spent nothing and hears nothing.
	for _, s := range m.sent {
		if s.to == owner.Email {
			t.Errorf("the owner was mailed about a budget they have not touched")
		}
	}
}

func TestWarnSkipsTheDefaultForAMemberWithTheirOwnRowAndTellsTheAgentOwner(t *testing.T) {
	owner := store.User{ID: uuid.New(), Email: "owner@example.com", Role: "owner"}
	bob := store.User{ID: uuid.New(), Email: "bob@example.com", Role: "member"}
	ag := store.Agent{ID: uuid.New(), OwnerID: bob.ID, Name: "nightly"}
	st := &fakeStore{
		users:  []store.User{owner, bob},
		agents: []store.Agent{ag},
		budgets: []store.Budget{
			{Scope: "user", Period: "month", LimitUsd: 10},                                                     // default: bob is over it, but
			{Scope: "user", ScopeID: uuid.NullUUID{UUID: bob.ID, Valid: true}, Period: "month", LimitUsd: 100}, // bob's own row is far from spent
			{Scope: "agent", Period: "month", LimitUsd: 5, OnExceed: "downgrade"},                              // default for every agent
			{Scope: "global", Period: "day", LimitUsd: 1},
		},
	}
	spend := fakeSpend{"user:" + bob.ID.String(): 20, "agent:" + ag.ID.String(): 4.2, "global:": 0.9}
	m := &fakeMail{}
	svc := &Service{DB: st, Spend: spend, Mail: m}
	if err := svc.Warn(context.Background()); err != nil {
		t.Fatal(err)
	}
	var toBob, toOwner int
	for _, s := range m.sent {
		switch s.to {
		case bob.Email:
			toBob++
			if !strings.Contains(s.subject, `the agent "nightly"`) {
				t.Errorf("bob's mail should be about his agent, got %q", s.subject)
			}
		case owner.Email:
			toOwner++
			if !strings.Contains(s.subject, "shared budget") {
				t.Errorf("the owner's mail should be about the shared budget, got %q", s.subject)
			}
		}
	}
	if toBob != 1 || toOwner != 1 {
		t.Fatalf("expected one mail to bob (agent at 84%%) and one to the owner (global at 90%%), got %+v", m.sent)
	}
}

func TestMessageWording(t *testing.T) {
	b := store.Budget{Period: "total", LimitUsd: 50, OnExceed: "downgrade"}
	subj, text := Message("your budget", b, 80, 41, "https://app.example.com/settings")
	if subj != "ws: your budget is 80% spent" {
		t.Errorf("subject %q", subj)
	}
	if !strings.Contains(text, "$41.00 of $50.00 in total") || !strings.Contains(text, "fall back to local models") {
		t.Errorf("text %q", text)
	}
}
