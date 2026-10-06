// Package httpx is the HTTP API: auth, projects, conversations, chat
// streaming, artifacts, admin, the external /v1 API, and the embedded
// frontend.
package httpx

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/jking323/ws/internal/agents"
	"github.com/jking323/ws/internal/artifacts"
	"github.com/jking323/ws/internal/auth"
	"github.com/jking323/ws/internal/ccweb"
	"github.com/jking323/ws/internal/chat"
	"github.com/jking323/ws/internal/config"
	"github.com/jking323/ws/internal/externalapi"
	"github.com/jking323/ws/internal/fleet/rental"
	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/github"
	"github.com/jking323/ws/internal/jobs"
	"github.com/jking323/ws/internal/mcpserver"
	"github.com/jking323/ws/internal/media"
	"github.com/jking323/ws/internal/sandbox"
	"github.com/jking323/ws/internal/store"
	"github.com/jking323/ws/internal/store/blob"
)

// Server holds dependencies for handlers.
type Server struct {
	Cfg       *config.Config
	DB        *store.DB
	Auth      *auth.Service
	GW        *gateway.Gateway
	Chat      *chat.Service
	Artifacts *artifacts.Service
	Jobs      *jobs.Client // nil in tests
	// Worker relays sandbox file/PTY calls to the worker; nil disables.
	Worker *sandbox.WorkerClient
	// Preview serves sandbox dev servers on preview hostnames; nil disables.
	Preview *PreviewProxy
	// GitHub resolves installations for projects; nil when unconfigured.
	GitHub *github.Host
	// CCJobs is the Claude Code job registry behind /api/jobs/cc; nil
	// answers 503 there.
	CCJobs *ccweb.Registry
	// MCP serves ws as an MCP server at /mcp; nil leaves the route out.
	MCP *mcpserver.Server
	// Media is the image and video gateway behind the project media
	// routes; nil answers 503 there.
	Media *media.Service
	// Blobs serves attachments; nil answers 503 for them.
	Blobs blob.Store
	// Agents starts and steers long-lived agent runs; nil answers 503 on
	// the routes that need it.
	Agents *agents.Service
	// Rental rents GPUs on demand (Admin); nil answers 503.
	Rental *rental.Controller
	Log    *slog.Logger
	// Web is the built frontend (web/dist). nil disables static serving.
	Web fs.FS
}

type ctxKey int

const principalKey ctxKey = 1

// Principal returns the authenticated principal or nil.
func Principal(ctx context.Context) *auth.Principal {
	p, _ := ctx.Value(principalKey).(*auth.Principal)
	return p
}

// Handler builds the router.
func (s *Server) Handler() http.Handler {
	h := s.appHandler()
	if s.Preview == nil {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if port, short, ok := s.Preview.Match(r.Host); ok {
			s.Preview.Handler(port, short).ServeHTTP(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func (s *Server) appHandler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(s.logging)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(15 * time.Minute)) // long streams
	r.Use(securityHeaders)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"ok": "true"}) })

	// Browser-facing link endpoints (no JSON)
	r.Get("/login/magic/{token}", s.handleMagicConsume)

	// Agent webhooks (PLAN M7): the secret in the path is the credential.
	r.Post("/hooks/agents/{id}/{secret}", s.handleAgentHook)

	// External API for Claude Code, Cursor, opencode, etc. Bearer / x-api-key.
	ext := &externalapi.Server{GW: s.GW, Log: s.Log, Principal: func(ctx context.Context) *externalapi.Principal {
		p := Principal(ctx)
		if p == nil {
			return nil
		}
		return &externalapi.Principal{UserID: p.UserID, APIKeyID: p.APIKeyID, DefaultSelector: p.DefaultPolicy}
	}}
	r.Group(func(v1 chi.Router) {
		v1.Use(s.authenticate)
		v1.Use(requireAuthV1)
		ext.Routes(v1)
	})

	// ws as an MCP server (PLAN M5): API key only, never a cookie session,
	// so a page in the browser cannot drive it.
	if s.MCP != nil {
		r.Group(func(m chi.Router) {
			m.Use(s.authenticate, requireAPIKeyMCP)
			m.Handle("/mcp", s.MCP.Handler())
		})
	}

	r.Route("/api", func(api chi.Router) {
		api.Use(s.authenticate) // attaches principal when present; does not reject
		// Claude Code probes a gateway with HEAD /api/hello before using it.
		hello := func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"ok": "true"}) }
		api.Head("/hello", hello)
		api.Get("/hello", hello)
		api.Route("/auth", s.authRoutes)

		api.Group(func(pr chi.Router) {
			pr.Use(requireAuth)
			pr.Get("/me", s.handleMe)
			pr.Get("/models", s.handleModels)

			pr.Get("/projects", s.handleListProjects)
			pr.Post("/projects", s.handleCreateProject)
			pr.Get("/projects/{id}", s.handleGetProject)
			pr.Get("/projects/{id}/conversations", s.handleListConversations)
			pr.Post("/projects/{id}/conversations", s.handleCreateConversation)
			pr.Get("/projects/{id}/sandbox", s.handleSandbox)
			pr.Post("/projects/{id}/sandbox/stop", s.handleSandboxStop)
			pr.Get("/projects/{id}/fs/tree", s.handleFSTree)
			pr.Get("/projects/{id}/fs/file", s.handleFSRead)
			pr.Put("/projects/{id}/fs/file", s.handleFSWrite)
			pr.Get("/projects/{id}/pty", s.handlePTY)
			pr.Get("/projects/{id}/preview/{port}", s.handlePreviewLink)

			pr.Get("/conversations/recent", s.handleRecentConversations)
			pr.Get("/conversations/{id}", s.handleGetConversation)
			pr.Patch("/conversations/{id}", s.handleUpdateConversation)
			pr.Delete("/conversations/{id}", s.handleArchiveConversation)
			pr.Post("/conversations/{id}/chat", s.handleChat)
			pr.Get("/conversations/{id}/artifacts", s.handleListArtifacts)
			pr.Get("/conversations/{id}/runs", s.handleListRuns)
			pr.Get("/runs/{id}", s.handleGetRun)
			pr.Post("/approvals/{id}", s.handleDecideApproval)

			pr.Get("/artifacts/{id}", s.handleGetArtifact)
			pr.Get("/artifacts/{id}/versions/{v}", s.handleGetArtifact)

			// Media (PLAN M6): image and video jobs per project, their
			// outputs as attachments.
			pr.Get("/projects/{id}/media", s.handleListMedia)
			pr.Post("/projects/{id}/media", s.handleCreateMedia)
			pr.Get("/media/{id}", s.handleGetMedia)
			pr.Delete("/media/{id}", s.handleDeleteMedia)
			pr.Get("/attachments/{id}", s.handleGetAttachment)

			// Long-lived agents (PLAN M7): definitions, triggers, runs.
			pr.Get("/agents", s.handleListAgents)
			pr.Post("/agents", s.handleCreateAgent)
			pr.Get("/agents/{id}", s.handleGetAgent)
			pr.Put("/agents/{id}", s.handleUpdateAgent)
			pr.Delete("/agents/{id}", s.handleDeleteAgent)
			pr.Post("/agents/{id}/enabled", s.handleSetAgentEnabled)
			pr.Post("/agents/{id}/run", s.handleRunAgent)
			pr.Post("/agents/{id}/triggers", s.handleCreateTrigger)
			pr.Delete("/agents/{id}/triggers/{tid}", s.handleDeleteTrigger)
			pr.Post("/agents/{id}/triggers/{tid}/enabled", s.handleSetTriggerEnabled)
			pr.Post("/runs/{id}/cancel", s.handleCancelRun)
			pr.Post("/runs/{id}/steer", s.handleSteerRun)

			pr.Get("/keys", s.handleListKeys)
			pr.Post("/keys", s.handleCreateKey)
			pr.Delete("/keys/{id}", s.handleRevokeKey)

			// Claude Code job tab: owner-only and tailnet-only (spec §6.5).
			ccAllow := "" // no config (tests): the default list below
			if s.Cfg != nil {
				ccAllow = s.Cfg.CCWebAllow
			}
			if strings.TrimSpace(ccAllow) == "" {
				ccAllow = "100.64.0.0/10,127.0.0.0/8,::1/128"
			}
			pr.Group(func(cc chi.Router) {
				cc.Use(requireOwner, netGate(ccAllow))
				cc.Get("/jobs/cc", s.handleListCCJobs)
				cc.Post("/jobs/cc", s.handleReportCCJob)
				cc.Delete("/jobs/cc/{id}", s.handleKillCCJob)
				cc.Get("/jobs/cc/{id}/term", s.handleCCJobTerm)
			})

			pr.Group(func(ad chi.Router) {
				ad.Use(requireOwner)
				ad.Get("/admin/invites", s.handleListInvites)
				ad.Post("/admin/invites", s.handleCreateInvite)
				ad.Get("/admin/users", s.handleListUsers)
				ad.Get("/admin/usage", s.handleUsage)
				ad.Get("/admin/github", s.handleGitHubStatus)
				ad.Get("/admin/endpoints", s.handleAdminEndpoints)
				ad.Put("/admin/providers", s.handleUpsertProvider)
				ad.Delete("/admin/providers/{id}", s.handleDeleteProvider)
				ad.Get("/admin/rentals", s.handleListRentals)
				ad.Post("/admin/rentals", s.handleStartRental)
				ad.Post("/admin/rentals/stop-all", s.handleStopAllRentals)
				ad.Post("/admin/rentals/{id}/stop", s.handleStopRental)
				ad.Get("/admin/rentals/offers/{provider}", s.handleRentalOffers)
				ad.Put("/admin/endpoints", s.handleUpsertEndpoint)
				ad.Delete("/admin/endpoints/*", s.handleDeleteEndpoint)
				ad.Post("/admin/endpoints/{id}/enabled", s.handleSetEndpointEnabled)
				ad.Get("/admin/providers/{id}/catalog", s.handleProviderCatalog)
				ad.Get("/admin/providers/{id}/routes", s.handleProviderRoutes)
				ad.Get("/admin/policies", s.handleListPolicies)
				ad.Put("/admin/policies/{name}", s.handlePutPolicy)
				ad.Delete("/admin/policies/{name}", s.handleDeletePolicy)
				ad.Get("/admin/budgets", s.handleListBudgets)
				ad.Put("/admin/budgets", s.handlePutBudget)
				ad.Delete("/admin/budgets/{id}", s.handleDeleteBudget)
			})
		})
	})

	if s.Web != nil {
		r.Handle("/*", spaHandler(s.Web))
	}
	return r
}

// ---- middleware ----

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			return
		}
		s.Log.Info("http", "method", r.Method, "path", r.URL.Path, "status", ww.Status(), "ms", time.Since(start).Milliseconds(), "ip", r.RemoteAddr)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := s.Auth.Authenticate(r.Context(), r)
		if err == nil && p != nil {
			r = r.WithContext(context.WithValue(r.Context(), principalKey, p))
		}
		next.ServeHTTP(w, r)
	})
}

// requireAuthV1 is requireAuth for the external API: a missing or bad key
// is answered in the protocol's own error shape (Anthropic for
// /v1/messages*, OpenAI otherwise) so SDK clients report it properly, and
// x-should-retry tells Claude Code not to retry it.
func requireAuthV1(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := Principal(r.Context())
		status, msg := 0, ""
		switch {
		case p == nil:
			status, msg = 401, "invalid or missing API key"
		case !p.HasScope(auth.ScopeChat):
			status, msg = 403, "this API key has no chat scope"
		}
		if status != 0 {
			w.Header().Set("x-should-retry", "false")
			if strings.HasPrefix(r.URL.Path, "/v1/messages") {
				kind := "authentication_error"
				if status == 403 {
					kind = "permission_error"
				}
				writeJSON(w, status, map[string]any{"type": "error", "error": map[string]any{"type": kind, "message": msg}})
			} else {
				code := "invalid_api_key"
				if status == 403 {
					code = "insufficient_scope"
				}
				writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg, "type": "invalid_request_error", "code": code}})
			}
			return
		}
		requireAuth(next).ServeHTTP(w, r)
	})
}

// requireAPIKeyMCP admits only API-key principals carrying the mcp scope
// to /mcp and hands the caller to the MCP server through its context.
func requireAPIKeyMCP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := Principal(r.Context())
		if p == nil || !p.APIKeyID.Valid {
			writeErr(w, 401, "a ws API key is required (Authorization: Bearer ws_...)")
			return
		}
		if !p.HasScope(auth.ScopeMCP) {
			writeErr(w, 403, "this API key has no mcp scope; mint one with MCP access in Settings")
			return
		}
		ctx := mcpserver.WithPrincipal(r.Context(), mcpserver.Principal{UserID: p.UserID, APIKeyID: p.APIKeyID, Owner: p.IsOwner(), Selector: p.DefaultPolicy})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if Principal(r.Context()) == nil {
			writeErr(w, 401, "unauthorized")
			return
		}
		// CSRF: state-changing browser requests must come from our origin.
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if p := Principal(r.Context()); p != nil && !p.APIKeyID.Valid {
				if sf := r.Header.Get("Sec-Fetch-Site"); sf != "" && sf != "same-origin" && sf != "none" {
					writeErr(w, 403, "cross-site request blocked")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func requireOwner(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !Principal(r.Context()).IsOwner() {
			writeErr(w, 403, "owner only")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func decode(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 32<<20)
	dec := json.NewDecoder(r.Body)
	return dec.Decode(v)
}

func uuidParam(r *http.Request, name string) (uuid.UUID, error) {
	return uuid.Parse(chi.URLParam(r, name))
}

func (s *Server) canAccessProject(ctx context.Context, projectID uuid.UUID) bool {
	p := Principal(ctx)
	if p == nil {
		return false
	}
	ok, err := s.DB.UserCanAccessProject(ctx, store.UserCanAccessProjectParams{ID: projectID, UserID: p.UserID})
	return err == nil && ok
}

func (s *Server) loadConversation(w http.ResponseWriter, r *http.Request) (*store.Conversation, bool) {
	id, err := uuidParam(r, "id")
	if err != nil {
		writeErr(w, 400, "bad id")
		return nil, false
	}
	conv, err := s.DB.GetConversation(r.Context(), id)
	if err != nil {
		writeErr(w, 404, "not found")
		return nil, false
	}
	if !s.canAccessProject(r.Context(), conv.ProjectID) {
		writeErr(w, 404, "not found")
		return nil, false
	}
	return &conv, true
}

// spaHandler serves the embedded frontend with index.html fallback.
func spaHandler(root fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p == "" {
			p = "index.html"
		}
		if f, err := root.Open(p); err == nil {
			f.Close()
			if strings.HasPrefix(p, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			fileServer.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		r.URL.Path = "/"
		fileServer.ServeHTTP(w, r)
	})
}
