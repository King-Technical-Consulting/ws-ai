package sandbox

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/moby/moby/client"
)

// InternalToken derives the shared secret serve uses to call the worker's
// internal API. Both processes hold WS_SESSION_SECRET; nothing else is
// configured. The API is reachable only on the Compose network.
func InternalToken(sessionSecret string) string {
	m := hmac.New(sha256.New, []byte(sessionSecret))
	m.Write([]byte("ws-internal-api-v1"))
	return hex.EncodeToString(m.Sum(nil))
}

// InternalHeader carries InternalToken.
const InternalHeader = "X-WS-Internal"

// Info is what serve learns about a sandbox.
type Info struct {
	ID          uuid.UUID `json:"id"`
	ProjectID   uuid.UUID `json:"project_id"`
	UserID      uuid.UUID `json:"user_id"`
	ContainerID string    `json:"container_id"`
	IP          string    `json:"ip"`
	Status      string    `json:"status"`
	Runtime     string    `json:"runtime"`
}

// Entry is one directory listing row.
type Entry struct {
	Name string `json:"name"`
	Type string `json:"type"` // file | dir | link | other
	Size int64  `json:"size"`
}

// InternalAPI is the worker-side HTTP surface serve talks to for sandbox
// files, directory trees, PTYs and lifecycle. Only the worker has Docker.
type InternalAPI struct {
	M     *Manager
	Token string
	Log   *slog.Logger
}

// Handler mounts the API.
func (a *InternalAPI) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			got := req.Header.Get(InternalHeader)
			if got == "" {
				got = req.URL.Query().Get("token") // browsers can't set headers on WebSocket dials; serve relays with the header, this is for tooling
			}
			if subtle.ConstantTimeCompare([]byte(got), []byte(a.Token)) != 1 {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, req)
		})
	})
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	r.Post("/internal/sandboxes/ensure", a.ensure)
	r.Get("/internal/sandboxes/{id}", a.get)
	r.Post("/internal/sandboxes/{id}/stop", a.stop)
	r.Get("/internal/sandboxes/{id}/tree", a.tree)
	r.Get("/internal/sandboxes/{id}/file", a.readFile)
	r.Put("/internal/sandboxes/{id}/file", a.writeFile)
	r.Get("/internal/sandboxes/{id}/pty", a.pty)
	return r
}

func jsonOut(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, code int, msg string) {
	jsonOut(w, code, map[string]string{"error": msg})
}

// Get loads a sandbox by id.
func (m *Manager) Get(ctx context.Context, id uuid.UUID) (*Sandbox, error) {
	row, err := m.db.GetSandboxByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return fromRow(row), nil
}

// Describe returns Info with live container state and IP.
func (m *Manager) Describe(ctx context.Context, sb *Sandbox) Info {
	info := Info{ID: sb.ID, ProjectID: sb.ProjectID, UserID: sb.UserID, ContainerID: sb.ContainerID, Runtime: sb.Runtime, Status: "stopped"}
	if sb.ContainerID == "" {
		return info
	}
	insp, err := m.cli.ContainerInspect(ctx, sb.ContainerID, client.ContainerInspectOptions{})
	if err != nil {
		info.Status = "missing"
		return info
	}
	if insp.Container.State != nil && insp.Container.State.Running {
		info.Status = "running"
	}
	if insp.Container.NetworkSettings != nil {
		for name, n := range insp.Container.NetworkSettings.Networks {
			if n == nil || !n.IPAddress.IsValid() {
				continue
			}
			if name == m.cfg.Network || info.IP == "" {
				info.IP = n.IPAddress.String()
			}
		}
	}
	return info
}

func (a *InternalAPI) ensure(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ProjectID uuid.UUID `json:"project_id"`
		UserID    uuid.UUID `json:"user_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.ProjectID == uuid.Nil || in.UserID == uuid.Nil {
		jsonErr(w, 400, "project_id and user_id required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute) // first use may pull the image and clone
	defer cancel()
	sb, err := a.M.Ensure(ctx, in.ProjectID, in.UserID)
	if err != nil {
		jsonErr(w, 500, err.Error())
		return
	}
	jsonOut(w, 200, a.M.Describe(r.Context(), sb))
}

func (a *InternalAPI) load(w http.ResponseWriter, r *http.Request) (*Sandbox, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonErr(w, 400, "bad id")
		return nil, false
	}
	sb, err := a.M.Get(r.Context(), id)
	if err != nil {
		jsonErr(w, 404, "no such sandbox")
		return nil, false
	}
	return sb, true
}

// running makes sure the container is up (restarting a stopped one).
func (a *InternalAPI) running(w http.ResponseWriter, r *http.Request) (*Sandbox, bool) {
	sb, ok := a.load(w, r)
	if !ok {
		return nil, false
	}
	sb2, err := a.M.Ensure(r.Context(), sb.ProjectID, sb.UserID)
	if err != nil {
		jsonErr(w, 500, err.Error())
		return nil, false
	}
	return sb2, true
}

func (a *InternalAPI) get(w http.ResponseWriter, r *http.Request) {
	sb, ok := a.load(w, r)
	if !ok {
		return
	}
	jsonOut(w, 200, a.M.Describe(r.Context(), sb))
}

func (a *InternalAPI) stop(w http.ResponseWriter, r *http.Request) {
	sb, ok := a.load(w, r)
	if !ok {
		return
	}
	if err := a.M.Stop(r.Context(), sb); err != nil {
		jsonErr(w, 500, err.Error())
		return
	}
	w.WriteHeader(204)
}

// Tree lists one directory (not recursive) with types and sizes.
func (m *Manager) Tree(ctx context.Context, sb *Sandbox, dir string) ([]Entry, error) {
	res, err := m.Exec(ctx, sb, ExecOptions{
		Cmd:     []string{"find", dir, "-mindepth", "1", "-maxdepth", "1", "-printf", "%y\t%s\t%f\n"},
		Timeout: 20 * time.Second, MaxOutput: 1 << 20,
	})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 && strings.TrimSpace(res.Stdout) == "" {
		return nil, errors.New(strings.TrimSpace(res.Stderr))
	}
	var out []Entry
	for _, line := range strings.Split(res.Stdout, "\n") {
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		e := Entry{Name: parts[2]}
		switch parts[0] {
		case "d":
			e.Type = "dir"
		case "f":
			e.Type = "file"
		case "l":
			e.Type = "link"
		default:
			e.Type = "other"
		}
		e.Size, _ = strconv.ParseInt(parts[1], 10, 64)
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		di, dj := out[i].Type == "dir", out[j].Type == "dir"
		if di != dj {
			return di
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

func (a *InternalAPI) tree(w http.ResponseWriter, r *http.Request) {
	sb, ok := a.running(w, r)
	if !ok {
		return
	}
	p, err := resolvePath(r.URL.Query().Get("path"))
	if err != nil {
		jsonErr(w, 400, err.Error())
		return
	}
	entries, err := a.M.Tree(r.Context(), sb, p)
	if err != nil {
		jsonErr(w, 500, err.Error())
		return
	}
	if entries == nil {
		entries = []Entry{}
	}
	jsonOut(w, 200, map[string]any{"path": p, "entries": entries})
}

func (a *InternalAPI) readFile(w http.ResponseWriter, r *http.Request) {
	sb, ok := a.running(w, r)
	if !ok {
		return
	}
	p, err := resolvePath(r.URL.Query().Get("path"))
	if err != nil {
		jsonErr(w, 400, err.Error())
		return
	}
	data, err := a.M.ReadFile(r.Context(), sb, p, 8<<20)
	if err != nil {
		jsonErr(w, 404, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-WS-Path", p)
	if isBinary(data) {
		w.Header().Set("X-WS-Binary", "1")
	}
	w.WriteHeader(200)
	_, _ = w.Write(data)
}

func (a *InternalAPI) writeFile(w http.ResponseWriter, r *http.Request) {
	sb, ok := a.running(w, r)
	if !ok {
		return
	}
	p, err := resolvePath(r.URL.Query().Get("path"))
	if err != nil {
		jsonErr(w, 400, err.Error())
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 8<<20+1))
	if err != nil || len(data) > 8<<20 {
		jsonErr(w, 413, "file too large (8 MiB limit)")
		return
	}
	if err := a.M.WriteFile(r.Context(), sb, p, data); err != nil {
		jsonErr(w, 500, err.Error())
		return
	}
	w.WriteHeader(204)
}

// ptyMsg is a control frame on the PTY WebSocket (text frames). Binary
// frames are terminal bytes in both directions.
type ptyMsg struct {
	Type string `json:"type"`
	Cols uint   `json:"cols,omitempty"`
	Rows uint   `json:"rows,omitempty"`
}

// pty attaches an interactive shell to the WebSocket.
func (a *InternalAPI) pty(w http.ResponseWriter, r *http.Request) {
	sb, ok := a.running(w, r)
	if !ok {
		return
	}
	cols, _ := strconv.Atoi(r.URL.Query().Get("cols"))
	rows, _ := strconv.Atoi(r.URL.Query().Get("rows"))
	if cols <= 0 {
		cols = 120
	}
	if rows <= 0 {
		rows = 32
	}
	cwd, err := resolvePath(r.URL.Query().Get("cwd"))
	if err != nil {
		cwd = Workspace
	}
	ex, err := a.M.cli.ExecCreate(r.Context(), sb.ContainerID, client.ExecCreateOptions{
		Cmd: []string{"bash", "-i"}, WorkingDir: cwd, User: "dev", TTY: true,
		AttachStdin: true, AttachStdout: true, AttachStderr: true,
		ConsoleSize: client.ConsoleSize{Height: uint(rows), Width: uint(cols)},
		Env:         []string{"TERM=xterm-256color", "COLORTERM=truecolor", fmt.Sprintf("COLUMNS=%d", cols), fmt.Sprintf("LINES=%d", rows)},
	})
	if err != nil {
		jsonErr(w, 500, "exec: "+err.Error())
		return
	}
	// Attach before upgrading so a Docker failure is a clean HTTP error.
	att, err := a.M.cli.ExecAttach(context.Background(), ex.ID, client.ExecAttachOptions{TTY: true, ConsoleSize: client.ConsoleSize{Height: uint(rows), Width: uint(cols)}})
	if err != nil {
		jsonErr(w, 500, "attach: "+err.Error())
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		att.Close()
		return
	}
	defer c.CloseNow()
	defer att.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.SetReadLimit(1 << 20)

	// container → browser
	go func() {
		defer cancel()
		buf := make([]byte, 32<<10)
		for {
			n, err := att.Reader.Read(buf)
			if n > 0 {
				if werr := c.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	// browser → container
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			break
		}
		switch typ {
		case websocket.MessageBinary:
			if _, err := att.Conn.Write(data); err != nil {
				break
			}
		case websocket.MessageText:
			var m ptyMsg
			if json.Unmarshal(data, &m) == nil && m.Type == "resize" && m.Cols > 0 && m.Rows > 0 {
				_, _ = a.M.cli.ExecResize(ctx, ex.ID, client.ExecResizeOptions{Height: m.Rows, Width: m.Cols})
			}
		}
	}
	_ = a.M.db.TouchSandbox(context.WithoutCancel(ctx), sb.ID)
	c.Close(websocket.StatusNormalClosure, "bye")
}
