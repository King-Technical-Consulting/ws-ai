package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// ErrBudgetExceeded is returned when a blocking budget is spent.
var ErrBudgetExceeded = errors.New("gateway: budget exceeded")

// BudgetLimit is one spending cap.
type BudgetLimit struct {
	Scope    string // global | user | agent | api_key
	ScopeID  string // empty for global
	Period   string // day | week | month | total
	LimitUSD float64
	OnExceed string // block | downgrade
}

// BudgetStore reads limits and spend. The store package implements it on
// the budgets and usage_ledger tables.
type BudgetStore interface {
	Limits(ctx context.Context, scope, scopeID string) ([]BudgetLimit, error)
	Spent(ctx context.Context, scope, scopeID string, since time.Time) (float64, error)
}

// PeriodStart returns the start of the rolling window for a period.
func PeriodStart(period string, now time.Time) time.Time {
	switch period {
	case "day":
		return now.Add(-24 * time.Hour)
	case "week":
		return now.Add(-7 * 24 * time.Hour)
	case "month":
		return now.Add(-30 * 24 * time.Hour)
	}
	return time.Time{} // total
}

type budgetCache struct {
	mu     sync.Mutex
	limits map[string]cachedLimits
	spent  map[string]cachedSpent
}

type cachedLimits struct {
	at   time.Time
	list []BudgetLimit
}

type cachedSpent struct {
	at  time.Time
	usd float64
}

// BudgetMiddleware enforces budgets before routing. A blocking budget
// returns ErrBudgetExceeded; a downgrade budget restricts routing to local
// endpoints. Limits are cached 30s and spend 10s so the ledger isn't
// summed on every call.
func BudgetMiddleware(store BudgetStore, log *slog.Logger) Middleware {
	if log == nil {
		log = slog.Default()
	}
	c := &budgetCache{limits: map[string]cachedLimits{}, spent: map[string]cachedSpent{}}
	return func(ctx context.Context, req *Request, in *RouteInput) error {
		scopes := [][2]string{{"global", ""}}
		if req.Metadata.UserID != "" {
			scopes = append(scopes, [2]string{"user", req.Metadata.UserID})
		}
		if req.Metadata.AgentID != "" {
			scopes = append(scopes, [2]string{"agent", req.Metadata.AgentID})
		}
		if req.Metadata.APIKeyID != "" {
			scopes = append(scopes, [2]string{"api_key", req.Metadata.APIKeyID})
		}
		now := time.Now()
		for _, sc := range scopes {
			limits, err := c.getLimits(ctx, store, sc[0], sc[1], now)
			if err != nil {
				log.Warn("budget: limits lookup failed", "scope", sc[0], "err", err)
				continue
			}
			for _, l := range limits {
				spent, err := c.getSpent(ctx, store, sc[0], sc[1], l.Period, now)
				if err != nil {
					log.Warn("budget: spend lookup failed", "scope", sc[0], "err", err)
					continue
				}
				if spent < l.LimitUSD {
					continue
				}
				if l.OnExceed == "block" {
					return fmt.Errorf("%w: %s budget of $%.2f per %s is spent ($%.2f)", ErrBudgetExceeded, l.Scope, l.LimitUSD, l.Period, spent)
				}
				if !in.Downgrade {
					log.Info("budget: downgrading to local endpoints", "scope", l.Scope, "period", l.Period, "limit", l.LimitUSD, "spent", spent)
				}
				in.Downgrade = true
			}
		}
		return nil
	}
}

func (c *budgetCache) getLimits(ctx context.Context, s BudgetStore, scope, id string, now time.Time) ([]BudgetLimit, error) {
	key := scope + ":" + id
	c.mu.Lock()
	if e, ok := c.limits[key]; ok && now.Sub(e.at) < 30*time.Second {
		c.mu.Unlock()
		return e.list, nil
	}
	c.mu.Unlock()
	list, err := s.Limits(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.limits[key] = cachedLimits{at: now, list: list}
	c.mu.Unlock()
	return list, nil
}

func (c *budgetCache) getSpent(ctx context.Context, s BudgetStore, scope, id, period string, now time.Time) (float64, error) {
	key := scope + ":" + id + ":" + period
	c.mu.Lock()
	if e, ok := c.spent[key]; ok && now.Sub(e.at) < 10*time.Second {
		c.mu.Unlock()
		return e.usd, nil
	}
	c.mu.Unlock()
	usd, err := s.Spent(ctx, scope, id, PeriodStart(period, now))
	if err != nil {
		return 0, err
	}
	c.mu.Lock()
	c.spent[key] = cachedSpent{at: now, usd: usd}
	c.mu.Unlock()
	return usd, nil
}
