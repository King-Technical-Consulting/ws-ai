package httpx

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
)

type policyView struct {
	Name     string `json:"name"`
	YAML     string `json:"yaml"`
	Priority int32  `json:"priority"`
	Enabled  bool   `json:"enabled"`
	Error    string `json:"error,omitempty"`
	// Unknown lists endpoint ids referenced by the policy that don't exist.
	Unknown []string `json:"unknown_endpoints,omitempty"`
}

func (s *Server) policyView(r store.RoutingPolicy) policyView {
	v := policyView{Name: r.Name, YAML: r.Yaml, Priority: r.Priority, Enabled: r.Enabled}
	p, err := gateway.ParsePolicy(r.Yaml)
	if err != nil {
		v.Error = err.Error()
		return v
	}
	seen := map[string]bool{}
	check := func(ids []string) {
		for _, id := range ids {
			if _, ok := s.GW.Registry.Endpoint(id); !ok && !seen[id] {
				seen[id] = true
				v.Unknown = append(v.Unknown, id)
			}
		}
	}
	for _, rl := range p.Rules {
		check(rl.Prefer)
		check(rl.Deny)
	}
	for _, a := range p.Aliases {
		check(a)
	}
	return v
}

func (s *Server) handleListPolicies(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.ListAllRoutingPolicies(r.Context())
	if err != nil {
		writeErr(w, 500, "db")
		return
	}
	out := make([]policyView, 0, len(rows))
	for _, row := range rows {
		out = append(out, s.policyView(row))
	}
	writeJSON(w, 200, out)
}

func (s *Server) handlePutPolicy(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(chi.URLParam(r, "name"))
	if name == "" {
		writeErr(w, 400, "name required")
		return
	}
	var in struct {
		YAML     string `json:"yaml"`
		Priority int32  `json:"priority"`
		Enabled  *bool  `json:"enabled"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	if _, err := gateway.ParsePolicy(in.YAML); err != nil {
		writeErr(w, 400, "invalid policy: "+err.Error())
		return
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	if in.Priority == 0 {
		in.Priority = 100
	}
	row, err := s.DB.UpsertRoutingPolicy(r.Context(), store.UpsertRoutingPolicyParams{Name: name, Yaml: in.YAML, Priority: in.Priority, Enabled: enabled})
	if err != nil {
		writeErr(w, 500, "db")
		return
	}
	if err := s.DB.ReloadPolicies(r.Context(), s.GW.Router); err != nil {
		s.Log.Warn("reload policies", "err", err)
	}
	writeJSON(w, 200, s.policyView(row))
}

func (s *Server) handleDeletePolicy(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := s.DB.DeleteRoutingPolicyByName(r.Context(), name); err != nil {
		writeErr(w, 500, "db")
		return
	}
	_ = s.DB.ReloadPolicies(r.Context(), s.GW.Router)
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// ---- budgets ----

func (s *Server) handleListBudgets(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.ListAllBudgets(r.Context())
	if err != nil {
		writeErr(w, 500, "db")
		return
	}
	writeJSON(w, 200, rows)
}

func (s *Server) handlePutBudget(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Scope    string  `json:"scope"`
		ScopeID  string  `json:"scope_id"`
		Period   string  `json:"period"`
		LimitUSD float64 `json:"limit_usd"`
		OnExceed string  `json:"on_exceed"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	switch in.Scope {
	case "global", "user", "agent", "api_key":
	default:
		writeErr(w, 400, "scope must be global, user, agent, or api_key")
		return
	}
	switch in.Period {
	case "day", "week", "month", "total":
	default:
		writeErr(w, 400, "period must be day, week, month, or total")
		return
	}
	if in.OnExceed == "" {
		in.OnExceed = "downgrade"
	}
	if in.OnExceed != "block" && in.OnExceed != "downgrade" {
		writeErr(w, 400, "on_exceed must be block or downgrade")
		return
	}
	if in.LimitUSD <= 0 {
		writeErr(w, 400, "limit_usd must be positive")
		return
	}
	// A user or agent budget with no scope_id is the default for every
	// member or agent without a row of its own (0020).
	var scopeID uuid.NullUUID
	if in.Scope == "api_key" || (in.ScopeID != "" && in.Scope != "global") {
		id, err := uuid.Parse(in.ScopeID)
		if err != nil {
			writeErr(w, 400, "scope_id must be a uuid (or empty for a default user or agent budget)")
			return
		}
		scopeID = uuid.NullUUID{UUID: id, Valid: true}
	}
	row, err := s.DB.UpsertBudget(r.Context(), store.UpsertBudgetParams{Scope: in.Scope, ScopeID: scopeID, Period: in.Period, LimitUsd: in.LimitUSD, OnExceed: in.OnExceed})
	if err != nil {
		writeErr(w, 500, "db")
		return
	}
	writeJSON(w, 200, row)
}

func (s *Server) handleDeleteBudget(w http.ResponseWriter, r *http.Request) {
	id, err := uuidParam(r, "id")
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	_ = s.DB.DeleteBudget(r.Context(), id)
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// handleAdminSpend is the owner's overview: this period's spend by member
// and by agent against the budget each is under (its own row, else the
// default for its scope), sorted by spend. Spend on a member's own key is
// not counted, as the budgets do not count it.
func (s *Server) handleAdminSpend(w http.ResponseWriter, r *http.Request) {
	period := r.URL.Query().Get("period")
	switch period {
	case "day", "week", "month":
	case "":
		period = "month"
	default:
		writeErr(w, 400, "period must be day, week or month")
		return
	}
	ctx := r.Context()
	since := gateway.PeriodStart(period, time.Now())
	budgets, err := s.DB.ListAllBudgets(ctx)
	if err != nil {
		writeErr(w, 500, "db")
		return
	}
	limitFor := func(scope string, id uuid.UUID) *store.Budget {
		var def *store.Budget
		for i := range budgets {
			b := budgets[i]
			if b.Scope != scope || b.Period != period {
				continue
			}
			if b.ScopeID.Valid && b.ScopeID.UUID == id {
				return &b
			}
			if !b.ScopeID.Valid {
				def = &b
			}
		}
		return def
	}
	type row struct {
		ID       uuid.UUID `json:"id"`
		Name     string    `json:"name"`
		Calls    int64     `json:"calls"`
		USD      float64   `json:"usd"`
		LimitUSD float64   `json:"limit_usd"`
		OnExceed string    `json:"on_exceed,omitempty"`
		Default  bool      `json:"default"`
		Pct      int       `json:"pct"`
	}
	fill := func(scope string, id uuid.UUID, name string, calls int64, usd float64) row {
		rw := row{ID: id, Name: name, Calls: calls, USD: usd}
		if b := limitFor(scope, id); b != nil {
			rw.LimitUSD, rw.OnExceed, rw.Default = b.LimitUsd, b.OnExceed, !b.ScopeID.Valid
			if b.LimitUsd > 0 {
				rw.Pct = int(usd / b.LimitUsd * 100)
			}
		}
		return rw
	}
	users := []row{}
	byUser, _ := s.DB.BudgetUsageByUserSince(ctx, since)
	seenUser := map[uuid.UUID]bool{}
	for _, u := range byUser {
		if !u.UserID.Valid {
			continue
		}
		name := u.UserID.UUID.String()
		if usr, err := s.DB.GetUserByID(ctx, u.UserID.UUID); err == nil {
			name = usr.Email
		}
		seenUser[u.UserID.UUID] = true
		users = append(users, fill("user", u.UserID.UUID, name, u.Calls, u.CostUsd))
	}
	// Members with no spend still show their budget, so the owner sees
	// everyone the default covers.
	if all, err := s.DB.ListUsers(ctx); err == nil {
		for _, usr := range all {
			if !seenUser[usr.ID] && usr.DisabledAt == nil {
				users = append(users, fill("user", usr.ID, usr.Email, 0, 0))
			}
		}
	}
	agentsOut := []row{}
	byAgent, _ := s.DB.UsageByAgentSince(ctx, since)
	seenAgent := map[uuid.UUID]bool{}
	for _, a := range byAgent {
		if !a.AgentID.Valid {
			continue
		}
		name := a.AgentID.UUID.String()
		if ag, err := s.DB.GetAgent(ctx, a.AgentID.UUID); err == nil {
			name = ag.Name
		}
		seenAgent[a.AgentID.UUID] = true
		agentsOut = append(agentsOut, fill("agent", a.AgentID.UUID, name, a.Calls, a.CostUsd))
	}
	if all, err := s.DB.ListAllAgents(ctx); err == nil {
		for _, ag := range all {
			if !seenAgent[ag.ID] {
				agentsOut = append(agentsOut, fill("agent", ag.ID, ag.Name, 0, 0))
			}
		}
	}
	total, _ := s.DB.SumUsageSince(ctx, since)
	writeJSON(w, 200, map[string]any{"period": period, "since": since, "total_usd": total, "users": users, "agents": agentsOut})
}
