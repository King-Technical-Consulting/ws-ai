// Package externalapi exposes the gateway to outside tools as an
// OpenAI-compatible (/v1/chat/completions) and Anthropic-compatible
// (/v1/messages) API, so Claude Code, Cursor, opencode and similar clients
// can use the platform's routing, failover and ledger with a platform API
// key. Compaction is off for these calls: the client owns its context.
package externalapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/gateway"
)

// Principal is what the auth layer resolved for the request.
type Principal struct {
	UserID   uuid.UUID
	APIKeyID uuid.NullUUID
	// DefaultSelector is the key's default policy alias, used when the
	// client's model name is not a known endpoint or alias.
	DefaultSelector string
}

// PrincipalFunc extracts the principal from the request context.
type PrincipalFunc func(ctx context.Context) *Principal

// Server serves both protocols.
type Server struct {
	GW        *gateway.Gateway
	Principal PrincipalFunc
	Log       *slog.Logger
}

// Routes mounts the handlers on mux under /v1.
func (s *Server) Routes(mux interface {
	Get(string, http.HandlerFunc)
	Post(string, http.HandlerFunc)
}) {
	mux.Get("/v1/models", s.handleModels)
	mux.Get("/v1/models/{id}", s.handleModel)
	mux.Post("/v1/chat/completions", s.handleChatCompletions)
	mux.Post("/v1/messages", s.handleMessages)
	mux.Post("/v1/messages/count_tokens", s.handleCountTokens)
}

// resolveSelector maps a client model name onto a selector the router
// understands: an endpoint id, an alias, or the key's default.
func (s *Server) resolveSelector(model string, p *Principal) (string, gateway.TaskClass) {
	model = strings.TrimSpace(model)
	tc := gateway.TaskChat
	switch model {
	case "code":
		tc = gateway.TaskCode
	}
	if model == "" || model == "default" {
		if p != nil && p.DefaultSelector != "" {
			return p.DefaultSelector, tc
		}
		return "auto", tc
	}
	if _, ok := s.GW.Registry.Endpoint(model); ok {
		return model, tc
	}
	for _, pol := range s.GW.Router.Policies() {
		if _, ok := pol.Aliases[model]; ok {
			return model, tc
		}
	}
	// Clients often send a provider's own model name (e.g. "claude-sonnet-5-5").
	// Match on the endpoint's model_name across providers.
	for _, ep := range s.GW.Registry.Endpoints() {
		if ep.Enabled && (ep.ModelName == model || strings.HasSuffix(ep.ID, "/"+model)) {
			return ep.ID, tc
		}
	}
	if p != nil && p.DefaultSelector != "" {
		return p.DefaultSelector, tc
	}
	return "auto", tc
}

// SessionHeader is the client session id Claude Code sends on every call.
const SessionHeader = "x-claude-code-session-id"

func (s *Server) metadata(r *http.Request, p *Principal, tc gateway.TaskClass) gateway.Metadata {
	md := gateway.Metadata{TaskClass: tc, External: true}
	if p != nil {
		md.UserID = p.UserID.String()
		if p.APIKeyID.Valid {
			md.APIKeyID = p.APIKeyID.UUID.String()
		}
	}
	if r != nil {
		if sid := strings.TrimSpace(r.Header.Get(SessionHeader)); sid != "" && len(sid) <= 128 {
			md.SessionID = sid
		}
	}
	return md
}

// estimateTokens is a rough pre-flight count: 4 chars per token.
func estimateTokens(req *gateway.Request) int {
	n := len(req.System)
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			n += len(p.Text) + len(p.Args)
			for _, c := range p.Content {
				n += len(c.Text)
			}
			if p.Kind == gateway.PartImage {
				n += 6000
			}
		}
	}
	for _, t := range req.Tools {
		n += len(t.Name) + len(t.Description) + len(t.InputSchema)
	}
	return n / 4
}

func newID(prefix string) string {
	return prefix + strings.ReplaceAll(uuid.NewString(), "-", "")[:24]
}

// sseWriter writes server-sent events and flushes after each.
type sseWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func newSSE(w http.ResponseWriter) *sseWriter {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	return &sseWriter{w: w, rc: http.NewResponseController(w)}
}

func (s *sseWriter) event(name string, v any) {
	b, _ := json.Marshal(v)
	if name != "" {
		fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, b)
	} else {
		fmt.Fprintf(s.w, "data: %s\n\n", b)
	}
	_ = s.rc.Flush()
}

func (s *sseWriter) raw(line string) {
	fmt.Fprint(s.w, line)
	_ = s.rc.Flush()
}

func readJSON(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 64<<20)
	dec := json.NewDecoder(r.Body)
	return dec.Decode(v)
}

// statusFor maps gateway errors to HTTP statuses.
func statusFor(err error) int {
	switch {
	case err == nil:
		return 200
	case isErr(err, gateway.ErrBudgetExceeded):
		return http.StatusPaymentRequired
	case isErr(err, gateway.ErrNoRoute):
		return http.StatusServiceUnavailable
	}
	return http.StatusBadGateway
}

func isErr(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

var _ = time.Now
