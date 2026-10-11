package httpx

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jking323/ws/internal/favorites"
)

// Favorites and saved prompts (PLAN M13 cut 7); every handler works on the
// signed-in member's own rows.

func (s *Server) favErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, favorites.ErrNotFound):
		writeErr(w, 404, "not found")
	case errors.Is(err, favorites.ErrInvalid):
		writeErr(w, 400, err.Error())
	default:
		s.Log.Warn("favorites", "err", err)
		writeErr(w, 500, "db")
	}
}

func (s *Server) favReady(w http.ResponseWriter) bool {
	if s.Favorites == nil {
		writeErr(w, 503, "favorites unavailable")
		return false
	}
	return true
}

func (s *Server) handleListFavorites(w http.ResponseWriter, r *http.Request) {
	if !s.favReady(w) {
		return
	}
	out, err := s.Favorites.List(r.Context(), Principal(r.Context()).UserID)
	if err != nil {
		s.favErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"favorites": out})
}

type favBody struct {
	Kind  string    `json:"kind"`
	RefID uuid.UUID `json:"ref_id"`
}

func (s *Server) handleAddFavorite(w http.ResponseWriter, r *http.Request) {
	if !s.favReady(w) {
		return
	}
	var in favBody
	if err := decode(r, &in); err != nil || in.RefID == uuid.Nil {
		writeErr(w, 400, "kind and ref_id are required")
		return
	}
	out, err := s.Favorites.Add(r.Context(), Principal(r.Context()).UserID, in.Kind, in.RefID)
	if err != nil {
		s.favErr(w, err)
		return
	}
	writeJSON(w, 201, out)
}

func (s *Server) handleRemoveFavorite(w http.ResponseWriter, r *http.Request) {
	if !s.favReady(w) {
		return
	}
	ref, err := uuid.Parse(chi.URLParam(r, "ref"))
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	if err := s.Favorites.Remove(r.Context(), Principal(r.Context()).UserID, chi.URLParam(r, "kind"), ref); err != nil {
		s.favErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (s *Server) handleListSavedPrompts(w http.ResponseWriter, r *http.Request) {
	if !s.favReady(w) {
		return
	}
	out, err := s.Favorites.ListPrompts(r.Context(), Principal(r.Context()).UserID)
	if err != nil {
		s.favErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"prompts": out})
}

type promptBody struct {
	Name string `json:"name"`
	Text string `json:"text"`
}

func (s *Server) handleCreateSavedPrompt(w http.ResponseWriter, r *http.Request) {
	if !s.favReady(w) {
		return
	}
	var in promptBody
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	out, err := s.Favorites.SavePrompt(r.Context(), Principal(r.Context()).UserID, in.Name, in.Text)
	if err != nil {
		s.favErr(w, err)
		return
	}
	writeJSON(w, 201, out)
}

func (s *Server) handleUpdateSavedPrompt(w http.ResponseWriter, r *http.Request) {
	if !s.favReady(w) {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	var in promptBody
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	out, err := s.Favorites.UpdatePrompt(r.Context(), Principal(r.Context()).UserID, id, in.Name, in.Text)
	if err != nil {
		s.favErr(w, err)
		return
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleDeleteSavedPrompt(w http.ResponseWriter, r *http.Request) {
	if !s.favReady(w) {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	if err := s.Favorites.DeletePrompt(r.Context(), Principal(r.Context()).UserID, id); err != nil {
		s.favErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}
