// Package sandbox runs coding sessions in hardened Docker containers. One
// sandbox per (project, user): a container on the internal sandbox network
// with no egress except the worker's proxy, a persistent volume at
// /workspace, and a read-only root. Tools (read_file, bash, …) run as
// `docker exec` inside it. Only the worker process has the Docker socket.
package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/jking323/ws/internal/store"
)

// Workspace is the mount point of the persistent volume inside the sandbox.
const Workspace = "/workspace"

// Config is the sandbox manager's configuration.
type Config struct {
	Image    string        // sandbox image, e.g. ghcr.io/king-technical-consulting/ws-sandbox:latest
	Network  string        // internal Docker network sandboxes join (compose: ws_sandbox-net)
	ProxyURL string        // egress proxy sandboxes must use, e.g. http://worker:3128
	Runtime  string        // "auto" (runsc if present, else runc), "runc", or "runsc"
	MemoryMB int64         // memory limit per sandbox
	CPUs     float64       // CPU limit per sandbox
	Pids     int64         // pids limit
	Jail     bool          // run bash tool commands inside bubblewrap (needs bwrap in the image and user namespaces in the container)
	IdleStop time.Duration // stop containers idle this long
	Remove   time.Duration // remove stopped containers idle this long (volume kept)
	Labels   map[string]string
}

// Sandbox is a (project, user) sandbox and its container.
type Sandbox struct {
	ID          uuid.UUID
	ProjectID   uuid.UUID
	UserID      uuid.UUID
	ContainerID string
	Volume      string
	Runtime     string
}

// Manager owns sandbox lifecycle against one Docker daemon.
type Manager struct {
	cli     *client.Client
	db      *store.DB
	log     *slog.Logger
	cfg     Config
	runtime string

	mu    sync.Mutex
	locks map[string]*sync.Mutex

	ipMu    sync.Mutex
	ipCache map[string]ipEntry
}

type ipEntry struct {
	sb *Sandbox
	at time.Time
}

// New connects to Docker (DOCKER_HOST or the default socket) and probes for
// gVisor. Fails if the daemon is unreachable.
func New(ctx context.Context, db *store.DB, log *slog.Logger, cfg Config) (*Manager, error) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("sandbox: docker client: %w", err)
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := cli.Ping(pctx, client.PingOptions{}); err != nil {
		return nil, fmt.Errorf("sandbox: docker ping: %w", err)
	}
	m := &Manager{cli: cli, db: db, log: log, cfg: cfg, locks: map[string]*sync.Mutex{}, ipCache: map[string]ipEntry{}}
	if m.cfg.MemoryMB <= 0 {
		m.cfg.MemoryMB = 4096
	}
	if m.cfg.CPUs <= 0 {
		m.cfg.CPUs = 2
	}
	if m.cfg.Pids <= 0 {
		m.cfg.Pids = 512
	}
	if m.cfg.IdleStop <= 0 {
		m.cfg.IdleStop = 15 * time.Minute
	}
	if m.cfg.Remove <= 0 {
		m.cfg.Remove = 7 * 24 * time.Hour
	}
	m.runtime = m.pickRuntime(pctx)
	log.Info("sandbox manager", "image", cfg.Image, "network", cfg.Network, "runtime", m.runtime, "proxy", cfg.ProxyURL)
	return m, nil
}

// Runtime reports the container runtime in use (runc or runsc).
func (m *Manager) Runtime() string { return m.runtime }

func (m *Manager) pickRuntime(ctx context.Context) string {
	switch m.cfg.Runtime {
	case "runc", "runsc":
		return m.cfg.Runtime
	}
	info, err := m.cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return "runc"
	}
	if _, ok := info.Info.Runtimes["runsc"]; ok {
		return "runsc"
	}
	return "runc"
}

func (m *Manager) lock(key string) func() {
	m.mu.Lock()
	l, ok := m.locks[key]
	if !ok {
		l = &sync.Mutex{}
		m.locks[key] = l
	}
	m.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func fromRow(r store.Sandbox) *Sandbox {
	sb := &Sandbox{ID: r.ID, ProjectID: r.ProjectID, UserID: r.UserID, Volume: r.VolumeName, Runtime: r.Runtime}
	if r.ContainerID != nil {
		sb.ContainerID = *r.ContainerID
	}
	return sb
}

func (m *Manager) event(ctx context.Context, id uuid.UUID, kind string, detail any) {
	b, _ := json.Marshal(detail)
	if b == nil {
		b = []byte("{}")
	}
	_ = m.db.InsertSandboxEvent(context.WithoutCancel(ctx), store.InsertSandboxEventParams{SandboxID: id, Kind: kind, Detail: b})
}

// Ensure returns a running sandbox for the project and user, creating the
// volume and container on first use and restarting a stopped one. On first
// creation, if the project has a repo_url, it is cloned into /workspace.
func (m *Manager) Ensure(ctx context.Context, projectID, userID uuid.UUID) (*Sandbox, error) {
	unlock := m.lock(projectID.String() + "/" + userID.String())
	defer unlock()

	row, err := m.db.GetSandbox(ctx, store.GetSandboxParams{ProjectID: projectID, UserID: userID})
	fresh := false
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		id := uuid.New()
		vol := "ws-sb-" + strings.ReplaceAll(id.String(), "-", "")[:16]
		row, err = m.db.InsertSandbox(ctx, store.InsertSandboxParams{ProjectID: projectID, UserID: userID, VolumeName: vol, Image: m.cfg.Image, Runtime: m.runtime})
		if err != nil {
			return nil, err
		}
		fresh = true
		m.event(ctx, row.ID, "created", map[string]any{"volume": vol, "image": m.cfg.Image})
	}
	sb := fromRow(row)

	if sb.ContainerID != "" {
		insp, err := m.cli.ContainerInspect(ctx, sb.ContainerID, client.ContainerInspectOptions{})
		switch {
		case err == nil && insp.Container.State != nil && insp.Container.State.Running:
			_ = m.db.TouchSandbox(ctx, sb.ID)
			return sb, nil
		case err == nil:
			if _, err := m.cli.ContainerStart(ctx, sb.ContainerID, client.ContainerStartOptions{}); err == nil {
				_ = m.db.SetSandboxContainer(ctx, store.SetSandboxContainerParams{ID: sb.ID, ContainerID: &sb.ContainerID, Status: "running"})
				m.event(ctx, sb.ID, "started", nil)
				return sb, nil
			}
			// Start failed (image gone, config drift): recreate below.
			_, _ = m.cli.ContainerRemove(ctx, sb.ContainerID, client.ContainerRemoveOptions{Force: true})
		case !cerrdefs.IsNotFound(err):
			return nil, fmt.Errorf("sandbox: inspect: %w", err)
		}
		sb.ContainerID = ""
	}

	if err := m.ensureImage(ctx); err != nil {
		_ = m.db.SetSandboxStatus(ctx, store.SetSandboxStatusParams{ID: sb.ID, Status: "failed", Error: ptr(err.Error())})
		return nil, err
	}
	if _, err := m.cli.VolumeCreate(ctx, client.VolumeCreateOptions{Name: sb.Volume, Labels: m.labels(sb)}); err != nil {
		return nil, fmt.Errorf("sandbox: volume: %w", err)
	}
	cid, err := m.create(ctx, sb)
	if err != nil {
		_ = m.db.SetSandboxStatus(ctx, store.SetSandboxStatusParams{ID: sb.ID, Status: "failed", Error: ptr(err.Error())})
		return nil, err
	}
	sb.ContainerID = cid
	if err := m.db.SetSandboxContainer(ctx, store.SetSandboxContainerParams{ID: sb.ID, ContainerID: &cid, Status: "running"}); err != nil {
		return nil, err
	}
	m.event(ctx, sb.ID, "started", map[string]any{"container": cid[:12], "runtime": m.runtime})

	if fresh {
		if proj, err := m.db.GetProject(ctx, projectID); err == nil && proj.RepoUrl != nil && *proj.RepoUrl != "" {
			m.clone(ctx, sb, *proj.RepoUrl, deref(proj.DefaultBranch))
		}
	}
	return sb, nil
}

func (m *Manager) labels(sb *Sandbox) map[string]string {
	l := map[string]string{"ws.sandbox": sb.ID.String(), "ws.project": sb.ProjectID.String(), "ws.user": sb.UserID.String()}
	for k, v := range m.cfg.Labels {
		l[k] = v
	}
	return l
}

func (m *Manager) ensureImage(ctx context.Context) error {
	if _, err := m.cli.ImageInspect(ctx, m.cfg.Image); err == nil {
		return nil
	}
	m.log.Info("sandbox: pulling image", "image", m.cfg.Image)
	pctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	resp, err := m.cli.ImagePull(pctx, m.cfg.Image, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("sandbox: pull %s: %w", m.cfg.Image, err)
	}
	defer resp.Close()
	if err := resp.Wait(pctx); err != nil {
		return fmt.Errorf("sandbox: pull %s: %w", m.cfg.Image, err)
	}
	return nil
}

func (m *Manager) create(ctx context.Context, sb *Sandbox) (string, error) {
	cfg, hc, nc := m.containerSpec([]mount.Mount{{Type: mount.TypeVolume, Source: sb.Volume, Target: Workspace}})
	cfg.Labels = m.labels(sb)
	res, err := m.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:             "ws-sb-" + strings.ReplaceAll(sb.ID.String(), "-", "")[:12],
		Config:           cfg,
		HostConfig:       hc,
		NetworkingConfig: nc,
	})
	if err != nil {
		return "", fmt.Errorf("sandbox: create: %w", err)
	}
	if _, err := m.cli.ContainerStart(ctx, res.ID, client.ContainerStartOptions{}); err != nil {
		_, _ = m.cli.ContainerRemove(ctx, res.ID, client.ContainerRemoveOptions{Force: true})
		return "", fmt.Errorf("sandbox: start: %w", err)
	}
	return res.ID, nil
}

func (m *Manager) clone(ctx context.Context, sb *Sandbox, repo, branch string) {
	u, err := url.Parse(repo)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		m.event(ctx, sb.ID, "error", map[string]any{"clone": repo, "error": "unsupported repo url"})
		return
	}
	args := []string{"git", "clone", "--depth", "50"}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	args = append(args, repo, ".")
	res, err := m.Exec(ctx, sb, ExecOptions{Cmd: args, Timeout: 5 * time.Minute, MaxOutput: 64 << 10})
	detail := map[string]any{"repo": repo, "branch": branch}
	if err != nil {
		detail["error"] = err.Error()
	} else {
		detail["exit_code"] = res.ExitCode
		if res.ExitCode != 0 {
			detail["stderr"] = res.Stderr
		}
	}
	m.event(ctx, sb.ID, "clone", detail)
}

// ExecOptions configures one command run inside the sandbox.
type ExecOptions struct {
	Cmd       []string // argv; use Shell for a shell string
	Shell     string   // run via bash -lc
	Dir       string   // working directory (default /workspace)
	Env       []string
	Stdin     []byte
	Timeout   time.Duration // enforced in-container with timeout(1); default 2 min
	MaxOutput int           // per-stream byte cap; default 1 MiB
}

// ExecResult is what a command produced.
type ExecResult struct {
	Stdout    string
	Stderr    string
	ExitCode  int
	Truncated bool
	TimedOut  bool
	Duration  time.Duration
}

// Exec runs a command inside the sandbox and waits for it.
func (m *Manager) Exec(ctx context.Context, sb *Sandbox, o ExecOptions) (ExecResult, error) {
	if sb.ContainerID == "" {
		return ExecResult{}, errors.New("sandbox: no container")
	}
	if o.Timeout <= 0 {
		o.Timeout = 2 * time.Minute
	}
	if o.MaxOutput <= 0 {
		o.MaxOutput = 1 << 20
	}
	if o.Dir == "" {
		o.Dir = Workspace
	}
	secs := fmt.Sprintf("%d", int(o.Timeout.Seconds())+1)
	var argv []string
	switch {
	case o.Shell != "":
		if m.cfg.Jail {
			argv = append([]string{"timeout", "-s", "KILL", secs}, jailArgv(jailBinds, o.Dir, o.Shell)...)
		} else {
			argv = []string{"timeout", "-s", "KILL", secs, "bash", "-c", o.Shell}
		}
	case len(o.Cmd) > 0:
		argv = append([]string{"timeout", "-s", "KILL", secs}, o.Cmd...)
	default:
		return ExecResult{}, errors.New("sandbox: empty command")
	}
	start := time.Now()
	ex, err := m.cli.ExecCreate(ctx, sb.ContainerID, client.ExecCreateOptions{
		Cmd: argv, WorkingDir: o.Dir, Env: o.Env, User: "dev",
		AttachStdout: true, AttachStderr: true, AttachStdin: len(o.Stdin) > 0,
	})
	if err != nil {
		return ExecResult{}, fmt.Errorf("sandbox: exec create: %w", err)
	}
	att, err := m.cli.ExecAttach(ctx, ex.ID, client.ExecAttachOptions{})
	if err != nil {
		return ExecResult{}, fmt.Errorf("sandbox: exec attach: %w", err)
	}
	defer att.Close()
	if len(o.Stdin) > 0 {
		_, _ = att.Conn.Write(o.Stdin)
		_ = att.CloseWrite()
	}
	out := &capWriter{max: o.MaxOutput}
	errw := &capWriter{max: o.MaxOutput}
	done := make(chan error, 1)
	go func() {
		_, err := stdcopy.StdCopy(out, errw, att.Reader)
		done <- err
	}()
	// Outer guard: the in-container timeout should fire first.
	guard := time.NewTimer(o.Timeout + 15*time.Second)
	defer guard.Stop()
	select {
	case <-done:
	case <-guard.C:
		att.Close()
		<-done
	case <-ctx.Done():
		att.Close()
		<-done
		return ExecResult{}, ctx.Err()
	}
	res := ExecResult{Stdout: out.String(), Stderr: errw.String(), Truncated: out.truncated || errw.truncated, Duration: time.Since(start)}
	if insp, err := m.cli.ExecInspect(ctx, ex.ID, client.ExecInspectOptions{}); err == nil {
		res.ExitCode = insp.ExitCode
	}
	if res.ExitCode == 137 || res.ExitCode == 124 {
		res.TimedOut = true
	}
	_ = m.db.TouchSandbox(context.WithoutCancel(ctx), sb.ID)
	return res, nil
}

// ReadFile returns a file from the sandbox (via the archive API, so no
// shell quoting). Size is capped at maxBytes (0 = 4 MiB).
func (m *Manager) ReadFile(ctx context.Context, sb *Sandbox, p string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		maxBytes = 4 << 20
	}
	res, err := m.cli.CopyFromContainer(ctx, sb.ContainerID, client.CopyFromContainerOptions{SourcePath: p})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return nil, fmt.Errorf("no such file: %s", p)
		}
		return nil, err
	}
	defer res.Content.Close()
	if res.Stat.Mode.IsDir() {
		return nil, fmt.Errorf("%s is a directory", p)
	}
	if res.Stat.Size > maxBytes {
		return nil, fmt.Errorf("%s is %d bytes; limit is %d", p, res.Stat.Size, maxBytes)
	}
	tr := tar.NewReader(res.Content)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("no such file: %s", p)
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg || h.Typeflag == tar.TypeRegA {
			return io.ReadAll(io.LimitReader(tr, maxBytes))
		}
	}
}

// WriteFile writes a file into the sandbox, creating parent directories,
// owned by the sandbox user.
func (m *Manager) WriteFile(ctx context.Context, sb *Sandbox, p string, data []byte) error {
	dir, base := path.Split(path.Clean(p))
	if dir == "" {
		dir = Workspace + "/"
	}
	if _, err := m.Exec(ctx, sb, ExecOptions{Cmd: []string{"mkdir", "-p", dir}, Timeout: 10 * time.Second}); err != nil {
		return err
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: base, Mode: 0o644, Size: int64(len(data)), Uid: 1000, Gid: 1000, ModTime: time.Now()}); err != nil {
		return err
	}
	if _, err := tw.Write(data); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if _, err := m.cli.CopyToContainer(ctx, sb.ContainerID, client.CopyToContainerOptions{DestinationPath: dir, Content: &buf}); err != nil {
		return fmt.Errorf("sandbox: write %s: %w", p, err)
	}
	_ = m.db.TouchSandbox(context.WithoutCancel(ctx), sb.ID)
	return nil
}

// Stop stops a sandbox container; the volume stays.
func (m *Manager) Stop(ctx context.Context, sb *Sandbox) error {
	if sb.ContainerID != "" {
		t := 5
		if _, err := m.cli.ContainerStop(ctx, sb.ContainerID, client.ContainerStopOptions{Timeout: &t}); err != nil && !cerrdefs.IsNotFound(err) {
			return err
		}
	}
	m.event(ctx, sb.ID, "stopped", nil)
	return m.db.SetSandboxStatus(ctx, store.SetSandboxStatusParams{ID: sb.ID, Status: "stopped"})
}

// Remove deletes the container (and the volume when wipe is true).
func (m *Manager) Remove(ctx context.Context, sb *Sandbox, wipe bool) error {
	if sb.ContainerID != "" {
		if _, err := m.cli.ContainerRemove(ctx, sb.ContainerID, client.ContainerRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
			return err
		}
	}
	if wipe {
		if _, err := m.cli.VolumeRemove(ctx, sb.Volume, client.VolumeRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
			return err
		}
		m.event(ctx, sb.ID, "removed", map[string]any{"volume": true})
		return m.db.DeleteSandbox(ctx, sb.ID)
	}
	m.event(ctx, sb.ID, "removed", nil)
	return m.db.SetSandboxContainer(ctx, store.SetSandboxContainerParams{ID: sb.ID, ContainerID: nil, Status: "stopped"})
}

// Reap stops idle sandboxes and removes long-stopped containers. Run it
// periodically from the worker.
func (m *Manager) Reap(ctx context.Context) error {
	idle, err := m.db.ListSandboxesRunningIdleSince(ctx, time.Now().Add(-m.cfg.IdleStop))
	if err != nil {
		return err
	}
	for _, r := range idle {
		sb := fromRow(r)
		if err := m.Stop(ctx, sb); err != nil {
			m.log.Warn("sandbox reap: stop", "sandbox", sb.ID, "err", err)
		} else {
			m.log.Info("sandbox reap: stopped idle", "sandbox", sb.ID)
		}
	}
	old, err := m.db.ListSandboxesStoppedSince(ctx, time.Now().Add(-m.cfg.Remove))
	if err != nil {
		return err
	}
	for _, r := range old {
		sb := fromRow(r)
		if err := m.Remove(ctx, sb, false); err != nil {
			m.log.Warn("sandbox reap: remove", "sandbox", sb.ID, "err", err)
		}
	}
	return nil
}

// ResolveIP maps a source IP on the sandbox network to its sandbox, for
// the egress proxy. Cached briefly; unknown IPs are refused by the caller.
func (m *Manager) ResolveIP(ctx context.Context, ip string) (*Sandbox, bool) {
	m.ipMu.Lock()
	if e, ok := m.ipCache[ip]; ok && time.Since(e.at) < 30*time.Second {
		m.ipMu.Unlock()
		return e.sb, e.sb != nil
	}
	m.ipMu.Unlock()

	var found *Sandbox
	list, err := m.cli.ContainerList(ctx, client.ContainerListOptions{Filters: client.Filters{}.Add("label", "ws.sandbox")})
	if err == nil {
		for _, c := range list.Items {
			for _, n := range c.NetworkSettings.Networks {
				if n != nil && n.IPAddress.IsValid() && n.IPAddress.String() == ip {
					if id, err := uuid.Parse(c.Labels["ws.sandbox"]); err == nil {
						if row, err := m.db.GetSandboxByID(ctx, id); err == nil {
							found = fromRow(row)
						}
					}
				}
			}
		}
	}
	m.ipMu.Lock()
	m.ipCache[ip] = ipEntry{sb: found, at: time.Now()}
	m.ipMu.Unlock()
	return found, found != nil
}

// capWriter keeps the first max bytes and notes truncation.
type capWriter struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (c *capWriter) Write(p []byte) (int, error) {
	room := c.max - c.buf.Len()
	if room <= 0 {
		c.truncated = true
		return len(p), nil
	}
	if len(p) > room {
		c.buf.Write(p[:room])
		c.truncated = true
		return len(p), nil
	}
	return c.buf.Write(p)
}

func (c *capWriter) String() string {
	s := c.buf.String()
	if c.truncated {
		s += "\n[output truncated]"
	}
	return s
}

func ptr[T any](v T) *T { return &v }

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
