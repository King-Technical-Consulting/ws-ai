package ccjobs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// nasty is the shell-injection prompt from the spec §8: quotes, $(), backticks,
// newlines, a trailing newline, and things that would be disastrous if parsed.
const nasty = "it's \"quoted\" $(echo pwned) `uname` ${HOME}\n\nline two; rm -rf / \\ end 'single' \t tab\n"

type stub struct {
	path string
	out  string
}

// writeStub creates a fake claude that records its last argument (the
// prompt) and then stays alive for `stay`.
func writeStub(t *testing.T, dir, stay string) stub {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	s := stub{path: filepath.Join(dir, "claude"), out: filepath.Join(dir, "argv.txt")}
	script := "#!/bin/sh\nfor a; do last=$a; done\nprintf '%s' \"$last\" > " + ShellQuote(s.out) + "\nprintf '%s\\n' \"$1\" \"$2\" > " + ShellQuote(s.out+".head") + "\n" + stay + "\n"
	if err := os.WriteFile(s.path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return s
}

func testLauncher(t *testing.T, h Host, claude string) *Launcher {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	// One tmux server per test: each test's cleanup kills its server, and
	// a client that reaches a server mid-shutdown gets "server exited
	// unexpectedly" instead of "no server running" (seen on CI).
	name := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, strings.ToLower(t.Name()))
	sock := fmt.Sprintf("wsj-test-%d-%s", os.Getpid(), name)
	l := &Launcher{Host: h, Session: "wsjtest", Claude: claude, TmuxSocket: sock, IdleCommand: "sleep 100000"}
	t.Cleanup(func() {
		_, _ = h.Run(context.Background(), nil, "tmux", "-L", sock, "kill-server")
	})
	return l
}

func TestLaunchListKillLocal(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp) // prompt files land under $HOME/.cache/wsj
	st := writeStub(t, tmp, "sleep 60")
	work := filepath.Join(tmp, "repo")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	l := testLauncher(t, LocalHost{name: "test"}, st.path)
	l.DefaultDir = work
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	h, err := l.Launch(ctx, Job{Prompt: nasty, Model: "opus", Cwd: "~/repo"})
	if err != nil {
		t.Fatal(err)
	}
	if h.Session != "wsjtest" || !strings.HasPrefix(h.Window, "@") || h.ID == "" {
		t.Fatalf("handle = %+v", h)
	}
	realWork, _ := filepath.EvalSymlinks(work)
	if h.Cwd != realWork {
		t.Errorf("cwd = %q want %q (~ expansion on the target)", h.Cwd, realWork)
	}

	// Prompt file: byte-identical, mode 600 in a mode 700 dir.
	pf := filepath.Join(tmp, ".cache", "wsj", "jobs", h.ID, "prompt.txt")
	b, err := os.ReadFile(pf)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != nasty {
		t.Errorf("prompt file differs:\n%q\n%q", string(b), nasty)
	}
	if fi, _ := os.Stat(pf); fi.Mode().Perm() != 0o600 {
		t.Errorf("prompt file mode = %o", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Dir(pf)); fi.Mode().Perm() != 0o700 {
		t.Errorf("job dir mode = %o", fi.Mode().Perm())
	}

	// The stub saw the prompt as one argument (minus $(cat) trailing newlines).
	got := waitFile(t, st.out)
	if got != strings.TrimRight(nasty, "\n") {
		t.Errorf("claude argv differs:\n%q\n%q", got, strings.TrimRight(nasty, "\n"))
	}
	if head := waitFile(t, st.out+".head"); head != "--model\nopus\n" {
		t.Errorf("claude first args = %q", head)
	}

	wins, err := l.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(wins) != 1 {
		t.Fatalf("windows = %+v", wins)
	}
	w := wins[0]
	if w.JobID != h.ID || w.Window != h.Window || w.Lane != LaneSubscription || w.Model != "opus" || w.Cwd != realWork || w.Dead || w.Started.IsZero() {
		t.Errorf("window = %+v", w)
	}
	if time.Since(w.Started) > time.Minute {
		t.Errorf("started = %v", w.Started)
	}
	argv := l.AttachArgv(w, true)
	if strings.Join(argv, " ") != "tmux -L "+l.TmuxSocket+" attach-session -r -t =wsjtest:"+h.Window {
		t.Errorf("attach argv = %q", argv)
	}
	if _, err := l.Find(ctx, h.Window); err != nil {
		t.Errorf("find by window id: %v", err)
	}

	if err := l.Kill(ctx, h.ID); err != nil {
		t.Fatal(err)
	}
	if wins, _ := l.List(ctx); len(wins) != 0 {
		t.Errorf("after kill: %+v", wins)
	}
	if _, err := os.Stat(filepath.Dir(pf)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("job dir still present after kill: %v", err)
	}
	if err := l.Kill(ctx, h.ID); err == nil {
		t.Errorf("second kill must fail")
	}
}

func TestRemainOnExit(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	st := writeStub(t, tmp, "sleep 1")
	l := testLauncher(t, LocalHost{name: "test"}, st.path)
	l.DefaultDir = tmp
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h, err := l.Launch(ctx, Job{Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		w, err := l.Find(ctx, h.ID)
		if err != nil {
			t.Fatalf("window vanished instead of staying with remain-on-exit: %v", err)
		}
		if w.Dead {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("window never reported dead")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestCheckStartup: a claude that exits at once (what "not logged in" looks
// like from outside) is reported from pane_dead alone, and the window is
// kept so the user can attach and read the real terminal.
func TestCheckStartup(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	dying := writeStub(t, filepath.Join(tmp, "dying"), "exit 1")
	l := testLauncher(t, LocalHost{name: "test"}, dying.path)
	l.DefaultDir = tmp
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	h, err := l.Launch(ctx, Job{Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	err = l.CheckStartup(ctx, h, 5*time.Second)
	var ex *ExitedError
	if !errors.As(err, &ex) || ex.Handle.ID != h.ID {
		t.Fatalf("err = %v, want ExitedError for %s", err, h.ID)
	}
	for _, want := range []string{"exited immediately on test", "attach", "/login"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q lacks %q", err, want)
		}
	}
	if w, err := l.Find(ctx, h.ID); err != nil || !w.Dead {
		t.Errorf("window must remain (dead) after the check: %+v %v", w, err)
	}

	// A claude that stays up passes the check within the grace period.
	alive := writeStub(t, filepath.Join(tmp, "alive"), "sleep 60")
	l.Claude = alive.path
	h2, err := l.Launch(ctx, Job{Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.CheckStartup(ctx, h2, time.Second); err != nil {
		t.Errorf("live claude reported as exited: %v", err)
	}
	if err := l.CheckStartup(ctx, h2, 0); err != nil {
		t.Errorf("grace 0 must be a no-op: %v", err)
	}
}

// TestClean: dead windows are killed with their prompt dirs, orphaned
// prompt dirs go, live jobs and anything not shaped like a job id stay.
func TestClean(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	alive := writeStub(t, filepath.Join(tmp, "alive"), "sleep 60")
	dying := writeStub(t, filepath.Join(tmp, "dying"), "exit 0")
	l := testLauncher(t, LocalHost{name: "test"}, alive.path)
	l.DefaultDir = tmp
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Fresh target: no session, no jobs dir, nothing to do, no error.
	rep, err := l.Clean(ctx)
	if err != nil || len(rep.Killed) != 0 || len(rep.Orphans) != 0 || rep.Target != "test" {
		t.Fatalf("clean on fresh target: %+v %v", rep, err)
	}

	live, err := l.Launch(ctx, Job{Prompt: "stay"})
	if err != nil {
		t.Fatal(err)
	}
	l.Claude = dying.path
	dead, err := l.Launch(ctx, Job{Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	jobs := filepath.Join(tmp, ".cache", "wsj", "jobs")
	orphan := filepath.Join(jobs, "orphan7890") // old, no window: removed
	fresh := filepath.Join(jobs, "fresh67890")  // seconds old: a launch in progress, kept
	keep := filepath.Join(jobs, "Not-A-Job!")   // not a job id: never touched
	for _, d := range []string{orphan, fresh, keep} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-5 * time.Minute)
	for _, d := range []string{orphan, keep} {
		if err := os.Chtimes(d, old, old); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		w, err := l.Find(ctx, dead.ID)
		if err != nil {
			t.Fatal(err)
		}
		if w.Dead {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stub never exited")
		}
		time.Sleep(100 * time.Millisecond)
	}

	rep, err = l.Clean(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Killed) != 1 || rep.Killed[0].JobID != dead.ID {
		t.Errorf("killed = %+v", rep.Killed)
	}
	if len(rep.Orphans) != 1 || rep.Orphans[0] != "orphan7890" {
		t.Errorf("orphans = %+v", rep.Orphans)
	}
	wins, _ := l.List(ctx)
	if len(wins) != 1 || wins[0].JobID != live.ID || wins[0].Dead {
		t.Errorf("after clean windows = %+v", wins)
	}
	for _, p := range []string{filepath.Join(jobs, live.ID, "prompt.txt"), fresh, keep} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s must survive clean: %v", p, err)
		}
	}
	for _, p := range []string{filepath.Join(jobs, dead.ID), orphan} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s must be removed by clean: %v", p, err)
		}
	}
	// Idempotent.
	if rep, err := l.Clean(ctx); err != nil || len(rep.Killed)+len(rep.Orphans) != 0 {
		t.Errorf("second clean: %+v %v", rep, err)
	}
}

// A jobs dir that exists but cannot be entered is an error, not "nothing
// to clean". (A file in its place works for root too, unlike chmod 000.)
func TestCleanReportsBrokenJobsDir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	l := testLauncher(t, LocalHost{name: "test"}, "/bin/true")
	if err := os.MkdirAll(filepath.Join(tmp, ".cache", "wsj"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, ".cache", "wsj", "jobs"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := l.Clean(context.Background())
	if err == nil || !strings.Contains(err.Error(), "list prompt dirs") {
		t.Fatalf("err = %v, report = %+v", err, rep)
	}
}

func TestClaudeMissing(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	l := testLauncher(t, LocalHost{name: "test"}, filepath.Join(tmp, "nope"))
	_, err := l.Launch(context.Background(), Job{Prompt: "hi", Cwd: tmp})
	if !errors.Is(err, ErrClaudeMissing) {
		t.Fatalf("err = %v", err)
	}
	if wins, _ := l.List(context.Background()); len(wins) != 0 {
		t.Errorf("no window must be created when claude is missing: %+v", wins)
	}
}

func TestLaunchRejectsBadInput(t *testing.T) {
	l := &Launcher{Host: LocalHost{name: "t"}, Session: "x", Claude: "/bin/true"}
	for _, j := range []Job{
		{Prompt: ""},
		{Prompt: "p", Lane: LaneAPI},
		{Prompt: "p", Model: "opus; rm -rf /"},
		{Prompt: "p", ID: "../../etc"},
	} {
		if _, err := l.Launch(context.Background(), j); err == nil {
			t.Errorf("job %+v accepted", j)
		}
	}
}

// TestSSH runs the same flow over ssh when WSJ_TEST_SSH names a destination
// with tmux and a stub claude is allowed to be written under its ~/.cache.
func TestSSH(t *testing.T) {
	dest := os.Getenv("WSJ_TEST_SSH")
	if dest == "" {
		t.Skip("set WSJ_TEST_SSH=user@host to run")
	}
	h := NewSSHHost("remote", dest, true)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// Stub claude on the remote, in a temp dir we remove afterwards.
	dir, err := h.Run(ctx, nil, "mktemp", "-d")
	if err != nil {
		t.Fatal(err)
	}
	rdir := strings.TrimSpace(string(dir))
	t.Cleanup(func() { _, _ = h.Run(context.Background(), nil, "rm", "-rf", "--", rdir) })
	stubPath := rdir + "/claude"
	script := "#!/bin/sh\nfor a; do last=$a; done\nprintf '%s' \"$last\" > " + ShellQuote(rdir+"/argv.txt") + "\nsleep 60\n"
	if _, err := h.Run(ctx, strings.NewReader(script), "sh", "-c", `cat > "$1" && chmod 755 "$1"`, "wsj", stubPath); err != nil {
		t.Fatal(err)
	}
	l := testLauncher(t, h, stubPath)
	l.DefaultDir = rdir
	hd, err := l.Launch(ctx, Job{Prompt: nasty})
	if err != nil {
		t.Fatal(err)
	}
	var got string
	for i := 0; i < 50 && got == ""; i++ {
		out, _ := h.Run(ctx, nil, "cat", rdir+"/argv.txt")
		got = string(out)
		if got == "" {
			time.Sleep(200 * time.Millisecond)
		}
	}
	if got != strings.TrimRight(nasty, "\n") {
		t.Errorf("remote claude argv differs:\n%q\n%q", got, nasty)
	}
	pf, err := h.Run(ctx, nil, "sh", "-c", `cat "$HOME/.cache/wsj/jobs/$1/prompt.txt"; stat -c %a "$HOME/.cache/wsj/jobs/$1/prompt.txt" 2>/dev/null || stat -f %Lp "$HOME/.cache/wsj/jobs/$1/prompt.txt"`, "wsj", hd.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(pf), nasty) || !strings.HasSuffix(strings.TrimSpace(string(pf)), "600") {
		t.Errorf("remote prompt file: %q", pf)
	}
	wins, err := l.List(ctx)
	if err != nil || len(wins) != 1 || wins[0].JobID != hd.ID || wins[0].Dead {
		t.Fatalf("list = %+v, %v", wins, err)
	}
	argv := l.AttachArgv(wins[0], false)
	if argv[0] != "ssh" || !strings.Contains(strings.Join(argv, " "), " -t ") {
		t.Errorf("attach argv = %q", argv)
	}
	if err := l.Kill(ctx, hd.ID); err != nil {
		t.Fatal(err)
	}
	if out, _ := h.Run(ctx, nil, "sh", "-c", `test -e "$HOME/.cache/wsj/jobs/$1" && echo present`, "wsj", hd.ID); strings.TrimSpace(string(out)) == "present" {
		t.Errorf("remote job dir not removed")
	}
}

func TestShellQuoteRoundTrip(t *testing.T) {
	argv := []string{"printf", "%s\n", nasty, "", "a b", "it's", `"dq"`, "$(x)", "`y`", "plain-arg_1.2:3", "=wsjtest", "~user", "a=b"}
	out, err := exec.Command("sh", "-c", ShellJoin(argv)).Output()
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join(argv[2:], "\n") + "\n"
	if string(out) != want {
		t.Errorf("round trip:\n%q\n%q", out, want)
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "targets.toml")
	c, err := LoadConfig(p)
	if err != nil || c.Default != "local" || c.Targets["local"].Type != "local" || c.Targets["local"].Session != "subscription" {
		t.Fatalf("missing file default: %+v %v", c, err)
	}
	os.WriteFile(p, []byte(`
default = "homelab"
[targets.local]
type = "local"
default_dir = "~/code"
[targets.homelab]
type = "ssh"
host = "homelab"
session = "subscription"
`), 0o600)
	c, err = LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Default != "homelab" || c.Targets["homelab"].Host != "homelab" || c.Targets["local"].Session != "subscription" {
		t.Errorf("%+v", c)
	}
	l, err := c.Launcher("")
	if err != nil || l.Host.Name() != "homelab" {
		t.Errorf("default launcher: %v %v", l, err)
	}
	if _, err := c.Launcher("nope"); err == nil {
		t.Error("unknown target accepted")
	}
	for _, bad := range []string{
		"[targets.x]\ntype = \"ssh\"\n",
		"[targets.x]\ntype = \"ftp\"\n",
		"[targets.a]\ntype = \"local\"\n[targets.b]\ntype = \"local\"\n",
		"[targets.x]\ntype = \"local\"\nsession = \"has space\"\n",
		"default = \"zzz\"\n[targets.x]\ntype = \"local\"\n",
	} {
		os.WriteFile(p, []byte(bad), 0o600)
		if _, err := LoadConfig(p); err == nil {
			t.Errorf("accepted:\n%s", bad)
		}
	}
}

func waitFile(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, err := os.ReadFile(path)
		if err == nil && len(b) > 0 {
			return string(b)
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never written (claude stub did not start?)", path)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
