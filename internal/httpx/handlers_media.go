package httpx

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/media"
	"github.com/jking323/ws/internal/store"
	"github.com/jking323/ws/internal/store/blob"
)

// attachmentView is an attachment as the browser sees it: the row minus
// the blob key, plus the URL to fetch it from.
type attachmentView struct {
	ID       uuid.UUID `json:"id"`
	URL      string    `json:"url"`
	Mime     string    `json:"mime"`
	Bytes    int64     `json:"bytes"`
	Filename string    `json:"filename"`
	Width    *int32    `json:"width"`
	Height   *int32    `json:"height"`
}

func attachmentURL(id uuid.UUID) string { return "/api/attachments/" + id.String() }

// mediaJobView is a media_jobs row with its outputs expanded.
type mediaJobView struct {
	store.MediaJob
	Outputs []attachmentView `json:"outputs"`
}

func (s *Server) mediaView(r *http.Request, job store.MediaJob) mediaJobView {
	v := mediaJobView{MediaJob: job, Outputs: []attachmentView{}}
	if s.Media == nil {
		return v
	}
	atts, err := s.Media.Outputs(r.Context(), &job)
	if err != nil {
		s.Log.Warn("media: outputs", "job", job.ID, "err", err)
		return v
	}
	for _, a := range atts {
		v.Outputs = append(v.Outputs, attachmentView{ID: a.ID, URL: attachmentURL(a.ID), Mime: a.Mime, Bytes: a.Bytes, Filename: a.Filename, Width: a.Width, Height: a.Height})
	}
	return v
}

// mediaModel is a media endpoint for the gallery's picker.
type mediaModel struct {
	ID          string   `json:"id"`
	DisplayName string   `json:"display_name"`
	Provider    string   `json:"provider"`
	Local       bool     `json:"local"`
	Engine      string   `json:"engine"`
	Image       bool     `json:"image"`
	ImageEdit   bool     `json:"image_edit"`
	Video       bool     `json:"video"`
	Sizes       []string `json:"sizes"`
	MaxImages   int      `json:"max_images"`
	MaxSeconds  int      `json:"max_seconds"`
	PerImage    float64  `json:"per_image"`
	PerSecond   float64  `json:"per_second"`
	Health      string   `json:"health"`
}

// mediaModels lists enabled media endpoints (part of GET /api/models).
func (s *Server) mediaModels() []mediaModel {
	out := []mediaModel{}
	for _, e := range s.GW.Registry.Endpoints() {
		m := e.Capabilities.Media
		if !e.Enabled || m == nil {
			continue
		}
		mm := mediaModel{ID: e.ID, DisplayName: e.DisplayName, Provider: e.ProviderID, Local: e.Local, Engine: m.Engine,
			Image: m.Image, ImageEdit: m.ImageEdit, Video: m.Video, Sizes: m.Sizes, MaxImages: m.MaxImages, MaxSeconds: m.MaxSeconds,
			Health: e.Health.Status}
		if mm.Sizes == nil {
			mm.Sizes = []string{}
		}
		if mm.MaxImages <= 0 {
			mm.MaxImages = 1
		}
		if !e.Local {
			mm.PerImage, mm.PerSecond = e.Pricing.PerImage, e.Pricing.PerSecond
		}
		out = append(out, mm)
	}
	return out
}

func (s *Server) handleListMedia(w http.ResponseWriter, r *http.Request) {
	id, err := uuidParam(r, "id")
	if err != nil || !s.canAccessProject(r.Context(), id) {
		writeErr(w, 404, "not found")
		return
	}
	if s.Media == nil {
		writeErr(w, 503, "media is not configured")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 60
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	var kind *string
	if k := r.URL.Query().Get("kind"); k != "" {
		kind = &k
	}
	rows, err := s.DB.ListMediaJobsForProject(r.Context(), store.ListMediaJobsForProjectParams{ProjectID: id, Limit: int32(limit), Offset: int32(offset), Kind: kind})
	if err != nil {
		writeErr(w, 500, "db")
		return
	}
	out := make([]mediaJobView, 0, len(rows))
	for _, j := range rows {
		out = append(out, s.mediaView(r, j))
	}
	writeJSON(w, 200, map[string]any{"jobs": out})
}

func (s *Server) handleCreateMedia(w http.ResponseWriter, r *http.Request) {
	p := Principal(r.Context())
	id, err := uuidParam(r, "id")
	if err != nil || !s.canAccessProject(r.Context(), id) {
		writeErr(w, 404, "not found")
		return
	}
	if s.Media == nil {
		writeErr(w, 503, "media is not configured")
		return
	}
	var in struct {
		Kind           string `json:"kind"`
		Prompt         string `json:"prompt"`
		Size           string `json:"size"`
		N              int    `json:"n"`
		Quality        string `json:"quality"`
		Seconds        int    `json:"seconds"`
		Model          string `json:"model"`
		ConversationID string `json:"conversation_id"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	var conv uuid.NullUUID
	if in.ConversationID != "" {
		cid, err := uuid.Parse(in.ConversationID)
		if err != nil {
			writeErr(w, 400, "bad conversation_id")
			return
		}
		c, err := s.DB.GetConversation(r.Context(), cid)
		if err != nil || c.ProjectID != id {
			writeErr(w, 400, "conversation is not in this project")
			return
		}
		conv = uuid.NullUUID{UUID: cid, Valid: true}
	}
	job, err := s.Media.Create(r.Context(), media.CreateParams{
		UserID: p.UserID, ProjectID: id, ConversationID: conv, Kind: in.Kind, Selector: in.Model,
		Inputs: media.Inputs{Prompt: in.Prompt, Size: in.Size, N: in.N, Quality: in.Quality, Seconds: in.Seconds},
	})
	if err != nil {
		switch {
		case errors.Is(err, media.ErrInvalid):
			writeErr(w, 400, strings.TrimPrefix(err.Error(), media.ErrInvalid.Error()+": "))
		case errors.Is(err, gateway.ErrBudgetExceeded):
			writeErr(w, 402, err.Error())
		default:
			s.Log.Error("media: create", "err", err)
			writeErr(w, 500, "could not create the job")
		}
		return
	}
	writeJSON(w, 201, s.mediaView(r, *job))
}

// loadMediaJob checks the caller may see the job through its project.
func (s *Server) loadMediaJob(w http.ResponseWriter, r *http.Request) (*store.MediaJob, bool) {
	id, err := uuidParam(r, "id")
	if err != nil {
		writeErr(w, 400, "bad id")
		return nil, false
	}
	job, err := s.DB.GetMediaJob(r.Context(), id)
	if err != nil || !s.canAccessProject(r.Context(), job.ProjectID) {
		writeErr(w, 404, "not found")
		return nil, false
	}
	return &job, true
}

func (s *Server) handleGetMedia(w http.ResponseWriter, r *http.Request) {
	job, ok := s.loadMediaJob(w, r)
	if !ok {
		return
	}
	writeJSON(w, 200, s.mediaView(r, *job))
}

// handleDeleteMedia cancels a queued job or deletes a finished one. A
// running job cannot be stopped mid-generation (the provider bills it
// anyway) and answers 409. Attachments are kept.
func (s *Server) handleDeleteMedia(w http.ResponseWriter, r *http.Request) {
	job, ok := s.loadMediaJob(w, r)
	if !ok {
		return
	}
	switch job.Status {
	case media.StatusQueued:
		if _, err := s.DB.CancelMediaJob(r.Context(), job.ID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeErr(w, 409, "the job already started")
				return
			}
			writeErr(w, 500, "db")
			return
		}
		writeJSON(w, 200, map[string]string{"status": media.StatusCancelled})
	case media.StatusRunning:
		writeErr(w, 409, "the job is running; it cannot be stopped")
	default:
		if err := s.DB.DeleteMediaJob(r.Context(), job.ID); err != nil {
			writeErr(w, 500, "db")
			return
		}
		writeJSON(w, 200, map[string]string{"status": "deleted"})
	}
}

// handleGetAttachment streams an attachment to its owner, or to anyone
// who can see a media job that produced it. Attachments never change, so
// the browser may cache them for a long time.
func (s *Server) handleGetAttachment(w http.ResponseWriter, r *http.Request) {
	p := Principal(r.Context())
	id, err := uuidParam(r, "id")
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	if s.Blobs == nil {
		writeErr(w, 503, "attachments are not configured")
		return
	}
	att, err := s.DB.GetAttachment(r.Context(), id)
	if err != nil {
		writeErr(w, 404, "not found")
		return
	}
	allowed := att.UserID == p.UserID
	if !allowed {
		projects, _ := s.DB.AttachmentMediaProjects(r.Context(), id)
		for _, pid := range projects {
			if s.canAccessProject(r.Context(), pid) {
				allowed = true
				break
			}
		}
	}
	if !allowed {
		writeErr(w, 404, "not found")
		return
	}
	rc, err := s.Blobs.Get(r.Context(), att.BlobKey)
	if err != nil {
		if errors.Is(err, blob.ErrNotFound) {
			writeErr(w, 404, "blob missing")
			return
		}
		writeErr(w, 500, "blob")
		return
	}
	defer rc.Close()
	mime := att.Mime
	if mime == "" || strings.HasPrefix(mime, "text/html") {
		mime = "application/octet-stream" // never render a stored file as a page on this origin
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Length", strconv.FormatInt(att.Bytes, 10))
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	name := att.Filename
	if name == "" {
		name = id.String()
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", name))
	_, _ = io.Copy(w, rc)
}
