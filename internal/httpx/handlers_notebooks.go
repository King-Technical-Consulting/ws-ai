package httpx

import (
	"errors"
	"io"
	"net/http"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/notebooks"
)

// Research notebooks (PLAN M13 cut 6). Every handler works on the
// signed-in member's own notebooks; another member's id is 404.

const maxNotebookUpload = 32 << 20

func (s *Server) notebookErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, notebooks.ErrNotFound):
		writeErr(w, 404, "not found")
	case errors.Is(err, notebooks.ErrInvalid):
		writeErr(w, 400, err.Error())
	default:
		s.Log.Warn("notebooks", "err", err)
		writeErr(w, 500, "db")
	}
}

func (s *Server) nbReady(w http.ResponseWriter) bool {
	if s.Notebooks == nil {
		writeErr(w, 503, "notebooks unavailable")
		return false
	}
	return true
}

func (s *Server) handleListNotebooks(w http.ResponseWriter, r *http.Request) {
	if !s.nbReady(w) {
		return
	}
	out, err := s.Notebooks.List(r.Context(), Principal(r.Context()).UserID)
	if err != nil {
		s.notebookErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"notebooks": out})
}

func (s *Server) handleCreateNotebook(w http.ResponseWriter, r *http.Request) {
	if !s.nbReady(w) {
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	out, err := s.Notebooks.Create(r.Context(), Principal(r.Context()).UserID, in.Name)
	if err != nil {
		s.notebookErr(w, err)
		return
	}
	writeJSON(w, 201, out)
}

func (s *Server) handleGetNotebook(w http.ResponseWriter, r *http.Request) {
	if !s.nbReady(w) {
		return
	}
	id, err := uuidParam(r, "id")
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	out, err := s.Notebooks.Get(r.Context(), Principal(r.Context()).UserID, id)
	if err != nil {
		s.notebookErr(w, err)
		return
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleRenameNotebook(w http.ResponseWriter, r *http.Request) {
	if !s.nbReady(w) {
		return
	}
	id, err := uuidParam(r, "id")
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	if err := s.Notebooks.Rename(r.Context(), Principal(r.Context()).UserID, id, in.Name); err != nil {
		s.notebookErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (s *Server) handleDeleteNotebook(w http.ResponseWriter, r *http.Request) {
	if !s.nbReady(w) {
		return
	}
	id, err := uuidParam(r, "id")
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	if err := s.Notebooks.Delete(r.Context(), Principal(r.Context()).UserID, id); err != nil {
		s.notebookErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// handleAddNotebookSource takes a multipart file (`file`), or JSON with one
// of `url`, `text` (and `title`), or `conversation_id`.
func (s *Server) handleAddNotebookSource(w http.ResponseWriter, r *http.Request) {
	if !s.nbReady(w) {
		return
	}
	id, err := uuidParam(r, "id")
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	uid := Principal(r.Context()).UserID
	ct := r.Header.Get("Content-Type")
	if len(ct) >= 19 && ct[:19] == "multipart/form-data" {
		r.Body = http.MaxBytesReader(w, r.Body, maxNotebookUpload)
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			writeErr(w, 400, "the file is too large or the upload was cut short")
			return
		}
		f, h, err := r.FormFile("file")
		if err != nil {
			writeErr(w, 400, "file is required")
			return
		}
		defer f.Close()
		data, err := io.ReadAll(f)
		if err != nil {
			writeErr(w, 400, "could not read the upload")
			return
		}
		out, err := s.Notebooks.AddFile(r.Context(), uid, id, h.Header.Get("Content-Type"), h.Filename, data)
		if err != nil {
			s.notebookErr(w, err)
			return
		}
		writeJSON(w, 201, out)
		return
	}
	var in struct {
		URL            string    `json:"url"`
		Title          string    `json:"title"`
		Text           string    `json:"text"`
		ConversationID uuid.UUID `json:"conversation_id"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	var out *notebooks.Source
	switch {
	case in.URL != "":
		out, err = s.Notebooks.AddURL(r.Context(), uid, id, in.URL)
	case in.Text != "":
		out, err = s.Notebooks.AddText(r.Context(), uid, id, in.Title, in.Text)
	case in.ConversationID != uuid.Nil:
		out, err = s.Notebooks.AddConversation(r.Context(), uid, id, in.ConversationID)
	default:
		writeErr(w, 400, "send a file, a url, text or a conversation_id")
		return
	}
	if err != nil {
		s.notebookErr(w, err)
		return
	}
	writeJSON(w, 201, out)
}

func (s *Server) handleGetNotebookSource(w http.ResponseWriter, r *http.Request) {
	if !s.nbReady(w) {
		return
	}
	id, err := uuidParam(r, "id")
	sid, err2 := uuidParam(r, "sid")
	if err != nil || err2 != nil {
		writeErr(w, 400, "bad id")
		return
	}
	out, err := s.Notebooks.Source(r.Context(), Principal(r.Context()).UserID, id, sid)
	if err != nil {
		s.notebookErr(w, err)
		return
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleDeleteNotebookSource(w http.ResponseWriter, r *http.Request) {
	if !s.nbReady(w) {
		return
	}
	id, err := uuidParam(r, "id")
	sid, err2 := uuidParam(r, "sid")
	if err != nil || err2 != nil {
		writeErr(w, 400, "bad id")
		return
	}
	if err := s.Notebooks.DeleteSource(r.Context(), Principal(r.Context()).UserID, id, sid); err != nil {
		s.notebookErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (s *Server) handleAddNotebookNote(w http.ResponseWriter, r *http.Request) {
	if !s.nbReady(w) {
		return
	}
	id, err := uuidParam(r, "id")
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	var in struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	out, err := s.Notebooks.AddNote(r.Context(), Principal(r.Context()).UserID, id, in.Title, in.Body)
	if err != nil {
		s.notebookErr(w, err)
		return
	}
	writeJSON(w, 201, out)
}

func (s *Server) handleUpdateNotebookNote(w http.ResponseWriter, r *http.Request) {
	if !s.nbReady(w) {
		return
	}
	id, err := uuidParam(r, "id")
	nid, err2 := uuidParam(r, "nid")
	if err != nil || err2 != nil {
		writeErr(w, 400, "bad id")
		return
	}
	var in struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	out, err := s.Notebooks.UpdateNote(r.Context(), Principal(r.Context()).UserID, id, nid, in.Title, in.Body)
	if err != nil {
		s.notebookErr(w, err)
		return
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleDeleteNotebookNote(w http.ResponseWriter, r *http.Request) {
	if !s.nbReady(w) {
		return
	}
	id, err := uuidParam(r, "id")
	nid, err2 := uuidParam(r, "nid")
	if err != nil || err2 != nil {
		writeErr(w, 400, "bad id")
		return
	}
	if err := s.Notebooks.DeleteNote(r.Context(), Principal(r.Context()).UserID, id, nid); err != nil {
		s.notebookErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// handleNotebookChat returns the notebook's conversation id, creating the
// conversation on first use.
func (s *Server) handleNotebookChat(w http.ResponseWriter, r *http.Request) {
	if !s.nbReady(w) {
		return
	}
	id, err := uuidParam(r, "id")
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	cid, err := s.Notebooks.Chat(r.Context(), Principal(r.Context()).UserID, id)
	if err != nil {
		s.notebookErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"conversation_id": cid})
}
