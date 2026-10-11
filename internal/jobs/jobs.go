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

// InsertOpts: one job per run at a time. River requires the running state
// in the set, so a job inserted while the run's job is still being
// completed is dropped; an approval decision therefore uses ResumeArgs.
func (RunArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: "agents", MaxAttempts: 3, UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning, rivertype.JobStateRetryable, rivertype.JobStateScheduled}}}
}

// ResumeArgs continues a run parked for approval once its decisions are
// in. It is not unique: the decision can land while River still holds the
// pausing agent.run job as running (the completion is written after Work
// returns), and a unique insert would be dropped then, leaving the run
// queued until the reaper. ClaimRun takes the run atomically, so a second
// job finds it not claimable and is cancelled; a run is never driven twice.
type ResumeArgs struct {
	RunID uuid.UUID `json:"run_id"`
}

// Kind implements river.JobArgs.
func (ResumeArgs) Kind() string { return "agent.resume" }

// InsertOpts: same queue and attempts as agent.run, no uniqueness.
func (ResumeArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: "agents", MaxAttempts: 3}
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
	// Personal is each member's own memory (internal/memory): the nightly
	// reflection job and its due-check; nil skips both.
	Personal interface {
		ReflectJob(ctx context.Context, userID uuid.UUID) error
		TickDue(ctx context.Context, enqueue func(context.Context, uuid.UUID) error) error
	}
	// Scheduled fires due scheduled prompts every minute (schedule.tick)
	// and settles the runs it started; nil skips it.
	Scheduled interface {
		Tick(ctx context.Context) error
	}
	// Rental reconciles rented GPUs every minute (rental.reconcile); nil
	// skips it.
	Rental interface {
		Reconcile(ctx context.Context) error
	}
	// Budgets sends the budget warning mails (budget.warn, hourly); nil
	// skips it.
	Budgets interface {
		Warn(ctx context.Context) error
	}
	// Training runs dataset builds, fine-tunes and evals (internal/training);
	// nil leaves the queue without a worker.
	Training interface {
		Build(ctx context.Context, datasetID uuid.UUID) error
		RunFinetune(ctx context.Context, jobID uuid.UUID) error
		Evaluate(ctx context.Context, adapterID uuid.UUID) error
		RunSpec(ctx context.Context, recipeID uuid.UUID, replace bool) error
	}
}

// TrainingBuildArgs exports a dataset.
type TrainingBuildArgs struct {
	DatasetID uuid.UUID `json:"dataset_id"`
}

// Kind implements river.JobArgs.
func (TrainingBuildArgs) Kind() string { return "training.build" }

// InsertOpts: one build per dataset; a failure is on the row.
func (TrainingBuildArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: "training", MaxAttempts: 1, UniqueOpts: river.UniqueOpts{ByArgs: true}}
}

// TrainingFinetuneArgs runs one fine-tune job.
type TrainingFinetuneArgs struct {
	JobID uuid.UUID `json:"job_id"`
}

// Kind implements river.JobArgs.
func (TrainingFinetuneArgs) Kind() string { return "training.finetune" }

// InsertOpts: one River job per fine-tune; never retried (hours of GPU).
func (TrainingFinetuneArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: "training", MaxAttempts: 1, UniqueOpts: river.UniqueOpts{ByArgs: true}}
}

// TrainingEvalArgs runs the eval gate on an adapter.
type TrainingEvalArgs struct {
	AdapterID uuid.UUID `json:"adapter_id"`
}

// Kind implements river.JobArgs.
func (TrainingEvalArgs) Kind() string { return "training.eval" }

// InsertOpts: one eval at a time per adapter.
func (TrainingEvalArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: "training", MaxAttempts: 1, UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning, rivertype.JobStateRetryable, rivertype.JobStateScheduled}}}
}

// TrainingSpecArgs fleshes out a training recipe's spec (M12): samples the
// attached datasets and has a model write the draft.
type TrainingSpecArgs struct {
	RecipeID uuid.UUID `json:"recipe_id"`
	// Replace drops the owner's hand edits to the spec when the draft lands.
	Replace bool `json:"replace,omitempty"`
}

// Kind implements river.JobArgs.
func (TrainingSpecArgs) Kind() string { return "training.spec" }

// InsertOpts: one flesh out per recipe at a time; a failure is on the row.
func (TrainingSpecArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: "training", MaxAttempts: 1, UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning, rivertype.JobStateRetryable, rivertype.JobStateScheduled}}}
}

type trainingSpecWorker struct {
	river.WorkerDefaults[TrainingSpecArgs]
	deps *Deps
}

func (w *trainingSpecWorker) Timeout(*river.Job[TrainingSpecArgs]) time.Duration {
	return 15 * time.Minute
}

func (w *trainingSpecWorker) Work(ctx context.Context, job *river.Job[TrainingSpecArgs]) error {
	return w.deps.Training.RunSpec(ctx, job.Args.RecipeID, job.Args.Replace)
}

type trainingBuildWorker struct {
	river.WorkerDefaults[TrainingBuildArgs]
	deps *Deps
}

func (w *trainingBuildWorker) Timeout(*river.Job[TrainingBuildArgs]) time.Duration {
	return 30 * time.Minute
}

func (w *trainingBuildWorker) Work(ctx context.Context, job *river.Job[TrainingBuildArgs]) error {
	return w.deps.Training.Build(ctx, job.Args.DatasetID)
}

type trainingFinetuneWorker struct {
	river.WorkerDefaults[TrainingFinetuneArgs]
	deps *Deps
}

func (w *trainingFinetuneWorker) Timeout(*river.Job[TrainingFinetuneArgs]) time.Duration {
	return 8 * time.Hour
}

func (w *trainingFinetuneWorker) Work(ctx context.Context, job *river.Job[TrainingFinetuneArgs]) error {
	return w.deps.Training.RunFinetune(ctx, job.Args.JobID)
}

type trainingEvalWorker struct {
	river.WorkerDefaults[TrainingEvalArgs]
	deps *Deps
}

func (w *trainingEvalWorker) Timeout(*river.Job[TrainingEvalArgs]) time.Duration {
	return 2 * time.Hour
}

func (w *trainingEvalWorker) Work(ctx context.Context, job *river.Job[TrainingEvalArgs]) error {
	return w.deps.Training.Evaluate(ctx, job.Args.AdapterID)
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

// MemoryReflectArgs tidies one member's memory (PLAN M13 cut 3).
type MemoryReflectArgs struct {
	UserID uuid.UUID `json:"user_id"`
}

// Kind implements river.JobArgs.
func (MemoryReflectArgs) Kind() string { return "memory.reflect" }

// InsertOpts: one at a time per member, so a double click or a tick racing
// the button does not reflect twice.
func (MemoryReflectArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: "housekeeping", MaxAttempts: 2, UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning, rivertype.JobStateRetryable, rivertype.JobStateScheduled}}}
}

type memoryReflectWorker struct {
	river.WorkerDefaults[MemoryReflectArgs]
	deps *Deps
}

func (w *memoryReflectWorker) Timeout(*river.Job[MemoryReflectArgs]) time.Duration {
	return 10 * time.Minute
}

func (w *memoryReflectWorker) Work(ctx context.Context, job *river.Job[MemoryReflectArgs]) error {
	return w.deps.Personal.ReflectJob(ctx, job.Args.UserID)
}

// MemoryTickArgs is the periodic check for members whose nightly
// reflection is due.
type MemoryTickArgs struct{}

// Kind implements river.JobArgs.
func (MemoryTickArgs) Kind() string { return "memory.tick" }

type memoryTickWorker struct {
	river.WorkerDefaults[MemoryTickArgs]
	deps *Deps
	cl   *Client
}

func (w *memoryTickWorker) Work(ctx context.Context, job *river.Job[MemoryTickArgs]) error {
	return w.deps.Personal.TickDue(ctx, w.cl.EnqueueMemoryReflect)
}

// ScheduleTickArgs is the periodic check for due scheduled prompts.
type ScheduleTickArgs struct{}

// Kind implements river.JobArgs.
func (ScheduleTickArgs) Kind() string { return "schedule.tick" }

type scheduleTickWorker struct {
	river.WorkerDefaults[ScheduleTickArgs]
	deps *Deps
}

func (w *scheduleTickWorker) Timeout(*river.Job[ScheduleTickArgs]) time.Duration {
	return 2 * time.Minute
}

func (w *scheduleTickWorker) Work(ctx context.Context, job *river.Job[ScheduleTickArgs]) error {
	return w.deps.Scheduled.Tick(ctx)
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

// BudgetWarnArgs is the hourly budget warning sweep.
type BudgetWarnArgs struct{}

// Kind implements river.JobArgs.
func (BudgetWarnArgs) Kind() string { return "budget.warn" }

type budgetWarnWorker struct {
	river.WorkerDefaults[BudgetWarnArgs]
	deps *Deps
}

func (w *budgetWarnWorker) Timeout(*river.Job[BudgetWarnArgs]) time.Duration { return 5 * time.Minute }

func (w *budgetWarnWorker) Work(ctx context.Context, job *river.Job[BudgetWarnArgs]) error {
	return w.deps.Budgets.Warn(ctx)
}

// AuthPurgeArgs is the hourly deletion of expired sign-in rows.
type AuthPurgeArgs struct{}

// Kind implements river.JobArgs.
func (AuthPurgeArgs) Kind() string { return "auth.purge" }

type authPurgeWorker struct {
	river.WorkerDefaults[AuthPurgeArgs]
	deps *Deps
}

// Work deletes expired magic links, invites, sessions and passkey
// ceremonies. The sign-in routes that create these rows take no session,
// so without this the tables only grow.
func (w *authPurgeWorker) Work(ctx context.Context, job *river.Job[AuthPurgeArgs]) error {
	q := w.deps.DB
	var total int64
	var first error
	// Every purge runs even if an earlier one fails, so one broken query
	// does not stop the others; the first error is returned at the end so
	// the job is retried.
	for _, purge := range []func(context.Context) (int64, error){q.PurgeMagicLinks, q.PurgeInvites, q.PurgeSessions, q.PurgeExpiredWebAuthnSessions} {
		n, err := purge(ctx)
		if err != nil && first == nil {
			first = err
		}
		total += n
	}
	if total > 0 {
		w.deps.Log.Info("auth purge", "rows", total)
	}
	return first
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
	memCl := &Client{log: deps.Log} // its river client is set below, before the workers start
	workers := river.NewWorkers()
	cfg := &river.Config{
		Logger:               deps.Log,
		JobTimeout:           30 * time.Minute,
		RescueStuckJobsAfter: 45 * time.Minute,
	}
	if startWorkers {
		river.AddWorker(workers, &runWorker{deps: deps})
		river.AddWorker(workers, &resumeWorker{deps: deps})
		river.AddWorker(workers, &compactWorker{deps: deps})
		river.AddWorker(workers, &reapWorker{deps: deps})
		river.AddWorker(workers, &authPurgeWorker{deps: deps})
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
		cfg.PeriodicJobs = append(cfg.PeriodicJobs, river.NewPeriodicJob(river.PeriodicInterval(time.Hour), func() (river.JobArgs, *river.InsertOpts) {
			return AuthPurgeArgs{}, &river.InsertOpts{Queue: "housekeeping", UniqueOpts: river.UniqueOpts{ByPeriod: time.Hour}}
		}, &river.PeriodicJobOpts{RunOnStart: true}))
		if deps.Memory != nil {
			river.AddWorker(workers, &reflectWorker{deps: deps})
		}
		if deps.Personal != nil {
			river.AddWorker(workers, &memoryReflectWorker{deps: deps})
			river.AddWorker(workers, &memoryTickWorker{deps: deps, cl: memCl})
			cfg.PeriodicJobs = append(cfg.PeriodicJobs, river.NewPeriodicJob(river.PeriodicInterval(15*time.Minute), func() (river.JobArgs, *river.InsertOpts) {
				return MemoryTickArgs{}, &river.InsertOpts{Queue: "housekeeping", UniqueOpts: river.UniqueOpts{ByPeriod: 15 * time.Minute}}
			}, &river.PeriodicJobOpts{RunOnStart: false}))
		}
		if deps.Scheduled != nil {
			river.AddWorker(workers, &scheduleTickWorker{deps: deps})
			cfg.PeriodicJobs = append(cfg.PeriodicJobs, river.NewPeriodicJob(river.PeriodicInterval(time.Minute), func() (river.JobArgs, *river.InsertOpts) {
				return ScheduleTickArgs{}, &river.InsertOpts{Queue: "housekeeping", UniqueOpts: river.UniqueOpts{ByPeriod: time.Minute}}
			}, &river.PeriodicJobOpts{RunOnStart: true}))
		}
		if deps.Agents != nil {
			river.AddWorker(workers, &agentTickWorker{deps: deps})
			cfg.PeriodicJobs = append(cfg.PeriodicJobs, river.NewPeriodicJob(river.PeriodicInterval(time.Minute), func() (river.JobArgs, *river.InsertOpts) {
				return AgentTickArgs{}, &river.InsertOpts{Queue: "housekeeping", UniqueOpts: river.UniqueOpts{ByPeriod: time.Minute}}
			}, &river.PeriodicJobOpts{RunOnStart: true}))
		}
		if deps.Budgets != nil {
			river.AddWorker(workers, &budgetWarnWorker{deps: deps})
			cfg.PeriodicJobs = append(cfg.PeriodicJobs, river.NewPeriodicJob(river.PeriodicInterval(time.Hour), func() (river.JobArgs, *river.InsertOpts) {
				return BudgetWarnArgs{}, &river.InsertOpts{Queue: "housekeeping", UniqueOpts: river.UniqueOpts{ByPeriod: time.Hour}}
			}, &river.PeriodicJobOpts{RunOnStart: true}))
		}
		if deps.Rental != nil {
			river.AddWorker(workers, &rentalWorker{deps: deps})
			cfg.PeriodicJobs = append(cfg.PeriodicJobs, river.NewPeriodicJob(river.PeriodicInterval(time.Minute), func() (river.JobArgs, *river.InsertOpts) {
				return RentalTickArgs{}, &river.InsertOpts{Queue: "housekeeping", UniqueOpts: river.UniqueOpts{ByPeriod: time.Minute}}
			}, &river.PeriodicJobOpts{RunOnStart: true}))
		}
		if deps.Training != nil {
			river.AddWorker(workers, &trainingBuildWorker{deps: deps})
			river.AddWorker(workers, &trainingFinetuneWorker{deps: deps})
			river.AddWorker(workers, &trainingEvalWorker{deps: deps})
			river.AddWorker(workers, &trainingSpecWorker{deps: deps})
			cfg.Queues["training"] = river.QueueConfig{MaxWorkers: 1}
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
	memCl.c = c
	if startWorkers {
		if err := c.Start(ctx); err != nil {
			return nil, err
		}
	}
	return cl, nil
}

// Stop drains workers.
func (cl *Client) Stop(ctx context.Context) error { return cl.c.Stop(ctx) }

// EnqueueMemoryReflect queues a reflection of one member's memory.
func (cl *Client) EnqueueMemoryReflect(ctx context.Context, userID uuid.UUID) error {
	_, err := cl.c.Insert(ctx, MemoryReflectArgs{UserID: userID}, nil)
	return err
}

// EnqueueRun schedules a run to be driven by the worker.
func (cl *Client) EnqueueRun(ctx context.Context, runID uuid.UUID) error {
	_, err := cl.c.Insert(ctx, RunArgs{RunID: runID}, nil)
	return err
}

// EnqueueResume continues a run parked for approval (see ResumeArgs).
func (cl *Client) EnqueueResume(ctx context.Context, runID uuid.UUID) error {
	_, err := cl.c.Insert(ctx, ResumeArgs{RunID: runID}, nil)
	return err
}

// EnqueueTrainingBuild, EnqueueFinetune and EnqueueEval schedule the
// training flywheel's jobs.
func (cl *Client) EnqueueTrainingBuild(ctx context.Context, datasetID uuid.UUID) error {
	_, err := cl.c.Insert(ctx, TrainingBuildArgs{DatasetID: datasetID}, nil)
	return err
}

func (cl *Client) EnqueueFinetune(ctx context.Context, jobID uuid.UUID) error {
	_, err := cl.c.Insert(ctx, TrainingFinetuneArgs{JobID: jobID}, nil)
	return err
}

// EnqueueTrainingSpec schedules the flesh out of a recipe.
func (cl *Client) EnqueueTrainingSpec(ctx context.Context, recipeID uuid.UUID, replace bool) error {
	_, err := cl.c.Insert(ctx, TrainingSpecArgs{RecipeID: recipeID, Replace: replace}, nil)
	return err
}

// EnqueueEval reports queued=false when an evaluation of the adapter is already queued or
// running (the insert is unique by adapter and state), so a second click can say so.
func (cl *Client) EnqueueEval(ctx context.Context, adapterID uuid.UUID) (queued bool, err error) {
	res, err := cl.c.Insert(ctx, TrainingEvalArgs{AdapterID: adapterID}, nil)
	if err != nil {
		return false, err
	}
	return !res.UniqueSkippedAsDuplicate, nil
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
	return w.drive(ctx, job.Args.RunID)
}

// drive is the body shared with resumeWorker: a pause is a clean end, a run
// someone else holds (or that finished) cancels the job, anything else
// retries.
func (w *runWorker) drive(ctx context.Context, runID uuid.UUID) error {
	sink := &agent.NotifySink{Pool: w.deps.DB.Pool, Channel: agent.NotifyChannel(runID.String()), Log: w.deps.Log}
	err := w.deps.Runtime.Drive(ctx, runID, sink)
	switch {
	case err == nil:
		w.reflect(ctx, runID)
		return nil
	case errors.Is(err, agent.ErrPaused):
		return nil
	case errors.Is(err, agent.ErrRunNotClaimable):
		return river.JobCancel(err) // someone else has it, or it's finished
	}
	w.reflect(ctx, runID) // a failed run is still an episode worth keeping
	return err
}

type resumeWorker struct {
	river.WorkerDefaults[ResumeArgs]
	deps *Deps
}

func (w *resumeWorker) Timeout(*river.Job[ResumeArgs]) time.Duration { return 25 * time.Minute }

func (w *resumeWorker) Work(ctx context.Context, job *river.Job[ResumeArgs]) error {
	return (&runWorker{deps: w.deps}).drive(ctx, job.Args.RunID)
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

// Timeout bounds one generation; video engines poll inside it, and a
// hosted video render can take ten minutes or more.
func (w *mediaWorker) Timeout(*river.Job[MediaArgs]) time.Duration { return 25 * time.Minute }

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
