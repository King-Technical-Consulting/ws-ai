package store

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/gateway"
)

// BudgetStore implements gateway.BudgetStore on Postgres.
type BudgetStore struct{ DB *DB }

// Limits implements gateway.BudgetStore.
func (b *BudgetStore) Limits(ctx context.Context, scope, scopeID string) ([]gateway.BudgetLimit, error) {
	rows, err := b.DB.ListBudgetsForScope(ctx, ListBudgetsForScopeParams{Scope: scope, ScopeID: parseNullUUID(scopeID)})
	if err != nil {
		return nil, err
	}
	out := make([]gateway.BudgetLimit, 0, len(rows))
	for _, r := range rows {
		out = append(out, gateway.BudgetLimit{Scope: r.Scope, ScopeID: scopeID, Period: r.Period, LimitUSD: r.LimitUsd, OnExceed: r.OnExceed})
	}
	return out, nil
}

// Spent implements gateway.BudgetStore.
func (b *BudgetStore) Spent(ctx context.Context, scope, scopeID string, since time.Time) (float64, error) {
	id := parseNullUUID(scopeID)
	switch scope {
	case "user":
		return b.DB.SumUsageForUserSince(ctx, SumUsageForUserSinceParams{UserID: id, CreatedAt: since})
	case "agent":
		return b.DB.SumUsageForAgentSince(ctx, SumUsageForAgentSinceParams{AgentID: id, CreatedAt: since})
	case "api_key":
		return b.DB.SumUsageForAPIKeySince(ctx, SumUsageForAPIKeySinceParams{ApiKeyID: id, CreatedAt: since})
	}
	return b.DB.SumUsageSince(ctx, since)
}

// RefreshHealth copies endpoint health written by the worker into this
// process's registry. serve and worker are separate processes sharing
// only Postgres, so each polls.
func (d *DB) RefreshHealth(ctx context.Context, reg *gateway.Registry) error {
	rows, err := d.ListEndpoints(ctx)
	if err != nil {
		return err
	}
	for _, r := range rows {
		p50, rate, errText := 0, 0.0, ""
		if r.P50LatencyMs != nil {
			p50 = int(*r.P50LatencyMs)
		}
		if r.ErrorRate != nil {
			rate = float64(*r.ErrorRate)
		}
		if r.HealthError != nil {
			errText = *r.HealthError
		}
		reg.SetHealth(r.ID, r.HealthStatus, p50, rate, errText)
		reg.SetEnabled(r.ID, r.Enabled)
	}
	return nil
}

// ReloadPolicies reads enabled policies and installs them on the router.
func (d *DB) ReloadPolicies(ctx context.Context, router *gateway.Router) error {
	ps, err := d.LoadPolicies(ctx)
	if err != nil {
		return err
	}
	router.SetPolicies(ps)
	return nil
}

var _ = uuid.Nil
