package ccjobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubSSH puts a fake `ssh` first on PATH that records its argv and exits
// with the code in STUB_SSH_EXIT, so the ssh host's argument building and
// error reporting are testable without a network.
func stubSSH(t *testing.T) (argvFile string) {
	t.Helper()
	dir := t.TempDir()
	argvFile = filepath.Join(dir, "argv.txt")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + ShellQuote(argvFile) + "\necho stub-stderr >&2\nexit \"${STUB_SSH_EXIT:-0}\"\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argvFile
}

func TestSSHHostMuxAndErrors(t *testing.T) {
	argvFile := stubSSH(t)
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	ctx := context.Background()

	h := NewSSHHost("box", "user@box", true)
	if h.ControlPath != filepath.Join(cache, "wsj", "cm-%C") {
		t.Fatalf("control path = %q", h.ControlPath)
	}
	// The control socket dir may not exist yet (first run, cleared cache).
	if _, err := os.Stat(filepath.Join(cache, "wsj")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("precondition: %v", err)
	}
	t.Setenv("STUB_SSH_EXIT", "0")
	if _, err := h.Run(ctx, nil, "tmux", "ls"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(cache, "wsj")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("control dir not created 0700: %v", err)
	}
	argv, _ := os.ReadFile(argvFile)
	got := string(argv)
	for _, want := range []string{"BatchMode=yes", "ConnectTimeout=10", "ControlMaster=auto", "ControlPath=" + h.ControlPath, "ControlPersist=10m", "-T\n--\nuser@box\ntmux ls\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("ssh argv lacks %q:\n%s", want, got)
		}
	}

	// Exit 255 is ssh's own failure: the message says what to check and
	// offers --no-mux because a stale control socket is one cause.
	t.Setenv("STUB_SSH_EXIT", "255")
	_, err := h.Run(ctx, nil, "tmux", "ls")
	var se *SSHError
	if !errors.As(err, &se) || se.Dest != "user@box" {
		t.Fatalf("err = %v, want SSHError", err)
	}
	for _, want := range []string{"exit 255", "ssh -o BatchMode=yes user@box true", "--no-mux", h.ControlPath, "stub-stderr"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message lacks %q: %v", want, err)
		}
	}
	// Any other code is the remote command's, reported as before.
	t.Setenv("STUB_SSH_EXIT", "1")
	_, err = h.Run(ctx, nil, "tmux", "ls")
	var re *RunError
	if errors.As(err, &se) || !errors.As(err, &re) || re.Stderr != "stub-stderr\n" {
		t.Errorf("exit 1: %v", err)
	}

	// --no-mux: no ControlMaster options at all, and the message does not
	// point at a control socket.
	nm := NewSSHHost("box", "user@box", false)
	if nm.ControlPath != "" {
		t.Fatalf("no-mux control path = %q", nm.ControlPath)
	}
	t.Setenv("STUB_SSH_EXIT", "255")
	_, err = nm.Run(ctx, nil, "true")
	if !errors.As(err, &se) || strings.Contains(err.Error(), "--no-mux") {
		t.Errorf("no-mux 255: %v", err)
	}
	argv, _ = os.ReadFile(argvFile)
	if strings.Contains(string(argv), "Control") {
		t.Errorf("no-mux argv still multiplexes:\n%s", argv)
	}
	if ia := strings.Join(nm.InteractiveArgv("tmux", "attach"), " "); !strings.HasSuffix(ia, " -t -- user@box tmux attach") || strings.Contains(ia, "Control") {
		t.Errorf("interactive argv = %q", ia)
	}
}

func TestSSHHostControlDirUnwritable(t *testing.T) {
	stubSSH(t)
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	// A file where the cache dir should be makes MkdirAll fail.
	if err := os.WriteFile(filepath.Join(cache, "wsj"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	h := NewSSHHost("box", "box", true)
	_, err := h.Run(context.Background(), nil, "true")
	if err == nil || !strings.Contains(err.Error(), "control socket dir") || !strings.Contains(err.Error(), "--no-mux") {
		t.Errorf("err = %v", err)
	}
}

// TestSSHConfigFile: an ssh_config from the targets file (or the
// environment) is passed as -F on every ssh call, interactive attach
// included, with ~ expanded; the environment wins over the file.
func TestSSHConfigFile(t *testing.T) {
	argvFile := stubSSH(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("WSJ_SSH_CONFIG", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir()
	p := filepath.Join(dir, "targets.toml")
	if err := os.WriteFile(p, []byte("ssh_config = \"~/cfg\"\n[targets.box]\ntype = \"ssh\"\nhost = \"box\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	l, err := c.Launcher("box")
	if err != nil {
		t.Fatal(err)
	}
	sh := l.Host.(*SSHHost)
	if sh.ConfigFile != filepath.Join(home, "cfg") {
		t.Errorf("config file = %q", sh.ConfigFile)
	}
	t.Setenv("STUB_SSH_EXIT", "0")
	if _, err := l.Host.Run(context.Background(), nil, "tmux", "ls"); err != nil {
		t.Fatal(err)
	}
	argv, _ := os.ReadFile(argvFile)
	if got := string(argv); !strings.HasPrefix(got, "-F\n"+filepath.Join(home, "cfg")+"\n-o\nBatchMode=yes\n") {
		t.Errorf("-F missing or misplaced:\n%s", got)
	}
	if ia := strings.Join(l.Host.InteractiveArgv("tmux", "attach"), " "); !strings.Contains(ia, "-F "+filepath.Join(home, "cfg")+" ") || !strings.Contains(ia, " -t ") {
		t.Errorf("interactive argv = %q", ia)
	}
	// Environment override, as ws sets it from WS_CC_SSH_CONFIG.
	t.Setenv("WSJ_SSH_CONFIG", "/secrets/ssh_config")
	l, _ = c.Launcher("box")
	if l.Host.(*SSHHost).ConfigFile != "/secrets/ssh_config" {
		t.Errorf("env override = %q", l.Host.(*SSHHost).ConfigFile)
	}
	// No config: no -F at all.
	t.Setenv("WSJ_SSH_CONFIG", "")
	c.SSHConfig = ""
	l, _ = c.Launcher("box")
	if l.Host.(*SSHHost).ConfigFile != "" {
		t.Errorf("unexpected config file %q", l.Host.(*SSHHost).ConfigFile)
	}
	_, _ = l.Host.Run(context.Background(), nil, "true")
	argv, _ = os.ReadFile(argvFile)
	if strings.Contains(string(argv), "-F") {
		t.Errorf("-F must not appear without a config: %s", argv)
	}
}
