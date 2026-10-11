package httpx

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jking323/ws/internal/memory"
)

// The member's own memory (PLAN M13 cut 3). Every handler works on the
// signed-in member's rows only; there is no way to name another member's.

func (s *Server) memoryErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, memory.ErrNotFound):
		writeErr(w, 404, "not found")
	case errors.Is(err, memory.ErrInvalid):
		writeErr(w, 400, err.Error())
	default:
		s.Log.Warn("memory", "err", err)
		writeErr(w, 500, "db")
	}
}

// handleGetMemory returns the settings with the card, and a page of the
// member's memories: `?limit=` (default 100, at most 200) and `?offset=`.
func (s *Server) handleGetMemory(w http.ResponseWriter, r *http.Request) {
	if s.Memory == nil {
		writeErr(w, 503, "memory unavailable")
		return
	}
	uid := Principal(r.Context()).UserID
	limit, offset := 100, 0
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = v
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && v > 0 {
		offset = v
	}
	set, err := s.Memory.Settings(r.Context(), uid)
	if err != nil {
		s.memoryErr(w, err)
		return
	}
	mems, total, err := s.Memory.List(r.Context(), uid, limit, offset)
	if err != nil {
		s.memoryErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"settings": set, "memories": mems, "total": total})
}

// handlePutMemorySettings replaces the switches and the card.
func (s *Server) handlePutMemorySettings(w http.ResponseWriter, r *http.Request) {
	if s.Memory == nil {
		writeErr(w, 503, "memory unavailable")
		return
	}
	var in memory.Settings
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	out, err := s.Memory.SaveSettings(r.Context(), Principal(r.Context()).UserID, in)
	if err != nil {
		s.memoryErr(w, err)
		return
	}
	writeJSON(w, 200, out)
}

type memoryBody struct {
	Kind       string  `json:"kind"`
	Content    string  `json:"content"`
	Importance float32 `json:"importance"`
}

// handleAddMemory stores something the person typed on the Memory page.
func (s *Server) handleAddMemory(w http.ResponseWriter, r *http.Request) {
	if s.Memory == nil {
		writeErr(w, 503, "memory unavailable")
		return
	}
	var in memoryBody
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	m, err := s.Memory.Add(r.Context(), Principal(r.Context()).UserID, in.Kind, in.Content, memory.SourceManual, in.Importance, uuid.NullUUID{})
	if err != nil {
		s.memoryErr(w, err)
		return
	}
	writeJSON(w, 201, m)
}

// handleUpdateMemory edits one memory.
func (s *Server) handleUpdateMemory(w http.ResponseWriter, r *http.Request) {
	if s.Memory == nil {
		writeErr(w, 503, "memory unavailable")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	var in memoryBody
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	m, err := s.Memory.Update(r.Context(), Principal(r.Context()).UserID, id, in.Kind, in.Content, in.Importance)
	if err != nil {
		s.memoryErr(w, err)
		return
	}
	writeJSON(w, 200, m)
}

// handleDeleteUserMemory deletes one memory.
func (s *Server) handleDeleteUserMemory(w http.ResponseWriter, r *http.Request) {
	if s.Memory == nil {
		writeErr(w, 503, "memory unavailable")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	if err := s.Memory.Delete(r.Context(), Principal(r.Context()).UserID, id); err != nil {
		s.memoryErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// handleForgetMemory deletes every memory of the member (the card stays).
func (s *Server) handleForgetMemory(w http.ResponseWriter, r *http.Request) {
	if s.Memory == nil {
		writeErr(w, 503, "memory unavailable")
		return
	}
	if err := s.Memory.Forget(r.Context(), Principal(r.Context()).UserID); err != nil {
		s.memoryErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// handleReflectMemory queues a reflection now ("tidy up now").
func (s *Server) handleReflectMemory(w http.ResponseWriter, r *http.Request) {
	if s.Memory == nil || s.Jobs == nil {
		writeErr(w, 503, "memory unavailable")
		return
	}
	if err := s.Jobs.EnqueueMemoryReflect(r.Context(), Principal(r.Context()).UserID); err != nil {
		s.Log.Warn("memory: enqueue reflect", "err", err)
		writeErr(w, 500, "could not queue")
		return
	}
	writeJSON(w, 202, map[string]string{"status": "queued"})
}
