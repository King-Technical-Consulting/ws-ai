package ccjobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Host runs commands on a target machine. argv is passed through verbatim
// on local hosts and shell-quoted per argument on ssh hosts, so callers
// never build command strings from untrusted text.
type Host interface {
	Name() string
	// Run executes argv with stdin and returns stdout. A non-zero exit
	// returns an error carrying stderr.
	Run(ctx context.Context, stdin io.Reader, argv ...string) ([]byte, error)
	// InteractiveArgv returns the local argv that runs argv on the host with
	// the caller's terminal attached (for exec).
	InteractiveArgv(argv ...string) []string
}

// LocalHost runs on this machine.
type LocalHost struct{ name string }

func (h LocalHost) Name() string { return h.name }

func (h LocalHost) Run(ctx context.Context, stdin io.Reader, argv ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = stdin
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.Bytes(), &RunError{Host: h.name, Argv: argv, Err: err, Stderr: errb.String()}
	}
	return out.Bytes(), nil
}

func (h LocalHost) InteractiveArgv(argv ...string) []string { return argv }

// SSHHost runs over ssh with BatchMode (keys only, never a password
// prompt) and a persistent control connection so repeat calls are fast.
type SSHHost struct {
	name string
	dest string
	// ControlPath is the ControlMaster socket pattern; empty disables
	// multiplexing (--no-mux), which is useful when a stale control socket
	// is suspected.
	ControlPath string
	// ConfigFile is passed as `ssh -F`: an ssh_config that names the key,
	// user, host name and known_hosts for each target. This is how the ws
	// container reaches targets with a mounted key and no ~/.ssh.
	ConfigFile string
}

// NewSSHHost returns an ssh host with multiplexing under ~/.cache/wsj.
// When mux is false, or the cache dir cannot be determined, every call
// opens its own connection.
func NewSSHHost(name, dest string, mux bool) *SSHHost {
	h := &SSHHost{name: name, dest: dest}
	if !mux {
		return h
	}
	if cache, err := cacheDir(); err == nil {
		h.ControlPath = filepath.Join(cache, "cm-%C")
	}
	return h
}

func (h *SSHHost) Name() string { return h.name }

// ensureControlDir creates the directory that holds control sockets. ssh
// fails with an unhelpful "mux_client" error when it is missing.
func (h *SSHHost) ensureControlDir() error {
	if h.ControlPath == "" {
		return nil
	}
	dir := filepath.Dir(h.ControlPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("%s: ssh control socket dir %s: %w (retry with --no-mux)", h.name, dir, err)
	}
	return nil
}

func (h *SSHHost) baseArgs(tty bool) []string {
	args := []string{"ssh"}
	if h.ConfigFile != "" {
		args = append(args, "-F", h.ConfigFile)
	}
	args = append(args, "-o", "BatchMode=yes", "-o", "ConnectTimeout=10")
	if h.ControlPath != "" {
		args = append(args, "-o", "ControlMaster=auto", "-o", "ControlPath="+h.ControlPath, "-o", "ControlPersist=10m")
	}
	if tty {
		args = append(args, "-t")
	} else {
		args = append(args, "-T")
	}
	return append(args, "--", h.dest)
}

func (h *SSHHost) Run(ctx context.Context, stdin io.Reader, argv ...string) ([]byte, error) {
	if err := h.ensureControlDir(); err != nil {
		return nil, err
	}
	args := append(h.baseArgs(false), ShellJoin(argv))
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdin = stdin
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.Bytes(), h.wrap(argv, err, errb.String())
	}
	return out.Bytes(), nil
}

// wrap turns an ssh failure into a RunError. Exit 255 is ssh itself (no
// route, refused key, unknown host, dead control socket) rather than the
// remote command, so the message says what to check instead of blaming tmux.
func (h *SSHHost) wrap(argv []string, err error, stderr string) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 255 {
		err = &SSHError{Dest: h.dest, ControlPath: h.ControlPath, Err: err}
	}
	return &RunError{Host: h.name, Argv: argv, Err: err, Stderr: stderr}
}

func (h *SSHHost) InteractiveArgv(argv ...string) []string {
	_ = h.ensureControlDir()
	return append(h.baseArgs(true), ShellJoin(argv))
}

// SSHError is an ssh connection or authentication failure (exit 255).
type SSHError struct {
	Dest        string
	ControlPath string
	Err         error
}

func (e *SSHError) Error() string {
	s := fmt.Sprintf("ssh to %s failed (exit 255: connection, host key or key auth, not the remote command); check that `ssh -o BatchMode=yes %s true` works without a prompt", e.Dest, e.Dest)
	if e.ControlPath != "" {
		s += fmt.Sprintf("; if a stale multiplexed connection is suspected, retry with --no-mux or remove %s", e.ControlPath)
	}
	return s
}

func (e *SSHError) Unwrap() error { return e.Err }

// RunError is a failed remote command.
type RunError struct {
	Host   string
	Argv   []string
	Err    error
	Stderr string
}

func (e *RunError) Error() string {
	s := fmt.Sprintf("%s: %s: %v", e.Host, e.Argv[0], e.Err)
	if t := strings.TrimSpace(e.Stderr); t != "" {
		s += ": " + t
	}
	return s
}

func (e *RunError) Unwrap() error { return e.Err }

// ShellQuote single-quotes s so it is one literal word in sh, bash and zsh
// (zsh expands a leading = or ~, so those are never left bare).
func ShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("@%+:,./-_", r)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ShellJoin quotes every argument and joins them with spaces.
func ShellJoin(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = ShellQuote(a)
	}
	return strings.Join(parts, " ")
}

func cacheDir() (string, error) {
	if x := os.Getenv("XDG_CACHE_HOME"); x != "" {
		return filepath.Join(x, "wsj"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cache", "wsj"), nil
}
