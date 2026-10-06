package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jking323/ws/internal/ccjobs"
	"github.com/jking323/ws/internal/ccweb"
)

// Claude Code job tab (docs/CLAUDE_CODE_JOBS.md §6.5): the job list from
// cc_jobs, handle reports from wsj, and the read-only terminal bridge.
// Owner-only (the subscription is one person's) and, on top of ws login,
// served only to the networks in WS_CC_WEB_ALLOW: the tailnet by default.
// Nothing here parses or stores what a terminal shows; the bridge relays
// bytes to one browser tab and drops them.

// netGate refuses requests whose client address is outside allow. allow is
// the WS_CC_WEB_ALLOW list; "*" disables the check. A list that parses to
// nothing refuses everything rather than failing open.
func netGate(allow string) func(http.Handler) http.Handler {
	allow = strings.TrimSpace(allow)
	var nets []*net.IPNet
	if allow != "*" {
		for _, s := range strings.Split(allow, ",") {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			if !strings.Contains(s, "/") {
				if strings.Contains(s, ":") {
					s += "/128"
				} else {
					s += "/32"
				}
			}
			if _, n, err := net.ParseCIDR(s); err == nil {
				nets = append(nets, n)
			}
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if allow != "*" {
				host := r.RemoteAddr
				if h, _, err := net.SplitHostPort(host); err == nil {
					host = h
				}
				ip := net.ParseIP(strings.Trim(host, "[]"))
				ok := false
				for _, n := range nets {
					if ip != nil && n.Contains(ip) {
						ok = true
						break
					}
				}
				if !ok {
					writeErr(w, 403, "this route is only served on the tailnet")
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

func (s *Server) ccJobs(w http.ResponseWriter) (*ccweb.Registry, bool) {
	if s.CCJobs == nil {
		writeErr(w, 503, "the job tab is not configured on this host")
		return nil, false
	}
	return s.CCJobs, true
}

// handleListCCJobs lists jobs, after a (throttled) refresh against the
// targets unless refresh=0.
func (s *Server) handleListCCJobs(w http.ResponseWriter, r *http.Request) {
	reg, ok := s.ccJobs(w)
	if !ok {
		return
	}
	var refresh ccweb.RefreshReport
	if r.URL.Query().Get("refresh") != "0" {
		refresh = reg.Refresh(r.Context(), r.URL.Query().Get("refresh") == "force")
	} else {
		refresh = reg.Last()
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := reg.List(r.Context(), int32(limit))
	if err != nil {
		writeErr(w, 500, "db")
		return
	}
	out := map[string]any{"jobs": rows, "refresh": refresh}
	if b, err := reg.Budget(r.Context()); err == nil {
		out["budget"] = map[string]any{"week": b.Week, "used": b.Used, "cap": b.Cap, "soft_pct": b.SoftPct, "level": b.Level()}
	}
	writeJSON(w, 200, out)
}

// handleReportCCJob is POST /api/jobs/cc: wsj reports a launch.
func (s *Server) handleReportCCJob(w http.ResponseWriter, r *http.Request) {
	reg, ok := s.ccJobs(w)
	if !ok {
		return
	}
	var rep ccjobs.Report
	if err := decode(r, &rep); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	p := Principal(r.Context())
	row, err := reg.Report(r.Context(), uuid.NullUUID{UUID: p.UserID, Valid: true}, ccweb.SourceWSJ, rep)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 201, row)
}

// handleKillCCJob is DELETE /api/jobs/cc/{id}: wsj reports a kill. The
// window is already gone on the target; nothing is killed from here.
func (s *Server) handleKillCCJob(w http.ResponseWriter, r *http.Request) {
	reg, ok := s.ccJobs(w)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if !ccjobs.ValidJobID(id) {
		writeErr(w, 400, "bad job id")
		return
	}
	if err := reg.Killed(r.Context(), id); err != nil {
		writeErr(w, 404, "no such job")
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// termMsg is a text frame on the terminal socket. The browser may only
// send its size; the server sends an error when the view cannot open.
// Terminal bytes never flow from the browser; the attach is read-only on
// the tmux side too (attach -r).
type termMsg struct {
	Type    string `json:"type"`
	Cols    int    `json:"cols,omitempty"`
	Rows    int    `json:"rows,omitempty"`
	Message string `json:"message,omitempty"`
}

// handleCCJobTerm is GET /api/jobs/cc/{id}/term: a WebSocket carrying the
// job window's terminal bytes (binary frames) to the browser.
func (s *Server) handleCCJobTerm(w http.ResponseWriter, r *http.Request) {
	reg, ok := s.ccJobs(w)
	if !ok {
		return
	}
	cols, _ := strconv.Atoi(r.URL.Query().Get("cols"))
	rows, _ := strconv.Atoi(r.URL.Query().Get("rows"))
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	c.SetReadLimit(64 << 10)
	// The socket is accepted first so the page can show why a view did
	// not open (a browser cannot read the body of a failed upgrade): the
	// reason goes down as one text frame, then the socket closes.
	term, err := reg.Open(ctx, chi.URLParam(r, "id"), cols, rows)
	if err != nil {
		msg := err.Error()
		if errors.Is(err, ccweb.ErrGone) {
			msg = "the job's window is gone"
		}
		b, _ := json.Marshal(termMsg{Type: "error", Message: msg})
		_ = c.Write(ctx, websocket.MessageText, b)
		c.Close(websocket.StatusPolicyViolation, "unavailable")
		return
	}

	// terminal → browser
	go func() {
		defer cancel()
		buf := make([]byte, 32<<10)
		for {
			n, err := term.Read(buf)
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
	// browser → size only
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			break
		}
		if typ != websocket.MessageText {
			continue
		}
		var m termMsg
		if json.Unmarshal(data, &m) == nil && m.Type == "resize" {
			term.Resize(m.Cols, m.Rows)
		}
	}
	cancel()
	_ = term.Close()
	c.Close(websocket.StatusNormalClosure, "bye")
}
