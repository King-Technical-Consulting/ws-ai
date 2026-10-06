// Package mcpclient connects ws's agent runtime to Model Context Protocol
// servers and exposes their tools as ordinary agent tools named
// mcp__<server>__<tool>, subject to the same auto/ask/deny policies.
// Servers are declared in config/mcp.yaml (Streamable HTTP; stdio servers
// run behind a bridge such as supergateway so their credentials never
// touch a user sandbox).
package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"

	"github.com/jking323/ws/internal/agent"
	"github.com/jking323/ws/internal/gateway"
)

// ServerConfig is one MCP server: a URL (Streamable HTTP or SSE, reached
// from both processes) or a command (a stdio server started inside the
// caller's coding sandbox, worker only).
type ServerConfig struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
	// Transport: streamable_http (default) or sse. URL servers only.
	Transport string `yaml:"transport"`
	// Command is the argv of a stdio server run inside the sandbox of the
	// conversation's project (docker exec, user dev, /workspace), with the
	// worker as the MCP client. Mutually exclusive with URL. The tool list
	// is read at boot by running the command once in a throwaway container
	// of the sandbox image.
	Command []string `yaml:"command"`
	// Env is extra KEY=value pairs for the command. Plain values only:
	// secrets never enter a sandbox (the egress proxy injects them).
	Env []string `yaml:"env"`
	// HeadersEnv maps header names to env var names holding their values
	// (e.g. Authorization: MCP_FOO_TOKEN as "Bearer <value>").
	HeadersEnv map[string]string `yaml:"headers_env"`
	// Policy is the default policy for every tool on this server (ask).
	Policy agent.Policy `yaml:"policy"`
	// Policies overrides per tool name.
	Policies map[string]agent.Policy `yaml:"policies"`
	// Allow restricts which tools are registered (empty = all); Deny removes.
	Allow   []string `yaml:"allow"`
	Deny    []string `yaml:"deny"`
	Enabled *bool    `yaml:"enabled"`
	// Idempotent lists tools safe to re-run after a crash (reads).
	Idempotent []string `yaml:"idempotent"`
}

// File is config/mcp.yaml.
type File struct {
	Servers []ServerConfig `yaml:"servers"`
}

// LoadFile parses the config; a missing file is an empty config.
func LoadFile(path string) (*File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &File{}, nil
		}
		return nil, err
	}
	var f File
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("mcp.yaml: %w", err)
	}
	seen := map[string]bool{}
	for i, s := range f.Servers {
		if s.Name == "" || !nameRe.MatchString(s.Name) {
			return nil, fmt.Errorf("mcp.yaml: server %d: name must match %s", i, nameRe)
		}
		if seen[s.Name] {
			return nil, fmt.Errorf("mcp.yaml: duplicate server %q", s.Name)
		}
		seen[s.Name] = true
		switch {
		case s.URL == "" && len(s.Command) == 0:
			return nil, fmt.Errorf("mcp.yaml: server %s: url or command required", s.Name)
		case s.URL != "" && len(s.Command) > 0:
			return nil, fmt.Errorf("mcp.yaml: server %s: url and command are mutually exclusive", s.Name)
		}
		for _, e := range s.Env {
			if !strings.Contains(e, "=") || strings.HasPrefix(e, "=") {
				return nil, fmt.Errorf("mcp.yaml: server %s: env entries are KEY=value, got %q", s.Name, e)
			}
		}
	}
	return &f, nil
}

// Sandbox reports whether the server runs inside a sandbox (stdio).
func (c ServerConfig) Sandbox() bool { return len(c.Command) > 0 }

// defaultPolicy is the policy for a tool on this server.
func (c ServerConfig) defaultPolicy(tool string) agent.Policy {
	if p, ok := c.Policies[tool]; ok && p != "" {
		return p
	}
	if c.Policy != "" {
		return c.Policy
	}
	return agent.PolicyAsk
}

func (c ServerConfig) idempotent(tool string) bool {
	for _, n := range c.Idempotent {
		if n == tool {
			return true
		}
	}
	return false
}

func (c ServerConfig) allowed(name string) bool {
	for _, d := range c.Deny {
		if d == name {
			return false
		}
	}
	if len(c.Allow) == 0 {
		return true
	}
	for _, a := range c.Allow {
		if a == name {
			return true
		}
	}
	return false
}

// listTools pages through tools/list on a session.
func listTools(ctx context.Context, sess *mcp.ClientSession) ([]*mcp.Tool, error) {
	var tools []*mcp.Tool
	cursor := ""
	for {
		res, err := sess.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, err
		}
		tools = append(tools, res.Tools...)
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	return tools, nil
}

// newTools wraps a server's tool list, filtered by allow/deny, around call.
func newTools(cfg ServerConfig, tools []*mcp.Tool, call callFunc) []agent.Tool {
	var out []agent.Tool
	for _, t := range tools {
		if !cfg.allowed(t.Name) {
			continue
		}
		out = append(out, &mcpTool{cfg: cfg, name: ToolName(cfg.Name, t.Name), remote: t.Name, def: t, call: call})
	}
	return out
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,30}$`)
var toolNameRe = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// ToolName builds the registered name; providers accept [A-Za-z0-9_-]{1,64}.
func ToolName(server, tool string) string {
	n := "mcp__" + server + "__" + toolNameRe.ReplaceAllString(tool, "_")
	if len(n) > 64 {
		n = n[:64]
	}
	return n
}

// Server is a live connection to one MCP server.
type Server struct {
	cfg  ServerConfig
	log  *slog.Logger
	http *http.Client

	mu      sync.Mutex
	session *mcp.ClientSession
}

type headerRT struct {
	base    http.RoundTripper
	headers map[string]string
}

func (h headerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	for k, v := range h.headers {
		r.Header.Set(k, v)
	}
	return h.base.RoundTrip(r)
}

// Connect opens a session and lists tools. The connection is retried for
// up to `wait` so a sidecar that is still starting isn't skipped.
func Connect(ctx context.Context, cfg ServerConfig, log *slog.Logger, wait time.Duration) (*Server, []agent.Tool, error) {
	headers := map[string]string{}
	for k, env := range cfg.HeadersEnv {
		if v := os.Getenv(env); v != "" {
			headers[k] = v
		}
	}
	s := &Server{cfg: cfg, log: log, http: &http.Client{Timeout: 0, Transport: headerRT{base: http.DefaultTransport, headers: headers}}}
	deadline := time.Now().Add(wait)
	var tools []*mcp.Tool
	for {
		var err error
		tools, err = s.connect(ctx)
		if err == nil {
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil, nil, err
		}
		time.Sleep(3 * time.Second)
	}
	return s, newTools(cfg, tools, func(ctx context.Context, _ agent.ToolCtx, name string, args map[string]any) (*mcp.CallToolResult, error) {
		return s.call(ctx, name, args)
	}), nil
}

func (s *Server) transport() mcp.Transport {
	if s.cfg.Transport == "sse" {
		return &mcp.SSEClientTransport{Endpoint: s.cfg.URL, HTTPClient: s.http}
	}
	return &mcp.StreamableClientTransport{Endpoint: s.cfg.URL, HTTPClient: s.http, MaxRetries: 3}
}

func (s *Server) connect(ctx context.Context) ([]*mcp.Tool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session != nil {
		_ = s.session.Close()
		s.session = nil
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "ws", Version: "dev"}, &mcp.ClientOptions{KeepAlive: 30 * time.Second})
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	sess, err := client.Connect(cctx, s.transport(), nil)
	if err != nil {
		return nil, fmt.Errorf("mcp %s: connect: %w", s.cfg.Name, err)
	}
	tools, err := listTools(cctx, sess)
	if err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("mcp %s: list tools: %w", s.cfg.Name, err)
	}
	s.session = sess
	return tools, nil
}

// Close ends the session.
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session != nil {
		_ = s.session.Close()
		s.session = nil
	}
}

func (s *Server) call(ctx context.Context, name string, args map[string]any) (*mcp.CallToolResult, error) {
	s.mu.Lock()
	sess := s.session
	s.mu.Unlock()
	if sess == nil {
		if _, err := s.connect(ctx); err != nil {
			return nil, err
		}
		s.mu.Lock()
		sess = s.session
		s.mu.Unlock()
	}
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		// One reconnect: the sidecar may have restarted.
		if _, cerr := s.connect(ctx); cerr != nil {
			return nil, err
		}
		s.mu.Lock()
		sess = s.session
		s.mu.Unlock()
		res, err = sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	}
	return res, err
}

// callFunc performs one tool call on whichever session the call's context
// needs (a shared session for URL servers, a per-sandbox one for stdio).
type callFunc func(ctx context.Context, tc agent.ToolCtx, name string, args map[string]any) (*mcp.CallToolResult, error)

// mcpTool adapts one remote tool.
type mcpTool struct {
	cfg    ServerConfig
	name   string
	remote string
	def    *mcp.Tool
	call   callFunc
}

func (t *mcpTool) Def() gateway.ToolDef {
	schema, _ := json.Marshal(t.def.InputSchema)
	if len(schema) == 0 || string(schema) == "null" {
		schema = []byte(`{"type":"object","properties":{}}`)
	}
	desc := t.def.Description
	if desc == "" {
		desc = "Tool " + t.remote + " on the " + t.cfg.Name + " MCP server."
	}
	return gateway.ToolDef{Name: t.name, Description: desc, InputSchema: schema}
}

func (t *mcpTool) DefaultPolicy() agent.Policy { return t.cfg.defaultPolicy(t.remote) }

func (t *mcpTool) Idempotent() bool { return t.cfg.idempotent(t.remote) }

func (t *mcpTool) Call(ctx context.Context, tc agent.ToolCtx, args json.RawMessage) (agent.Result, error) {
	var in map[string]any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return agent.Result{Text: "bad arguments: " + err.Error(), IsError: true}, nil
		}
	}
	if in == nil {
		in = map[string]any{}
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	res, err := t.call(cctx, tc, t.remote, in)
	if err != nil {
		return agent.Result{Text: fmt.Sprintf("%s: %v", t.name, err), IsError: true}, nil
	}
	var b strings.Builder
	var data []any
	for _, c := range res.Content {
		switch v := c.(type) {
		case *mcp.TextContent:
			b.WriteString(v.Text)
			b.WriteString("\n")
			data = append(data, map[string]any{"type": "text", "text": v.Text})
		case *mcp.ImageContent:
			fmt.Fprintf(&b, "[image %s, %d bytes]\n", v.MIMEType, len(v.Data))
			data = append(data, map[string]any{"type": "image", "mime": v.MIMEType, "bytes": len(v.Data)})
		default:
			if j, err := json.Marshal(c); err == nil {
				b.Write(j)
				b.WriteString("\n")
			}
		}
	}
	text := strings.TrimSpace(b.String())
	if text == "" && res.StructuredContent != nil {
		j, _ := json.Marshal(res.StructuredContent)
		text = string(j)
	}
	if text == "" {
		text = "(no output)"
	}
	out := agent.Result{Text: text, IsError: res.IsError}
	if res.StructuredContent != nil {
		out.Data = res.StructuredContent
	} else if len(data) > 0 {
		out.Data = data
	}
	return out, nil
}

// ConnectAll connects every enabled server in the file and returns the
// tools plus a closer. Failures are logged and skipped so one dead sidecar
// doesn't take the process down. Command (sandbox stdio) servers need a
// Runner, which only the worker has; without one they are skipped.
func ConnectAll(ctx context.Context, f *File, runner Runner, log *slog.Logger, wait time.Duration) ([]agent.Tool, func()) {
	var tools []agent.Tool
	var closers []func()
	for _, cfg := range f.Servers {
		if cfg.Enabled != nil && !*cfg.Enabled {
			continue
		}
		var ts []agent.Tool
		var closer func()
		var err error
		if cfg.Sandbox() {
			if runner == nil {
				log.Info("mcp: sandbox server needs the worker's sandbox manager, skipping here", "server", cfg.Name)
				continue
			}
			var s *SandboxServer
			s, ts, err = ConnectSandbox(ctx, cfg, runner, log, wait)
			if err == nil {
				closer = s.Close
			}
		} else {
			var s *Server
			s, ts, err = Connect(ctx, cfg, log, wait)
			if err == nil {
				closer = s.Close
			}
		}
		if err != nil {
			log.Warn("mcp: server unavailable, skipping", "server", cfg.Name, "url", cfg.URL, "command", strings.Join(cfg.Command, " "), "err", err)
			continue
		}
		names := make([]string, 0, len(ts))
		for _, t := range ts {
			names = append(names, t.Def().Name)
		}
		log.Info("mcp: connected", "server", cfg.Name, "sandbox", cfg.Sandbox(), "tools", len(ts), "names", strings.Join(names, ","))
		closers = append(closers, closer)
		tools = append(tools, ts...)
	}
	return tools, func() {
		for _, c := range closers {
			c()
		}
	}
}
