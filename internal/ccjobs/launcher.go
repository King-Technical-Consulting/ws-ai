package ccjobs

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Lane names from docs/CLAUDE_CODE_JOBS.md §4.
const (
	LaneSubscription = "claude-subscription"
	LaneAPI          = "api"
	LaneOpenRouter   = "openrouter"
	LaneLocal        = "local"
)

// Job is the single job spec every lane consumes (§5).
type Job struct {
	ID      string    `json:"id"`
	Lane    string    `json:"lane"`
	Target  string    `json:"target"`
	Cwd     string    `json:"cwd"`
	Model   string    `json:"model,omitempty"`
	Prompt  string    `json:"prompt"`
	Created time.Time `json:"created"`
}

// Handle is what a launch returns: where the session lives, never output.
type Handle struct {
	ID      string `json:"id"`
	Target  string `json:"target"`
	Session string `json:"session"`
	// Window is the tmux window id (e.g. "@7"), stable for the window's life.
	Window string `json:"window"`
	Cwd    string `json:"cwd"`
}

// Window is a job window as read back from tmux metadata.
type Window struct {
	Target  string    `json:"target"`
	Session string    `json:"session"`
	Window  string    `json:"window"`
	Name    string    `json:"name"`
	JobID   string    `json:"job_id"`
	Lane    string    `json:"lane"`
	Cwd     string    `json:"cwd"`
	Started time.Time `json:"started"`
	Model   string    `json:"model,omitempty"`
	Dead    bool      `json:"dead"`
}

// Launcher drives tmux on one target.
type Launcher struct {
	Host    Host
	Session string
	// Claude pins the binary path; empty resolves `command -v claude` on the target.
	Claude string
	// TmuxSocket selects a tmux server by name (-L); empty is the default server.
	TmuxSocket string
	// DefaultDir is used when a job has no cwd.
	DefaultDir string
	// IdleCommand runs in the session's first window when the session is
	// created (default: the user's shell). Tests use a sleep.
	IdleCommand string
}

var (
	jobIDRe   = regexp.MustCompile(`^[a-z0-9]{6,32}$`)
	modelRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)
	sessionRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
)

// ErrClaudeMissing is returned when the target has no claude binary.
var ErrClaudeMissing = errors.New("claude not found")

// ExitedError says the job's pane died within the startup grace period,
// which is what a not-logged-in or misconfigured claude looks like from the
// outside. It is derived from tmux's pane_dead flag only; the pane's
// contents are never read, so the user attaches to see why.
type ExitedError struct {
	Handle Handle
	After  time.Duration
}

func (e *ExitedError) Error() string {
	return fmt.Sprintf("claude exited immediately on %s (job %s, within %s); attach to see why, and run `claude` there once to /login", e.Handle.Target, e.Handle.ID, e.After.Round(time.Millisecond))
}

// NewJobID returns a short random lowercase id.
func NewJobID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return strings.ToLower(strings.TrimRight(base32.StdEncoding.EncodeToString(b[:]), "="))[:10]
}

func (l *Launcher) tmux(args ...string) []string {
	argv := []string{"tmux"}
	if l.TmuxSocket != "" {
		argv = append(argv, "-L", l.TmuxSocket)
	}
	return append(argv, args...)
}

// sh runs a fixed POSIX script with positional arguments; data never
// enters the script text.
func (l *Launcher) sh(ctx context.Context, stdin *bytes.Reader, script string, args ...string) (string, error) {
	argv := append([]string{"sh", "-c", script, "wsj"}, args...)
	var in bytes.Reader
	if stdin != nil {
		in = *stdin
	}
	out, err := l.Host.Run(ctx, &in, argv...)
	return strings.TrimRight(string(out), "\n"), err
}

const (
	scriptWritePrompt = `umask 077 && d="$HOME/.cache/wsj/jobs/$1" && mkdir -p "$d" && cat > "$d/prompt.txt" && printf %s "$d/prompt.txt"`
	scriptResolveDir  = `case "$1" in "~") p="$HOME";; "~/"*) p="$HOME/${1#\~/}";; *) p="$1";; esac; cd "$p" 2>/dev/null && pwd -P`
	scriptFindClaude  = `p=$(command -v claude 2>/dev/null) || p=$("${SHELL:-/bin/sh}" -lc 'command -v claude' 2>/dev/null | tail -n 1); case "$p" in /*) printf %s "$p";; esac`
	scriptCheckExec   = `[ -x "$1" ] && printf %s "$1"`
	scriptRemoveJob   = `rm -rf -- "$HOME/.cache/wsj/jobs/$1"`
	// Only dirs older than a minute: Launch writes the prompt dir before
	// the window exists, so a very new dir may be a job that is starting.
	// No jobs dir at all is the normal state of a fresh target and exits 0
	// with no output; any other failure (permissions, a file in the way)
	// exits non-zero so Clean reports it instead of saying "nothing to clean".
	scriptListJobDirs = `d="$HOME/.cache/wsj/jobs"; [ -e "$d" ] || exit 0; cd "$d" && find . -mindepth 1 -maxdepth 1 -type d -mmin +1`
)

// ResolveDir expands ~ on the target and verifies the directory exists.
func (l *Launcher) ResolveDir(ctx context.Context, dir string) (string, error) {
	if dir == "" {
		dir = l.DefaultDir
	}
	if dir == "" {
		dir = "~"
	}
	out, err := l.sh(ctx, nil, scriptResolveDir, dir)
	if err != nil || out == "" {
		return "", fmt.Errorf("%s: directory %q not found", l.Host.Name(), dir)
	}
	return out, nil
}

// ResolveClaude finds the claude binary on the target. It never installs,
// updates or logs anything in; a missing binary is reported to the user.
func (l *Launcher) ResolveClaude(ctx context.Context) (string, error) {
	if l.Claude != "" {
		out, err := l.sh(ctx, nil, scriptCheckExec, l.Claude)
		if err != nil || out == "" {
			return "", fmt.Errorf("%s: %w at configured path %s", l.Host.Name(), ErrClaudeMissing, l.Claude)
		}
		return out, nil
	}
	out, err := l.sh(ctx, nil, scriptFindClaude)
	if err != nil || out == "" {
		return "", fmt.Errorf("%s: %w on PATH; install Claude Code there and run `claude` once to /login, or set claude = \"/path/to/claude\" in targets.toml", l.Host.Name(), ErrClaudeMissing)
	}
	return out, nil
}

// EnsureSession creates the tmux session if it does not exist.
func (l *Launcher) EnsureSession(ctx context.Context) error {
	if _, err := l.Host.Run(ctx, nil, l.tmux("has-session", "-t", "="+l.Session)...); err == nil {
		return nil
	}
	args := []string{"new-session", "-d", "-s", l.Session, "-n", "idle"}
	if l.IdleCommand != "" {
		args = append(args, "--", l.IdleCommand)
	}
	_, err := l.Host.Run(ctx, nil, l.tmux(args...)...)
	return err
}

// Launch writes the prompt file and starts the official claude in a new
// tmux window. The prompt only ever travels over stdin into a mode-600 file
// and reaches claude as "$(cat file)", so no shell ever parses it.
func (l *Launcher) Launch(ctx context.Context, job Job) (Handle, error) {
	if job.ID == "" {
		job.ID = NewJobID()
	}
	if !jobIDRe.MatchString(job.ID) {
		return Handle{}, fmt.Errorf("bad job id %q", job.ID)
	}
	if job.Lane == "" {
		job.Lane = LaneSubscription
	}
	if job.Lane != LaneSubscription {
		return Handle{}, fmt.Errorf("lane %q is not launched in tmux; it runs on the ws router", job.Lane)
	}
	if job.Model != "" && !modelRe.MatchString(job.Model) {
		return Handle{}, fmt.Errorf("bad model %q", job.Model)
	}
	if strings.TrimSpace(job.Prompt) == "" {
		return Handle{}, errors.New("empty prompt")
	}
	if job.Created.IsZero() {
		job.Created = time.Now().UTC()
	}

	claude, err := l.ResolveClaude(ctx)
	if err != nil {
		return Handle{}, err
	}
	cwd, err := l.ResolveDir(ctx, job.Cwd)
	if err != nil {
		return Handle{}, err
	}
	promptPath, err := l.sh(ctx, bytes.NewReader([]byte(job.Prompt)), scriptWritePrompt, job.ID)
	if err != nil || promptPath == "" {
		return Handle{}, fmt.Errorf("write prompt file: %w", err)
	}
	if err := l.EnsureSession(ctx); err != nil {
		return Handle{}, fmt.Errorf("tmux session: %w", err)
	}

	// The window command is assembled from shell-quoted paths we generated;
	// $(cat …) output is not re-parsed, so the prompt text is inert.
	cmd := "exec " + ShellQuote(claude)
	if job.Model != "" {
		cmd += " --model " + ShellQuote(job.Model)
	}
	cmd += ` "$(cat ` + ShellQuote(promptPath) + `)"`
	// new-window and the option sets go to the server as one command list,
	// which it runs without returning to its event loop in between. That
	// matters for remain-on-exit: a claude that exits at once (not logged
	// in, bad flag) would otherwise close the window before the option
	// landed, and the user would have nothing to attach to. The options
	// target the window by exact name since the id is only printed back.
	name := "job-" + job.ID
	opts := [][2]string{
		{"remain-on-exit", "on"},
		{"@job_id", job.ID},
		{"@lane", job.Lane},
		{"@cwd", cwd},
		{"@started", job.Created.UTC().Format(time.RFC3339)},
		{"@model", job.Model},
	}
	args := []string{"new-window", "-d", "-P", "-F", "#{window_id}", "-t", "=" + l.Session + ":", "-n", name, "-c", cwd, "--", cmd}
	for _, o := range opts {
		args = append(args, ";", "set-option", "-w", "-t", "="+l.Session+":="+name, o[0], o[1])
	}
	out, err := l.Host.Run(ctx, nil, l.tmux(args...)...)
	win := strings.TrimSpace(string(out))
	if err != nil {
		// tmux keeps going after a failed command in a list, so new-window
		// may have succeeded (its id is on stdout) while a set-option did
		// not. Remove the half-set window rather than leak it without a
		// handle; a window with no metadata is not a job.
		if strings.HasPrefix(win, "@") {
			_ = l.killWindow(ctx, Window{Window: win, JobID: job.ID})
		}
		return Handle{}, fmt.Errorf("tmux new-window: %w", err)
	}
	if !strings.HasPrefix(win, "@") {
		return Handle{}, fmt.Errorf("tmux new-window: unexpected window id %q", win)
	}
	return Handle{ID: job.ID, Target: l.Host.Name(), Session: l.Session, Window: win, Cwd: cwd}, nil
}

// tmux rewrites control characters in format output as "_", so fields are
// separated by a printable token that cannot appear in ids or sane paths.
const listSep = "|~|"

var listFormat = strings.Join([]string{"#{window_id}", "#{window_name}", "#{@job_id}", "#{@lane}", "#{@cwd}", "#{@started}", "#{@model}", "#{pane_dead}"}, listSep)

// List returns job windows in the session; a missing session is empty.
func (l *Launcher) List(ctx context.Context) ([]Window, error) {
	out, err := l.Host.Run(ctx, nil, l.tmux("list-windows", "-t", "="+l.Session, "-F", listFormat)...)
	if err != nil {
		var re *RunError
		if errors.As(err, &re) && (strings.Contains(re.Stderr, "can't find session") || strings.Contains(re.Stderr, "no server running") || strings.Contains(re.Stderr, "No such file or directory")) {
			return nil, nil
		}
		return nil, err
	}
	var wins []Window
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		f := strings.Split(line, listSep)
		if len(f) < 8 || !strings.HasPrefix(f[1], "job-") {
			continue
		}
		w := Window{Target: l.Host.Name(), Session: l.Session, Window: f[0], Name: f[1], JobID: f[2], Lane: f[3], Cwd: f[4], Model: f[6], Dead: f[7] == "1"}
		if w.JobID == "" {
			w.JobID = strings.TrimPrefix(f[1], "job-")
		}
		w.Started, _ = time.Parse(time.RFC3339, f[5])
		wins = append(wins, w)
	}
	return wins, nil
}

// Find locates a job by id or by tmux window id.
func (l *Launcher) Find(ctx context.Context, job string) (Window, error) {
	wins, err := l.List(ctx)
	if err != nil {
		return Window{}, err
	}
	for _, w := range wins {
		if w.JobID == job || w.Window == job {
			return w, nil
		}
	}
	return Window{}, fmt.Errorf("%s: no job %q in session %s", l.Host.Name(), job, l.Session)
}

// AttachArgv is the local command that attaches the caller's terminal to
// the job window. readOnly adds -r for the web bridge.
func (l *Launcher) AttachArgv(w Window, readOnly bool) []string {
	args := []string{"attach-session"}
	if readOnly {
		args = append(args, "-r")
	}
	args = append(args, "-t", "="+l.Session+":"+w.Window)
	return l.Host.InteractiveArgv(l.tmux(args...)...)
}

// Kill removes the window and the job's prompt directory.
func (l *Launcher) Kill(ctx context.Context, job string) error {
	w, err := l.Find(ctx, job)
	if err != nil {
		return err
	}
	return l.killWindow(ctx, w)
}

func (l *Launcher) killWindow(ctx context.Context, w Window) error {
	if _, err := l.Host.Run(ctx, nil, l.tmux("kill-window", "-t", w.Window)...); err != nil {
		return err
	}
	if jobIDRe.MatchString(w.JobID) {
		if _, err := l.sh(ctx, nil, scriptRemoveJob, w.JobID); err != nil {
			return fmt.Errorf("window killed but prompt dir not removed: %w", err)
		}
	}
	return nil
}

// paneDead reports tmux's pane_dead flag for the job window. It is the only
// liveness signal used anywhere: no pane contents are ever read.
func (l *Launcher) paneDead(ctx context.Context, win string) (bool, error) {
	out, err := l.Host.Run(ctx, nil, l.tmux("display-message", "-p", "-t", win, "#{pane_dead}")...)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "1", nil
}

// CheckStartup watches the new window for grace and returns an *ExitedError
// if its pane dies in that time. A claude that is not logged in, or that
// rejects its flags, exits within a second or two; one that is working stays
// up. The window is left alone either way (remain-on-exit keeps it) so the
// user can attach and read the real terminal.
func (l *Launcher) CheckStartup(ctx context.Context, h Handle, grace time.Duration) error {
	if grace <= 0 {
		return nil
	}
	start := time.Now()
	deadline := start.Add(grace)
	for {
		dead, err := l.paneDead(ctx, h.Window)
		if err != nil {
			return fmt.Errorf("startup check: %w", err)
		}
		if dead {
			return &ExitedError{Handle: h, After: time.Since(start)}
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil
		}
		wait := 250 * time.Millisecond
		if remaining < wait {
			wait = remaining
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// CleanReport lists what Clean removed on one target.
type CleanReport struct {
	Target string `json:"target"`
	// Killed are the dead job windows that were closed (and their prompt dirs removed).
	Killed []Window `json:"killed"`
	// Orphans are prompt-dir job ids under ~/.cache/wsj/jobs that had no
	// window at all, alive or dead, and were removed.
	Orphans []string `json:"orphans"`
}

// Clean kills dead job windows (their claude has exited; remain-on-exit kept
// them for inspection) and removes prompt dirs that no window refers to.
// Live jobs and their files are never touched. Errors on individual items
// are collected; the report still says what succeeded.
func (l *Launcher) Clean(ctx context.Context) (CleanReport, error) {
	rep := CleanReport{Target: l.Host.Name(), Killed: []Window{}, Orphans: []string{}}
	wins, err := l.List(ctx)
	if err != nil {
		return rep, err
	}
	var errs []error
	known := map[string]bool{}
	for _, w := range wins {
		if !w.Dead {
			known[w.JobID] = true
			continue
		}
		if err := l.killWindow(ctx, w); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", w.JobID, err))
			known[w.JobID] = true
			continue
		}
		rep.Killed = append(rep.Killed, w)
	}
	out, err := l.sh(ctx, nil, scriptListJobDirs)
	if err != nil {
		errs = append(errs, fmt.Errorf("list prompt dirs: %w", err))
		return rep, errors.Join(errs...)
	}
	for _, id := range strings.Split(out, "\n") {
		id = strings.TrimPrefix(strings.TrimSpace(id), "./")
		if id == "" || known[id] || !jobIDRe.MatchString(id) {
			continue
		}
		if _, err := l.sh(ctx, nil, scriptRemoveJob, id); err != nil {
			errs = append(errs, fmt.Errorf("remove prompt dir %s: %w", id, err))
			continue
		}
		rep.Orphans = append(rep.Orphans, id)
	}
	return rep, errors.Join(errs...)
}
