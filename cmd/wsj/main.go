// Command wsj launches, lists, attaches to and kills Claude Code sessions in
// tmux windows on local or SSH targets (docs/CLAUDE_CODE_JOBS.md). It never
// reads Claude Code's output or credentials; attach hands your terminal to
// the real session.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/jking323/ws/internal/ccjobs"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx := context.Background()
	var err error
	switch os.Args[1] {
	case "run":
		err = cmdRun(ctx, os.Args[2:])
	case "ls":
		err = cmdLs(ctx, os.Args[2:])
	case "attach":
		err = cmdAttach(ctx, os.Args[2:])
	case "kill":
		err = cmdKill(ctx, os.Args[2:])
	case "clean":
		err = cmdClean(ctx, os.Args[2:])
	case "route":
		err = cmdRoute(os.Args[2:])
	case "targets":
		err = cmdTargets(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "wsj:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `wsj: Claude Code sessions in tmux, local or over ssh

  wsj run    [--on TARGET] [--dir PATH] [--model M] [--grace 3s] [--json] [PROMPT | -f FILE | -]
  wsj ls     [--on TARGET | --all] [--json]
  wsj attach [--on TARGET] JOB
  wsj kill   [--on TARGET] JOB...
  wsj clean  [--on TARGET | --all] [--json]   kill dead job windows, remove orphaned prompt dirs
  wsj route  [--dir PATH] [--json] [PROMPT | -f FILE | -]   dry-run the task router: print the lane and target
  wsj targets

Every command takes --no-mux to open a fresh ssh connection instead of the
multiplexed one (for debugging a stale control socket).

Targets: `+ccjobs.DefaultConfigPath()+` (override with WSJ_TARGETS).
With a [ws] section there (url, key_file: an owner's ws API key minted with
job reporting) or WSJ_WS_URL and
WSJ_WS_KEY, run, kill and clean report job handles to ws for its read-only
job tab. Only the handle is sent: never the prompt, never any output.
`)
}

// common holds the flags every target-addressing command shares.
type common struct {
	on    *string
	noMux *bool
}

func addCommon(fs *flag.FlagSet) common {
	return common{
		on:    fs.String("on", "", "target name"),
		noMux: fs.Bool("no-mux", false, "do not multiplex ssh connections (debugging)"),
	}
}

func (c common) config() (*ccjobs.Config, error) {
	cfg, err := ccjobs.LoadConfig(ccjobs.DefaultConfigPath())
	if err != nil {
		return nil, err
	}
	cfg.NoMux = *c.noMux
	return cfg, nil
}

func (c common) launcher() (*ccjobs.Config, *ccjobs.Launcher, error) {
	cfg, err := c.config()
	if err != nil {
		return nil, nil, err
	}
	l, err := cfg.Launcher(*c.on)
	return cfg, l, err
}

func cmdRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	c := addCommon(fs)
	dir := fs.String("dir", "", "working directory on the target (default: target's default_dir)")
	model := fs.String("model", "", "claude --model value")
	file := fs.String("f", "", "read the prompt from FILE")
	lane := fs.String("lane", ccjobs.LaneSubscription, "lane (only claude-subscription launches here)")
	grace := fs.Duration("grace", 3*time.Second, "how long to watch for claude exiting right away (0 disables)")
	asJSON := fs.Bool("json", false, "print the handle as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	prompt, err := readPrompt(*file, fs.Args())
	if err != nil {
		return err
	}
	cfg, l, err := c.launcher()
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second+*grace)
	defer cancel()
	job := ccjobs.Job{Lane: *lane, Target: l.Host.Name(), Cwd: *dir, Model: *model, Prompt: prompt, Created: time.Now().UTC()}
	h, err := l.Launch(cctx, job)
	if err != nil {
		return err
	}
	// The window exists now, so print the handle before the startup check:
	// if claude dies right away the window stays (remain-on-exit) and the
	// handle is what the user attaches with to see why.
	onFlag := ""
	if h.Target != cfg.Default {
		onFlag = " --on " + h.Target
	}
	if *asJSON {
		if err := json.NewEncoder(os.Stdout).Encode(h); err != nil {
			return err
		}
	} else {
		fmt.Printf("job %s on %s (%s:%s) in %s\n", h.ID, h.Target, h.Session, h.Window, h.Cwd)
		fmt.Printf("attach: wsj attach%s %s\n", onFlag, h.ID)
	}
	dead := false
	var startupErr error
	if err := l.CheckStartup(cctx, h, *grace); err != nil {
		var ex *ccjobs.ExitedError
		if errors.As(err, &ex) {
			dead = true
			startupErr = fmt.Errorf("%w (wsj attach%s %s)", err, onFlag, h.ID)
		} else {
			// A failed check is not a failed launch; say so and keep exit 0.
			fmt.Fprintln(os.Stderr, "wsj: warning:", err)
		}
	}
	// The web tab gets the handle and what the check saw, never the prompt.
	job.Prompt = ""
	report(cfg, func(r *ccjobs.Reporter) error { return r.Launched(cctx, ccjobs.ReportFor(h, job, dead)) })
	return startupErr
}

// report sends a handle to ws when a [ws] section (or WSJ_WS_URL and
// WSJ_WS_KEY) is configured. ws is a convenience view of tmux, so a report
// that fails is a warning: the job is unaffected and the next refresh
// from the ws host finds the window anyway.
func report(cfg *ccjobs.Config, send func(*ccjobs.Reporter) error) {
	r, err := cfg.Reporter()
	if err != nil {
		fmt.Fprintln(os.Stderr, "wsj: warning: not reporting to ws:", err)
		return
	}
	if r == nil {
		return
	}
	if err := send(r); err != nil {
		fmt.Fprintln(os.Stderr, "wsj: warning: report to ws failed:", err)
	}
}

// readPrompt takes the prompt from -f, the positional argument, or stdin.
// It is passed to the launcher as bytes and never through a shell.
func readPrompt(file string, rest []string) (string, error) {
	switch {
	case file != "":
		b, err := os.ReadFile(file)
		return string(b), err
	case len(rest) == 1 && rest[0] != "-":
		return rest[0], nil
	case len(rest) > 1:
		return "", errors.New("pass one prompt argument, quote it, or use -f FILE / stdin")
	}
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 && len(rest) == 0 {
		return "", errors.New("no prompt: give one argument, -f FILE, or pipe it on stdin")
	}
	b, err := io.ReadAll(os.Stdin)
	return string(b), err
}

// targetNames resolves --on / --all to the targets a command walks.
func targetNames(cfg *ccjobs.Config, on string, all bool) []string {
	switch {
	case all:
		return cfg.Names()
	case on == "":
		return []string{cfg.Default}
	}
	return []string{on}
}

func cmdLs(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	c := addCommon(fs)
	all := fs.Bool("all", false, "every configured target")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := c.config()
	if err != nil {
		return err
	}
	names := targetNames(cfg, *c.on, *all)
	var wins []ccjobs.Window
	var failed []string
	for _, n := range names {
		l, err := cfg.Launcher(n)
		if err != nil {
			return err
		}
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		ws, err := l.List(cctx)
		cancel()
		if err != nil {
			// RunError already names the target.
			fmt.Fprintf(os.Stderr, "wsj: %v\n", err)
			failed = append(failed, n)
			continue
		}
		wins = append(wins, ws...)
	}
	if *asJSON {
		if wins == nil {
			wins = []ccjobs.Window{}
		}
		if err := json.NewEncoder(os.Stdout).Encode(wins); err != nil {
			return err
		}
		return failedErr(failed)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "JOB\tTARGET\tSTATE\tSTARTED\tMODEL\tCWD")
	for _, w := range wins {
		state := "alive"
		if w.Dead {
			state = "dead"
		}
		started := ""
		if !w.Started.IsZero() {
			started = ago(time.Since(w.Started))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", w.JobID, w.Target, state, started, w.Model, w.Cwd)
	}
	_ = tw.Flush()
	return failedErr(failed)
}

// failedErr summarizes per-target failures already printed to stderr.
func failedErr(failed []string) error {
	if len(failed) == 0 {
		return nil
	}
	return fmt.Errorf("failed on %s", strings.Join(failed, ", "))
}

func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func cmdAttach(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	c := addCommon(fs)
	ro := fs.Bool("r", false, "read-only")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: wsj attach [--on TARGET] JOB")
	}
	_, l, err := c.launcher()
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	w, err := l.Find(cctx, fs.Arg(0))
	cancel()
	if err != nil {
		return err
	}
	argv := l.AttachArgv(w, *ro)
	bin, err := exec.LookPath(argv[0])
	if err != nil {
		return err
	}
	// Hand the terminal to tmux/ssh; nothing of the session passes through wsj.
	return syscall.Exec(bin, argv, os.Environ())
}

func cmdKill(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("kill", flag.ContinueOnError)
	c := addCommon(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("usage: wsj kill [--on TARGET] JOB...")
	}
	cfg, l, err := c.launcher()
	if err != nil {
		return err
	}
	var errs []string
	for _, j := range fs.Args() {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		w, ferr := l.Find(cctx, j)
		err := ferr
		if err == nil {
			err = l.Kill(cctx, j)
		}
		if err == nil {
			report(cfg, func(r *ccjobs.Reporter) error { return r.Killed(cctx, w.JobID) })
		}
		cancel()
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		fmt.Printf("killed %s on %s\n", j, l.Host.Name())
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// cmdClean kills dead job windows and removes orphaned prompt dirs on each
// target. Live jobs are untouched; the pane contents are never read.
func cmdClean(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("clean", flag.ContinueOnError)
	c := addCommon(fs)
	all := fs.Bool("all", false, "every configured target")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := c.config()
	if err != nil {
		return err
	}
	var reports []ccjobs.CleanReport
	var failed []string
	for _, n := range targetNames(cfg, *c.on, *all) {
		l, err := cfg.Launcher(n)
		if err != nil {
			// Keep the reports of targets already cleaned.
			fmt.Fprintf(os.Stderr, "wsj: %s: %v\n", n, err)
			failed = append(failed, n)
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		rep, err := l.Clean(cctx)
		for _, w := range rep.Killed {
			report(cfg, func(r *ccjobs.Reporter) error { return r.Killed(cctx, w.JobID) })
		}
		cancel()
		reports = append(reports, rep)
		if err != nil {
			fmt.Fprintf(os.Stderr, "wsj: %s: %v\n", n, err)
			failed = append(failed, n)
		}
	}
	if *asJSON {
		if err := json.NewEncoder(os.Stdout).Encode(reports); err != nil {
			return err
		}
		return failedErr(failed)
	}
	for _, rep := range reports {
		for _, w := range rep.Killed {
			fmt.Printf("%s: killed dead job %s (%s)\n", rep.Target, w.JobID, w.Cwd)
		}
		for _, id := range rep.Orphans {
			fmt.Printf("%s: removed orphaned prompt dir for %s\n", rep.Target, id)
		}
		if len(rep.Killed) == 0 && len(rep.Orphans) == 0 {
			fmt.Printf("%s: nothing to clean\n", rep.Target)
		}
	}
	return failedErr(failed)
}

// cmdRoute prints what the task router (docs/CLAUDE_CODE_JOBS.md §6.4)
// would decide for a prompt, with the target it would pick from the
// targets file. It starts nothing and logs nothing: the dry run of the
// rules, for tuning them.
func cmdRoute(args []string) error {
	fs := flag.NewFlagSet("route", flag.ContinueOnError)
	dir := fs.String("dir", "", "working directory the job would run in (routes to the subscription lane)")
	file := fs.String("f", "", "read the prompt from FILE")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	prompt, err := readPrompt(*file, fs.Args())
	if err != nil {
		return err
	}
	cfg, err := ccjobs.LoadConfig(ccjobs.DefaultConfigPath())
	if err != nil {
		return err
	}
	d := ccjobs.Route(ccjobs.RouteInput{Prompt: prompt, Cwd: *dir})
	target := ""
	if d.Lane == ccjobs.LaneSubscription {
		target = cfg.ChooseTarget(*dir)
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(struct {
			ccjobs.Decision
			Target string `json:"target,omitempty"`
		}{d, target})
	}
	fmt.Printf("lane:    %s\nrule:    %s\nreason:  %s\n", d.Lane, d.Rule, d.Reason)
	if target != "" {
		fmt.Printf("target:  %s\n", target)
	}
	if d.Ambiguous {
		fmt.Println("note:    no rule matched; the M4 classifier would decide")
	}
	f := d.Features
	fmt.Printf("features: chars=%d words=%d fences=%d paths=%d cwd=%v agentic=%d reasoning=%d simple=%d sensitive=%v override=%q\n",
		f.Chars, f.Words, f.CodeFences, f.FilePaths, f.HasCwd, f.Agentic, f.Reasoning, f.Simple, f.Sensitive, f.Override)
	return nil
}

func cmdTargets(_ []string) error {
	cfg, err := ccjobs.LoadConfig(ccjobs.DefaultConfigPath())
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tTYPE\tHOST\tSESSION\tDEFAULT_DIR")
	for _, n := range cfg.Names() {
		t := cfg.Targets[n]
		mark := ""
		if n == cfg.Default {
			mark = "*"
		}
		fmt.Fprintf(tw, "%s%s\t%s\t%s\t%s\t%s\n", n, mark, t.Type, t.Host, t.Session, t.DefaultDir)
	}
	return tw.Flush()
}
