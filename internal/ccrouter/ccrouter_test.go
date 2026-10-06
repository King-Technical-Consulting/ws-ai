package ccrouter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/agent"
	"github.com/jking323/ws/internal/ccjobs"
)

func TestDefAndPolicy(t *testing.T) {
	tl := &Tool{}
	d := tl.Def()
	if d.Name != ToolName || !json.Valid(d.InputSchema) {
		t.Fatalf("def = %+v", d)
	}
	if tl.DefaultPolicy() != agent.PolicyAuto {
		t.Error("dry-run tool must not need approval")
	}
	tl.Dispatch = true
	if tl.DefaultPolicy() != agent.PolicyAsk {
		t.Error("a dispatching tool must ask")
	}
	if a := ParseAliases("api=anthropic/claude-sonnet-5-5, local = qwen ,bad,=x"); a["api"] != "anthropic/claude-sonnet-5-5" || a["local"] != "qwen" || a["openrouter"] != "cheap" || len(a) != 3 {
		t.Errorf("aliases = %v", a)
	}
}

// TestDryRun: with dispatch off every call only decides. No DB, no
// targets, no runtime are needed, and nothing is started.
func TestDryRun(t *testing.T) {
	tl := &Tool{IsOwner: func(context.Context, uuid.UUID) (bool, error) { return true, nil }}
	call := func(args string) Output {
		t.Helper()
		res, err := tl.Call(context.Background(), agent.ToolCtx{}, json.RawMessage(args))
		if err != nil || res.IsError {
			t.Fatalf("call %s: %v %s", args, err, res.Text)
		}
		out, ok := res.Data.(*Output)
		if !ok {
			t.Fatalf("data type %T", res.Data)
		}
		return *out
	}
	out := call(`{"prompt":"refactor internal/a/x.go and internal/b/y.go to share a helper","cwd":"~/code/ws"}`)
	if out.Decision.Lane != ccjobs.LaneSubscription || !out.DryRun || out.Dispatched || out.Handle != nil || !strings.Contains(out.Note, "WS_CC_DISPATCH") {
		t.Errorf("subscription dry run = %+v", out)
	}
	if !strings.Contains(out.Note, "no launcher targets") {
		t.Errorf("missing targets must be said: %q", out.Note)
	}
	out = call(`{"prompt":"summarize: cats are great","dry_run":true}`)
	if out.Decision.Lane != ccjobs.LaneOpenRouter || out.Selector != "cheap" || !out.DryRun {
		t.Errorf("openrouter dry run = %+v", out)
	}
	out = call(`{"prompt":"anything","lane":"local","model":"qwen3-14b"}`)
	if out.Decision.Rule != "requested" || out.Decision.Lane != ccjobs.LaneLocal || out.Selector != "qwen3-14b" {
		t.Errorf("forced lane = %+v", out)
	}
	// The sensitive rule applies to a forced lane and to a model hint: a
	// secret never goes to OpenRouter whatever the calling model asked.
	out = call(`{"prompt":"rewrite this: my password is hunter2","lane":"openrouter","model":"openrouter/deepseek/deepseek-chat-v3"}`)
	if out.Decision.Lane != ccjobs.LaneLocal || out.Decision.Rule != "sensitive" || out.Selector != "local" || !strings.Contains(out.Note, "model hint ignored") {
		t.Errorf("sensitive forced lane = %+v", out)
	}
	out = call(`{"prompt":"is storing a password hash with bcrypt fine","lane":"api"}`)
	if out.Decision.Lane != ccjobs.LaneAPI || out.Decision.Rule != "requested" {
		t.Errorf("sensitive on api must stay api = %+v", out)
	}
	// The subscription lane is the owner's alone, dry run included.
	res, _ := (&Tool{}).Call(context.Background(), agent.ToolCtx{}, json.RawMessage(`{"prompt":"@claude tidy up"}`))
	if !res.IsError || !strings.Contains(res.Text, "no owner check") {
		t.Errorf("no owner check configured: %+v", res)
	}
	tl.IsOwner = func(context.Context, uuid.UUID) (bool, error) { return false, nil }
	res, _ = tl.Call(context.Background(), agent.ToolCtx{}, json.RawMessage(`{"prompt":"@claude tidy up"}`))
	if !res.IsError || !strings.Contains(res.Text, "only the owner") {
		t.Errorf("non-owner: %+v", res)
	}
	tl.IsOwner = func(context.Context, uuid.UUID) (bool, error) { return true, nil }
	if out := call(`{"prompt":"@claude tidy up"}`); out.Decision.Lane != ccjobs.LaneSubscription {
		t.Errorf("owner: %+v", out)
	}

	for _, bad := range []string{`{}`, `{"prompt":"  "}`, `{"prompt":"x","lane":"subscription"}`, `not json`} {
		if res, _ := tl.Call(context.Background(), agent.ToolCtx{}, json.RawMessage(bad)); !res.IsError {
			t.Errorf("%s accepted", bad)
		}
	}
}

// TestLaunchSubscription is the spec's M3 done-criterion: a prompt routed
// to claude-subscription launches a real tmux window, through the same
// launcher the CLI uses, with a stub claude and a private tmux server.
func TestLaunchSubscription(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	stub := filepath.Join(tmp, "claude")
	seen := filepath.Join(tmp, "argv.txt")
	script := "#!/bin/sh\nfor a; do last=$a; done\nprintf '%s' \"$last\" > " + ccjobs.ShellQuote(seen) + "\nsleep 60\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(tmp, "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	sock := fmt.Sprintf("wsj-test-%d-ccrouter", os.Getpid())
	cfg := &ccjobs.Config{Default: "box", Targets: map[string]ccjobs.Target{
		"box": {Type: "local", Session: "wsjrouter", DefaultDir: tmp, Claude: stub, TmuxSocket: sock},
	}}
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", sock, "kill-server").Run() })
	l, _ := cfg.Launcher("box")
	l.IdleCommand = "sleep 100000"
	// EnsureSession first with the idle command so no shell writes into $HOME.
	if err := l.EnsureSession(context.Background()); err != nil {
		t.Fatal(err)
	}

	owner := func(context.Context, uuid.UUID) (bool, error) { return true, nil }
	tl := &Tool{Dispatch: true, Targets: func() (*ccjobs.Config, error) { return cfg, nil }, IsOwner: owner}
	prompt := "fix the failing test in internal/x/y_test.go; it's \"quoted\" $(not run)"
	res, err := tl.Call(context.Background(), agent.ToolCtx{}, json.RawMessage(fmt.Sprintf(`{"prompt":%q,"cwd":%q}`, prompt, repo)))
	if err != nil || res.IsError {
		t.Fatalf("call: %v %s", err, res.Text)
	}
	out := res.Data.(*Output)
	if out.Decision.Lane != ccjobs.LaneSubscription || out.DryRun || !out.Dispatched || out.Target != "box" || out.Handle == nil {
		t.Fatalf("out = %+v", out)
	}
	if !strings.Contains(out.Note, "wsj attach --on box "+out.Handle.ID) {
		t.Errorf("note = %q", out.Note)
	}
	wins, err := l.List(context.Background())
	if err != nil || len(wins) != 1 || wins[0].JobID != out.Handle.ID || wins[0].Dead {
		t.Fatalf("windows = %+v %v", wins, err)
	}
	// The stub got the prompt byte-identical: the dispatcher never let a
	// shell parse it.
	var got string
	for i := 0; i < 50 && got == ""; i++ {
		b, _ := os.ReadFile(seen)
		got = string(b)
	}
	if got != prompt {
		t.Errorf("claude saw %q", got)
	}
	// A claude that exits at once is reported but the handle still comes back.
	dying := filepath.Join(tmp, "dying")
	if err := os.WriteFile(dying, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg.Targets["box"] = ccjobs.Target{Type: "local", Session: "wsjrouter", DefaultDir: tmp, Claude: dying, TmuxSocket: sock}
	res, _ = tl.Call(context.Background(), agent.ToolCtx{}, json.RawMessage(`{"prompt":"go","lane":"claude-subscription"}`))
	out = res.Data.(*Output)
	if res.IsError || out.Handle == nil || !strings.Contains(out.Note, "exited immediately") {
		t.Errorf("dying claude: %s %+v", res.Text, out)
	}
}

func TestTitleFor(t *testing.T) {
	long := strings.Repeat("é", 100) // 2 bytes per rune: a byte cut would split one
	got := titleFor("  " + long + "  ")
	if !utf8.ValidString(got) || utf8.RuneCountInString(got) != 60 || !strings.HasSuffix(got, "...") {
		t.Errorf("titleFor = %q (%d runes)", got, utf8.RuneCountInString(got))
	}
	if got := titleFor("short   one"); got != "short one" {
		t.Errorf("titleFor short = %q", got)
	}
}

// TestNonSubscriptionNeedsRuntime: with dispatch on but no DB/runtime,
// the other lanes fail loudly instead of pretending.
func TestNonSubscriptionNeedsRuntime(t *testing.T) {
	tl := &Tool{Dispatch: true}
	res, _ := tl.Call(context.Background(), agent.ToolCtx{}, json.RawMessage(`{"prompt":"summarize: hi"}`))
	if !res.IsError || !strings.Contains(res.Text, "not available") {
		t.Errorf("res = %+v", res)
	}
}

// TestBudget: the weekly soft cap (spec M6) moves low-value subscription
// work to the api lane as the pool fills, notes a forced lane instead of
// overriding it, and a failed read keeps the decision.
func TestBudget(t *testing.T) {
	pool := ccjobs.Budget{Used: 0, Cap: 10, SoftPct: 75}
	var poolErr error
	tl := &Tool{
		IsOwner: func(context.Context, uuid.UUID) (bool, error) { return true, nil },
		Budget:  func(context.Context) (ccjobs.Budget, error) { return pool, poolErr },
	}
	call := func(args string) Output {
		t.Helper()
		res, err := tl.Call(context.Background(), agent.ToolCtx{}, json.RawMessage(args))
		if err != nil || res.IsError {
			t.Fatalf("call %s: %v %s", args, err, res.Text)
		}
		return *res.Data.(*Output)
	}
	longAgentic := `{"prompt":"` + strings.Repeat("refactor the module and add tests, then fix the build. ", 40) + `"}`
	cwd := `{"prompt":"tidy the helpers","cwd":"~/code/ws"}`

	if out := call(longAgentic); out.Decision.Lane != ccjobs.LaneSubscription || out.Decision.Rule != "repo" {
		t.Fatalf("empty pool must not change the decision: %+v", out.Decision)
	}
	pool.Used = 8 // soft
	out := call(longAgentic)
	if out.Decision.Lane != ccjobs.LaneAPI || out.Decision.Rule != "budget:soft" || out.Selector != "best" || !strings.Contains(out.Note, "8 of 10") {
		t.Errorf("soft pool, low-value = %+v note %q", out.Decision, out.Note)
	}
	if out := call(cwd); out.Decision.Lane != ccjobs.LaneSubscription || out.Decision.Rule != "repo" {
		t.Errorf("soft pool, cwd work must still launch: %+v", out.Decision)
	}
	pool.Used = 10 // full
	if out := call(cwd); out.Decision.Lane != ccjobs.LaneAPI || out.Decision.Rule != "budget:full" {
		t.Errorf("full pool = %+v", out.Decision)
	}
	out = call(`{"prompt":"tidy up","lane":"claude-subscription"}`)
	if out.Decision.Lane != ccjobs.LaneSubscription || out.Decision.Rule != "requested" || !strings.Contains(out.Note, "launching anyway") {
		t.Errorf("forced lane on a full pool = %+v note %q", out.Decision, out.Note)
	}
	if out := call(`{"prompt":"@claude tidy up"}`); out.Decision.Lane != ccjobs.LaneSubscription || !strings.Contains(out.Note, "launching anyway") {
		t.Errorf("override on a full pool = %+v note %q", out.Decision, out.Note)
	}
	// Other lanes never consult the pool; a failed read keeps the decision.
	if out := call(`{"prompt":"summarize: cats are great"}`); out.Decision.Lane != ccjobs.LaneOpenRouter || strings.Contains(out.Note, "pool") {
		t.Errorf("openrouter with a full pool = %+v", out)
	}
	poolErr = errors.New("db down")
	if out := call(cwd); out.Decision.Lane != ccjobs.LaneSubscription || !strings.Contains(out.Note, "budget unavailable") {
		t.Errorf("unreadable pool = %+v note %q", out.Decision, out.Note)
	}
}
