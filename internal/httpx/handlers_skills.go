package httpx

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/jking323/ws/internal/agents"
)

// The skill shelf (OpenClaw plan, step 2): a member's approved skill
// folders, kept once with their findings and file hashes. Nothing here
// grants a tool, a secret or network access, and nothing is run.

func (s *Server) skillStore() *agents.SkillStore {
	return &agents.SkillStore{DB: s.DB, Blobs: s.Blobs}
}

// readSkillZip reads the multipart field "file", up to limit bytes.
func readSkillZip(w http.ResponseWriter, r *http.Request, limit int) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, int64(limit)+(1<<20))
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		writeErr(w, 400, "the archive is too large or the upload was cut short")
		return nil, false
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		writeErr(w, 400, "file is required (a .zip)")
		return nil, false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		writeErr(w, 400, "could not read the upload")
		return nil, false
	}
	return data, true
}

// skillError maps an import or shelf failure to a status.
func skillError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, agents.ErrSkill):
		writeErr(w, 400, strings.TrimPrefix(err.Error(), agents.ErrSkill.Error()+": "))
	case errors.Is(err, agents.ErrSkillLimit):
		writeErr(w, 409, strings.TrimPrefix(err.Error(), agents.ErrSkillLimit.Error()+": "))
	default:
		writeErr(w, 500, "could not store the skill")
	}
}

// handleListShelf is GET /api/skills.
func (s *Server) handleListShelf(w http.ResponseWriter, r *http.Request) {
	out, err := s.skillStore().Shelf(r.Context(), Principal(r.Context()).UserID)
	if err != nil {
		writeErr(w, 500, "could not list the shelf")
		return
	}
	writeJSON(w, 200, out)
}

// handlePreviewSkill is POST /api/skills/preview: the import's findings and,
// when a skill of that name is on the shelf, what would change. Nothing is
// stored.
func (s *Server) handlePreviewSkill(w http.ResponseWriter, r *http.Request) {
	data, ok := readSkillZip(w, r, agents.MaxSkillZipBytes)
	if !ok {
		return
	}
	pv, err := s.skillStore().Preview(r.Context(), Principal(r.Context()).UserID, data)
	if err != nil {
		skillError(w, err)
		return
	}
	writeJSON(w, 200, pv)
}

// handleApproveSkill is POST /api/skills: approve one folder onto the shelf,
// replacing a skill of the same name.
func (s *Server) handleApproveSkill(w http.ResponseWriter, r *http.Request) {
	if s.Blobs == nil {
		writeErr(w, 503, "file storage is not configured")
		return
	}
	data, ok := readSkillZip(w, r, agents.MaxSkillZipBytes)
	if !ok {
		return
	}
	me := Principal(r.Context()).UserID
	k, err := s.skillStore().Approve(r.Context(), me, me, data)
	if err != nil {
		skillError(w, err)
		return
	}
	k.Instructions = ""
	writeJSON(w, 200, k)
}

// handleDeleteSkill is DELETE /api/skills/{id}: off the shelf and so off
// every agent that used it.
func (s *Server) handleDeleteSkill(w http.ResponseWriter, r *http.Request) {
	id, err := uuidParam(r, "id")
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	me := Principal(r.Context()).UserID
	row, err := s.DB.GetSkill(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && row.OwnerID != me) {
		writeErr(w, 404, "not found")
		return
	}
	if err != nil {
		writeErr(w, 500, "could not remove the skill")
		return
	}
	if err := s.skillStore().Delete(r.Context(), me, id); err != nil {
		writeErr(w, 500, "could not remove the skill")
		return
	}
	w.WriteHeader(204)
}

// handleBulkPreviewSkills is POST /api/skills/bulk/preview: a zip of a skills
// directory, each folder previewed on its own.
func (s *Server) handleBulkPreviewSkills(w http.ResponseWriter, r *http.Request) {
	data, ok := readSkillZip(w, r, agents.MaxBulkZipBytes)
	if !ok {
		return
	}
	items, err := s.skillStore().BulkPreview(r.Context(), Principal(r.Context()).UserID, data)
	if err != nil {
		skillError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

// handleBulkApproveSkills is POST /api/skills/bulk/approve: the same zip and
// the folders to approve (repeated form field "dir"); the others are skipped.
func (s *Server) handleBulkApproveSkills(w http.ResponseWriter, r *http.Request) {
	if s.Blobs == nil {
		writeErr(w, 503, "file storage is not configured")
		return
	}
	data, ok := readSkillZip(w, r, agents.MaxBulkZipBytes)
	if !ok {
		return
	}
	dirs := r.MultipartForm.Value["dir"]
	if len(dirs) == 0 {
		writeErr(w, 400, "choose at least one skill to approve")
		return
	}
	me := Principal(r.Context()).UserID
	done, failed, err := s.skillStore().BulkApprove(r.Context(), me, me, data, dirs)
	if err != nil {
		skillError(w, err)
		return
	}
	for i := range done {
		done[i].Instructions = ""
	}
	writeJSON(w, 200, map[string]any{"approved": done, "failed": failed})
}
