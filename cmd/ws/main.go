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
	"sync"
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
	"github.com/jking323/ws/internal/sealed"
	"github.com/jking323/ws/internal/secrets"
	"github.com/jking323/ws/internal/store"
	"github.com/jking323/ws/internal/store/blob"
	"github.com/jking323/ws/internal/training"
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
	// Speed measurements outlive the process: load last time's, write each
	// new one through. A write that fails costs nothing but the memory.
	if rows, err := db.ListEndpointThroughput(ctx); err == nil {
		for _, r := range rows {
			router.Throughput().Load(r.EndpointID, gateway.ThroughputStat{TokensPerSec: r.TokensPerSec, TTFTMS: r.TtftMs, Samples: int(r.Samples)}, r.UpdatedAt)
		}
	} else {
		log.Warn("endpoint throughput: load", "err", err)
	}
	router.Throughput().Persist = func(id string, s gateway.ThroughputStat, at time.Time) {
		bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := db.UpsertEndpointThroughput(bg, store.UpsertEndpointThroughputParams{EndpointID: id, TokensPerSec: s.TokensPerSec, TtftMs: s.TTFTMS, Samples: int32(s.Samples), UpdatedAt: at}); err != nil {
			log.Warn("endpoint throughput: persist", "endpoint", id, "err", err)
		}
	}
	gw := gateway.New(reg, router, &store.UsageRecorder{DB: db, Log: log}, log)
	secretBox := secretsBox(cfg, log)
	keyStore := &store.KeyStore{DB: db, Box: secretBox}
	gw.Keys = keyStore
	gw.RegisterAdapter(gateway.ProviderAnthropic, anthropicad.New())
	gw.RegisterAdapter(gateway.ProviderOpenAICompat, openaicompat.New())
	gw.Priority = newPriorityFunc(db)
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
	secrets   *sealed.Box
	keyStore  *store.KeyStore
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
	training  *training.Service
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
	// generate_image and generate_video exist only when an endpoint of
	// that kind is configured on this box (a restart picks up a newly
	// added one); the gallery routes work either way.
	med := &media.Service{DB: db, Blobs: blobs, GW: gw, Engines: media.DefaultEngines(cfg.ComfyWorkflows), Log: log}
	if n := len(med.Endpoints(media.KindImage)); n > 0 {
		tools.Add(media.NewTool(med, db))
		chat.AllowTools(media.ToolName)
		log.Info("media: image endpoints", "count", n)
	}
	if n := len(med.Endpoints(media.KindVideo)); n > 0 {
		tools.Add(media.NewVideoTool(med, db))
		chat.AllowTools(media.ToolNameVideo)
		log.Info("media: video endpoints", "count", n)
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
	// remember and recall exist for agent runs only (a chat turn gets an
	// error result); chat conversations are not offered them.
	tools.Add(agents.NewRememberTool(mem))
	tools.Add(agents.NewRecallTool(mem))
	// Agent presets: starting points for the new-agent form, from disk.
	if presets, warn, err := agents.LoadPresets(cfg.PresetsDir, presetToolKnown(tools)); err != nil {
		log.Warn("agent presets", "dir", cfg.PresetsDir, "err", err)
	} else {
		ags.Presets = presets
		for _, w := range warn {
			log.Warn("agent presets: " + w)
		}
	}
	// Rented GPUs (PLAN M9): templates from disk, RunPod when its key is
	// set. The controller exists in both roles (Admin starts and stops in
	// serve); the worker's minute tick reconciles.
	rent := buildRental(ctx, cfg, db, gw, log)
	// Training flywheel (PLAN M10): datasets and adapters live in both
	// roles; the worker runs the builds, the trainer container and the
	// eval gate. The runner exists only where WS_FINETUNE_IMAGE is set
	// and Docker answers.
	trn := buildTraining(ctx, cfg, db, gw, blobs, rent, log, workers)
	deps := &jobs.Deps{DB: db, Runtime: rt, Compactor: comp, Log: log, Media: med, Agents: ags, Memory: mem, Rental: rent, Training: trn}
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
	trn.EnqueueBuild, trn.EnqueueFinetune, trn.EnqueueEval = jc.EnqueueTrainingBuild, jc.EnqueueFinetune, jc.EnqueueEval
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
				for k, v := range p.Selectors() {
					if _, ok := aliases[k]; !ok {
						aliases[k] = v
					}
				}
			}
			return models, aliases
		}
	}
	ks, _ := gw.Keys.(*store.KeyStore)
	var box *sealed.Box
	if ks != nil {
		box = ks.Box
	}
	return &stack{gw: gw, secrets: box, keyStore: ks, blobs: blobs, artifacts: art, runtime: rt, compactor: comp, jobs: jc, sandbox: sbm, ccJobs: ccJobs, mcp: mcpSrv, media: med, agents: ags, rental: rent, training: trn}, nil
}

// buildTraining wires the training service. The targets a job can name
// are declared from the config in both roles (serve accepts the job,
// the worker runs it): local when a trainer image is configured, remote
// when WS_FINETUNE_URL names a trainer box, rental when
// WS_FINETUNE_TEMPLATE names a trainer-kind rental template whose
// provider is configured. The runners themselves exist on the worker
// only; a declared target the worker cannot build fails the job with
// the reason on its row.
func buildTraining(ctx context.Context, cfg *config.Config, db *store.DB, gw *gateway.Gateway, blobs blob.Store, rent *rental.Controller, log *slog.Logger, worker bool) *training.Service {
	t := &training.Service{
		DB: db, Blobs: blobs, GW: gw, Log: log, JudgeSelector: cfg.TrainingJudge,
		// The base model picker searches the hub through the server; the
		// token (when set) lets it see gated and private models too.
		Hub:            &training.Hub{Token: os.Getenv("HF_TOKEN")},
		Endpoint:       func(id string) (*gateway.Endpoint, bool) { return gw.Registry.Endpoint(id) },
		Reload:         func(ctx context.Context) error { return db.LoadRegistry(ctx, gw.Registry) },
		ReloadPolicies: func(ctx context.Context) error { return db.ReloadPolicies(ctx, gw.Router) },
		// What the other policies prefer for a task class: the fallbacks a
		// training-adapters rule keeps behind the adapter.
		BasePrefer: func(tc string) []string {
			var others []gateway.Policy
			for _, p := range gw.Router.Policies() {
				if p.Name != training.AdapterPolicy {
					others = append(others, p)
				}
			}
			prefs, _ := gateway.PreferredBy(others, gateway.RouteInput{Selector: "auto", TaskClass: gateway.TaskClass(tc)}, nil)
			return prefs
		},
	}
	tpl, rentable := finetuneTemplate(cfg, rent, log)
	remote := &training.RemoteRunner{URL: cfg.FinetuneURL, Key: func() string { return os.Getenv("WS_FINETUNE_KEY") }, Log: log}
	targets := &training.Targets{RentalLabel: tpl.Name, RemoteLabel: remote.Host()}
	if cfg.FinetuneImage != "" {
		t.Available = append(t.Available, training.Target{ID: training.TargetLocal, Label: "this worker's GPU"})
	}
	if cfg.FinetuneURL != "" {
		t.Available = append(t.Available, training.Target{ID: training.TargetRemote, Label: training.RemoteLabel(remote.Host())})
	}
	if rentable {
		t.Available = append(t.Available, training.Target{ID: training.TargetRental, Label: "rented GPU (" + tpl.Name + ")"})
	}
	if !worker {
		return t
	}
	if cfg.FinetuneImage != "" {
		var env []string
		if tok := os.Getenv("HF_TOKEN"); tok != "" {
			env = append(env, "HF_TOKEN="+tok)
		}
		r, err := training.NewDockerRunner(ctx, cfg.FinetuneImage, cfg.FinetuneGPUs, cfg.TrainingDir, env)
		if err != nil {
			log.Warn("training: no local fine-tune runner; jobs targeting it will fail", "err", err)
		} else {
			targets.Local = r
			log.Info("training: local fine-tune runner", "image", cfg.FinetuneImage, "gpus", cfg.FinetuneGPUs)
		}
	}
	if cfg.FinetuneURL != "" {
		targets.Remote = remote
		log.Info("training: trainer box", "url", cfg.FinetuneURL, "key", cfg.FinetuneKey != "")
	}
	if rentable {
		targets.Rental = &training.RentalRunner{Rental: rent, Template: tpl.Name, Key: func() string { return os.Getenv("WS_RENTAL_API_KEY") }, Log: log}
		log.Info("training: rented fine-tune runner", "template", tpl.Name, "provider", tpl.Provider, "gpu", tpl.GPU)
	}
	if targets.Local != nil || targets.Remote != nil || targets.Rental != nil {
		t.Runner = targets
	}
	return t
}

// finetuneTemplate resolves WS_FINETUNE_TEMPLATE to a usable trainer
// template: it must exist, be kind trainer, and its provider must be
// configured. Problems are logged once, in every role.
func finetuneTemplate(cfg *config.Config, rent *rental.Controller, log *slog.Logger) (rental.Template, bool) {
	if cfg.FinetuneTemplate == "" || rent == nil {
		return rental.Template{}, false
	}
	tpl, ok := rent.Template(cfg.FinetuneTemplate)
	switch {
	case !ok:
		log.Warn("training: WS_FINETUNE_TEMPLATE names no template", "template", cfg.FinetuneTemplate)
	case !tpl.IsTrainer():
		log.Warn("training: WS_FINETUNE_TEMPLATE is not a trainer template (kind: trainer)", "template", cfg.FinetuneTemplate)
	case rent.Providers[tpl.Provider] == nil:
		log.Warn("training: the fine-tune template's provider is not configured", "template", cfg.FinetuneTemplate, "provider", tpl.Provider)
	default:
		return tpl, true
	}
	return rental.Template{}, false
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
		Jail: cfg.SandboxJail, MemoryMB: cfg.SandboxMemoryMB, CPUs: cfg.SandboxCPUs, IdleStop: idle, Remove: remove,
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
		Cfg: cfg, DB: db, Auth: authSvc, GW: gw, Secrets: st.secrets, KeyStore: st.keyStore,
		Chat:                &chat.Service{DB: db, GW: gw, Runtime: st.runtime, Log: log, Enqueue: st.jobs.EnqueueRun},
		Artifacts:           art,
		Jobs:                st.jobs,
		Worker:              worker,
		Preview:             preview,
		GitHub:              buildGitHub(cfg, db, log),
		CCJobs:              st.ccJobs,
		MCP:                 st.mcp,
		Media:               st.media,
		Blobs:               st.blobs,
		Agents:              st.agents,
		GitHubWebhookSecret: cfg.GitHubWebhookSecret,
		Rental:              st.rental,
		Training:            st.training,
		Log:                 log,
		Web:                 web.Dist(),
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

// presetToolKnown says whether a preset's tool name is one the deployment
// can offer. The serve role registers no sandbox or git tools (the worker
// does), so those names count as known here or every boot warns about
// presets that are fine.
func presetToolKnown(tools *agent.Registry) func(string) bool {
	return func(name string) bool {
		if _, ok := tools.Get(name); ok {
			return true
		}
		for _, n := range sandbox.ToolNames {
			if n == name {
				return true
			}
		}
		for _, n := range sandbox.GitToolNames {
			if n == name {
				return true
			}
		}
		return false
	}
}

// secretsBox seals people's own provider keys. Without WS_SECRETS_KEY it is
// nil and saving a key is refused, so a key is never stored unsealed; a key
// that cannot be read is logged and treated the same way.
func secretsBox(cfg *config.Config, log *slog.Logger) *sealed.Box {
	if cfg.SecretsKey == "" {
		return nil
	}
	b, err := sealed.New(cfg.SecretsKey)
	if err != nil {
		log.Warn("WS_SECRETS_KEY is set but unusable: people cannot save their own provider keys", "err", err)
		return nil
	}
	return b
}

// newPriorityFunc ranks requests for a full local endpoint by who sent them:
// the owner's go ahead of everyone else's. Roles are cached for a minute so
// the lookup is not a query per request; an unknown user ranks as a member.
func newPriorityFunc(db *store.DB) func(context.Context, gateway.Metadata) int {
	type entry struct {
		prio int
		at   time.Time
	}
	var mu sync.Mutex
	cache := map[string]entry{}
	return func(ctx context.Context, md gateway.Metadata) int {
		if md.UserID == "" {
			return gateway.PriorityDefault
		}
		mu.Lock()
		e, ok := cache[md.UserID]
		mu.Unlock()
		if ok && time.Since(e.at) < time.Minute {
			return e.prio
		}
		prio := gateway.PriorityMember
		if id, err := uuid.Parse(md.UserID); err == nil {
			if u, err := db.GetUserByID(ctx, id); err == nil && u.Role == "owner" {
				prio = gateway.PriorityOwner
			}
		}
		mu.Lock()
		cache[md.UserID] = entry{prio, time.Now()}
		mu.Unlock()
		return prio
	}
}
