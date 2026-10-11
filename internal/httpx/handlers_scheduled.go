package httpx

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/jking323/ws/internal/scheduled"
)

// Scheduled prompts (PLAN M13 cut 5). Every handler works on the signed-in
// member's own rows; another member's id is 404.

func (s *Server) scheduledErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, scheduled.ErrNotFound):
		writeErr(w, 404, "not found")
	case errors.Is(err, scheduled.ErrInvalid):
		writeErr(w, 400, err.Error())
	default:
		s.Log.Warn("scheduled", "err", err)
		writeErr(w, 500, "db")
	}
}

func (s *Server) handleListScheduled(w http.ResponseWriter, r *http.Request) {
	if s.Scheduled == nil {
		writeErr(w, 503, "scheduled prompts unavailable")
		return
	}
	out, err := s.Scheduled.List(r.Context(), Principal(r.Context()).UserID)
	if err != nil {
		s.scheduledErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"schedules": out})
}

func (s *Server) handleCreateScheduled(w http.ResponseWriter, r *http.Request) {
	if s.Scheduled == nil {
		writeErr(w, 503, "scheduled prompts unavailable")
		return
	}
	var in scheduled.Spec
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	out, err := s.Scheduled.Create(r.Context(), Principal(r.Context()).UserID, in)
	if err != nil {
		s.scheduledErr(w, err)
		return
	}
	writeJSON(w, 201, out)
}

func (s *Server) handleUpdateScheduled(w http.ResponseWriter, r *http.Request) {
	if s.Scheduled == nil {
		writeErr(w, 503, "scheduled prompts unavailable")
		return
	}
	id, err := uuidParam(r, "id")
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	var in scheduled.Spec
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	out, err := s.Scheduled.Update(r.Context(), Principal(r.Context()).UserID, id, in)
	if err != nil {
		s.scheduledErr(w, err)
		return
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleDeleteScheduled(w http.ResponseWriter, r *http.Request) {
	if s.Scheduled == nil {
		writeErr(w, 503, "scheduled prompts unavailable")
		return
	}
	id, err := uuidParam(r, "id")
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	if err := s.Scheduled.Delete(r.Context(), Principal(r.Context()).UserID, id); err != nil {
		s.scheduledErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (s *Server) handleRunScheduled(w http.ResponseWriter, r *http.Request) {
	if s.Scheduled == nil {
		writeErr(w, 503, "scheduled prompts unavailable")
		return
	}
	id, err := uuidParam(r, "id")
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	run, err := s.Scheduled.RunNow(r.Context(), Principal(r.Context()).UserID, id)
	if err != nil {
		s.scheduledErr(w, err)
		return
	}
	writeJSON(w, 202, run)
}

func (s *Server) handleScheduledRuns(w http.ResponseWriter, r *http.Request) {
	if s.Scheduled == nil {
		writeErr(w, 503, "scheduled prompts unavailable")
		return
	}
	id, err := uuidParam(r, "id")
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	out, err := s.Scheduled.History(r.Context(), Principal(r.Context()).UserID, id, limit)
	if err != nil {
		s.scheduledErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"runs": out})
}
