package httpx

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jking323/ws/internal/auth"
	"github.com/jking323/ws/internal/store"
)

func (s *Server) authRoutes(r chi.Router) {
	r.Get("/config", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"passkeys": s.Auth.PasskeysEnabled(), "magic_links": true})
	})
	r.Post("/invite/peek", s.handleInvitePeek)
	r.Post("/invite/accept", s.handleInviteAccept)
	r.Post("/magic/send", s.handleMagicSend)
	r.Post("/passkey/login/begin", s.handlePasskeyLoginBegin)
	r.Post("/passkey/login/finish", s.handlePasskeyLoginFinish)
	r.Group(func(pr chi.Router) {
		pr.Use(requireAuth)
		pr.Post("/passkey/register/begin", s.handlePasskeyRegisterBegin)
		pr.Post("/passkey/register/finish", s.handlePasskeyRegisterFinish)
		pr.Get("/passkeys", s.handleListPasskeys)
		pr.Delete("/passkeys/{id}", s.handleDeletePasskey)
		pr.Post("/logout", s.handleLogout)
	})
}

func (s *Server) handleInvitePeek(w http.ResponseWriter, r *http.Request) {
	var in struct{ Token string `json:"token"` }
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	email, err := s.Auth.PeekInvite(r.Context(), in.Token)
	if err != nil {
		writeErr(w, 404, "invalid or expired invite")
		return
	}
	writeJSON(w, 200, map[string]string{"email": email})
}

func (s *Server) handleInviteAccept(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token       string `json:"token"`
		DisplayName string `json:"display_name"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	u, err := s.Auth.AcceptInvite(r.Context(), in.Token, in.DisplayName)
	if err != nil {
		writeErr(w, 400, "invalid or expired invite")
		return
	}
	if err := s.Auth.CreateSession(r.Context(), w, r, u.ID); err != nil {
		s.Log.Error("create session", "err", err)
		writeErr(w, 500, "session")
		return
	}
	writeJSON(w, 200, map[string]any{"id": u.ID, "email": u.Email, "display_name": u.DisplayName, "role": u.Role})
}

func (s *Server) handleMagicSend(w http.ResponseWriter, r *http.Request) {
	var in struct{ Email string `json:"email"` }
	if err := decode(r, &in); err != nil || !strings.Contains(in.Email, "@") {
		writeErr(w, 400, "email required")
		return
	}
	_ = s.Auth.SendMagicLink(r.Context(), in.Email)
	writeJSON(w, 200, map[string]string{"status": "sent-if-known"})
}

func (s *Server) handleMagicConsume(w http.ResponseWriter, r *http.Request) {
	u, err := s.Auth.ConsumeMagicLink(r.Context(), chi.URLParam(r, "token"))
	if err != nil {
		http.Redirect(w, r, "/login?error=magic", http.StatusSeeOther)
		return
	}
	if err := s.Auth.CreateSession(r.Context(), w, r, u.ID); err != nil {
		http.Redirect(w, r, "/login?error=session", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handlePasskeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	opts, cid, err := s.Auth.BeginLogin(r.Context())
	if errors.Is(err, auth.ErrPasskeysUnavailable) {
		writeErr(w, 503, "Passkeys need a domain name over https. Sign in with an email link instead.")
		return
	}
	if err != nil {
		s.Log.Error("passkey login begin", "err", err)
		writeErr(w, 500, "webauthn")
		return
	}
	writeJSON(w, 200, map[string]any{"options": opts, "ceremony_id": cid})
}

func (s *Server) handlePasskeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	cid, err := uuid.Parse(r.URL.Query().Get("ceremony"))
	if err != nil {
		writeErr(w, 400, "ceremony required")
		return
	}
	u, err := s.Auth.FinishLogin(r.Context(), cid, r)
	if err != nil {
		s.Log.Warn("passkey login failed", "err", err)
		writeErr(w, 401, "passkey login failed")
		return
	}
	if err := s.Auth.CreateSession(r.Context(), w, r, u.ID); err != nil {
		writeErr(w, 500, "session")
		return
	}
	writeJSON(w, 200, map[string]any{"id": u.ID, "email": u.Email, "display_name": u.DisplayName, "role": u.Role})
}

func (s *Server) handlePasskeyRegisterBegin(w http.ResponseWriter, r *http.Request) {
	p := Principal(r.Context())
	opts, cid, err := s.Auth.BeginRegistration(r.Context(), p.UserID)
	if errors.Is(err, auth.ErrPasskeysUnavailable) {
		writeErr(w, 503, "Passkeys need a domain name over https. They'll be available once the app is served on one.")
		return
	}
	if err != nil {
		s.Log.Error("passkey register begin", "err", err)
		writeErr(w, 500, "webauthn")
		return
	}
	writeJSON(w, 200, map[string]any{"options": opts, "ceremony_id": cid})
}

func (s *Server) handlePasskeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	cid, err := uuid.Parse(r.URL.Query().Get("ceremony"))
	if err != nil {
		writeErr(w, 400, "ceremony required")
		return
	}
	if err := s.Auth.FinishRegistration(r.Context(), cid, r.URL.Query().Get("name"), r); err != nil {
		s.Log.Warn("passkey register failed", "err", err)
		writeErr(w, 400, "passkey registration failed")
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (s *Server) handleListPasskeys(w http.ResponseWriter, r *http.Request) {
	p := Principal(r.Context())
	rows, err := s.DB.ListPasskeysByUser(r.Context(), p.UserID)
	if err != nil {
		writeErr(w, 500, "db")
		return
	}
	type pk struct {
		ID        uuid.UUID `json:"id"`
		Name      string    `json:"name"`
		CreatedAt string    `json:"created_at"`
		LastUsed  *string   `json:"last_used_at"`
	}
	out := make([]pk, 0, len(rows))
	for _, r := range rows {
		var lu *string
		if r.LastUsedAt != nil {
			s := r.LastUsedAt.Format("2006-01-02T15:04:05Z07:00")
			lu = &s
		}
		out = append(out, pk{ID: r.ID, Name: r.Name, CreatedAt: r.CreatedAt.Format("2006-01-02T15:04:05Z07:00"), LastUsed: lu})
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleDeletePasskey(w http.ResponseWriter, r *http.Request) {
	p := Principal(r.Context())
	id, err := uuidParam(r, "id")
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	_ = s.DB.DeletePasskey(r.Context(), store.DeletePasskeyParams{ID: id, UserID: p.UserID})
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	_ = s.Auth.Logout(r.Context(), w, Principal(r.Context()))
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	p := Principal(r.Context())
	consent := false
	if u, err := s.DB.GetUserByID(r.Context(), p.UserID); err == nil {
		consent = u.TrainingConsent
	}
	writeJSON(w, 200, map[string]any{"id": p.UserID, "email": p.Email, "display_name": p.DisplayName, "role": p.Role, "via_api_key": p.APIKeyID.Valid, "training_consent": consent})
}

func isUnauthorized(err error) bool { return errors.Is(err, auth.ErrUnauthorized) }
