package httpx

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	"github.com/jking323/ws/internal/sandbox"
	"github.com/jking323/ws/internal/store"
)

// Sandbox file, tree, PTY and preview endpoints. serve has no Docker
// socket; everything here relays to the worker's internal API.

func (s *Server) loadCodeProject(w http.ResponseWriter, r *http.Request) (*store.Project, bool) {
	id, err := uuidParam(r, "id")
	if err != nil {
		writeErr(w, 400, "bad id")
		return nil, false
	}
	proj, err := s.DB.GetProject(r.Context(), id)
	if err != nil || !s.canAccessProject(r.Context(), proj.ID) {
		writeErr(w, 404, "not found")
		return nil, false
	}
	if proj.Kind != "code" {
		writeErr(w, 400, "not a code project")
		return nil, false
	}
	if s.Worker == nil {
		writeErr(w, 503, "sandboxes are not available: no worker configured")
		return nil, false
	}
	return &proj, true
}

func workerErr(w http.ResponseWriter, err error) {
	var we *sandbox.WorkerError
	if errors.As(err, &we) {
		code := we.Status
		if code == 403 {
			code = 502 // misconfigured shared secret, not the user's fault
		}
		writeErr(w, code, we.Msg)
		return
	}
	writeErr(w, 502, err.Error())
}

// ensureSandbox returns the running sandbox for the caller in a project.
func (s *Server) ensureSandbox(ctx context.Context, proj *store.Project) (sandbox.Info, error) {
	p := Principal(ctx)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	return s.Worker.Ensure(ctx, proj.ID, p.UserID)
}

// handleSandbox ensures and describes the caller's sandbox.
func (s *Server) handleSandbox(w http.ResponseWriter, r *http.Request) {
	proj, ok := s.loadCodeProject(w, r)
	if !ok {
		return
	}
	info, err := s.ensureSandbox(r.Context(), proj)
	if err != nil {
		workerErr(w, err)
		return
	}
	out := map[string]any{
		"id": info.ID, "status": info.Status, "runtime": info.Runtime, "container_id": short(info.ContainerID),
		"repo_url": proj.RepoUrl,
	}
	if s.Preview != nil {
		// Pattern with the sandbox filled in and {port} left for the UI.
		out["preview_pattern"] = strings.Replace(s.Preview.Pattern, "{id}", shortID(info.ID), 1)
	}
	writeJSON(w, 200, out)
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func (s *Server) handleSandboxStop(w http.ResponseWriter, r *http.Request) {
	proj, ok := s.loadCodeProject(w, r)
	if !ok {
		return
	}
	p := Principal(r.Context())
	row, err := s.DB.GetSandbox(r.Context(), store.GetSandboxParams{ProjectID: proj.ID, UserID: p.UserID})
	if err != nil {
		w.WriteHeader(204)
		return
	}
	if err := s.Worker.Stop(r.Context(), row.ID); err != nil {
		workerErr(w, err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) handleFSTree(w http.ResponseWriter, r *http.Request) {
	proj, ok := s.loadCodeProject(w, r)
	if !ok {
		return
	}
	info, err := s.ensureSandbox(r.Context(), proj)
	if err != nil {
		workerErr(w, err)
		return
	}
	raw, err := s.Worker.Tree(r.Context(), info.ID, r.URL.Query().Get("path"))
	if err != nil {
		workerErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

func (s *Server) handleFSRead(w http.ResponseWriter, r *http.Request) {
	proj, ok := s.loadCodeProject(w, r)
	if !ok {
		return
	}
	info, err := s.ensureSandbox(r.Context(), proj)
	if err != nil {
		workerErr(w, err)
		return
	}
	data, binary, err := s.Worker.ReadFile(r.Context(), info.ID, r.URL.Query().Get("path"))
	if err != nil {
		workerErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"path": r.URL.Query().Get("path"), "binary": binary, "size": len(data), "content": string(data)})
}

func (s *Server) handleFSWrite(w http.ResponseWriter, r *http.Request) {
	proj, ok := s.loadCodeProject(w, r)
	if !ok {
		return
	}
	var in struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := decode(r, &in); err != nil || in.Path == "" {
		writeErr(w, 400, "path and content required")
		return
	}
	info, err := s.ensureSandbox(r.Context(), proj)
	if err != nil {
		workerErr(w, err)
		return
	}
	if err := s.Worker.WriteFile(r.Context(), info.ID, in.Path, []byte(in.Content)); err != nil {
		workerErr(w, err)
		return
	}
	w.WriteHeader(204)
}

// handlePTY bridges the browser's WebSocket to the worker's PTY WebSocket.
func (s *Server) handlePTY(w http.ResponseWriter, r *http.Request) {
	proj, ok := s.loadCodeProject(w, r)
	if !ok {
		return
	}
	info, err := s.ensureSandbox(r.Context(), proj)
	if err != nil {
		workerErr(w, err)
		return
	}
	q := r.URL.Query()
	target := s.Worker.WSURL("/internal/sandboxes/" + info.ID.String() + "/pty?cols=" + q.Get("cols") + "&rows=" + q.Get("rows") + "&cwd=" + q.Get("cwd"))
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	up, _, err := websocket.Dial(ctx, target, &websocket.DialOptions{HTTPHeader: http.Header{sandbox.InternalHeader: {s.Worker.Token}}})
	if err != nil {
		writeErr(w, 502, "worker pty: "+err.Error())
		return
	}
	defer up.CloseNow()
	down, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer down.CloseNow()
	up.SetReadLimit(1 << 20)
	down.SetReadLimit(1 << 20)
	pump := func(from, to *websocket.Conn) {
		defer cancel()
		for {
			typ, data, err := from.Read(ctx)
			if err != nil {
				return
			}
			if err := to.Write(ctx, typ, data); err != nil {
				return
			}
		}
	}
	go pump(up, down)
	pump(down, up)
	down.Close(websocket.StatusNormalClosure, "")
	up.Close(websocket.StatusNormalClosure, "")
}

// handlePreviewLink mints a preview cookie token and redirects the browser
// to the preview host for a port.
func (s *Server) handlePreviewLink(w http.ResponseWriter, r *http.Request) {
	proj, ok := s.loadCodeProject(w, r)
	if !ok {
		return
	}
	port, err := strconv.Atoi(chi.URLParam(r, "port"))
	if err != nil || port < 1 || port > 65535 {
		writeErr(w, 400, "bad port")
		return
	}
	if s.Preview == nil {
		writeErr(w, 503, "previews are not configured")
		return
	}
	info, err := s.ensureSandbox(r.Context(), proj)
	if err != nil {
		workerErr(w, err)
		return
	}
	loc := s.Preview.AuthURL(info.ID, port)
	if loc == "" {
		writeErr(w, 503, "previews are not configured")
		return
	}
	http.Redirect(w, r, loc, http.StatusFound)
}
