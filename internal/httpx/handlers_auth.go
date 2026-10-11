package httpx

import (
	"unicode/utf8"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jking323/ws/internal/auth"
	"github.com/jking323/ws/internal/store"
)

func (s *Server) authRoutes(r chi.Router) {
	su := s.stepUp()
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
		// Adding or removing a passkey is gated like the other sensitive actions: a stolen
		// cookie used from away must not be able to register a passkey of its own.
		pr.With(su.require(ActPasskey)).Post("/passkey/register/begin", s.handlePasskeyRegisterBegin)
		pr.With(su.require(ActPasskey)).Post("/passkey/register/finish", s.handlePasskeyRegisterFinish)
		pr.Get("/passkeys", s.handleListPasskeys)
		pr.With(su.require(ActPasskey)).Delete("/passkeys/{id}", s.handleDeletePasskey)
		// Step-up (M5): a passkey assertion on this session unlocks the gated actions for the window.
		pr.Post("/stepup/begin", s.handleStepUpBegin)
		pr.Post("/stepup/finish", s.handleStepUpFinish)
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
	if err := s.Auth.CreateSession(r.Context(), w, r, u.ID, false); err != nil {
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
	if err := s.Auth.CreateSession(r.Context(), w, r, u.ID, false); err != nil {
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
	if err := s.Auth.CreateSession(r.Context(), w, r, u.ID, true); err != nil {
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
	loc := LocalityOf(r.Context())
	su := s.stepUp()
	// Step-up state, for the sidebar mark and Settings: only meaningful once home and away are judged.
	var until *time.Time
	hasPasskey := false
	if loc.Configured && !p.APIKeyID.Valid {
		if u := su.until(p.SteppedUpAt); !u.IsZero() {
			until = &u
		}
		hasPasskey = su.hasPasskey(r.Context(), p.UserID)
	}
	writeJSON(w, 200, map[string]any{
		"id": p.UserID, "email": p.Email, "display_name": p.DisplayName, "role": p.Role, "via_api_key": p.APIKeyID.Valid, "training_consent": consent,
		"locality": loc, "step_up_until": until, "step_up_window_seconds": int(su.window.Seconds()), "has_passkey": hasPasskey,
	})
}

// handleUpdateMe changes the caller's display name, the one field of a
// person's own that is theirs to edit (the address is the sign-in and the
// role is the owner's). The API group never admits an API key, so this is a
// signed-in person by construction.
func (s *Server) handleUpdateMe(w http.ResponseWriter, r *http.Request) {
	p := Principal(r.Context())
	var in struct {
		DisplayName string `json:"display_name"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	name := strings.TrimSpace(in.DisplayName)
	if name == "" || utf8.RuneCountInString(name) > 80 {
		writeErr(w, 400, "display name must be 1 to 80 characters")
		return
	}
	if err := s.DB.SetDisplayName(r.Context(), store.SetDisplayNameParams{ID: p.UserID, DisplayName: name}); err != nil {
		writeErr(w, 500, "db")
		return
	}
	writeJSON(w, 200, map[string]any{"id": p.UserID, "email": p.Email, "display_name": name, "role": p.Role})
}

func isUnauthorized(err error) bool { return errors.Is(err, auth.ErrUnauthorized) }

// stepUpLimit keeps a session from starting ceremonies in a loop.
var stepUpLimit stepUpBegins

// handleStepUpBegin is POST /api/auth/stepup/begin: the options for a passkey
// assertion limited to the signed-in user's own passkeys, user verification required.
func (s *Server) handleStepUpBegin(w http.ResponseWriter, r *http.Request) {
	p := Principal(r.Context())
	if p.APIKeyID.Valid || p.SessionID == uuid.Nil {
		writeErr(w, 403, "step-up is for a signed-in browser session")
		return
	}
	if !stepUpLimit.allow(p.UserID, time.Now()) {
		writeErr(w, 429, "too many step-up attempts; wait a minute")
		return
	}
	opts, cid, err := s.Auth.BeginStepUp(r.Context(), p.UserID)
	switch {
	case errors.Is(err, auth.ErrPasskeysUnavailable):
		writeErr(w, 503, "Passkeys need a domain name over https.")
		return
	case errors.Is(err, auth.ErrNoPasskey):
		writeStepUpErr(w, codeStepUpUnavailable, "add a passkey to do this away from home", "", s.stepUp().window)
		return
	case err != nil:
		s.Log.Error("step-up begin", "err", err)
		writeErr(w, 500, "webauthn")
		return
	}
	var options map[string]any
	_ = json.Unmarshal(opts, &options)
	writeJSON(w, 200, map[string]any{"options": options, "ceremony_id": cid})
}

// handleStepUpFinish is POST /api/auth/stepup/finish?ceremony=: validates the
// assertion and marks the session, so the gated actions pass until
// stepped_up_until.
func (s *Server) handleStepUpFinish(w http.ResponseWriter, r *http.Request) {
	p := Principal(r.Context())
	if p.APIKeyID.Valid || p.SessionID == uuid.Nil {
		writeErr(w, 403, "step-up is for a signed-in browser session")
		return
	}
	cid, err := uuid.Parse(r.URL.Query().Get("ceremony"))
	if err != nil {
		writeErr(w, 400, "ceremony required")
		return
	}
	at, err := s.Auth.FinishStepUp(r.Context(), cid, p.SessionID, p.UserID, r)
	if err != nil {
		s.Log.Warn("step-up failed", "err", err)
		writeErr(w, 401, "passkey confirmation failed")
		return
	}
	writeJSON(w, 200, map[string]any{"stepped_up_until": at.Add(s.stepUp().window)})
}
