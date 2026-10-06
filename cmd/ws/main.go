// Command ws is the single binary: `ws serve`, `ws worker`, `ws migrate`.
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"

	"github.com/jking323/ws/internal/agent"
	"github.com/jking323/ws/internal/agents"
	"github.com/jking323/ws/internal/artifacts"
	"github.com/jking323/ws/internal/auth"
	"github.com/jking323/ws/internal/ccjobs"
	"github.com/jking323/ws/internal/ccrouter"
	"github.com/jking323/ws/internal/ccweb"
	"github.com/jking323/ws/internal/chat"
	"github.com/jking323/ws/internal/compaction"
	"github.com/jking323/ws/internal/config"
	"github.com/jking323/ws/internal/fleet/rental"
	"github.com/jking323/ws/internal/gateway"
	anthropicad "github.com/jking323/ws/internal/gateway/adapter/anthropic"
	"github.com/jking323/ws/internal/gateway/adapter/openaicompat"
	"github.com/jking323/ws/internal/github"
	"github.com/jking323/ws/internal/httpx"
	"github.com/jking323/ws/internal/jobs"
	"github.com/jking323/ws/internal/mcpclient"
	"github.com/jking323/ws/internal/mcpserver"
	"github.com/jking323/ws/internal/media"
	"github.com/jking323/ws/internal/sandbox"
	"github.com/jking323/ws/internal/secrets"
	"github.com/jking323/ws/internal/store"
	"github.com/jking323/ws/internal/store/blob"
	"github.com/jking323/ws/web"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel()}))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// .env first (bootstrap: database URL, Infisical machine identity), then
	// Infisical fills in everything else, then config parses the environment.
	_ = godotenv.Load()
	secrets.Load(ctx, log)
	cfg, err := config.Load()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}

	switch os.Args[1] {
	case "serve":
		err = serve(ctx, cfg, log)
	case "worker":
		err = worker(ctx, cfg, log)
	case "migrate":
		dir := "up"
		if len(os.Args) > 2 {
			dir = os.Args[2]
		}
		err = migrate(ctx, cfg, dir)
	case "health":
		// Container healthcheck: distroless has no shell or curl.
		os.Exit(healthcheck(cfg))
	case "version":
		fmt.Println("ws dev")
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		log.Error("exit", "err", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: ws serve | worker | migrate [up|down|status] | version")
}

func logLevel() slog.Level {
	switch os.Getenv("WS_LOG") {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	}
	return slog.LevelInfo
}

func migrate(ctx context.Context, cfg *config.Config, dir string) error {
	db, err := store.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	return db.Migrate(ctx, dir)
}

// buildGateway wires registry, router, adapters, and recorder. Shared by
// serve and worker.
func buildGateway(ctx context.Context, cfg *config.Config, db *store.DB, log *slog.Logger) (*gateway.Gateway, error) {
	if sf, err := gateway.LoadSeed(cfg.EndpointsFile); err == nil {
		if err := db.SeedRegistry(ctx, sf); err != nil {
			return nil, fmt.Errorf("seed endpoints: %w", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	} else {
		log.Warn("no endpoints seed file", "path", cfg.EndpointsFile)
	}
	if err := db.SeedPolicies(ctx, cfg.PoliciesDir); err != nil {
		log.Warn("seed policies", "err", err)
	}

	reg := gateway.NewRegistry()
	if err := db.LoadRegistry(ctx, reg); err != nil {
		return nil, fmt.Errorf("load registry: %w", err)
	}
	policies, err := db.LoadPolicies(ctx)
	if err != nil {
		return nil, fmt.Errorf("load policies: %w", err)
	}
	router := gateway.NewRouter(reg, policies)
	gw := gateway.New(reg, router, &store.UsageRecorder{DB: db, Log: log}, log)
	gw.RegisterAdapter(gateway.ProviderAnthropic, anthropicad.New())
	gw.RegisterAdapter(gateway.ProviderOpenAICompat, openaicompat.New())
	gw.Use(gateway.BudgetMiddleware(&store.BudgetStore{DB: db}, log))

	// serve and worker are separate processes; each polls Postgres for the
	// health the worker writes and for policy edits made in the admin UI.
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				// Full reload: picks up endpoints added in the admin UI by the
				// other process as well as the worker's health writes.
				if err := db.LoadRegistry(ctx, reg); err != nil {
					log.Warn("reload registry", "err", err)
				}
				if err := db.ReloadPolicies(ctx, router); err != nil {
					log.Warn("reload policies", "err", err)
				}
			}
		}
	}()

	for _, p := range reg.Providers() {
		n := 0
		for _, e := range reg.Endpoints() {
			if e.ProviderID == p.ID && e.Enabled {
				n++
			}
		}
		log.Info("provider", "id", p.ID, "kind", p.Kind, "base_url", p.BaseURL, "has_key", p.APIKey != "", "endpoints", n)
	}
	return gw, nil
}

// stack is everything both roles share above the gateway.
type stack struct {
	gw        *gateway.Gateway
	blobs     blob.Store
	artifacts *artifacts.Service
	runtime   *agent.Runtime
	compactor *compaction.Compactor
	jobs      *jobs.Client
	sandbox   *sandbox.Manager // worker only
	ccJobs    *ccweb.Registry
	mcp       *mcpserver.Server
	media     *media.Service
	agents    *agents.Service
	rental    *rental.Controller
}

// buildStack wires blobs, artifacts, tools, the agent runtime, compaction
// and River. workers=true runs the queues (worker process).
func buildStack(ctx context.Context, cfg *config.Config, db *store.DB, log *slog.Logger, workers bool) (*stack, error) {
	gw, err := buildGateway(ctx, cfg, db, log)
	if err != nil {
		return nil, err
	}
	blobs, err := blob.NewFS(cfg.BlobDir)
	if err != nil {
		return nil, err
	}
	art := &artifacts.Service{DB: db, Secret: []byte(cfg.SessionSecret), BaseURL: cfg.ArtifactURL}
	tools := agent.NewRegistry(agent.ReadBlobTool{}, &agent.WebFetchTool{}, agent.AskUserTool{})
	for _, t := range agent.ArtifactTools(art) {
		tools.Add(t)
	}
	// Sandbox tools live only in the worker, which has the Docker socket.
	// Code-mode runs are enqueued by serve and driven here.
	var sbm *sandbox.Manager
	// stdioRunner starts mcp.yaml command servers inside sandboxes; only
	// the worker has one, so serve skips those servers.
	var stdioRunner mcpclient.Runner
	if workers && cfg.SandboxEnabled {
		sbm, err = buildSandbox(ctx, cfg, db, log)
		if err != nil {
			log.Warn("sandbox disabled", "err", err)
		} else {
			resolve := func(ctx context.Context, tc agent.ToolCtx) (*sandbox.Sandbox, error) {
				conv, err := db.GetConversation(ctx, tc.ConversationID)
				if err != nil {
					return nil, err
				}
				proj, err := db.GetProject(ctx, conv.ProjectID)
				if err != nil {
					return nil, err
				}
				if proj.Kind != "code" {
					return nil, errors.New("sandbox tools are only available in code projects")
				}
				return sbm.Ensure(ctx, proj.ID, tc.UserID)
			}
			for _, t := range sandbox.Tools(sbm, resolve) {
				tools.Add(t)
			}
			for _, t := range sandbox.GitTools(sbm, resolve, buildGitHub(cfg, db, log)) {
				tools.Add(t)
			}
			stdioRunner = &sandbox.StdioRunner{M: sbm, Resolve: resolve}
		}
	}
	// MCP servers (config/mcp.yaml): tools become mcp__<server>__<tool>.
	// URL servers connect from both processes; command servers run inside
	// the caller's sandbox and exist in the worker only.
	if f, err := mcpclient.LoadFile(cfg.MCPFile); err != nil {
		log.Warn("mcp config", "err", err)
	} else if len(f.Servers) > 0 {
		mcpTools, _ := mcpclient.ConnectAll(ctx, f, stdioRunner, log, 45*time.Second)
		names := make([]string, 0, len(mcpTools))
		for _, t := range mcpTools {
			tools.Add(t)
			names = append(names, t.Def().Name)
		}
		chat.WithMCP(names) // chat-mode conversations may use them too
	}
	// Task router (docs/CLAUDE_CODE_JOBS.md §6.4): spawn_job decides a lane
	// and dispatches. Dry-run only until WS_CC_DISPATCH=true; the runtime
	// and job client it needs are filled in below once they exist.
	spawn := &ccrouter.Tool{DB: db, Dispatch: cfg.CCDispatch, Aliases: ccrouter.ParseAliases(cfg.CCAliases), Log: log}
	if cfg.CCClassify {
		// Task class classify resolves to a local or cheap endpoint by
		// policy; the gateway holds no subscription credential.
		spawn.Classifier = &ccrouter.Classifier{GW: gw}
	}
	targetsPath := cfg.CCTargets
	if targetsPath == "" {
		targetsPath = ccjobs.DefaultConfigPath()
	}
	spawn.Targets = func() (*ccjobs.Config, error) {
		c, err := ccjobs.LoadConfig(targetsPath)
		if err != nil {
			return nil, err
		}
		if cfg.CCSSHConfig != "" {
			c.SSHConfig = cfg.CCSSHConfig
		}
		return c, nil
	}
	// The job tab's registry (spec §6.5) shares the targets loader: the
	// router records its launches there and serve lists and views them.
	ccJobs := &ccweb.Registry{DB: db, Targets: spawn.Targets, Cap: cfg.CCWeeklyCap, SoftPct: cfg.CCWeeklySoft, Log: log}
	spawn.Jobs = ccJobs
	spawn.Budget = ccJobs.Budget // the weekly soft cap (spec M6)
	// Single user: the subscription lane is the owner's alone (spec §2).
	spawn.IsOwner = func(ctx context.Context, userID uuid.UUID) (bool, error) {
		u, err := db.GetUserByID(ctx, userID)
		if err != nil {
			return false, err
		}
		return u.Role == "owner" && u.DisabledAt == nil, nil
	}
	tools.Add(spawn)
	// Chat conversations see the tool only once dispatch is on; a dry-run
	// host keeps the decision log for `wsj route` and deliberate tests.
	if cfg.CCDispatch {
		chat.AllowTools(ccrouter.ToolName)
	}

	// Media gateway (PLAN M6): image and video jobs on media endpoints.
	// generate_image exists only when an image endpoint is configured on
	// this box (a restart picks up a newly added one); the gallery routes
	// work either way.
	med := &media.Service{DB: db, Blobs: blobs, GW: gw, Engines: media.DefaultEngines(), Log: log}
	if n := len(med.Endpoints(media.KindImage)); n > 0 {
		tools.Add(media.NewTool(med, db))
		chat.AllowTools(media.ToolName)
		log.Info("media: image endpoints", "count", n)
	}

	rt := agent.New(db, gw, tools, blobs, log)
	spawn.Runtime = rt
	comp := &compaction.Compactor{DB: db, GW: gw, Log: log, BudgetTokens: cfg.CompactionBudgetTokens}
	gw.Use(comp.Middleware())

	if err := jobs.Migrate(ctx, db.Pool); err != nil {
		return nil, fmt.Errorf("river migrate: %w", err)
	}
	// Long-lived agents (PLAN M7): runs go to the worker like code turns;
	// the worker's minute tick fires due cron triggers.
	mem := &agents.Memory{DB: db, GW: gw, Log: log}
	ags := &agents.Service{DB: db, Runtime: rt, Memory: mem, Log: log}
	// Rented GPUs (PLAN M9): templates from disk, RunPod when its key is
	// set. The controller exists in both roles (Admin starts and stops in
	// serve); the worker's minute tick reconciles.
	rent := buildRental(ctx, cfg, db, gw, log)
	deps := &jobs.Deps{DB: db, Runtime: rt, Compactor: comp, Log: log, Media: med, Agents: ags, Memory: mem, Rental: rent}
	if sbm != nil {
		deps.Sandbox = sbm
	}
	jc, err := jobs.New(ctx, db.Pool, deps, workers)
	if err != nil {
		return nil, fmt.Errorf("river: %w", err)
	}
	comp.Enqueue = func(ctx context.Context, convID uuid.UUID) {
		if err := jc.EnqueueCompact(ctx, convID); err != nil {
			log.Warn("enqueue compaction", "err", err)
		}
	}
	spawn.Enqueue = jc.EnqueueRun
	med.Enqueue = jc.EnqueueMedia
	ags.Enqueue = jc.EnqueueRun
	// ws as an MCP server (PLAN M5): the same gateway, runtime, worker
	// queue and task router, scoped to the API key's user per request.
	var mcpSrv *mcpserver.Server
	if cfg.MCPServer {
		mcpSrv = &mcpserver.Server{DB: db, GW: gw, Runtime: rt, Enqueue: jc.EnqueueRun, Spawn: spawn, Version: "dev", Log: log}
		mcpSrv.Models = func() ([]mcpserver.Model, map[string][]string) {
			var models []mcpserver.Model
			for _, e := range gw.Registry.Endpoints() {
				if !e.Enabled || e.Capabilities.Embeddings || e.Capabilities.IsMedia() {
					continue
				}
				models = append(models, mcpserver.Model{ID: e.ID, DisplayName: e.DisplayName, Provider: e.ProviderID, Local: e.Local, Health: e.Health.Status})
			}
			aliases := map[string][]string{}
			for _, p := range gw.Router.Policies() {
				for k, v := range p.Aliases {
					if _, ok := aliases[k]; !ok {
						aliases[k] = v
					}
				}
			}
			return models, aliases
		}
	}
	return &stack{gw: gw, blobs: blobs, artifacts: art, runtime: rt, compactor: comp, jobs: jc, sandbox: sbm, ccJobs: ccJobs, mcp: mcpSrv, media: med, agents: ags, rental: rent}, nil
}

// buildRental wires the rental controller: templates, providers whose
// keys are set, the registry reload and the ledger recorder.
func buildRental(ctx context.Context, cfg *config.Config, db *store.DB, gw *gateway.Gateway, log *slog.Logger) *rental.Controller {
	tpls, err := rental.LoadTemplates(cfg.RentalTemplates)
	if err != nil {
		log.Warn("rental templates", "dir", cfg.RentalTemplates, "err", err)
	}
	provs := map[string]rental.Provider{}
	if cfg.RunPodAPIKey != "" {
		provs["runpod"] = &rental.RunPod{APIKey: cfg.RunPodAPIKey}
	}
	c := &rental.Controller{
		DB: db, Providers: provs, Templates: tpls,
		Recorder:      &store.UsageRecorder{DB: db, Log: log},
		Reload:        func(ctx context.Context) error { return db.LoadRegistry(ctx, gw.Registry) },
		DailyCapHours: cfg.RentalDailyCapHours, Disabled: cfg.RentalDisabled, Log: log,
	}
	log.Info("rental", "templates", len(tpls), "providers", len(provs), "daily_cap_hours", cfg.RentalDailyCapHours, "disabled", cfg.RentalDisabled)
	return c
}

// buildSandbox connects to Docker, starts the egress proxy the sandboxes
// use, and returns the manager. Worker only.
func buildSandbox(ctx context.Context, cfg *config.Config, db *store.DB, log *slog.Logger) (*sandbox.Manager, error) {
	idle, _ := time.ParseDuration(cfg.SandboxIdleStop)
	remove, _ := time.ParseDuration(cfg.SandboxRemoveAfter)
	// "none" disables (env parsing treats an empty value as unset).
	network, proxyURL := cfg.SandboxNetwork, cfg.SandboxProxyURL
	if network == "none" {
		network = ""
	}
	if proxyURL == "none" {
		proxyURL = ""
	}
	m, err := sandbox.New(ctx, db, log, sandbox.Config{
		Image: cfg.SandboxImage, Network: network, ProxyURL: proxyURL, Runtime: cfg.SandboxRuntime,
		MemoryMB: cfg.SandboxMemoryMB, CPUs: cfg.SandboxCPUs, IdleStop: idle, Remove: remove,
	})
	if err != nil {
		return nil, err
	}
	gh := buildGitHub(cfg, db, log)
	allow := append([]string{}, sandbox.DefaultAllow...)
	allow = append(allow, sandbox.ParseAllow(cfg.EgressAllow)...)
	allow = append(allow, sandbox.ParseAllow(cfg.PublicURL)...) // M5: Claude Code in the sandbox talks to ws
	proxy := &sandbox.Proxy{
		Allow:   allow,
		Upgrade: map[string]bool{"github.com": true},
		Resolve: m.ResolveIP,
		Credential: func(ctx context.Context, sb *sandbox.Sandbox, host string) string {
			if host == "github.com" && sb != nil {
				return gh.Credential(ctx, sb.ProjectID)
			}
			return ""
		},
		Log: log,
	}
	srv := proxy.Server(cfg.EgressListen)
	go func() {
		log.Info("sandbox egress proxy", "listen", cfg.EgressListen, "allow", len(allow), "github_app", gh.Configured(), "github_token_fallback", cfg.GitHubToken != "")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("egress proxy", "err", err)
		}
	}()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	return m, nil
}

// buildGitHub returns the GitHub host (App when configured, else the
// GITHUB_TOKEN fallback). Never nil; Configured() says which.
func buildGitHub(cfg *config.Config, db *store.DB, log *slog.Logger) *github.Host {
	h := &github.Host{DB: db, FallbackToken: cfg.GitHubToken, Log: log}
	key, err := github.LoadKey(cfg.GitHubAppPrivateKey, cfg.GitHubAppKeyFile)
	if err != nil {
		log.Warn("github app", "err", err)
		return h
	}
	app, err := github.New(github.Config{AppID: cfg.GitHubAppID, PrivateKey: key, Slug: cfg.GitHubAppSlug})
	if err != nil {
		log.Warn("github app", "err", err)
		return h
	}
	h.App = app
	return h
}

func serve(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	db, err := store.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.Migrate(ctx, "up"); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	var mailer auth.Mailer
	if cfg.ResendAPIKey != "" {
		mailer = auth.NewResendMailer(cfg.ResendAPIKey, cfg.EmailFrom)
	} else {
		log.Warn("RESEND_API_KEY not set; magic links and invites are logged, not emailed")
		mailer = auth.LogMailer{Log: log}
	}
	authSvc, err := auth.New(db, mailer, auth.Config{
		RPID: cfg.RPID, RPDisplayName: cfg.RPDisplayName, RPOrigins: cfg.RPOrigins,
		PublicURL: cfg.PublicURL, SecureCookies: !cfg.IsDev(),
	})
	if err != nil {
		return err
	}
	if link, err := authSvc.BootstrapOwner(ctx, cfg.OwnerEmail); err != nil {
		log.Warn("bootstrap owner", "err", err)
	} else if link != "" {
		log.Info("OWNER INVITE created (open this link to set up the first account)", "link", link)
	}

	st, err := buildStack(ctx, cfg, db, log, false)
	if err != nil {
		return err
	}
	gw, art := st.gw, st.artifacts
	var worker *sandbox.WorkerClient
	var preview *httpx.PreviewProxy
	if cfg.WorkerURL != "" && cfg.WorkerURL != "none" {
		worker = sandbox.NewWorkerClient(cfg.WorkerURL, sandbox.InternalToken(cfg.SessionSecret))
		pattern := cfg.PreviewHost
		if pattern == "" && cfg.PreviewDomain != "" {
			pattern = "{port}-{id}." + cfg.PreviewDomain
		}
		scheme := "http"
		if strings.HasPrefix(cfg.PublicURL, "https://") {
			scheme = "https"
		}
		preview = httpx.NewPreviewProxy(pattern, scheme, []byte(cfg.SessionSecret), worker, db, log)
		if preview != nil {
			log.Info("sandbox previews", "host_pattern", pattern)
		}
	}
	srv := &httpx.Server{
		Cfg: cfg, DB: db, Auth: authSvc, GW: gw,
		Chat:      &chat.Service{DB: db, GW: gw, Runtime: st.runtime, Log: log, Enqueue: st.jobs.EnqueueRun},
		Artifacts: art,
		Jobs:      st.jobs,
		Worker:    worker,
		Preview:   preview,
		GitHub:    buildGitHub(cfg, db, log),
		CCJobs:    st.ccJobs,
		MCP:       st.mcp,
		Media:     st.media,
		Blobs:     st.blobs,
		Agents:    st.agents,
		Rental:    st.rental,
		Log:       log,
		Web:       web.Dist(),
	}
	h := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	// Artifacts are served from a second listener so they live on their own
	// origin (art.<domain> in production). Never serve them from the app origin.
	ah := &http.Server{
		Addr:              cfg.ArtifactListen,
		Handler:           art.OriginHandler(cfg.PublicURL),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = h.Shutdown(sctx)
		_ = ah.Shutdown(sctx)
	}()
	errc := make(chan error, 2)
	go func() {
		log.Info("ws artifacts", "listen", cfg.ArtifactListen, "origin", cfg.ArtifactURL)
		if err := ah.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- fmt.Errorf("artifact origin: %w", err)
		}
	}()
	go func() {
		log.Info("ws serve", "listen", cfg.Listen, "public_url", cfg.PublicURL, "env", cfg.Env)
		if err := h.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		return nil
	}
}

func worker(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	db, err := store.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.Migrate(ctx, "up"); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	st, err := buildStack(ctx, cfg, db, log, true)
	if err != nil {
		return err
	}
	log.Info("ws worker", "endpoints", len(st.gw.Registry.Endpoints()), "queues", "agents,housekeeping,media")
	go runHealthChecks(ctx, db, st.gw, log)
	if st.sandbox != nil && cfg.InternalListen != "" {
		api := &sandbox.InternalAPI{M: st.sandbox, Token: sandbox.InternalToken(cfg.SessionSecret), Log: log}
		ih := &http.Server{Addr: cfg.InternalListen, Handler: api.Handler(), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			log.Info("worker internal api", "listen", cfg.InternalListen)
			if err := ih.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("internal api", "err", err)
			}
		}()
		defer func() {
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = ih.Shutdown(sctx)
		}()
	}
	<-ctx.Done()
	sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return st.jobs.Stop(sctx)
}
