// Package jobs wires River (Postgres-backed durable jobs): the run driver
// used by the worker, compaction, and a reaper that hands abandoned runs
// to the worker. serve inserts jobs; worker executes them.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/riverqueue/river/rivertype"

	"github.com/jking323/ws/internal/agent"
	"github.com/jking323/ws/internal/compaction"
	"github.com/jking323/ws/internal/store"
)

// RunArgs drives an agent run to completion or pause.
type RunArgs struct {
	RunID uuid.UUID `json:"run_id"`
}

// Kind implements river.JobArgs.
func (RunArgs) Kind() string { return "agent.run" }

// InsertOpts: one job per run at a time.
func (RunArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: "agents", MaxAttempts: 3, UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning, rivertype.JobStateRetryable, rivertype.JobStateScheduled}}}
}

// CompactArgs summarizes a conversation.
type CompactArgs struct {
	ConversationID uuid.UUID `json:"conversation_id"`
}

// Kind implements river.JobArgs.
func (CompactArgs) Kind() string { return "conversation.compact" }

// InsertOpts: at most one compaction per conversation per 2 minutes.
func (CompactArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: "housekeeping", MaxAttempts: 2, UniqueOpts: river.UniqueOpts{ByArgs: true, ByPeriod: 2 * time.Minute}}
}

// MediaArgs runs one media job (image or video generation).
type MediaArgs struct {
	JobID uuid.UUID `json:"job_id"`
}

// Kind implements river.JobArgs.
func (MediaArgs) Kind() string { return "media.generate" }

// InsertOpts: one River job per media job at a time; a failure is recorded
// on the media_jobs row, so River does not retry.
func (MediaArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: "media", MaxAttempts: 1, UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning, rivertype.JobStateRetryable, rivertype.JobStateScheduled}}}
}

// ReapArgs is the periodic sweep for abandoned runs.
type ReapArgs struct{}

// Kind implements river.JobArgs.
func (ReapArgs) Kind() string { return "agent.reap" }

// Deps are what workers need.
type Deps struct {
	DB        *store.DB
	Runtime   *agent.Runtime
	Compactor *compaction.Compactor
	Log       *slog.Logger
	// Sandbox, when set (worker with Docker), gets Reap called periodically
	// to stop idle sandboxes and remove stale containers.
	Sandbox interface{ Reap(context.Context) error }
	// Media runs media.generate jobs (internal/media); nil leaves the queue
	// without a worker.
	Media interface {
		Run(ctx context.Context, jobID uuid.UUID) error
	}
	// Agents fires due cron triggers (internal/agents) from the minute
	// tick; nil skips the tick.
	Agents interface {
		Tick(ctx context.Context) error
	}
	// Memory reflects on a finished agent run (agent.reflect); nil skips it.
	Memory interface {
		Reflect(ctx context.Context, runID uuid.UUID) error
	}
	// Rental reconciles rented GPUs every minute (rental.reconcile); nil
	// skips it.
	Rental interface {
		Reconcile(ctx context.Context) error
	}
}

// RentalTickArgs is the periodic rented-GPU reconcile.
type RentalTickArgs struct{}

// Kind implements river.JobArgs.
func (RentalTickArgs) Kind() string { return "rental.reconcile" }

type rentalWorker struct {
	river.WorkerDefaults[RentalTickArgs]
	deps *Deps
}

func (w *rentalWorker) Timeout(*river.Job[RentalTickArgs]) time.Duration { return 3 * time.Minute }

func (w *rentalWorker) Work(ctx context.Context, job *river.Job[RentalTickArgs]) error {
	return w.deps.Rental.Reconcile(ctx)
}

// ReflectArgs extracts memories from a finished agent run.
type ReflectArgs struct {
	RunID uuid.UUID `json:"run_id"`
}

// Kind implements river.JobArgs.
func (ReflectArgs) Kind() string { return "agent.reflect" }

// InsertOpts: once per run.
func (ReflectArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: "housekeeping", MaxAttempts: 2, UniqueOpts: river.UniqueOpts{ByArgs: true}}
}

type reflectWorker struct {
	river.WorkerDefaults[ReflectArgs]
	deps *Deps
}

func (w *reflectWorker) Timeout(*river.Job[ReflectArgs]) time.Duration { return 5 * time.Minute }

func (w *reflectWorker) Work(ctx context.Context, job *river.Job[ReflectArgs]) error {
	return w.deps.Memory.Reflect(ctx, job.Args.RunID)
}

// AgentTickArgs is the periodic cron-trigger sweep.
type AgentTickArgs struct{}

// Kind implements river.JobArgs.
func (AgentTickArgs) Kind() string { return "agent.tick" }

type agentTickWorker struct {
	river.WorkerDefaults[AgentTickArgs]
	deps *Deps
}

func (w *agentTickWorker) Timeout(*river.Job[AgentTickArgs]) time.Duration { return 2 * time.Minute }

func (w *agentTickWorker) Work(ctx context.Context, job *river.Job[AgentTickArgs]) error {
	return w.deps.Agents.Tick(ctx)
}

// SandboxReapArgs is the periodic sandbox housekeeping job.
type SandboxReapArgs struct{}

// Kind implements river.JobArgs.
func (SandboxReapArgs) Kind() string { return "sandbox.reap" }

type sandboxReapWorker struct {
	river.WorkerDefaults[SandboxReapArgs]
	deps *Deps
}

func (w *sandboxReapWorker) Timeout(*river.Job[SandboxReapArgs]) time.Duration {
	return 5 * time.Minute
}

func (w *sandboxReapWorker) Work(ctx context.Context, job *river.Job[SandboxReapArgs]) error {
	if w.deps.Sandbox == nil {
		return nil
	}
	return w.deps.Sandbox.Reap(ctx)
}

// Migrate applies River's own schema. Idempotent.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	m, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return err
	}
	_, err = m.Migrate(ctx, rivermigrate.DirectionUp, nil)
	return err
}

// Client is a thin wrapper so callers don't import river.
type Client struct {
	c   *river.Client[pgx.Tx]
	log *slog.Logger
}

// New builds a client. With startWorkers, it also runs the queues (the
// worker process); otherwise it only inserts (serve).
func New(ctx context.Context, pool *pgxpool.Pool, deps *Deps, startWorkers bool) (*Client, error) {
	workers := river.NewWorkers()
	cfg := &river.Config{
		Logger:               deps.Log,
		JobTimeout:           30 * time.Minute,
		RescueStuckJobsAfter: 45 * time.Minute,
	}
	if startWorkers {
		river.AddWorker(workers, &runWorker{deps: deps})
		river.AddWorker(workers, &compactWorker{deps: deps})
		river.AddWorker(workers, &reapWorker{deps: deps})
		cfg.Workers = workers
		cfg.Queues = map[string]river.QueueConfig{
			"agents":       {MaxWorkers: 8},
			"housekeeping": {MaxWorkers: 2},
		}
		if deps.Media != nil {
			river.AddWorker(workers, &mediaWorker{deps: deps})
			cfg.Queues["media"] = river.QueueConfig{MaxWorkers: 2}
		}
		cfg.PeriodicJobs = []*river.PeriodicJob{
			river.NewPeriodicJob(river.PeriodicInterval(time.Minute), func() (river.JobArgs, *river.InsertOpts) {
				return ReapArgs{}, &river.InsertOpts{Queue: "housekeeping", UniqueOpts: river.UniqueOpts{ByPeriod: time.Minute}}
			}, &river.PeriodicJobOpts{RunOnStart: true}),
		}
		if deps.Memory != nil {
			river.AddWorker(workers, &reflectWorker{deps: deps})
		}
		if deps.Agents != nil {
			river.AddWorker(workers, &agentTickWorker{deps: deps})
			cfg.PeriodicJobs = append(cfg.PeriodicJobs, river.NewPeriodicJob(river.PeriodicInterval(time.Minute), func() (river.JobArgs, *river.InsertOpts) {
				return AgentTickArgs{}, &river.InsertOpts{Queue: "housekeeping", UniqueOpts: river.UniqueOpts{ByPeriod: time.Minute}}
			}, &river.PeriodicJobOpts{RunOnStart: true}))
		}
		if deps.Rental != nil {
			river.AddWorker(workers, &rentalWorker{deps: deps})
			cfg.PeriodicJobs = append(cfg.PeriodicJobs, river.NewPeriodicJob(river.PeriodicInterval(time.Minute), func() (river.JobArgs, *river.InsertOpts) {
				return RentalTickArgs{}, &river.InsertOpts{Queue: "housekeeping", UniqueOpts: river.UniqueOpts{ByPeriod: time.Minute}}
			}, &river.PeriodicJobOpts{RunOnStart: true}))
		}
		if deps.Sandbox != nil {
			river.AddWorker(workers, &sandboxReapWorker{deps: deps})
			cfg.PeriodicJobs = append(cfg.PeriodicJobs, river.NewPeriodicJob(river.PeriodicInterval(5*time.Minute), func() (river.JobArgs, *river.InsertOpts) {
				return SandboxReapArgs{}, &river.InsertOpts{Queue: "housekeeping", UniqueOpts: river.UniqueOpts{ByPeriod: 5 * time.Minute}}
			}, &river.PeriodicJobOpts{RunOnStart: true}))
		}
	}
	c, err := river.NewClient(riverpgxv5.New(pool), cfg)
	if err != nil {
		return nil, err
	}
	cl := &Client{c: c, log: deps.Log}
	if startWorkers {
		if err := c.Start(ctx); err != nil {
			return nil, err
		}
	}
	return cl, nil
}

// Stop drains workers.
func (cl *Client) Stop(ctx context.Context) error { return cl.c.Stop(ctx) }

// EnqueueRun schedules a run to be driven by the worker.
func (cl *Client) EnqueueRun(ctx context.Context, runID uuid.UUID) error {
	_, err := cl.c.Insert(ctx, RunArgs{RunID: runID}, nil)
	return err
}

// EnqueueMedia schedules a media job for the worker.
func (cl *Client) EnqueueMedia(ctx context.Context, jobID uuid.UUID) error {
	_, err := cl.c.Insert(ctx, MediaArgs{JobID: jobID}, nil)
	return err
}

// EnqueueCompact schedules a summary for a conversation.
func (cl *Client) EnqueueCompact(ctx context.Context, convID uuid.UUID) error {
	_, err := cl.c.Insert(ctx, CompactArgs{ConversationID: convID}, nil)
	return err
}

// ---- workers ----

type runWorker struct {
	river.WorkerDefaults[RunArgs]
	deps *Deps
}

func (w *runWorker) Timeout(*river.Job[RunArgs]) time.Duration { return 25 * time.Minute }

func (w *runWorker) Work(ctx context.Context, job *river.Job[RunArgs]) error {
	sink := &agent.NotifySink{Pool: w.deps.DB.Pool, Channel: agent.NotifyChannel(job.Args.RunID.String()), Log: w.deps.Log}
	err := w.deps.Runtime.Drive(ctx, job.Args.RunID, sink)
	switch {
	case err == nil:
		w.reflect(ctx, job.Args.RunID)
		return nil
	case errors.Is(err, agent.ErrPaused):
		return nil
	case errors.Is(err, agent.ErrRunNotClaimable):
		return river.JobCancel(err) // someone else has it, or it's finished
	}
	w.reflect(ctx, job.Args.RunID) // a failed run is still an episode worth keeping
	return err
}

// reflect queues agent.reflect for a run that belongs to an agent.
func (w *runWorker) reflect(ctx context.Context, runID uuid.UUID) {
	if w.deps.Memory == nil {
		return
	}
	run, err := w.deps.DB.GetRun(ctx, runID)
	if err != nil || !run.AgentID.Valid {
		return
	}
	cl := river.ClientFromContext[pgx.Tx](ctx)
	if _, err := cl.Insert(ctx, ReflectArgs{RunID: runID}, nil); err != nil {
		w.deps.Log.Warn("reflect: enqueue", "run", runID, "err", err)
	}
}

type compactWorker struct {
	river.WorkerDefaults[CompactArgs]
	deps *Deps
}

func (w *compactWorker) Timeout(*river.Job[CompactArgs]) time.Duration { return 5 * time.Minute }

func (w *compactWorker) Work(ctx context.Context, job *river.Job[CompactArgs]) error {
	return w.deps.Compactor.Compact(ctx, job.Args.ConversationID)
}

type mediaWorker struct {
	river.WorkerDefaults[MediaArgs]
	deps *Deps
}

// Timeout bounds one generation; video engines poll inside it.
func (w *mediaWorker) Timeout(*river.Job[MediaArgs]) time.Duration { return 15 * time.Minute }

func (w *mediaWorker) Work(ctx context.Context, job *river.Job[MediaArgs]) error {
	return w.deps.Media.Run(ctx, job.Args.JobID)
}

type reapWorker struct {
	river.WorkerDefaults[ReapArgs]
	deps *Deps
}

// Work finds runs whose owner stopped heartbeating, or that were queued
// and never picked up, and enqueues them for the worker.
func (w *reapWorker) Work(ctx context.Context, job *river.Job[ReapArgs]) error {
	stale, err := w.deps.DB.ListStaleRuns(ctx)
	if err != nil {
		return err
	}
	queued, err := w.deps.DB.ListQueuedRuns(ctx)
	if err != nil {
		return err
	}
	cl := river.ClientFromContext[pgx.Tx](ctx)
	n := 0
	for _, r := range append(stale, queued...) {
		if _, err := cl.Insert(ctx, RunArgs{RunID: r.ID}, nil); err != nil {
			w.deps.Log.Warn("reap: enqueue", "run", r.ID, "err", err)
			continue
		}
		n++
	}
	if n > 0 {
		w.deps.Log.Info("reap: requeued runs", "count", n, "stale", len(stale), "queued", len(queued))
	}
	return nil
}

var _ = fmt.Sprintf
