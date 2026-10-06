package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jking323/ws/internal/agent"
)

// The test binary doubles as a stdio MCP server when this variable is
// set, so the sandbox path is exercised with real processes and pipes
// without Docker.
const stdioServerEnv = "WS_MCP_STDIO_TEST_SERVER"

func TestMain(m *testing.M) {
	if os.Getenv(stdioServerEnv) == "1" {
		runStdioServer()
		return
	}
	os.Exit(m.Run())
}

func runStdioServer() {
	srv := mcp.NewServer(&mcp.Implementation{Name: "stub-stdio", Version: "0"}, nil)
	type echoIn struct {
		Text string `json:"text"`
	}
	mcp.AddTool(srv, &mcp.Tool{Name: "echo", Description: "Echo"}, func(_ context.Context, _ *mcp.CallToolRequest, in echoIn) (*mcp.CallToolResult, any, error) {
		fmt.Fprintln(os.Stderr, "stderr noise must not break the protocol")
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "echo: " + in.Text}}}, nil, nil
	})
	type envIn struct {
		Name string `json:"name"`
	}
	mcp.AddTool(srv, &mcp.Tool{Name: "env", Description: "Read an env var"}, func(_ context.Context, _ *mcp.CallToolRequest, in envIn) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: os.Getenv(in.Name)}}}, nil, nil
	})
	mcp.AddTool(srv, &mcp.Tool{Name: "pid", Description: "Process id"}, func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: strconv.Itoa(os.Getpid())}}}, nil, nil
	})
	if err := srv.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		os.Exit(1)
	}
}

// localRunner runs the stub as a local process: Key is the conversation
// id, so each conversation is its own "sandbox".
type localRunner struct {
	mu      sync.Mutex
	probes  int
	opens   int
	streams map[string]*Stream
	keyErr  error
}

func (r *localRunner) start(ctx context.Context, argv, env []string) (*Stream, error) {
	cmd := exec.CommandContext(ctx, os.Args[0], argv[1:]...)
	cmd.Env = append(os.Environ(), stdioServerEnv+"=1")
	cmd.Env = append(cmd.Env, env...)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &Stream{Stdin: stdin, Stdout: stdout, Close: func() error {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil
	}}, nil
}

func (r *localRunner) Probe(ctx context.Context, argv, env []string) (*Stream, error) {
	r.mu.Lock()
	r.probes++
	r.mu.Unlock()
	return r.start(context.Background(), argv, env)
}

func (r *localRunner) Key(_ context.Context, tc agent.ToolCtx) (string, error) {
	if r.keyErr != nil {
		return "", r.keyErr
	}
	return tc.ConversationID.String(), nil
}

func (r *localRunner) Open(_ context.Context, key string, argv, env []string) (*Stream, error) {
	st, err := r.start(context.Background(), argv, env)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.opens++
	if r.streams == nil {
		r.streams = map[string]*Stream{}
	}
	r.streams[key] = st
	r.mu.Unlock()
	return st, nil
}

func text(t *testing.T, tl agent.Tool, tc agent.ToolCtx, args string) string {
	t.Helper()
	res, err := tl.Call(context.Background(), tc, json.RawMessage(args))
	if err != nil || res.IsError {
		t.Fatalf("%s %s: %v %+v", tl.Def().Name, args, err, res)
	}
	return strings.TrimSpace(res.Text)
}

func TestSandboxServer(t *testing.T) {
	r := &localRunner{}
	cfg := ServerConfig{Name: "local", Command: []string{"stub", "serve"}, Env: []string{"MCP_TEST_FLAVOUR=sandbox"}, Policy: agent.PolicyAuto, Idempotent: []string{"pid"}}
	s, tools, err := ConnectSandbox(context.Background(), cfg, r, slog.Default(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	byName := map[string]agent.Tool{}
	for _, tl := range tools {
		byName[tl.Def().Name] = tl
	}
	if len(byName) != 3 || byName["mcp__local__echo"] == nil || byName["mcp__local__env"] == nil || byName["mcp__local__pid"] == nil {
		t.Fatalf("tools = %v", byName)
	}
	if r.probes != 1 || r.opens != 0 {
		t.Errorf("probe must list tools without opening a sandbox session: probes=%d opens=%d", r.probes, r.opens)
	}
	if byName["mcp__local__pid"].DefaultPolicy() != agent.PolicyAuto || !byName["mcp__local__pid"].Idempotent() || byName["mcp__local__echo"].Idempotent() {
		t.Errorf("policy or idempotent wrong")
	}

	a := agent.ToolCtx{ConversationID: uuid.New()}
	b := agent.ToolCtx{ConversationID: uuid.New()}
	if got := text(t, byName["mcp__local__echo"], a, `{"text":"hi"}`); got != "echo: hi" {
		t.Errorf("echo = %q", got)
	}
	if got := text(t, byName["mcp__local__env"], a, `{"name":"MCP_TEST_FLAVOUR"}`); got != "sandbox" {
		t.Errorf("env from mcp.yaml not passed: %q", got)
	}
	pidA := text(t, byName["mcp__local__pid"], a, `{}`)
	if r.opens != 1 {
		t.Errorf("one session per sandbox: opens=%d", r.opens)
	}
	if again := text(t, byName["mcp__local__pid"], a, `{}`); again != pidA || r.opens != 1 {
		t.Errorf("session not reused: pid %s -> %s, opens=%d", pidA, again, r.opens)
	}
	pidB := text(t, byName["mcp__local__pid"], b, `{}`)
	if pidB == pidA || r.opens != 2 {
		t.Errorf("second sandbox must get its own process: %s vs %s, opens=%d", pidA, pidB, r.opens)
	}

	// The process behind A dies (the sandbox was stopped): one reconnect.
	r.mu.Lock()
	dead := r.streams[a.ConversationID.String()]
	r.mu.Unlock()
	_ = dead.Close()
	pidA2 := text(t, byName["mcp__local__pid"], a, `{}`)
	if pidA2 == pidA || r.opens != 3 {
		t.Errorf("reconnect after death: pid %s -> %s, opens=%d", pidA, pidA2, r.opens)
	}

	// Drop closes the session; the next call opens a new one.
	s.Drop(a.ConversationID.String())
	if pidA3 := text(t, byName["mcp__local__pid"], a, `{}`); pidA3 == pidA2 || r.opens != 4 {
		t.Errorf("after Drop: pid %s -> %s, opens=%d", pidA2, pidA3, r.opens)
	}

	// A conversation outside a code project cannot resolve a sandbox.
	r.keyErr = fmt.Errorf("sandbox tools are only available in code projects")
	res, _ := byName["mcp__local__echo"].Call(context.Background(), a, json.RawMessage(`{"text":"x"}`))
	if !res.IsError || !strings.Contains(res.Text, "code projects") {
		t.Errorf("resolver error must surface: %+v", res)
	}
	r.keyErr = nil

	// Bad arguments are a tool error, not a dead session.
	res, _ = byName["mcp__local__echo"].Call(context.Background(), a, json.RawMessage(`{"text":`))
	if !res.IsError {
		t.Errorf("bad json must be IsError: %+v", res)
	}
}

func TestConnectAllSandboxServers(t *testing.T) {
	f := &File{Servers: []ServerConfig{
		{Name: "local", Command: []string{"stub"}},
		{Name: "broken", Command: []string{"stub"}, Env: []string{"X=1"}},
	}}
	// Without a runner (serve) command servers are skipped, not errors.
	tools, closer := ConnectAll(context.Background(), f, nil, slog.Default(), time.Second)
	closer()
	if len(tools) != 0 {
		t.Errorf("no runner must skip sandbox servers: %d tools", len(tools))
	}
	r := &localRunner{}
	tools, closer = ConnectAll(context.Background(), f, r, slog.Default(), 5*time.Second)
	defer closer()
	if len(tools) != 6 || r.probes != 2 {
		t.Errorf("with a runner: %d tools, probes=%d", len(tools), r.probes)
	}
}

func TestLoadFileCommandServers(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) (*File, error) {
		p := dir + "/mcp.yaml"
		_ = os.WriteFile(p, []byte(s), 0o644)
		return LoadFile(p)
	}
	f, err := write("servers:\n  - name: fs\n    command: [npx, -y, \"@modelcontextprotocol/server-filesystem\", /workspace]\n    env: [FOO=bar]\n    policy: ask\n")
	if err != nil || len(f.Servers) != 1 || !f.Servers[0].Sandbox() || f.Servers[0].Command[0] != "npx" || f.Servers[0].Env[0] != "FOO=bar" {
		t.Errorf("command server: %v %+v", err, f)
	}
	if _, err := write("servers:\n  - name: both\n    url: http://x/mcp\n    command: [x]\n"); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("url+command must be rejected: %v", err)
	}
	if _, err := write("servers:\n  - name: none\n    policy: ask\n"); err == nil || !strings.Contains(err.Error(), "url or command") {
		t.Errorf("neither must be rejected: %v", err)
	}
	if _, err := write("servers:\n  - name: env\n    command: [x]\n    env: [NOEQUALS]\n"); err == nil || !strings.Contains(err.Error(), "KEY=value") {
		t.Errorf("bad env must be rejected: %v", err)
	}
}
