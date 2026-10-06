package ccweb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jking323/ws/internal/ccjobs"
	"github.com/jking323/ws/internal/store"
)

// fakeStore mirrors the cc_jobs queries' semantics in memory.
type fakeStore struct {
	mu     sync.Mutex
	rows   map[string]store.CcJob
	counts map[time.Time]int32
}

func newFake() *fakeStore {
	return &fakeStore{rows: map[string]store.CcJob{}, counts: map[time.Time]int32{}}
}

func (f *fakeStore) IncrementCCLaunchCounter(_ context.Context, week time.Time) (int32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts[week]++
	return f.counts[week], nil
}

func (f *fakeStore) GetCCLaunchCount(_ context.Context, week time.Time) (int32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.counts[week]
	if !ok {
		return 0, pgx.ErrNoRows
	}
	return n, nil
}

func (f *fakeStore) UpsertCCJob(_ context.Context, p store.UpsertCCJobParams) (store.CcJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	j, ok := f.rows[p.ID]
	if !ok {
		j = store.CcJob{ID: p.ID, UserID: p.UserID, Source: p.Source, CreatedAt: now}
	}
	j.Target, j.Session, j.Window, j.Cwd, j.Lane, j.Status, j.StartedAt = p.Target, p.Session, p.Window, p.Cwd, p.Lane, p.Status, p.StartedAt
	if p.Model != nil {
		j.Model = p.Model
	}
	j.SeenAt, j.UpdatedAt = &now, now
	if p.Status == StatusDead {
		if j.EndedAt == nil {
			j.EndedAt = &now
		}
	} else {
		j.EndedAt = nil
	}
	f.rows[p.ID] = j
	return j, nil
}

func (f *fakeStore) GetCCJob(_ context.Context, id string) (store.CcJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.rows[id]
	if !ok {
		return store.CcJob{}, errors.New("no rows")
	}
	return j, nil
}

func (f *fakeStore) list(filter func(store.CcJob) bool) []store.CcJob {
	var out []store.CcJob
	for _, j := range f.rows {
		if filter(j) {
			out = append(out, j)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].StartedAt.After(out[b].StartedAt) })
	return out
}

func (f *fakeStore) ListCCJobs(_ context.Context, limit int32) ([]store.CcJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.list(func(store.CcJob) bool { return true })
	if int32(len(out)) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeStore) ListCCJobsOpen(_ context.Context) ([]store.CcJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.list(func(j store.CcJob) bool {
		return j.Status == StatusAlive || j.Status == StatusDead || j.Status == StatusUnknown
	}), nil
}

func (f *fakeStore) MarkCCJobSeen(_ context.Context, p store.MarkCCJobSeenParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.rows[p.ID]
	if !ok {
		return errors.New("no rows")
	}
	now := time.Now()
	j.Status, j.SeenAt, j.UpdatedAt = p.Status, &now, now
	if p.Status == StatusDead {
		if j.EndedAt == nil {
			j.EndedAt = &now
		}
	} else {
		j.EndedAt = nil
	}
	f.rows[p.ID] = j
	return nil
}

func (f *fakeStore) MarkCCJobEnded(_ context.Context, p store.MarkCCJobEndedParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.rows[p.ID]
	if !ok || j.Status == StatusKilled || j.Status == StatusGone {
		return nil
	}
	now := time.Now()
	j.Status, j.UpdatedAt = p.Status, now
	if j.EndedAt == nil {
		j.EndedAt = &now
	}
	f.rows[p.ID] = j
	return nil
}

func (f *fakeStore) get(t *testing.T, id string) store.CcJob {
	t.Helper()
	j, err := f.GetCCJob(context.Background(), id)
	if err != nil {
		t.Fatalf("job %s: %v", id, err)
	}
	return j
}

// testConfig is a targets config with one local target on a tmux server of
// its own and a stub claude that stays up for `stay`.
func testConfig(t *testing.T, stay string) (*ccjobs.Config, *ccjobs.Launcher) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	claude := filepath.Join(tmp, "claude")
	if err := os.WriteFile(claude, []byte("#!/bin/sh\n"+stay+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	name := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, strings.ToLower(t.Name()))
	sock := fmt.Sprintf("wsj-test-%d-%s", os.Getpid(), name)
	cfg := &ccjobs.Config{Default: "local", Targets: map[string]ccjobs.Target{
		"local": {Type: "local", Session: "wsjtest", DefaultDir: tmp, Claude: claude, TmuxSocket: sock},
	}}
	l, err := cfg.Launcher("local")
	if err != nil {
		t.Fatal(err)
	}
	l.IdleCommand = "sleep 100000"
	t.Cleanup(func() {
		_, _ = l.Host.Run(context.Background(), nil, "tmux", "-L", sock, "kill-server")
	})
	return cfg, l
}

func TestReportValidation(t *testing.T) {
	r := &Registry{DB: newFake()}
	ctx := context.Background()
	uid := uuid.NullUUID{UUID: uuid.New(), Valid: true}
	good := ccjobs.Report{ID: "abcdef1234", Target: "mac", Window: "@3", Cwd: "/x"}
	j, err := r.Report(ctx, uid, SourceWSJ, good)
	if err != nil {
		t.Fatal(err)
	}
	if j.Session != "subscription" || j.Lane != ccjobs.LaneSubscription || j.Status != StatusAlive || j.StartedAt.IsZero() || j.SeenAt == nil || j.Source != SourceWSJ || j.UserID != uid {
		t.Errorf("defaults = %+v", j)
	}
	// A second report keeps user and source, updates the rest.
	again := good
	again.Status = StatusDead
	again.Model = "opus"
	j, err = r.Report(ctx, uuid.NullUUID{}, SourceTmux, again)
	if err != nil {
		t.Fatal(err)
	}
	if j.Source != SourceWSJ || j.UserID != uid || j.Status != StatusDead || j.EndedAt == nil || j.Model == nil || *j.Model != "opus" {
		t.Errorf("re-report = %+v", j)
	}
	bad := []ccjobs.Report{
		{ID: "../x", Target: "mac", Window: "@1"},
		{ID: "abcdef1234", Window: "@1"},
		{ID: "abcdef1234", Target: "mac", Window: "7"},
		{ID: "abcdef1234", Target: "mac", Window: "@1", Lane: "api"},
		{ID: "abcdef1234", Target: "mac", Window: "@1", Status: "running"},
	}
	for _, b := range bad {
		if _, err := r.Report(ctx, uid, SourceWSJ, b); err == nil {
			t.Errorf("accepted %+v", b)
		}
	}
	if _, err := r.Report(ctx, uid, "cli", good); err == nil {
		t.Error("accepted a bad source")
	}
	if err := r.Killed(ctx, "nothere123"); err == nil {
		t.Error("killed an unknown job")
	}
	if err := r.Killed(ctx, good.ID); err != nil {
		t.Fatal(err)
	}
	if j := r.DB.(*fakeStore).get(t, good.ID); j.Status != StatusKilled || j.EndedAt == nil {
		t.Errorf("after kill = %+v", j)
	}
}

// TestRefresh: a refresh asks the target's tmux and brings the rows in
// step. Reported jobs are seen alive or dead, unreported windows are
// adopted, a reported window the target no longer has is gone, and a
// target unknown here leaves its rows alone with a note.
func TestRefresh(t *testing.T) {
	cfg, l := testConfig(t, "sleep 60")
	db := newFake()
	r := &Registry{DB: db, Targets: func() (*ccjobs.Config, error) { return cfg, nil }, MinInterval: time.Hour}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	h1, err := l.Launch(ctx, ccjobs.Job{Prompt: "stays"})
	if err != nil {
		t.Fatal(err)
	}
	h2, err := l.Launch(ctx, ccjobs.Job{Prompt: "unreported", Model: "sonnet"})
	if err != nil {
		t.Fatal(err)
	}
	uid := uuid.New()
	if err := r.Launched(ctx, uid, h1, ccjobs.Job{Prompt: "stays"}, false); err != nil {
		t.Fatal(err)
	}
	// Reported, then its window vanished without a kill report.
	if _, err := r.Report(ctx, uuid.NullUUID{}, SourceWSJ, ccjobs.Report{ID: "vanished12", Target: "local", Session: "wsjtest", Window: "@99"}); err != nil {
		t.Fatal(err)
	}
	// On a target this host does not know.
	if _, err := r.Report(ctx, uuid.NullUUID{}, SourceWSJ, ccjobs.Report{ID: "elsewhere1", Target: "homelab", Window: "@2"}); err != nil {
		t.Fatal(err)
	}
	// Kill h1's window behind the registry's back later; first a refresh.
	rep := r.Refresh(ctx, true)
	if rep.Error != "" || len(rep.Targets) != 2 {
		t.Fatalf("report = %+v", rep)
	}
	if rep.Targets[0].Name != "local" || !rep.Targets[0].OK || rep.Targets[0].Windows != 2 {
		t.Errorf("local status = %+v", rep.Targets[0])
	}
	if rep.Targets[1].Name != "homelab" || rep.Targets[1].OK || !strings.Contains(rep.Targets[1].Error, "not a configured target") {
		t.Errorf("homelab status = %+v", rep.Targets[1])
	}
	if j := db.get(t, h1.ID); j.Status != StatusAlive || j.Source != SourceRouter || j.UserID.UUID != uid {
		t.Errorf("reported job = %+v", j)
	}
	if j := db.get(t, h2.ID); j.Status != StatusAlive || j.Source != SourceTmux || j.UserID.Valid || j.Model == nil || *j.Model != "sonnet" || j.Window != h2.Window || j.Session != "wsjtest" {
		t.Errorf("adopted job = %+v", j)
	}
	if j := db.get(t, "vanished12"); j.Status != StatusGone || j.EndedAt == nil {
		t.Errorf("vanished job = %+v", j)
	}
	if j := db.get(t, "elsewhere1"); j.Status != StatusAlive {
		t.Errorf("unknown-target job must be untouched: %+v", j)
	}

	// Throttled: the same report comes back until MinInterval passes or force.
	if again := r.Refresh(ctx, false); !again.At.Equal(rep.At) {
		t.Error("refresh within MinInterval must not run again")
	}

	// The window of h1 is killed outside ws; a forced refresh sees it gone.
	if err := l.Kill(ctx, h1.ID); err != nil {
		t.Fatal(err)
	}
	rep = r.Refresh(ctx, true)
	if rep.At.IsZero() || rep.Targets[0].Windows != 1 {
		t.Errorf("after kill = %+v", rep)
	}
	if j := db.get(t, h1.ID); j.Status != StatusGone {
		t.Errorf("killed-outside job = %+v", j)
	}

	// A target that cannot be listed changes nothing.
	broken := &ccjobs.Config{Default: "local", Targets: map[string]ccjobs.Target{"local": {Type: "ssh", Host: "nowhere.invalid", Session: "wsjtest"}}}
	rb := &Registry{DB: db, Targets: func() (*ccjobs.Config, error) { return broken, nil }, TargetTimeout: 5 * time.Second}
	rb.Targets = func() (*ccjobs.Config, error) { return broken, nil }
	before := db.get(t, h2.ID)
	rep = rb.Refresh(ctx, true)
	if len(rep.Targets) == 0 || rep.Targets[0].OK || rep.Targets[0].Error == "" {
		t.Errorf("broken target = %+v", rep)
	}
	if after := db.get(t, h2.ID); after.Status != before.Status || !after.SeenAt.Equal(*before.SeenAt) {
		t.Errorf("unreachable target must not change rows: %+v -> %+v", before, after)
	}
	if rep := (&Registry{DB: db}).Refresh(ctx, true); rep.Error == "" {
		t.Error("no targets must be reported")
	}
}

// TestDeadThenTerminal: a job whose claude exited is seen dead, and a
// terminal on a live job delivers bytes read-only through a grouped
// session that is removed on close.
func TestDeadThenTerminal(t *testing.T) {
	cfg, l := testConfig(t, "if [ \"$1\" = exit ]; then exit 0; fi; sleep 60")
	db := newFake()
	r := &Registry{DB: db, Targets: func() (*ccjobs.Config, error) { return cfg, nil }}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	hd, err := l.Launch(ctx, ccjobs.Job{Prompt: "exit"})
	if err != nil {
		t.Fatal(err)
	}
	ha, err := l.Launch(ctx, ccjobs.Job{Prompt: "stay"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Launched(ctx, uuid.New(), hd, ccjobs.Job{}, false); err != nil {
		t.Fatal(err)
	}
	if err := r.Launched(ctx, uuid.New(), ha, ccjobs.Job{}, false); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		r.Refresh(ctx, true)
		if db.get(t, hd.ID).Status == StatusDead {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("exited job never seen dead: %+v", db.get(t, hd.ID))
		}
		time.Sleep(200 * time.Millisecond)
	}
	if j := db.get(t, hd.ID); j.EndedAt == nil {
		t.Errorf("dead job has no ended_at: %+v", j)
	}

	term, err := r.Open(ctx, ha.ID, 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	if term.Job.ID != ha.ID {
		t.Errorf("terminal job = %+v", term.Job)
	}
	if !strings.Contains(strings.Join(term.view.Argv, " "), "attach-session -r") {
		t.Errorf("terminal must attach read-only: %q", term.view.Argv)
	}
	got := make(chan int, 1)
	go func() {
		buf := make([]byte, 4096)
		n, _ := term.Read(buf)
		got <- n
	}()
	select {
	case n := <-got:
		if n == 0 {
			t.Error("no terminal bytes")
		}
	case <-time.After(15 * time.Second):
		t.Error("terminal produced nothing")
	}
	term.Resize(80, 24)
	// The view is a session of its own; the job's session current window is not moved.
	out, _ := l.Host.Run(ctx, nil, l.Host.InteractiveArgv("tmux", "-L", cfg.Targets["local"].TmuxSocket, "list-sessions", "-F", "#{session_name}|~|#{session_attached}")...)
	if !strings.Contains(string(out), term.view.Session+"|~|1") {
		t.Errorf("view session not attached: %q", out)
	}
	if err := term.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
	out, _ = l.Host.Run(ctx, nil, "tmux", "-L", cfg.Targets["local"].TmuxSocket, "list-sessions", "-F", "#{session_name}")
	if strings.Contains(string(out), "wsview-") {
		t.Errorf("view session left behind: %q", out)
	}
	if wins, _ := l.List(ctx); len(wins) != 2 {
		t.Errorf("job windows after view close = %+v", wins)
	}

	// A gone job is reported as such and marked.
	if _, err := r.Report(ctx, uuid.NullUUID{}, SourceWSJ, ccjobs.Report{ID: "vanished12", Target: "local", Session: "wsjtest", Window: "@98"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Open(ctx, "vanished12", 80, 24); !errors.Is(err, ErrGone) {
		t.Errorf("open gone job: %v", err)
	}
	if j := db.get(t, "vanished12"); j.Status != StatusGone {
		t.Errorf("gone job = %+v", j)
	}
	if _, err := r.Open(ctx, "nothere123", 80, 24); err == nil {
		t.Error("open unknown job must fail")
	}
}

// TestLaunchCounter: every new job counts once in the week it started,
// whoever reported it; a re-report does not; Budget reads the current
// week against the configured cap.
func TestLaunchCounter(t *testing.T) {
	db := newFake()
	r := &Registry{DB: db, Cap: 4, SoftPct: 50}
	ctx := context.Background()
	now := time.Now().UTC()
	thisWeek := ccjobs.WeekStart(now)
	lastWeek := thisWeek.AddDate(0, 0, -7)

	b, err := r.Budget(ctx)
	if err != nil || b.Used != 0 || b.Cap != 4 || b.SoftPct != 50 || !b.Week.Equal(thisWeek) || b.Level() != ccjobs.BudgetOK {
		t.Fatalf("empty budget = %+v %v", b, err)
	}
	rep := func(id string, started time.Time, src string) {
		t.Helper()
		if _, err := r.Report(ctx, uuid.NullUUID{}, src, ccjobs.Report{ID: id, Target: "mac", Window: "@1", Started: started}); err != nil {
			t.Fatal(err)
		}
	}
	rep("aaaaaaaaaa", now, SourceWSJ)
	rep("aaaaaaaaaa", now, SourceWSJ) // re-report: not a launch
	rep("bbbbbbbbbb", now, SourceRouter)
	rep("cccccccccc", lastWeek.Add(time.Hour), SourceTmux) // adopted, last week
	if db.counts[thisWeek] != 2 || db.counts[lastWeek] != 1 {
		t.Errorf("counts = %v", db.counts)
	}
	if err := r.Launched(ctx, uuid.New(), ccjobs.Handle{ID: "dddddddddd", Target: "mac", Session: "subscription", Window: "@2"}, ccjobs.Job{Created: now}, false); err != nil {
		t.Fatal(err)
	}
	b, _ = r.Budget(ctx)
	if b.Used != 3 || b.Level() != ccjobs.BudgetSoft {
		t.Errorf("budget after three launches = %+v level %s", b, b.Level())
	}
	rep("eeeeeeeeee", now, SourceWSJ)
	if b, _ = r.Budget(ctx); b.Level() != ccjobs.BudgetFull {
		t.Errorf("budget at the cap = %+v level %s", b, b.Level())
	}
}
