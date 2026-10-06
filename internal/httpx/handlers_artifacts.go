package httpx

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// loadArtifactConversation checks the caller may see the artifact.
func (s *Server) artifactAllowed(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuidParam(r, "id")
	if err != nil {
		writeErr(w, 400, "bad id")
		return uuid.Nil, false
	}
	conv, err := s.DB.GetArtifactConversation(r.Context(), id)
	if err != nil || !s.canAccessProject(r.Context(), conv.ProjectID) {
		writeErr(w, 404, "not found")
		return uuid.Nil, false
	}
	return id, true
}

// handleGetArtifact returns the artifact, its versions, and one version
// (the current one, or ?version=N) with content and a signed viewer URL.
func (s *Server) handleGetArtifact(w http.ResponseWriter, r *http.Request) {
	id, ok := s.artifactAllowed(w, r)
	if !ok {
		return
	}
	version, _ := strconv.Atoi(r.URL.Query().Get("version"))
	if v := chi.URLParam(r, "v"); v != "" {
		version, _ = strconv.Atoi(v)
	}
	a, versions, v, ref, err := s.Artifacts.Get(r.Context(), id, version)
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	content := ""
	if v.Content != nil {
		content = *v.Content
	}
	writeJSON(w, 200, map[string]any{
		"artifact": a, "versions": versions, "version": ref.Version, "version_id": ref.VersionID,
		"url": ref.URL, "content": content, "kind": ref.Kind, "title": a.Title,
	})
}

func (s *Server) handleListArtifacts(w http.ResponseWriter, r *http.Request) {
	conv, ok := s.loadConversation(w, r)
	if !ok {
		return
	}
	rows, err := s.DB.ListArtifactsForConversation(r.Context(), conv.ID)
	if err != nil {
		writeErr(w, 500, "db")
		return
	}
	writeJSON(w, 200, rows)
}
