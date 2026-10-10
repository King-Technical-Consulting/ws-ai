package httpx

import (
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jking323/ws/internal/auth"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
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

func (s *Server) handleRevokeInvite(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeErr(w, 400, "bad invite id")
		return
	}
	switch err := s.Auth.RevokeInvite(r.Context(), id); {
	case err == nil:
		w.WriteHeader(204)
	case errors.Is(err, auth.ErrInviteUsed):
		writeErr(w, 409, "invite already accepted")
	case errors.Is(err, auth.ErrNotFound):
		writeErr(w, 404, "not found")
	default:
		writeErr(w, 500, "db")
	}
}

func (s *Server) handleResendInvite(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeErr(w, 400, "bad invite id")
		return
	}
	link, err := s.Auth.ResendInvite(r.Context(), id, Principal(r.Context()).UserID)
	switch {
	case err == nil:
		writeJSON(w, 201, map[string]string{"link": link})
	case errors.Is(err, auth.ErrInviteUsed):
		writeErr(w, 409, "invite already accepted")
	case errors.Is(err, auth.ErrNotFound):
		writeErr(w, 404, "not found")
	default:
		writeErr(w, 400, err.Error())
	}
}

// validInviteAddress accepts a bare address only: "Name <a@b>" and lists
// are not addresses to mail.
func validInviteAddress(email string) bool {
	email = strings.TrimSpace(email)
	a, err := mail.ParseAddress(email)
	return err == nil && a.Address == email
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
	if !validInviteAddress(in.Email) {
		writeErr(w, 400, "not a valid email address")
		return
	}
	link, err := s.Auth.Invite(r.Context(), in.Email, p.UserID, in.Role)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 201, map[string]string{"link": link})
}

// handleSetDisabled parks or restores a member. A disabled person's
// sessions and API keys stop working at once and a sign-in for them fails
// like a bad token; enabling them again needs no new invite. The owner
// cannot be disabled, by anyone.
func (s *Server) handleSetDisabled(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeErr(w, 400, "bad user id")
		return
	}
	var in struct {
		Disabled bool `json:"disabled"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	if Principal(r.Context()).UserID == id {
		writeErr(w, 400, "the owner cannot be disabled")
		return
	}
	u, err := s.DB.SetUserDisabled(r.Context(), store.SetUserDisabledParams{ID: id, Disabled: in.Disabled})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			if _, uerr := s.DB.GetUserByID(r.Context(), id); uerr == nil {
				writeErr(w, 400, "the owner cannot be disabled")
				return
			}
			writeErr(w, 404, "not found")
			return
		}
		writeErr(w, 500, "db")
		return
	}
	writeJSON(w, 200, u)
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
	out := map[string]any{"providers": s.GW.Registry.Providers(), "endpoints": s.GW.Registry.Endpoints(), "policies": s.GW.Router.Policies(), "throughput": s.GW.Router.Throughput().All()}
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

// endpointEnabledID extracts the endpoint id from the wildcard part of
// POST /admin/endpoints/<id>/enabled. Ids contain slashes
// (local-llama/gpt-oss-20b), so the id is everything before the final
// "/enabled".
func endpointEnabledID(wild string) (string, bool) {
	id, ok := strings.CutSuffix(wild, "/enabled")
	return id, ok && id != ""
}

func (s *Server) handleSetEndpointEnabled(w http.ResponseWriter, r *http.Request) {
	id, ok := endpointEnabledID(chi.URLParam(r, "*"))
	if !ok {
		writeErr(w, 400, "id required")
		return
	}
	var in struct {
		Enabled bool `json:"enabled"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	if err := s.DB.SetEndpointEnabled(r.Context(), store.SetEndpointEnabledParams{ID: id, Enabled: in.Enabled}); err != nil {
		writeErr(w, 500, "db")
		return
	}
	s.GW.Registry.SetEnabled(id, in.Enabled)
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// adminUser resolves the {id} of an /admin/users/{id}/... route to a user,
// answering 400 for a malformed id and 404 for an unknown one. The owner
// may act on their own account too.
func (s *Server) adminUser(w http.ResponseWriter, r *http.Request) (store.User, bool) {
	id, err := uuidParam(r, "id")
	if err != nil {
		writeErr(w, 400, "bad user id")
		return store.User{}, false
	}
	u, err := s.DB.GetUserByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeErr(w, 404, "no such user")
		} else {
			writeErr(w, 500, "db")
		}
		return store.User{}, false
	}
	return u, true
}

// handleAdminRevokeUserSessions ends every browser session of one user:
// their next request answers 401. Passkeys, API keys and the account stay;
// a fresh sign-in works at once.
func (s *Server) handleAdminRevokeUserSessions(w http.ResponseWriter, r *http.Request) {
	u, ok := s.adminUser(w, r)
	if !ok {
		return
	}
	if err := s.DB.RevokeAllSessionsForUser(r.Context(), u.ID); err != nil {
		writeErr(w, 500, "db")
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// handleAdminListUserKeys lists one user's live API keys as GET /api/keys
// does for oneself: the same view, never the hash.
func (s *Server) handleAdminListUserKeys(w http.ResponseWriter, r *http.Request) {
	u, ok := s.adminUser(w, r)
	if !ok {
		return
	}
	rows, err := s.DB.ListAPIKeysByUser(r.Context(), u.ID)
	if err != nil {
		writeErr(w, 500, "db")
		return
	}
	out := make([]apiKeyView, 0, len(rows))
	for _, k := range rows {
		out = append(out, keyView(k))
	}
	writeJSON(w, 200, out)
}

// handleAdminRevokeUserKey revokes one of a user's API keys. The key must be
// that user's and still live, else 404.
func (s *Server) handleAdminRevokeUserKey(w http.ResponseWriter, r *http.Request) {
	u, ok := s.adminUser(w, r)
	if !ok {
		return
	}
	keyID, err := uuidParam(r, "keyId")
	if err != nil {
		writeErr(w, 400, "bad key id")
		return
	}
	n, err := s.DB.RevokeUserAPIKey(r.Context(), store.RevokeUserAPIKeyParams{ID: keyID, UserID: u.ID})
	if err != nil {
		writeErr(w, 500, "db")
		return
	}
	if n == 0 {
		writeErr(w, 404, "no such key")
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}
