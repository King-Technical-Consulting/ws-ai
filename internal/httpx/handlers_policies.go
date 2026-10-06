package httpx

import (
	"net/http"
	"strings"

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
	var scopeID uuid.NullUUID
	if in.Scope != "global" {
		id, err := uuid.Parse(in.ScopeID)
		if err != nil {
			writeErr(w, 400, "scope_id must be a uuid for non-global budgets")
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
