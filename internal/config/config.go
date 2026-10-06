// Package config loads runtime configuration from the environment (and a
// .env file in dev). Secrets stay in env; the database only stores env var
// names.
package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"
)

// Config is the process configuration shared by serve and worker.
type Config struct {
	Env            string `env:"WS_ENV" envDefault:"dev"`
	Listen         string `env:"WS_LISTEN" envDefault:":8080"`
	ArtifactListen string `env:"WS_ARTIFACT_LISTEN" envDefault:":8081"`
	PublicURL      string `env:"WS_PUBLIC_URL" envDefault:"http://localhost:8080"`
	ArtifactURL    string `env:"WS_ARTIFACT_URL" envDefault:"http://localhost:8081"`
	PreviewDomain  string `env:"WS_PREVIEW_DOMAIN" envDefault:"preview.localhost"`
	SessionSecret  string `env:"WS_SESSION_SECRET,required"`

	DatabaseURL string `env:"WS_DATABASE_URL,required"`
	BlobDir     string `env:"WS_BLOB_DIR" envDefault:"./data/blobs"`

	ResendAPIKey string `env:"RESEND_API_KEY"`
	EmailFrom    string `env:"WS_EMAIL_FROM" envDefault:"ws@localhost"`

	OwnerEmail string `env:"WS_OWNER_EMAIL"`

	// CompactionBudgetTokens is the soft ceiling on tokens sent per request
	// before older turns are summarized away. Keep it under the smallest
	// context window you route to.
	CompactionBudgetTokens int `env:"WS_COMPACTION_BUDGET_TOKENS" envDefault:"60000"`

	// Coding sandboxes (worker only; needs the Docker socket).
	SandboxEnabled     bool    `env:"WS_SANDBOX_ENABLED" envDefault:"true"`
	SandboxImage       string  `env:"WS_SANDBOX_IMAGE" envDefault:"ghcr.io/king-technical-consulting/ws-sandbox:latest"`
	SandboxNetwork     string  `env:"WS_SANDBOX_NETWORK" envDefault:"ws_sandbox-net"`
	SandboxProxyURL    string  `env:"WS_SANDBOX_PROXY_URL" envDefault:"http://worker:3128"`
	SandboxRuntime     string  `env:"WS_SANDBOX_RUNTIME" envDefault:"auto"` // auto | runc | runsc
	SandboxMemoryMB    int64   `env:"WS_SANDBOX_MEMORY_MB" envDefault:"4096"`
	SandboxCPUs        float64 `env:"WS_SANDBOX_CPUS" envDefault:"2"`
	SandboxIdleStop    string  `env:"WS_SANDBOX_IDLE_STOP" envDefault:"15m"`
	SandboxRemoveAfter string  `env:"WS_SANDBOX_REMOVE_AFTER" envDefault:"168h"`
	// Worker internal API (sandbox files, PTY) that serve relays to.
	WorkerURL      string `env:"WS_WORKER_URL" envDefault:"http://worker:8082"`
	InternalListen string `env:"WS_INTERNAL_LISTEN" envDefault:":8082"`
	// PreviewHost is the hostname pattern for sandbox dev-server previews,
	// with {port} and {id} placeholders. Empty derives "{port}-{id}.<WS_PREVIEW_DOMAIN>".
	// Behind a single-level wildcard cert use e.g. "ws-p-{port}-{id}.home.arpa".
	PreviewHost string `env:"WS_PREVIEW_HOST"`
	// Egress proxy the sandboxes use; extra allowlisted hosts are comma-separated.
	EgressListen string `env:"WS_EGRESS_LISTEN" envDefault:":3128"`
	EgressAllow  string `env:"WS_EGRESS_ALLOW"`
	// GitHub App: installation tokens for sandbox pushes and PR creation.
	// GitHubToken is the fallback when no App is configured. Neither ever
	// reaches a sandbox; the egress proxy injects them.
	GitHubAppID         int64  `env:"GITHUB_APP_ID"`
	GitHubAppPrivateKey string `env:"GITHUB_APP_PRIVATE_KEY"`      // PEM (\n or base64 ok)
	GitHubAppKeyFile    string `env:"GITHUB_APP_PRIVATE_KEY_FILE"` // or a path
	GitHubAppSlug       string `env:"GITHUB_APP_SLUG"`
	GitHubToken         string `env:"GITHUB_TOKEN"`

	// EndpointsFile seeds providers/endpoints on boot (idempotent upsert).
	EndpointsFile string `env:"WS_ENDPOINTS_FILE" envDefault:"config/endpoints.yaml"`
	PoliciesDir   string `env:"WS_POLICIES_DIR" envDefault:"config/policies"`
	// MCPFile declares MCP servers whose tools the runtime exposes.
	MCPFile string `env:"WS_MCP_FILE" envDefault:"config/mcp.yaml"`
	// MCPServer serves ws itself as an MCP server at /mcp (Streamable
	// HTTP, ws API key as bearer) so Claude Code and other clients can use
	// ws's projects, conversations, models, runs and the task router as
	// tools. false removes the route.
	MCPServer bool `env:"WS_MCP_SERVER" envDefault:"true"`

	// Claude Code task router (docs/CLAUDE_CODE_JOBS.md §6.4): the
	// spawn_job tool. CCTargets is the launcher targets file as seen from
	// this host (empty: the wsj default, WSJ_TARGETS or ~/.config/wsj/
	// targets.toml). CCDispatch gates real launches; while false every
	// spawn_job call is a dry run that only logs its decision. CCAliases
	// maps the non-subscription lanes to gateway selectors.
	CCTargets  string `env:"WS_CC_TARGETS"`
	CCDispatch bool   `env:"WS_CC_DISPATCH" envDefault:"false"`
	// CCSSHConfig is an ssh_config passed as `ssh -F` for every ssh target
	// in the targets file (key, user, host name, known_hosts per target).
	// In the container: /secrets/ssh_config next to the mounted key.
	// Overrides the file's ssh_config key.
	CCSSHConfig string `env:"WS_CC_SSH_CONFIG"`
	CCAliases  string `env:"WS_CC_ALIASES" envDefault:"api=best,openrouter=cheap,local=local"`
	// CCClassify lets the router ask a small model (task class classify)
	// about prompts the rules find ambiguous; off means rules only.
	CCClassify bool `env:"WS_CC_CLASSIFY" envDefault:"true"`
	// CCWebAllow lists the client networks (CIDRs) that may reach the
	// Claude Code job tab's routes (/api/jobs/cc, the job list, handle
	// reports and the read-only terminal): the tailnet's CGNAT range and
	// loopback by default, so the routes are never served to the public
	// origin (spec §6.5). "*" disables the check. The client address is
	// what the reverse proxy forwards (X-Forwarded-For / X-Real-IP).
	CCWebAllow string `env:"WS_CC_WEB_ALLOW" envDefault:"100.64.0.0/10,127.0.0.0/8,::1/128"`
	// CCWeeklyCap is the soft cap on subscription launches per week
	// (Monday 00:00 UTC), counted locally in cc_launch_counter since
	// Claude's usage is never read. 0 counts without ever downgrading.
	// From CCWeeklySoft percent of the cap, spawn_job sends subscription
	// work that is not clearly repo-bound (no cwd, fewer than two files)
	// to the api lane; at the cap everything goes there unless the lane
	// was asked for. wsj launches are counted but never refused.
	CCWeeklyCap  int `env:"WS_CC_WEEKLY_CAP" envDefault:"0"`
	CCWeeklySoft int `env:"WS_CC_WEEKLY_SOFT" envDefault:"75"`

	// Rented GPUs (PLAN M9, internal/fleet/rental). Templates come from
	// RentalTemplates; RUNPOD_API_KEY enables the RunPod provider;
	// WS_RENTAL_API_KEY is the key every rented server is started with and
	// the provider rows reference by name (set it to a long random string);
	// RentalDailyCapHours refuses starts past that many rented hours in a
	// UTC day (0: no cap); RentalDisabled is the kill switch: no starts and
	// every open machine is stopped at the next reconcile.
	RentalTemplates     string  `env:"WS_RENTAL_TEMPLATES" envDefault:"infra/rental/templates"`
	RunPodAPIKey        string  `env:"RUNPOD_API_KEY"`
	RentalDailyCapHours float64 `env:"WS_RENTAL_DAILY_CAP_HOURS" envDefault:"8"`
	RentalDisabled      bool    `env:"WS_RENTAL_DISABLED" envDefault:"false"`

	// WebAuthn relying party. Derived from PublicURL when empty.
	RPID          string   `env:"WS_RP_ID"`
	RPDisplayName string   `env:"WS_RP_NAME" envDefault:"ws"`
	RPOrigins     []string `env:"WS_RP_ORIGINS" envSeparator:","`
}

// Load reads .env (if present) then the environment.
func Load() (*Config, error) {
	_ = godotenv.Load() // ignore missing .env
	var c Config
	if err := env.Parse(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if len(c.SessionSecret) < 32 {
		return nil, fmt.Errorf("config: WS_SESSION_SECRET must be at least 32 bytes")
	}
	if c.RPID == "" {
		c.RPID = hostOf(c.PublicURL)
	}
	if len(c.RPOrigins) == 0 {
		c.RPOrigins = []string{c.PublicURL}
	}
	return &c, nil
}

// Secret returns the value of a named env var (used for provider API keys
// referenced from the database by name).
func Secret(envName string) string {
	return os.Getenv(envName)
}

// IsDev reports development mode.
func (c *Config) IsDev() bool { return c.Env == "dev" }

func hostOf(u string) string {
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimPrefix(u, "http://")
	if i := strings.IndexAny(u, ":/"); i >= 0 {
		u = u[:i]
	}
	return u
}
