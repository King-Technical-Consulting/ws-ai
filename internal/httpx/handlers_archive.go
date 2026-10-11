package httpx

import (
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jking323/ws/internal/purge"
	"github.com/jking323/ws/internal/store"
)

// Archive then delete (v0.4.0): archiving only hides a conversation or a
// project and can be undone; permanent deletion removes rows and files and
// cannot. Only archived items are deleted. A conversation is deleted by its
// own member or by its project's owner; a project by its owner.

func (s *Server) purger() *purge.Purger {
	p := &purge.Purger{Pool: s.DB.Pool, Blobs: s.Blobs}
	if s.Worker != nil {
		p.Sandboxes = s.Worker
	}
	return p
}

// archivedConversation loads a conversation (archived or not) the caller
// may delete: their own, or one in a project they own.
func (s *Server) archivedConversation(w http.ResponseWriter, r *http.Request) (*store.Conversation, bool) {
	id, err := uuidParam(r, "id")
	if err != nil {
		writeErr(w, 400, "bad id")
		return nil, false
	}
	conv, err := s.DB.GetConversationAny(r.Context(), id)
	if err != nil {
		writeErr(w, 404, "not found")
		return nil, false
	}
	me := Principal(r.Context()).UserID
	if conv.UserID != me {
		proj, err := s.DB.GetProjectAny(r.Context(), conv.ProjectID)
		if err != nil || proj.OwnerID != me {
			writeErr(w, 404, "not found")
			return nil, false
		}
	}
	return &conv, true
}

// ownedProject loads a project (archived or not) the caller owns.
func (s *Server) ownedProject(w http.ResponseWriter, r *http.Request) (*store.Project, bool) {
	id, err := uuidParam(r, "id")
	if err != nil {
		writeErr(w, 400, "bad id")
		return nil, false
	}
	proj, err := s.DB.GetProjectAny(r.Context(), id)
	if err != nil || proj.OwnerID != Principal(r.Context()).UserID {
		writeErr(w, 404, "not found")
		return nil, false
	}
	return &proj, true
}

// purgeError maps a purge failure to a status and a message.
func purgeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, purge.ErrNotArchived):
		writeErr(w, 409, "archive it first: only archived items are deleted")
	case errors.Is(err, purge.ErrBusy):
		writeErr(w, 409, "an agent run in it is still open; cancel it first")
	case errors.Is(err, pgx.ErrNoRows):
		writeErr(w, 404, "not found")
	default:
		writeErr(w, 500, "could not delete: "+err.Error())
	}
}

// handleListArchived is GET /api/archive.
func (s *Server) handleListArchived(w http.ResponseWriter, r *http.Request) {
	out, err := s.purger().ListArchived(r.Context(), Principal(r.Context()).UserID)
	if err != nil {
		writeErr(w, 500, "could not list the archive")
		return
	}
	writeJSON(w, 200, out)
}

// handleRestoreConversation is POST /api/conversations/{id}/restore.
func (s *Server) handleRestoreConversation(w http.ResponseWriter, r *http.Request) {
	conv, ok := s.archivedConversation(w, r)
	if !ok {
		return
	}
	if err := s.DB.RestoreConversation(r.Context(), conv.ID); err != nil {
		writeErr(w, 500, "could not restore")
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// handleDeleteConversationPermanently is DELETE /api/conversations/{id}/permanent.
func (s *Server) handleDeleteConversationPermanently(w http.ResponseWriter, r *http.Request) {
	if s.Blobs == nil {
		writeErr(w, 503, "file storage is not configured")
		return
	}
	conv, ok := s.archivedConversation(w, r)
	if !ok {
		return
	}
	rep, err := s.purger().DeleteConversations(r.Context(), []uuid.UUID{conv.ID})
	if err != nil {
		purgeError(w, err)
		return
	}
	writeJSON(w, 200, rep)
}

// handleDeleteAllArchived is POST /api/archive/delete-all: every archived
// conversation the caller may delete, outside archived projects.
func (s *Server) handleDeleteAllArchived(w http.ResponseWriter, r *http.Request) {
	if s.Blobs == nil {
		writeErr(w, 503, "file storage is not configured")
		return
	}
	pg := s.purger()
	ids, err := pg.ArchivedIDs(r.Context(), Principal(r.Context()).UserID)
	if err != nil {
		writeErr(w, 500, "could not list the archive")
		return
	}
	rep, err := pg.DeleteConversations(r.Context(), ids)
	if err != nil {
		purgeError(w, err)
		return
	}
	writeJSON(w, 200, rep)
}

// handleArchiveProject is POST /api/projects/{id}/archive.
func (s *Server) handleArchiveProject(w http.ResponseWriter, r *http.Request) {
	proj, ok := s.ownedProject(w, r)
	if !ok {
		return
	}
	if err := s.DB.ArchiveProject(r.Context(), proj.ID); err != nil {
		writeErr(w, 500, "could not archive")
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// handleRestoreProject is POST /api/projects/{id}/restore.
func (s *Server) handleRestoreProject(w http.ResponseWriter, r *http.Request) {
	proj, ok := s.ownedProject(w, r)
	if !ok {
		return
	}
	if err := s.DB.RestoreProject(r.Context(), proj.ID); err != nil {
		writeErr(w, 500, "could not restore")
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// handleDeleteProjectPermanently is POST /api/projects/{id}/delete-permanently
// with {"name": "<the project's name, typed>"}.
func (s *Server) handleDeleteProjectPermanently(w http.ResponseWriter, r *http.Request) {
	if s.Blobs == nil {
		writeErr(w, 503, "file storage is not configured")
		return
	}
	proj, ok := s.ownedProject(w, r)
	if !ok {
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	if strings.TrimSpace(in.Name) != proj.Name {
		writeErr(w, 400, "type the project's name exactly to confirm")
		return
	}
	rep, err := s.purger().DeleteProject(r.Context(), proj.ID)
	if err != nil {
		purgeError(w, err)
		return
	}
	writeJSON(w, 200, rep)
}

// handleProjectPlan is GET /api/projects/{id}/purge-plan: what deleting the
// project would remove, for the confirmation dialog.
func (s *Server) handleProjectPlan(w http.ResponseWriter, r *http.Request) {
	proj, ok := s.ownedProject(w, r)
	if !ok {
		return
	}
	pl, err := s.purger().PlanProject(r.Context(), proj.ID)
	if err != nil {
		writeErr(w, 500, "could not count")
		return
	}
	writeJSON(w, 200, map[string]any{"name": proj.Name, "archived": proj.ArchivedAt != nil, "plan": pl})
}
