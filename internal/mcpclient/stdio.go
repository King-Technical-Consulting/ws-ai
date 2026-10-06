package mcpclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jking323/ws/internal/agent"
)

// Stdio servers inside the sandbox (PLAN M5). A command in mcp.yaml is
// started by the worker inside the coding sandbox of the conversation's
// project, over docker exec, with its stdin and stdout as the MCP
// transport. The server therefore sees /workspace and nothing else, runs
// as the sandbox user, reaches the network only through the egress proxy,
// and holds no credential. One session per sandbox is kept for the life
// of the container; a stopped or recreated sandbox gets a new one on the
// next call.

// Stream is a running stdio server's pipes.
type Stream struct {
	Stdin  io.WriteCloser
	Stdout io.ReadCloser
	// Close ends the process (and, for a probe, its container).
	Close func() error
}

// Runner starts stdio servers. The worker implements it with the sandbox
// manager; tests use local processes.
type Runner interface {
	// Probe runs argv in a throwaway container of the sandbox image, so the
	// tool list can be read at boot without any user sandbox.
	Probe(ctx context.Context, argv, env []string) (*Stream, error)
	// Key resolves the sandbox a call belongs to (conversation → project →
	// sandbox, created or restarted as needed) and returns an identifier
	// that changes when the container does, so a session is reused only
	// while its process can still be alive.
	Key(ctx context.Context, tc agent.ToolCtx) (string, error)
	// Open runs argv in the sandbox Key named.
	Open(ctx context.Context, key string, argv, env []string) (*Stream, error)
}

// SandboxServer is one stdio server definition with a session per sandbox.
type SandboxServer struct {
	cfg    ServerConfig
	runner Runner
	log    *slog.Logger

	mu       sync.Mutex
	sessions map[string]*stdioSession
}

type stdioSession struct {
	sess   *mcp.ClientSession
	stream *Stream
}

func (s *stdioSession) close() {
	if s == nil {
		return
	}
	if s.sess != nil {
		_ = s.sess.Close()
	}
	if s.stream != nil && s.stream.Close != nil {
		_ = s.stream.Close()
	}
}

// connectStream runs the MCP handshake over a stream.
func connectStream(ctx context.Context, name string, st *Stream) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "ws", Version: "dev"}, nil)
	sess, err := client.Connect(ctx, &mcp.IOTransport{Reader: st.Stdout, Writer: st.Stdin}, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp %s: connect: %w", name, err)
	}
	return sess, nil
}

// ConnectSandbox reads the server's tool list by probing it once and
// returns the tools; sessions in real sandboxes open on first use. The
// probe is retried for up to wait (the image may still be pulling).
func ConnectSandbox(ctx context.Context, cfg ServerConfig, runner Runner, log *slog.Logger, wait time.Duration) (*SandboxServer, []agent.Tool, error) {
	if !cfg.Sandbox() {
		return nil, nil, fmt.Errorf("mcp %s: not a command server", cfg.Name)
	}
	if runner == nil {
		return nil, nil, fmt.Errorf("mcp %s: no sandbox runner", cfg.Name)
	}
	s := &SandboxServer{cfg: cfg, runner: runner, log: log, sessions: map[string]*stdioSession{}}
	deadline := time.Now().Add(wait)
	var tools []*mcp.Tool
	for {
		var err error
		tools, err = s.probe(ctx)
		if err == nil {
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil, nil, err
		}
		time.Sleep(3 * time.Second)
	}
	return s, newTools(cfg, tools, s.call), nil
}

func (s *SandboxServer) probe(ctx context.Context) ([]*mcp.Tool, error) {
	pctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	st, err := s.runner.Probe(pctx, s.cfg.Command, s.cfg.Env)
	if err != nil {
		return nil, fmt.Errorf("mcp %s: probe: %w", s.cfg.Name, err)
	}
	ss := &stdioSession{stream: st}
	defer ss.close()
	sess, err := connectStream(pctx, s.cfg.Name, st)
	if err != nil {
		return nil, err
	}
	ss.sess = sess
	tools, err := listTools(pctx, sess)
	if err != nil {
		return nil, fmt.Errorf("mcp %s: list tools: %w", s.cfg.Name, err)
	}
	return tools, nil
}

// session returns the live session for a sandbox key, opening one when
// there is none; fresh replaces whatever is cached (after a failed call).
func (s *SandboxServer) session(ctx context.Context, key string, fresh bool) (*mcp.ClientSession, error) {
	s.mu.Lock()
	cur, ok := s.sessions[key]
	if ok && !fresh {
		s.mu.Unlock()
		return cur.sess, nil
	}
	delete(s.sessions, key)
	s.mu.Unlock()
	cur.close()

	st, err := s.runner.Open(ctx, key, s.cfg.Command, s.cfg.Env)
	if err != nil {
		return nil, fmt.Errorf("mcp %s: start in sandbox: %w", s.cfg.Name, err)
	}
	sess, err := connectStream(ctx, s.cfg.Name, st)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	s.mu.Lock()
	if other, ok := s.sessions[key]; ok {
		// Lost a race with another call for the same sandbox: keep theirs.
		s.mu.Unlock()
		_ = sess.Close()
		_ = st.Close()
		return other.sess, nil
	}
	s.sessions[key] = &stdioSession{sess: sess, stream: st}
	s.mu.Unlock()
	return sess, nil
}

// call runs one tool on the call's sandbox, reconnecting once when the
// session has died (the container was stopped or recreated).
func (s *SandboxServer) call(ctx context.Context, tc agent.ToolCtx, name string, args map[string]any) (*mcp.CallToolResult, error) {
	key, err := s.runner.Key(ctx, tc)
	if err != nil {
		return nil, fmt.Errorf("mcp %s: sandbox: %w", s.cfg.Name, err)
	}
	sess, err := s.session(ctx, key, false)
	if err != nil {
		return nil, err
	}
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err == nil || ctx.Err() != nil {
		return res, err
	}
	sess, cerr := s.session(ctx, key, true)
	if cerr != nil {
		return nil, errors.Join(err, cerr)
	}
	return sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
}

// Close ends every session.
func (s *SandboxServer) Close() {
	s.mu.Lock()
	sessions := s.sessions
	s.sessions = map[string]*stdioSession{}
	s.mu.Unlock()
	for _, ss := range sessions {
		ss.close()
	}
}

// Drop forgets the session for one sandbox key (for example when the
// worker stops that sandbox), closing it.
func (s *SandboxServer) Drop(key string) {
	s.mu.Lock()
	ss := s.sessions[key]
	delete(s.sessions, key)
	s.mu.Unlock()
	ss.close()
}
