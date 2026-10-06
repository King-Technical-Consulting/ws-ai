package httpx

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/jking323/ws/internal/store"
)

func (s *Server) handleListInvites(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.ListInvites(r.Context())
	if err != nil {
		writeErr(w, 500, "db")
		return
	}
	writeJSON(w, 200, rows)
}

func (s *Server) handleCreateInvite(w http.ResponseWriter, r *http.Request) {
	p := Principal(r.Context())
	var in struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := decode(r, &in); err != nil || in.Email == "" {
		writeErr(w, 400, "email required")
		return
	}
	link, err := s.Auth.Invite(r.Context(), in.Email, p.UserID, in.Role)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 201, map[string]string{"link": link})
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.ListUsers(r.Context())
	if err != nil {
		writeErr(w, 500, "db")
		return
	}
	writeJSON(w, 200, rows)
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 {
		days = 30
	}
	since := time.Now().AddDate(0, 0, -days)
	byEp, err := s.DB.UsageByEndpointSince(r.Context(), since)
	if err != nil {
		writeErr(w, 500, "db")
		return
	}
	byUser, err := s.DB.UsageByUserSince(r.Context(), since)
	if err != nil {
		writeErr(w, 500, "db")
		return
	}
	total, _ := s.DB.SumUsageSince(r.Context(), since)
	recent, _ := s.DB.ListUsageAll(r.Context(), store.ListUsageAllParams{Limit: 100, Offset: 0})
	writeJSON(w, 200, map[string]any{"since": since, "total_usd": total, "by_endpoint": byEp, "by_user": byUser, "recent": recent})
}

func (s *Server) handleAdminEndpoints(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"providers": s.GW.Registry.Providers(), "endpoints": s.GW.Registry.Endpoints(), "policies": s.GW.Router.Policies()}
	// Every provider row, configured on this box or not, so the UI can
	// edit one whose key is missing and say why it is skipped.
	if rows, err := s.DB.ListProviders(r.Context()); err == nil {
		all := make([]providerView, 0, len(rows))
		for _, p := range rows {
			all = append(all, viewProvider(p, s.GW.Registry))
		}
		out["all_providers"] = all
	}
	if s.Media != nil {
		out["engines"] = s.Media.EngineList()
	} else {
		out["engines"] = []any{}
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleSetEndpointEnabled(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeErr(w, 400, "id required")
		return
	}
	var in struct{ Enabled bool `json:"enabled"` }
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	if err := s.DB.SetEndpointEnabled(r.Context(), store.SetEndpointEnabledParams{ID: id, Enabled: in.Enabled}); err != nil {
		writeErr(w, 500, "db")
		return
	}
	if ep, ok := s.GW.Registry.Endpoint(id); ok {
		ep.Enabled = in.Enabled
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}
