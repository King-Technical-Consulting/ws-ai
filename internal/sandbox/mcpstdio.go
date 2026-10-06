package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/jking323/ws/internal/agent"
	"github.com/jking323/ws/internal/mcpclient"
)

// StdioRunner runs MCP stdio servers inside sandboxes (mcpclient.Runner,
// PLAN M5). The worker wires it with the manager and the same resolver
// the sandbox tools use, so a server command runs as the sandbox user in
// /workspace of the conversation's project, with the sandbox's egress
// proxy and no credential.
type StdioRunner struct {
	M       *Manager
	Resolve Resolver
}

// Key ensures the sandbox for the call and returns its container id: a
// stopped and restarted sandbox gets a new container, so the client's
// per-key session cannot outlive the process it talks to.
func (r *StdioRunner) Key(ctx context.Context, tc agent.ToolCtx) (string, error) {
	sb, err := r.Resolve(ctx, tc)
	if err != nil {
		return "", err
	}
	if sb.ContainerID == "" {
		return "", errors.New("sandbox has no container")
	}
	return sb.ContainerID, nil
}

// Open starts argv in the container Key named.
func (r *StdioRunner) Open(ctx context.Context, key string, argv, env []string) (*mcpclient.Stream, error) {
	return r.M.ExecStream(ctx, key, argv, env)
}

// Probe starts argv in a throwaway container of the sandbox image (no
// project volume: an empty tmpfs at /workspace) and returns its stdio;
// closing the stream removes the container.
func (r *StdioRunner) Probe(ctx context.Context, argv, env []string) (*mcpclient.Stream, error) {
	id, err := r.M.probeContainer(ctx)
	if err != nil {
		return nil, err
	}
	st, err := r.M.ExecStream(ctx, id, argv, env)
	if err != nil {
		_, _ = r.M.cli.ContainerRemove(context.WithoutCancel(ctx), id, client.ContainerRemoveOptions{Force: true})
		return nil, err
	}
	inner := st.Close
	st.Close = func() error {
		err := inner()
		rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, rerr := r.M.cli.ContainerRemove(rctx, id, client.ContainerRemoveOptions{Force: true}); rerr != nil {
			return errors.Join(err, rerr)
		}
		return err
	}
	return st, nil
}

// ExecStream starts argv in a container with its stdin and stdout
// attached (no tty), for a long-lived stdio protocol. stderr is kept in a
// small buffer and surfaced by Close's error when the process failed to
// produce a stream at all. The process ends when stdin is closed, which is
// how MCP stdio servers are told to exit.
func (m *Manager) ExecStream(ctx context.Context, containerID string, argv, env []string) (*mcpclient.Stream, error) {
	if containerID == "" {
		return nil, errors.New("sandbox: no container")
	}
	if len(argv) == 0 {
		return nil, errors.New("sandbox: empty command")
	}
	ex, err := m.cli.ExecCreate(ctx, containerID, client.ExecCreateOptions{
		Cmd: argv, WorkingDir: Workspace, Env: env, User: "dev",
		AttachStdin: true, AttachStdout: true, AttachStderr: true,
	})
	if err != nil {
		return nil, fmt.Errorf("sandbox: exec create: %w", err)
	}
	// The attach outlives the request that opened it: it is a session.
	att, err := m.cli.ExecAttach(context.WithoutCancel(ctx), ex.ID, client.ExecAttachOptions{})
	if err != nil {
		return nil, fmt.Errorf("sandbox: exec attach: %w", err)
	}
	pr, pw := io.Pipe()
	errb := &capWriter{max: 16 << 10}
	go func() {
		_, err := stdcopy.StdCopy(pw, errb, att.Reader)
		_ = pw.CloseWithError(err)
	}()
	var once sync.Once
	closeFn := func() error {
		once.Do(func() {
			_ = att.CloseWrite()
			att.Close()
			_ = pr.Close()
		})
		if s := strings.TrimSpace(errb.String()); s != "" {
			return fmt.Errorf("stderr: %s", s)
		}
		return nil
	}
	return &mcpclient.Stream{Stdin: writeCloser{att.Conn, att.CloseWrite}, Stdout: pr, Close: closeFn}, nil
}

// writeCloser is the exec's stdin: writes go to the attach connection,
// Close half-closes it so the process sees EOF.
type writeCloser struct {
	w     io.Writer
	close func() error
}

func (w writeCloser) Write(p []byte) (int, error) { return w.w.Write(p) }
func (w writeCloser) Close() error                { return w.close() }

// probeContainer creates and starts a container of the sandbox image with
// the sandbox's env, limits and network but no project volume. The caller
// removes it.
func (m *Manager) probeContainer(ctx context.Context) (string, error) {
	if err := m.ensureImage(ctx); err != nil {
		return "", err
	}
	cfg, hc, nc := m.containerSpec(nil)
	hc.Tmpfs[Workspace] = "rw,size=256m"
	labels := map[string]string{"ws.probe": "mcp"}
	for k, v := range m.cfg.Labels {
		labels[k] = v
	}
	cfg.Labels = labels
	res, err := m.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:   "ws-probe-" + strings.ReplaceAll(time.Now().UTC().Format("150405.000"), ".", ""),
		Config: cfg, HostConfig: hc, NetworkingConfig: nc,
	})
	if err != nil {
		return "", fmt.Errorf("sandbox: probe create: %w", err)
	}
	if _, err := m.cli.ContainerStart(ctx, res.ID, client.ContainerStartOptions{}); err != nil {
		_, _ = m.cli.ContainerRemove(ctx, res.ID, client.ContainerRemoveOptions{Force: true})
		return "", fmt.Errorf("sandbox: probe start: %w", err)
	}
	return res.ID, nil
}

// containerSpec is the sandbox container definition shared by user
// sandboxes and probes: env, hardening, limits and network. mounts is
// the volume list (nil for a probe).
func (m *Manager) containerSpec(mounts []mount.Mount) (*container.Config, *container.HostConfig, *network.NetworkingConfig) {
	env := []string{
		"HOME=/home/dev", "USER=dev", "TERM=xterm-256color", "LANG=C.UTF-8",
		"WORKSPACE=" + Workspace,
		"GIT_TERMINAL_PROMPT=0",
		// git talks plain HTTP to the proxy for github.com; the proxy upgrades
		// to HTTPS and injects credentials, so none live in here.
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=url.http://github.com/.insteadOf", "GIT_CONFIG_VALUE_0=https://github.com/",
		"GIT_CONFIG_KEY_1=url.http://github.com/.insteadOf", "GIT_CONFIG_VALUE_1=git@github.com:",
		"CI=true", "npm_config_update_notifier=false", "DO_NOT_TRACK=1",
	}
	if m.cfg.ProxyURL != "" {
		for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
			env = append(env, k+"="+m.cfg.ProxyURL)
		}
		env = append(env, "NO_PROXY=localhost,127.0.0.1,::1", "no_proxy=localhost,127.0.0.1,::1")
	}
	pids := m.cfg.Pids
	hc := &container.HostConfig{
		Mounts: mounts,
		// Root is the image; everything writable is a tmpfs or the volume.
		ReadonlyRootfs: true,
		Tmpfs:          map[string]string{"/tmp": "rw,size=2g", "/home/dev": "rw,size=1g", "/run": "rw,size=16m"},
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges:true"},
		Resources: container.Resources{
			Memory:    m.cfg.MemoryMB << 20,
			NanoCPUs:  int64(m.cfg.CPUs * 1e9),
			PidsLimit: &pids,
		},
		Init:        ptr(true),
		NetworkMode: container.NetworkMode(m.cfg.Network),
	}
	if m.runtime == "runsc" {
		hc.Runtime = "runsc"
	}
	var nc *network.NetworkingConfig
	if m.cfg.Network != "" {
		nc = &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{m.cfg.Network: {}}}
	}
	cfg := &container.Config{
		Image:      m.cfg.Image,
		Cmd:        []string{"sleep", "infinity"},
		User:       "dev",
		WorkingDir: Workspace,
		Env:        env,
		Hostname:   "sandbox",
		StopSignal: "SIGKILL",
	}
	return cfg, hc, nc
}
