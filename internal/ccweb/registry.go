// Package ccweb is the server side of the read-only Claude Code job tab
// (docs/CLAUDE_CODE_JOBS.md §6.5, spec M5): the cc_jobs registry that wsj
// and the router report handles to, the liveness refresh that asks each
// target's tmux which job windows still exist, and the terminal bridge
// that runs `tmux attach -r` on a job window for the browser.
//
// It carries metadata and terminal bytes only. Nothing here reads a
// prompt, parses or stores what a window shows, or touches a credential:
// the bridge relays the live terminal to one browser tab and forgets it.
package ccweb

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jking323/ws/internal/ccjobs"
	"github.com/jking323/ws/internal/store"
)

// Store is the slice of the store the registry uses (*store.DB implements it).
type Store interface {
	UpsertCCJob(ctx context.Context, arg store.UpsertCCJobParams) (store.CcJob, error)
	GetCCJob(ctx context.Context, id string) (store.CcJob, error)
	ListCCJobs(ctx context.Context, limit int32) ([]store.CcJob, error)
	ListCCJobsOpen(ctx context.Context) ([]store.CcJob, error)
	MarkCCJobSeen(ctx context.Context, arg store.MarkCCJobSeenParams) error
	MarkCCJobEnded(ctx context.Context, arg store.MarkCCJobEndedParams) error
	IncrementCCLaunchCounter(ctx context.Context, week time.Time) (int32, error)
	GetCCLaunchCount(ctx context.Context, week time.Time) (int32, error)
}

// Sources of a cc_jobs row.
const (
	SourceWSJ    = "wsj"    // reported by the CLI
	SourceRouter = "router" // launched by spawn_job on this host
	SourceTmux   = "tmux"   // found on a target by a refresh, never reported
)

// Job statuses (cc_jobs.status).
const (
	StatusAlive   = "alive"
	StatusDead    = "dead"
	StatusKilled  = "killed"
	StatusGone    = "gone"
	StatusUnknown = "unknown"
)

// ErrGone says the job's window no longer exists on its target.
var ErrGone = errors.New("job window is gone")

// Registry keeps cc_jobs in step with the targets' tmux servers.
type Registry struct {
	DB Store
	// Targets loads the launcher targets as seen from this host; the same
	// loader the router uses. nil means no target is reachable from here,
	// so refreshes and terminals are unavailable while reports still land.
	Targets func() (*ccjobs.Config, error)
	// MinInterval throttles Refresh (default 10s): the web tab polls while
	// it is open, and each refresh is one ssh round trip per target.
	MinInterval time.Duration
	// TargetTimeout bounds one target's tmux listing (default 15s).
	TargetTimeout time.Duration
	// Cap and SoftPct are the week's soft launch cap (WS_CC_WEEKLY_CAP,
	// WS_CC_WEEKLY_SOFT); Cap 0 counts launches without ever downgrading.
	Cap     int
	SoftPct int
	Log     *slog.Logger

	mu       sync.Mutex
	last     RefreshReport
	inflight chan struct{}
}

// TargetStatus is one target's part of a refresh.
type TargetStatus struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	// Error says why the target could not be listed (ssh, tmux), trimmed.
	Error string `json:"error,omitempty"`
	// Windows is how many job windows the target listed.
	Windows int `json:"windows"`
}

// RefreshReport says what the last refresh found, for the UI's status line.
type RefreshReport struct {
	At      time.Time      `json:"at"`
	Targets []TargetStatus `json:"targets"`
	// Error is set when no target could be consulted at all (no targets
	// file on this host, or it failed to load).
	Error string `json:"error,omitempty"`
}

func (r *Registry) minInterval() time.Duration {
	if r.MinInterval > 0 {
		return r.MinInterval
	}
	return 10 * time.Second
}

func (r *Registry) targetTimeout() time.Duration {
	if r.TargetTimeout > 0 {
		return r.TargetTimeout
	}
	return 15 * time.Second
}

// Report records a handle reported over the API (wsj) or by the router.
// userID is the reporting user; source says who. A known job keeps its
// original user and source and only has its handle and liveness updated.
func (r *Registry) Report(ctx context.Context, userID uuid.NullUUID, source string, rep ccjobs.Report) (store.CcJob, error) {
	if !ccjobs.ValidJobID(rep.ID) {
		return store.CcJob{}, fmt.Errorf("bad job id %q", rep.ID)
	}
	if rep.Target == "" || !strings.HasPrefix(rep.Window, "@") {
		return store.CcJob{}, errors.New("target and a tmux window id (@N) are required")
	}
	if rep.Session == "" {
		rep.Session = "subscription"
	}
	if rep.Lane == "" {
		rep.Lane = ccjobs.LaneSubscription
	}
	if rep.Lane != ccjobs.LaneSubscription {
		return store.CcJob{}, fmt.Errorf("lane %q has no tmux window; only %s jobs are listed", rep.Lane, ccjobs.LaneSubscription)
	}
	switch rep.Status {
	case StatusAlive, StatusDead:
	case "":
		rep.Status = StatusAlive
	default:
		return store.CcJob{}, fmt.Errorf("status %q: want alive or dead", rep.Status)
	}
	switch source {
	case SourceWSJ, SourceRouter, SourceTmux:
	default:
		return store.CcJob{}, fmt.Errorf("bad source %q", source)
	}
	started := rep.Started
	if started.IsZero() {
		started = time.Now().UTC()
	}
	p := store.UpsertCCJobParams{
		ID: rep.ID, UserID: userID, Target: rep.Target, Session: rep.Session, Window: rep.Window,
		Cwd: rep.Cwd, Lane: rep.Lane, Source: source, Status: rep.Status, StartedAt: started,
	}
	if rep.Model != "" {
		m := rep.Model
		p.Model = &m
	}
	// A launch counts once, in the week it started, whoever reported it;
	// a re-report of a known job is not a launch.
	_, lookupErr := r.DB.GetCCJob(ctx, rep.ID)
	row, err := r.DB.UpsertCCJob(ctx, p)
	if err == nil && lookupErr != nil {
		if _, cerr := r.DB.IncrementCCLaunchCounter(ctx, ccjobs.WeekStart(started)); cerr != nil {
			r.warn("cc_launch_counter", cerr)
		}
	}
	return row, err
}

// Budget is this week's pool: the launches counted so far against the
// configured cap. The router consults it before a subscription launch.
func (r *Registry) Budget(ctx context.Context) (ccjobs.Budget, error) {
	b := ccjobs.Budget{Week: ccjobs.WeekStart(time.Now()), Cap: r.Cap, SoftPct: r.SoftPct}
	n, err := r.DB.GetCCLaunchCount(ctx, b.Week)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return b, nil
		}
		return b, err
	}
	b.Used = int(n)
	return b, nil
}

// Launched records a launch made on this host (the router's spawn_job).
func (r *Registry) Launched(ctx context.Context, userID uuid.UUID, h ccjobs.Handle, job ccjobs.Job, dead bool) error {
	if r == nil || r.DB == nil {
		return nil
	}
	_, err := r.Report(ctx, uuid.NullUUID{UUID: userID, Valid: userID != uuid.Nil}, SourceRouter, ccjobs.ReportFor(h, job, dead))
	return err
}

// Killed records that wsj removed the window.
func (r *Registry) Killed(ctx context.Context, id string) error {
	if !ccjobs.ValidJobID(id) {
		return fmt.Errorf("bad job id %q", id)
	}
	if _, err := r.DB.GetCCJob(ctx, id); err != nil {
		return err
	}
	return r.DB.MarkCCJobEnded(ctx, store.MarkCCJobEndedParams{ID: id, Status: StatusKilled})
}

// List returns the newest jobs.
func (r *Registry) List(ctx context.Context, limit int32) ([]store.CcJob, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := r.DB.ListCCJobs(ctx, limit)
	if rows == nil {
		rows = []store.CcJob{}
	}
	return rows, err
}

// Last returns the most recent refresh report without refreshing.
func (r *Registry) Last() RefreshReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

// Refresh lists every configured target's job windows and brings cc_jobs
// in step: listed windows are alive or dead (pane exited), open jobs that
// a target no longer lists are gone, and windows nobody reported are
// adopted. A target that cannot be listed changes nothing for its jobs.
// Calls within MinInterval of the last refresh return that report, and a
// refresh in flight is joined rather than duplicated.
func (r *Registry) Refresh(ctx context.Context, force bool) RefreshReport {
	r.mu.Lock()
	if !force && time.Since(r.last.At) < r.minInterval() && r.inflight == nil {
		rep := r.last
		r.mu.Unlock()
		return rep
	}
	if r.inflight != nil {
		done := r.inflight
		r.mu.Unlock()
		select {
		case <-done:
			return r.Last()
		case <-ctx.Done():
			return r.Last()
		}
	}
	done := make(chan struct{})
	r.inflight = done
	r.mu.Unlock()

	rep := r.refresh(ctx)

	r.mu.Lock()
	r.last = rep
	r.inflight = nil
	close(done)
	r.mu.Unlock()
	return rep
}

func (r *Registry) refresh(ctx context.Context) RefreshReport {
	rep := RefreshReport{At: time.Now().UTC(), Targets: []TargetStatus{}}
	if r.Targets == nil {
		rep.Error = "no launcher targets on this host"
		return rep
	}
	cfg, err := r.Targets()
	if err != nil {
		rep.Error = "launcher targets: " + err.Error()
		return rep
	}
	open, err := r.DB.ListCCJobsOpen(ctx)
	if err != nil {
		rep.Error = "db: " + err.Error()
		return rep
	}
	byTarget := map[string][]store.CcJob{}
	for _, j := range open {
		byTarget[j.Target] = append(byTarget[j.Target], j)
	}

	type listing struct {
		name string
		wins []ccjobs.Window
		err  error
	}
	results := make([]listing, len(cfg.Names()))
	var wg sync.WaitGroup
	for i, name := range cfg.Names() {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			l, err := cfg.Launcher(name)
			if err != nil {
				results[i] = listing{name: name, err: err}
				return
			}
			tctx, cancel := context.WithTimeout(ctx, r.targetTimeout())
			defer cancel()
			wins, err := l.List(tctx)
			if err == nil {
				// Views left behind by bridges that never closed.
				l.PruneViews(tctx)
			}
			results[i] = listing{name: name, wins: wins, err: err}
		}(i, name)
	}
	wg.Wait()

	for _, res := range results {
		st := TargetStatus{Name: res.name, OK: res.err == nil, Windows: len(res.wins)}
		if res.err != nil {
			st.Error = trim(res.err.Error(), 300)
			rep.Targets = append(rep.Targets, st)
			continue
		}
		listed := map[string]bool{}
		for _, w := range res.wins {
			listed[w.JobID] = true
			status := StatusAlive
			if w.Dead {
				status = StatusDead
			}
			known := false
			for _, j := range byTarget[res.name] {
				if j.ID == w.JobID {
					known = true
					break
				}
			}
			if known {
				if err := r.DB.MarkCCJobSeen(ctx, store.MarkCCJobSeenParams{ID: w.JobID, Status: status}); err != nil {
					r.warn("cc_jobs seen", err)
				}
				continue
			}
			if _, err := r.DB.GetCCJob(ctx, w.JobID); err == nil {
				// Known but already closed (killed, or gone and back?): a
				// window that exists is what counts.
				if err := r.DB.MarkCCJobSeen(ctx, store.MarkCCJobSeenParams{ID: w.JobID, Status: status}); err != nil {
					r.warn("cc_jobs seen", err)
				}
				continue
			}
			rep := ccjobs.Report{ID: w.JobID, Target: res.name, Session: w.Session, Window: w.Window, Cwd: w.Cwd, Lane: w.Lane, Model: w.Model, Started: w.Started, Status: status}
			if rep.Lane == "" {
				rep.Lane = ccjobs.LaneSubscription
			}
			if _, err := r.Report(ctx, uuid.NullUUID{}, SourceTmux, rep); err != nil {
				r.warn("cc_jobs adopt", err)
			}
		}
		for _, j := range byTarget[res.name] {
			if listed[j.ID] {
				continue
			}
			if err := r.DB.MarkCCJobEnded(ctx, store.MarkCCJobEndedParams{ID: j.ID, Status: StatusGone}); err != nil {
				r.warn("cc_jobs gone", err)
			}
		}
		rep.Targets = append(rep.Targets, st)
	}
	// Open jobs whose target this host does not know stay as they are;
	// say so once per target so the UI can explain a stale row.
	known := map[string]bool{}
	for _, t := range rep.Targets {
		known[t.Name] = true
	}
	var missing []string
	for name := range byTarget {
		if !known[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	for _, name := range missing {
		rep.Targets = append(rep.Targets, TargetStatus{Name: name, Error: "not a configured target on this host"})
	}
	return rep
}

func (r *Registry) warn(msg string, err error) {
	if r.Log != nil {
		r.Log.Warn(msg, "err", err)
	}
}

func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
