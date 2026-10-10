package httpx

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jking323/ws/internal/store"
	"github.com/jking323/ws/internal/training"
)

// Training flywheel (PLAN M10): consent and ratings for everyone; the
// datasets, fine-tune jobs and adapters for the owner.

// handleSetTrainingConsent is PUT /api/me/training-consent {enabled}.
func (s *Server) handleSetTrainingConsent(w http.ResponseWriter, r *http.Request) {
	p := Principal(r.Context())
	var in struct {
		Enabled bool `json:"enabled"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	if err := s.DB.SetTrainingConsent(r.Context(), store.SetTrainingConsentParams{ID: p.UserID, TrainingConsent: in.Enabled}); err != nil {
		writeErr(w, 500, "db")
		return
	}
	writeJSON(w, 200, map[string]any{"status": "ok", "training_consent": in.Enabled})
}

// messageAllowed checks the caller may see the message's conversation.
func (s *Server) messageAllowed(w http.ResponseWriter, r *http.Request) (*store.Message, bool) {
	id, err := uuidParam(r, "id")
	if err != nil {
		writeErr(w, 400, "bad id")
		return nil, false
	}
	m, err := s.DB.GetMessage(r.Context(), id)
	if err != nil {
		writeErr(w, 404, "not found")
		return nil, false
	}
	conv, err := s.DB.GetConversation(r.Context(), m.ConversationID)
	if err != nil || !s.canAccessProject(r.Context(), conv.ProjectID) {
		writeErr(w, 404, "not found")
		return nil, false
	}
	return &m, true
}

// handleRateMessage is PUT /api/messages/{id}/rating {score, note}: a
// thumbs up (1) or down (-1) on an assistant message, one per user.
func (s *Server) handleRateMessage(w http.ResponseWriter, r *http.Request) {
	m, ok := s.messageAllowed(w, r)
	if !ok {
		return
	}
	if m.Role != "assistant" {
		writeErr(w, 400, "only an assistant message can be rated")
		return
	}
	var in struct {
		Score int    `json:"score"`
		Note  string `json:"note"`
	}
	if err := decode(r, &in); err != nil || (in.Score != 1 && in.Score != -1) {
		writeErr(w, 400, "score must be 1 or -1")
		return
	}
	var note *string
	if n := strings.TrimSpace(in.Note); n != "" {
		if len(n) > 2000 {
			n = n[:2000]
		}
		note = &n
	}
	p := Principal(r.Context())
	if err := s.DB.UpsertMessageRating(r.Context(), store.UpsertMessageRatingParams{MessageID: m.ID, UserID: p.UserID, Score: int16(in.Score), Note: note}); err != nil {
		writeErr(w, 500, "db")
		return
	}
	writeJSON(w, 200, map[string]any{"message_id": m.ID, "score": in.Score})
}

// handleUnrateMessage is DELETE /api/messages/{id}/rating.
func (s *Server) handleUnrateMessage(w http.ResponseWriter, r *http.Request) {
	m, ok := s.messageAllowed(w, r)
	if !ok {
		return
	}
	p := Principal(r.Context())
	_ = s.DB.DeleteMessageRating(r.Context(), store.DeleteMessageRatingParams{MessageID: m.ID, UserID: p.UserID})
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// handleConversationRatings is GET /api/conversations/{id}/ratings: the
// caller's ratings in the conversation, {message_id: score}.
func (s *Server) handleConversationRatings(w http.ResponseWriter, r *http.Request) {
	conv, ok := s.loadConversation(w, r)
	if !ok {
		return
	}
	p := Principal(r.Context())
	rows, err := s.DB.ListRatingsForConversation(r.Context(), conv.ID)
	if err != nil {
		writeErr(w, 500, "db")
		return
	}
	out := map[string]int{}
	for _, row := range rows {
		if row.UserID == p.UserID {
			out[row.MessageID.String()] = int(row.Score)
		}
	}
	writeJSON(w, 200, map[string]any{"ratings": out})
}

func (s *Server) needTraining(w http.ResponseWriter) bool {
	if s.Training == nil {
		writeErr(w, 503, "training is not configured")
		return false
	}
	return true
}

func writeTrainingErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, training.ErrInvalid):
		writeErr(w, 400, strings.TrimPrefix(err.Error(), training.ErrInvalid.Error()+": "))
	case errors.Is(err, training.ErrNotReady):
		writeErr(w, 409, strings.TrimPrefix(err.Error(), training.ErrNotReady.Error()+": "))
	case errors.Is(err, training.ErrNoRunner):
		writeErr(w, 503, err.Error())
	case errors.Is(err, training.ErrNotFound):
		writeErr(w, 404, "not found")
	default:
		writeErr(w, 500, err.Error())
	}
}

// handleListDatasets is GET /api/training/datasets.
func (s *Server) handleListDatasets(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.ListDatasets(r.Context(), 100)
	if err != nil {
		writeErr(w, 500, "db")
		return
	}
	writeJSON(w, 200, map[string]any{"datasets": rows})
}

// handleCreateDataset is POST /api/training/datasets {name, task_class, filters}.
func (s *Server) handleCreateDataset(w http.ResponseWriter, r *http.Request) {
	if !s.needTraining(w) {
		return
	}
	var in struct {
		Name      string           `json:"name"`
		TaskClass string           `json:"task_class"`
		Filters   training.Filters `json:"filters"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	ds, err := s.Training.CreateDataset(r.Context(), Principal(r.Context()).UserID, training.DatasetSpec{Name: in.Name, TaskClass: in.TaskClass, Filters: in.Filters})
	if err != nil {
		writeTrainingErr(w, err)
		return
	}
	writeJSON(w, 201, ds)
}

// handleGetDataset is GET /api/training/datasets/{id}.
func (s *Server) handleGetDataset(w http.ResponseWriter, r *http.Request) {
	id, err := uuidParam(r, "id")
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	ds, err := s.DB.GetDataset(r.Context(), id)
	if err != nil {
		writeErr(w, 404, "not found")
		return
	}
	writeJSON(w, 200, ds)
}

// handleDownloadDataset is GET /api/training/datasets/{id}/download?split=train|eval.
func (s *Server) handleDownloadDataset(w http.ResponseWriter, r *http.Request) {
	if !s.needTraining(w) {
		return
	}
	id, err := uuidParam(r, "id")
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	eval := r.URL.Query().Get("split") == "eval"
	b, err := s.Training.DatasetFile(r.Context(), id, eval)
	if err != nil {
		writeTrainingErr(w, err)
		return
	}
	name := "train"
	if eval {
		name = "eval"
	}
	w.Header().Set("Content-Type", "application/jsonl; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="dataset-%s-%s.jsonl"`, id.String()[:8], name))
	w.Header().Set("Cache-Control", "private, no-store")
	_, _ = w.Write(b)
}

// handleDatasetPreview is GET /api/training/datasets/{id}/preview?split=train|eval&n=5:
// the first examples, parsed.
func (s *Server) handleDatasetPreview(w http.ResponseWriter, r *http.Request) {
	if !s.needTraining(w) {
		return
	}
	id, err := uuidParam(r, "id")
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	exs, err := s.Training.Preview(r.Context(), id, r.URL.Query().Get("split") == "eval", n)
	if err != nil {
		writeTrainingErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"examples": exs})
}

// handleDeleteDataset is DELETE /api/training/datasets/{id}.
func (s *Server) handleDeleteDataset(w http.ResponseWriter, r *http.Request) {
	id, err := uuidParam(r, "id")
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	if err := s.DB.DeleteDataset(r.Context(), id); err != nil {
		writeErr(w, 500, "db")
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// handleListFinetuneJobs is GET /api/training/jobs.
func (s *Server) handleListFinetuneJobs(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.ListFinetuneJobs(r.Context(), 100)
	if err != nil {
		writeErr(w, 500, "db")
		return
	}
	targets := []training.Target{}
	if s.Training != nil {
		targets = append(targets, s.Training.Targets()...)
	}
	writeJSON(w, 200, map[string]any{"jobs": rows, "runner": len(targets) > 0, "targets": targets})
}

// handleStartFinetune is POST /api/training/jobs {dataset_id, base_model,
// base_endpoint_id, adapter_name, config}.
func (s *Server) handleStartFinetune(w http.ResponseWriter, r *http.Request) {
	if !s.needTraining(w) {
		return
	}
	var in struct {
		DatasetID      string                  `json:"dataset_id"`
		BaseModel      string                  `json:"base_model"`
		BaseEndpointID string                  `json:"base_endpoint_id"`
		AdapterName    string                  `json:"adapter_name"`
		Config         training.FinetuneConfig `json:"config"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	dsID, err := uuid.Parse(in.DatasetID)
	if err != nil {
		writeErr(w, 400, "bad dataset_id")
		return
	}
	job, err := s.Training.StartFinetune(r.Context(), Principal(r.Context()).UserID, training.FinetuneSpec{DatasetID: dsID, BaseModel: in.BaseModel, BaseEndpointID: in.BaseEndpointID, AdapterName: in.AdapterName, Config: in.Config})
	if err != nil {
		writeTrainingErr(w, err)
		return
	}
	writeJSON(w, 201, job)
}

// handleGetFinetuneJob is GET /api/training/jobs/{id}.
func (s *Server) handleGetFinetuneJob(w http.ResponseWriter, r *http.Request) {
	id, err := uuidParam(r, "id")
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	job, err := s.DB.GetFinetuneJob(r.Context(), id)
	if err != nil {
		writeErr(w, 404, "not found")
		return
	}
	writeJSON(w, 200, job)
}

// handleCancelFinetune is POST /api/training/jobs/{id}/cancel.
func (s *Server) handleCancelFinetune(w http.ResponseWriter, r *http.Request) {
	if !s.needTraining(w) {
		return
	}
	id, err := uuidParam(r, "id")
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	if err := s.Training.CancelFinetune(r.Context(), id); err != nil {
		writeErr(w, 500, "db")
		return
	}
	writeJSON(w, 200, map[string]string{"status": "cancelled"})
}

// handleListAdapters is GET /api/training/adapters.
func (s *Server) handleListAdapters(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.ListAdapters(r.Context(), 100)
	if err != nil {
		writeErr(w, 500, "db")
		return
	}
	writeJSON(w, 200, map[string]any{"adapters": rows})
}

// handleDownloadAdapter is GET /api/training/adapters/{id}/download: the
// adapter as a gzipped tar, to put where the serving engine loads LoRA
// modules from.
func (s *Server) handleDownloadAdapter(w http.ResponseWriter, r *http.Request) {
	if !s.needTraining(w) {
		return
	}
	id, err := uuidParam(r, "id")
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	ad, b, err := s.Training.AdapterFile(r.Context(), id)
	if err != nil {
		writeTrainingErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+ad.Name+`.tar.gz"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.Header().Set("Cache-Control", "private, no-store")
	_, _ = w.Write(b)
}

// handleEvaluateAdapter is POST /api/training/adapters/{id}/evaluate:
// queues the eval gate.
func (s *Server) handleEvaluateAdapter(w http.ResponseWriter, r *http.Request) {
	if !s.needTraining(w) {
		return
	}
	id, err := uuidParam(r, "id")
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	if _, err := s.DB.GetAdapter(r.Context(), id); err != nil {
		writeErr(w, 404, "not found")
		return
	}
	if s.Training.EnqueueEval == nil {
		writeErr(w, 503, "no worker queue")
		return
	}
	if err := s.Training.EnqueueEval(r.Context(), id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 202, map[string]any{"status": "queued", "at": time.Now().UTC()})
}

// handlePromoteAdapter is POST /api/training/adapters/{id}/promote
// {force, task_class?}; DELETE unpromotes. With a task class the adapter
// goes first for that class in the training-adapters policy.
func (s *Server) handlePromoteAdapter(w http.ResponseWriter, r *http.Request) {
	if !s.needTraining(w) {
		return
	}
	id, err := uuidParam(r, "id")
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	var in struct {
		Force     bool   `json:"force"`
		TaskClass string `json:"task_class"`
	}
	_ = decode(r, &in)
	if err := s.Training.Promote(r.Context(), id, in.Force, in.TaskClass); err != nil {
		writeTrainingErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "promoted"})
}

func (s *Server) handleUnpromoteAdapter(w http.ResponseWriter, r *http.Request) {
	if !s.needTraining(w) {
		return
	}
	id, err := uuidParam(r, "id")
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	if err := s.Training.Unpromote(r.Context(), id); err != nil {
		writeTrainingErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// handleDeleteAdapter is DELETE /api/training/adapters/{id}.
func (s *Server) handleDeleteAdapter(w http.ResponseWriter, r *http.Request) {
	if !s.needTraining(w) {
		return
	}
	id, err := uuidParam(r, "id")
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	if err := s.Training.DeleteAdapter(r.Context(), id); err != nil {
		writeTrainingErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// handleListRoutes is GET /api/training/routes: the task-class routes
// the training-adapters policy holds, newest first.
func (s *Server) handleListRoutes(w http.ResponseWriter, r *http.Request) {
	if !s.needTraining(w) {
		return
	}
	routes, err := s.Training.Routes(r.Context())
	if err != nil {
		writeTrainingErr(w, err)
		return
	}
	if routes == nil {
		routes = []training.Route{}
	}
	writeJSON(w, 200, map[string]any{"routes": routes, "policy": training.AdapterPolicy})
}

// handleAddRoute is PUT /api/training/routes {task_class, endpoint_id}:
// send a task class to an endpoint first (an adapter, or a frontier
// model to distil from), ahead of what the other policies prefer.
func (s *Server) handleAddRoute(w http.ResponseWriter, r *http.Request) {
	if !s.needTraining(w) {
		return
	}
	var in struct {
		TaskClass  string `json:"task_class"`
		EndpointID string `json:"endpoint_id"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	if err := s.Training.AddRoute(r.Context(), in.TaskClass, in.EndpointID); err != nil {
		writeTrainingErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// handleRemoveRoute is POST /api/training/routes/remove {task_class, endpoint_id}.
func (s *Server) handleRemoveRoute(w http.ResponseWriter, r *http.Request) {
	if !s.needTraining(w) {
		return
	}
	var in struct {
		TaskClass  string `json:"task_class"`
		EndpointID string `json:"endpoint_id"`
	}
	if err := decode(r, &in); err != nil || in.EndpointID == "" {
		writeErr(w, 400, "endpoint_id required")
		return
	}
	if err := s.Training.RemoveRoute(r.Context(), in.TaskClass, in.EndpointID); err != nil {
		writeTrainingErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// handleSearchBaseModels is GET /api/training/models?q=: base models the
// trainer can load, from the Hugging Face hub through the server (the
// suggested small bases when q is empty). 502 when the hub is unreachable.
func (s *Server) handleSearchBaseModels(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	hub := s.Training.Hub
	if hub == nil {
		hub = &training.Hub{}
		if strings.TrimSpace(q) != "" {
			writeJSON(w, 200, map[string]any{"models": []training.HubModel{}, "source": "none"})
			return
		}
	}
	models, err := hub.Search(r.Context(), q)
	if err != nil {
		s.Log.Warn("hub search", "err", err)
		writeErr(w, 502, "the model hub did not answer; type the id by hand")
		return
	}
	if models == nil {
		models = []training.HubModel{}
	}
	writeJSON(w, 200, map[string]any{"models": models})
}

// trainingRoutes mounts the owner-only training routes.
func (s *Server) trainingRoutes(r chi.Router) {
	r.Get("/datasets", s.handleListDatasets)
	r.Post("/datasets", s.handleCreateDataset)
	r.Get("/datasets/{id}", s.handleGetDataset)
	r.Get("/datasets/{id}/download", s.handleDownloadDataset)
	r.Get("/datasets/{id}/preview", s.handleDatasetPreview)
	r.Delete("/datasets/{id}", s.handleDeleteDataset)
	r.Get("/models", s.handleSearchBaseModels)
	r.Get("/jobs", s.handleListFinetuneJobs)
	r.Post("/jobs", s.handleStartFinetune)
	r.Get("/jobs/{id}", s.handleGetFinetuneJob)
	r.Post("/jobs/{id}/cancel", s.handleCancelFinetune)
	r.Get("/adapters", s.handleListAdapters)
	r.Get("/adapters/{id}/download", s.handleDownloadAdapter)
	r.Post("/adapters/{id}/evaluate", s.handleEvaluateAdapter)
	r.Post("/adapters/{id}/promote", s.handlePromoteAdapter)
	r.Delete("/adapters/{id}/promote", s.handleUnpromoteAdapter)
	r.Delete("/adapters/{id}", s.handleDeleteAdapter)
	r.Get("/routes", s.handleListRoutes)
	r.Put("/routes", s.handleAddRoute)
	r.Post("/routes/remove", s.handleRemoveRoute)
}
