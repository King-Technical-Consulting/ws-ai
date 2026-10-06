package gateway

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeBudgets struct {
	limits map[string][]BudgetLimit
	spent  map[string]float64
}

func (f *fakeBudgets) Limits(_ context.Context, scope, id string) ([]BudgetLimit, error) {
	return f.limits[scope+":"+id], nil
}
func (f *fakeBudgets) Spent(_ context.Context, scope, id string, _ time.Time) (float64, error) {
	return f.spent[scope+":"+id], nil
}

func TestBudgetBlocksAndDowngrades(t *testing.T) {
	fb := &fakeBudgets{
		limits: map[string][]BudgetLimit{
			"user:u1":   {{Scope: "user", ScopeID: "u1", Period: "month", LimitUSD: 10, OnExceed: "downgrade"}},
			"api_key:k": {{Scope: "api_key", ScopeID: "k", Period: "day", LimitUSD: 1, OnExceed: "block"}},
		},
		spent: map[string]float64{"user:u1": 12, "api_key:k": 0.5},
	}
	mw := BudgetMiddleware(fb, nil)

	in := RouteInput{}
	err := mw(context.Background(), &Request{Metadata: Metadata{UserID: "u1"}}, &in)
	if err != nil || !in.Downgrade {
		t.Errorf("user over downgrade budget: err=%v downgrade=%v", err, in.Downgrade)
	}

	in = RouteInput{}
	err = mw(context.Background(), &Request{Metadata: Metadata{UserID: "u2", APIKeyID: "k"}}, &in)
	if err != nil || in.Downgrade {
		t.Errorf("under budget should pass: err=%v downgrade=%v", err, in.Downgrade)
	}

	fb.spent["api_key:k"] = 1.0
	mw2 := BudgetMiddleware(fb, nil) // fresh cache
	in = RouteInput{}
	err = mw2(context.Background(), &Request{Metadata: Metadata{UserID: "u2", APIKeyID: "k"}}, &in)
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Errorf("blocking budget should error, got %v", err)
	}
}

func TestPeriodStart(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	if PeriodStart("day", now) != now.Add(-24*time.Hour) {
		t.Error("day")
	}
	if !PeriodStart("total", now).IsZero() {
		t.Error("total should be zero time")
	}
}
