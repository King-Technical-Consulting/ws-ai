// Package training is the training flywheel (PLAN.md M10, first cut):
// ratings on answers, datasets exported from opted-in conversations as
// chat-format JSONL, fine-tune jobs that run a trainer container and
// produce a LoRA adapter, an adapter registry whose rows are endpoints,
// and an eval gate that judges an adapter against its base on the
// held-out split before it is promoted.
package training

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
	"github.com/jking323/ws/internal/store/blob"
)

// Errors callers map to HTTP statuses.
var (
	ErrInvalid  = errors.New("training: invalid")
	ErrNotFound = errors.New("training: not found")
	ErrNoRunner = errors.New("training: no fine-tune runner is configured (set WS_FINETUNE_IMAGE on a worker with Docker and a GPU)")
	ErrNotReady = errors.New("training: not ready")
)

// Statuses.
const (
	StatusQueued    = "queued"
	StatusBuilding  = "building"
	StatusReady     = "ready"
	StatusRunning   = "running"
	StatusDone      = "done"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

// Store is the slice of the store the service uses (*store.DB implements it).
type Store interface {
	ListTrainingConversations(ctx context.Context, arg store.ListTrainingConversationsParams) ([]store.Conversation, error)
	ListMessages(ctx context.Context, conversationID uuid.UUID) ([]store.Message, error)
	ListRatingsForConversation(ctx context.Context, conversationID uuid.UUID) ([]store.MessageRating, error)

	CreateDataset(ctx context.Context, arg store.CreateDatasetParams) (store.Dataset, error)
	GetDataset(ctx context.Context, id uuid.UUID) (store.Dataset, error)
	ListDatasets(ctx context.Context, limit int32) ([]store.Dataset, error)
	SetDatasetStatus(ctx context.Context, arg store.SetDatasetStatusParams) error
	FinishDataset(ctx context.Context, arg store.FinishDatasetParams) error
	DeleteDataset(ctx context.Context, id uuid.UUID) error

	CreateFinetuneJob(ctx context.Context, arg store.CreateFinetuneJobParams) (store.FinetuneJob, error)
	GetFinetuneJob(ctx context.Context, id uuid.UUID) (store.FinetuneJob, error)
	ListFinetuneJobs(ctx context.Context, limit int32) ([]store.FinetuneJob, error)
	ClaimFinetuneJob(ctx context.Context, id uuid.UUID) (store.FinetuneJob, error)
	SetFinetuneProgress(ctx context.Context, arg store.SetFinetuneProgressParams) error
	FinishFinetuneJob(ctx context.Context, arg store.FinishFinetuneJobParams) error
	CancelFinetuneJob(ctx context.Context, id uuid.UUID) error

	CreateAdapter(ctx context.Context, arg store.CreateAdapterParams) (store.Adapter, error)
	GetAdapter(ctx context.Context, id uuid.UUID) (store.Adapter, error)
	ListAdapters(ctx context.Context, limit int32) ([]store.Adapter, error)
	SetAdapterEval(ctx context.Context, arg store.SetAdapterEvalParams) error
	SetAdapterPromoted(ctx context.Context, arg store.SetAdapterPromotedParams) error
	DeleteAdapter(ctx context.Context, id uuid.UUID) error

	UpsertEndpoint(ctx context.Context, arg store.UpsertEndpointParams) error
	SetEndpointEnabled(ctx context.Context, arg store.SetEndpointEnabledParams) error
	DeleteEndpoint(ctx context.Context, id string) error
}

// Completer is the slice of the gateway the eval gate uses.
type Completer interface {
	Complete(ctx context.Context, req *gateway.Request) (*gateway.Response, error)
}

// FinetuneConfig is what a job tells the trainer.
type FinetuneConfig struct {
	Epochs       int     `json:"epochs"`
	LearningRate float64 `json:"learning_rate"`
	Rank         int     `json:"rank"`
	Alpha        int     `json:"alpha"`
	MaxSeqLen    int     `json:"max_seq_len"`
	// Image overrides the worker's trainer image for this job (local
	// target only; a rented trainer runs its template's image).
	Image string `json:"image,omitempty"`
	// Target is where the job runs: local (the worker's Docker and GPU) or
	// rental (a trainer-kind rental template); empty takes the default.
	Target string `json:"target,omitempty"`
}

func (c *FinetuneConfig) defaults() {
	if c.Epochs <= 0 {
		c.Epochs = 2
	}
	if c.LearningRate <= 0 {
		c.LearningRate = 2e-4
	}
	if c.Rank <= 0 {
		c.Rank = 16
	}
	if c.Alpha <= 0 {
		c.Alpha = 2 * c.Rank
	}
	if c.MaxSeqLen <= 0 {
		c.MaxSeqLen = 4096
	}
}

// RunSpec is what a Runner gets for one job.
type RunSpec struct {
	JobID       uuid.UUID
	OwnerID     uuid.UUID // who started it (a rented machine is billed to them)
	BaseModel   string
	AdapterName string
	Config      FinetuneConfig
	Train, Eval []byte // JSONL
}

// Runner trains an adapter. progress gets a fraction and the log tail as
// the run goes. It returns the adapter as a gzipped tar of the trainer's
// output directory (adapter_config.json, adapter_model.safetensors, …).
type Runner interface {
	Run(ctx context.Context, spec RunSpec, progress func(frac float64, log string)) (adapter []byte, log string, err error)
}

// Service owns datasets, fine-tune jobs and adapters.
type Service struct {
	DB    Store
	Blobs blob.Store
	GW    Completer
	// Endpoint looks an endpoint up in the gateway registry (an adapter is
	// registered next to its base); nil means no registration.
	Endpoint func(id string) (*gateway.Endpoint, bool)
	// Reload reloads the gateway registry after an endpoint row changes.
	Reload func(ctx context.Context) error
	// Runner trains; it exists on the worker only.
	Runner Runner
	// Available declares where this deployment can run a fine-tune job
	// (from the config, in the serve role as well as the worker, since the
	// routes that accept a job are served by serve). Empty falls back to
	// what Runner offers; neither means jobs are refused up front.
	Available []Target
	// Enqueue hands a dataset build, a fine-tune or an eval to the worker.
	EnqueueBuild    func(ctx context.Context, datasetID uuid.UUID) error
	EnqueueFinetune func(ctx context.Context, jobID uuid.UUID) error
	EnqueueEval     func(ctx context.Context, adapterID uuid.UUID) error
	// JudgeSelector picks the model that scores eval answers (default auto
	// under task class classify, so the policy routes it cheap).
	JudgeSelector string
	// EvalLimit caps the held-out examples one eval runs (default 40).
	EvalLimit int
	Log       *slog.Logger
	Now       func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// ---- datasets ----

// DatasetSpec creates a dataset.
type DatasetSpec struct {
	Name      string
	TaskClass string
	Filters   Filters
}

// CreateDataset records a dataset and queues its build.
func (s *Service) CreateDataset(ctx context.Context, ownerID uuid.UUID, spec DatasetSpec) (*store.Dataset, error) {
	spec.Name = strings.TrimSpace(spec.Name)
	if spec.Name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrInvalid)
	}
	if len(spec.Name) > 120 {
		return nil, fmt.Errorf("%w: name is too long", ErrInvalid)
	}
	spec.Filters.defaults()
	for _, m := range spec.Filters.Modes {
		switch m {
		case "chat", "code", "design", "images", "agent":
		default:
			return nil, fmt.Errorf("%w: unknown mode %q", ErrInvalid, m)
		}
	}
	raw, _ := json.Marshal(spec.Filters)
	ds, err := s.DB.CreateDataset(ctx, store.CreateDatasetParams{OwnerID: ownerID, Name: spec.Name, TaskClass: spec.TaskClass, Filters: raw})
	if err != nil {
		return nil, err
	}
	if s.EnqueueBuild != nil {
		if err := s.EnqueueBuild(ctx, ds.ID); err != nil {
			msg := "could not enqueue: " + err.Error()
			_ = s.DB.SetDatasetStatus(ctx, store.SetDatasetStatusParams{ID: ds.ID, Status: StatusFailed, Error: &msg})
			return nil, fmt.Errorf("training: enqueue: %w", err)
		}
	}
	return &ds, nil
}

// Build exports a dataset: the opted-in conversations the filters select,
// one example per kept assistant turn, split into a train and an eval
// file in the blob store.
func (s *Service) Build(ctx context.Context, datasetID uuid.UUID) error {
	ds, err := s.DB.GetDataset(ctx, datasetID)
	if err != nil {
		return nil // gone
	}
	if ds.Status != StatusQueued {
		return nil
	}
	_ = s.DB.SetDatasetStatus(ctx, store.SetDatasetStatusParams{ID: ds.ID, Status: StatusBuilding})
	fail := func(err error) error {
		msg := err.Error()
		_ = s.DB.SetDatasetStatus(ctx, store.SetDatasetStatusParams{ID: ds.ID, Status: StatusFailed, Error: &msg})
		s.log().Warn("training: dataset build failed", "dataset", ds.ID, "err", msg)
		return nil
	}
	var f Filters
	_ = json.Unmarshal(ds.Filters, &f)
	f.defaults()
	since := f.Since
	if since.IsZero() {
		since = time.Unix(0, 0)
	}
	convs, err := s.DB.ListTrainingConversations(ctx, store.ListTrainingConversationsParams{Modes: f.Modes, Since: since, MaxConversations: int32(f.MaxConversations)})
	if err != nil {
		return fail(err)
	}
	var train, eval []Example
	for _, c := range convs {
		if len(train) >= f.MaxExamples {
			break
		}
		msgs, err := s.DB.ListMessages(ctx, c.ID)
		if err != nil {
			return fail(err)
		}
		rows, _ := s.DB.ListRatingsForConversation(ctx, c.ID)
		rt := ratings{}
		for _, r := range rows {
			if cur, ok := rt[r.MessageID]; !ok || int(r.Score) > cur {
				rt[r.MessageID] = int(r.Score)
			}
		}
		exs := Examples(c, msgs, rt, f)
		if Holdout(c.ID, f.HoldoutPct) {
			eval = append(eval, exs...)
		} else {
			train = append(train, exs...)
		}
	}
	if len(train) > f.MaxExamples {
		train = train[:f.MaxExamples]
	}
	if len(train) == 0 {
		return fail(errors.New("no examples matched: no opted-in conversations with answers that pass the filters"))
	}
	tb := JSONL(train)
	key, err := blob.PutBytes(ctx, s.Blobs, tb)
	if err != nil {
		return fail(err)
	}
	var evalKey *string
	if len(eval) > 0 {
		k, err := blob.PutBytes(ctx, s.Blobs, JSONL(eval))
		if err != nil {
			return fail(err)
		}
		evalKey = &k
	}
	if err := s.DB.FinishDataset(ctx, store.FinishDatasetParams{ID: ds.ID, BlobKey: &key, EvalBlobKey: evalKey, Bytes: int64(len(tb)), Examples: int32(len(train)), EvalExamples: int32(len(eval))}); err != nil {
		return fail(err)
	}
	s.log().Info("training: dataset built", "dataset", ds.ID, "examples", len(train), "eval", len(eval), "conversations", len(convs))
	return nil
}

// DatasetFile returns a dataset's train (or eval) JSONL.
func (s *Service) DatasetFile(ctx context.Context, id uuid.UUID, eval bool) ([]byte, error) {
	ds, err := s.DB.GetDataset(ctx, id)
	if err != nil {
		return nil, ErrNotFound
	}
	key := ds.BlobKey
	if eval {
		key = ds.EvalBlobKey
	}
	if ds.Status != StatusReady || key == nil {
		return nil, fmt.Errorf("%w: the dataset is %s", ErrNotReady, ds.Status)
	}
	return blob.GetBytes(ctx, s.Blobs, *key)
}

// ---- fine-tune jobs ----

var adapterNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,63}$`)

// FinetuneSpec starts a job.
type FinetuneSpec struct {
	DatasetID uuid.UUID
	// BaseModel is the model id the trainer loads (a Hugging Face id or a
	// path the trainer image can see); BaseEndpointID names the ws
	// endpoint serving that base, so the adapter can be registered next
	// to it.
	BaseModel      string
	BaseEndpointID string
	AdapterName    string
	Config         FinetuneConfig
}

// StartFinetune validates and queues a fine-tune job. The job is accepted
// when the deployment declares a target for it (Available, set from the
// config in every role); the worker's Runner does the run.
func (s *Service) StartFinetune(ctx context.Context, ownerID uuid.UUID, spec FinetuneSpec) (*store.FinetuneJob, error) {
	avail := s.Targets()
	if len(avail) == 0 {
		return nil, ErrNoRunner
	}
	ds, err := s.DB.GetDataset(ctx, spec.DatasetID)
	if err != nil {
		return nil, fmt.Errorf("%w: dataset not found", ErrInvalid)
	}
	if ds.Status != StatusReady || ds.BlobKey == nil {
		return nil, fmt.Errorf("%w: the dataset is %s", ErrInvalid, ds.Status)
	}
	spec.AdapterName = strings.ToLower(strings.TrimSpace(spec.AdapterName))
	if !adapterNameRe.MatchString(spec.AdapterName) {
		return nil, fmt.Errorf("%w: adapter name: lowercase letters, digits, dots, dashes and underscores, 2 to 64 characters", ErrInvalid)
	}
	spec.BaseModel = strings.TrimSpace(spec.BaseModel)
	if spec.BaseEndpointID != "" && s.Endpoint != nil {
		ep, ok := s.Endpoint(spec.BaseEndpointID)
		if !ok {
			return nil, fmt.Errorf("%w: unknown base endpoint %q", ErrInvalid, spec.BaseEndpointID)
		}
		if spec.BaseModel == "" {
			spec.BaseModel = ep.ModelName
		}
	}
	if spec.BaseModel == "" {
		return nil, fmt.Errorf("%w: base model is required", ErrInvalid)
	}
	spec.Config.defaults()
	// The target is stored explicitly so the job row says where it ran.
	if spec.Config.Target == "" {
		spec.Config.Target = avail[0].ID
	}
	if !hasTarget(avail, spec.Config.Target) {
		switch spec.Config.Target {
		case TargetLocal:
			return nil, fmt.Errorf("%w: no trainer image on the worker (WS_FINETUNE_IMAGE)", ErrInvalid)
		case TargetRental:
			return nil, fmt.Errorf("%w: no rented trainer (WS_FINETUNE_TEMPLATE and a rental provider)", ErrInvalid)
		}
		return nil, fmt.Errorf("%w: unknown target %q", ErrInvalid, spec.Config.Target)
	}
	cfg, _ := json.Marshal(spec.Config)
	var baseEP *string
	if spec.BaseEndpointID != "" {
		baseEP = &spec.BaseEndpointID
	}
	job, err := s.DB.CreateFinetuneJob(ctx, store.CreateFinetuneJobParams{OwnerID: ownerID, DatasetID: store.NullUUID(ds.ID), BaseModel: spec.BaseModel, BaseEndpointID: baseEP, AdapterName: spec.AdapterName, Config: cfg})
	if err != nil {
		return nil, err
	}
	if s.EnqueueFinetune != nil {
		if err := s.EnqueueFinetune(ctx, job.ID); err != nil {
			msg := "could not enqueue: " + err.Error()
			_ = s.DB.FinishFinetuneJob(ctx, store.FinishFinetuneJobParams{ID: job.ID, Status: StatusFailed, Error: &msg})
			return nil, fmt.Errorf("training: enqueue: %w", err)
		}
	}
	return &job, nil
}

// RunFinetune is the training.finetune job: it runs the trainer on the
// dataset, stores the adapter and registers it.
func (s *Service) RunFinetune(ctx context.Context, jobID uuid.UUID) error {
	job, err := s.DB.ClaimFinetuneJob(ctx, jobID)
	if err != nil {
		return nil // not queued any more
	}
	fail := func(log string, err error) error {
		msg := err.Error()
		_ = s.DB.FinishFinetuneJob(ctx, store.FinishFinetuneJobParams{ID: job.ID, Status: StatusFailed, Error: &msg, Log: tail(log, 64<<10)})
		s.log().Warn("training: fine-tune failed", "job", job.ID, "err", msg)
		return nil
	}
	if s.Runner == nil {
		return fail("", ErrNoRunner)
	}
	if !job.DatasetID.Valid {
		return fail("", errors.New("the dataset was deleted"))
	}
	train, err := s.DatasetFile(ctx, job.DatasetID.UUID, false)
	if err != nil {
		return fail("", err)
	}
	eval, _ := s.DatasetFile(ctx, job.DatasetID.UUID, true)
	var cfg FinetuneConfig
	_ = json.Unmarshal(job.Config, &cfg)
	cfg.defaults()
	spec := RunSpec{JobID: job.ID, OwnerID: job.OwnerID, BaseModel: job.BaseModel, AdapterName: job.AdapterName, Config: cfg, Train: train, Eval: eval}
	progress := func(frac float64, log string) {
		_ = s.DB.SetFinetuneProgress(context.WithoutCancel(ctx), store.SetFinetuneProgressParams{ID: job.ID, Progress: float32(frac), Log: tail(log, 64<<10)})
	}
	// A cancel from the page ends the runner's context: the container is
	// removed, a rented machine stopped.
	rctx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	go s.watchCancel(rctx, job.ID, cancelRun)
	adapter, log, err := s.Runner.Run(rctx, spec, progress)
	if err != nil {
		if cur, gerr := s.DB.GetFinetuneJob(ctx, job.ID); gerr == nil && cur.Status == StatusCancelled {
			return nil
		}
		return fail(log, err)
	}
	if len(adapter) == 0 {
		return fail(log, errors.New("the trainer produced no adapter"))
	}
	key, err := blob.PutBytes(ctx, s.Blobs, adapter)
	if err != nil {
		return fail(log, err)
	}
	ad, err := s.registerAdapter(ctx, job, key, int64(len(adapter)))
	if err != nil {
		return fail(log, err)
	}
	if err := s.DB.FinishFinetuneJob(ctx, store.FinishFinetuneJobParams{ID: job.ID, Status: StatusDone, AdapterID: store.NullUUID(ad.ID), Log: tail(log, 64<<10)}); err != nil {
		return err
	}
	s.log().Info("training: adapter ready", "job", job.ID, "adapter", ad.ID, "name", ad.Name, "bytes", len(adapter))
	return nil
}

// registerAdapter records the adapter and, when the base endpoint is
// known, an endpoint row for it next to the base: same provider and
// capabilities, model name = the adapter name (what vLLM serves a LoRA
// module as), disabled until the operator loads the adapter on the
// server and enables it, or promotes it here.
func (s *Service) registerAdapter(ctx context.Context, job store.FinetuneJob, key string, size int64) (*store.Adapter, error) {
	var epID *string
	if job.BaseEndpointID != nil && s.Endpoint != nil {
		if base, ok := s.Endpoint(*job.BaseEndpointID); ok {
			id := "lora/" + job.AdapterName
			caps, _ := json.Marshal(base.Capabilities)
			pricing, _ := json.Marshal(base.Pricing)
			extra := base.ExtraBody
			if extra == nil {
				extra = map[string]any{}
			}
			eb, _ := json.Marshal(extra)
			if err := s.DB.UpsertEndpoint(ctx, store.UpsertEndpointParams{
				ID: id, ProviderID: base.ProviderID, ModelName: job.AdapterName, DisplayName: job.AdapterName + " (adapter on " + base.DisplayName + ")",
				Capabilities: caps, Pricing: pricing, ThroughputClass: base.ThroughputClass, LatencyClass: base.LatencyClass, IsLocal: base.Local, Enabled: false, ExtraBody: eb,
			}); err != nil {
				return nil, fmt.Errorf("register endpoint: %w", err)
			}
			_ = s.DB.SetEndpointEnabled(ctx, store.SetEndpointEnabledParams{ID: id, Enabled: false})
			epID = &id
			s.reload(ctx)
		}
	}
	ad, err := s.DB.CreateAdapter(ctx, store.CreateAdapterParams{Name: job.AdapterName, BaseModel: job.BaseModel, BaseEndpointID: job.BaseEndpointID, FinetuneJobID: store.NullUUID(job.ID), BlobKey: key, Bytes: size, EndpointID: epID})
	if err != nil {
		return nil, err
	}
	return &ad, nil
}

// CancelFinetune marks a job cancelled; the worker running it sees the
// status (watchCancel) and ends the runner's context.
func (s *Service) CancelFinetune(ctx context.Context, id uuid.UUID) error {
	return s.DB.CancelFinetuneJob(ctx, id)
}

// CancelPoll is how often a running job checks for a cancel (default 15s).
var CancelPoll = 15 * time.Second

func (s *Service) watchCancel(ctx context.Context, jobID uuid.UUID, cancel context.CancelFunc) {
	t := time.NewTicker(CancelPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if cur, err := s.DB.GetFinetuneJob(ctx, jobID); err == nil && cur.Status == StatusCancelled {
				cancel()
				return
			}
		}
	}
}

// AdapterFile returns the adapter tarball.
func (s *Service) AdapterFile(ctx context.Context, id uuid.UUID) (*store.Adapter, []byte, error) {
	ad, err := s.DB.GetAdapter(ctx, id)
	if err != nil {
		return nil, nil, ErrNotFound
	}
	b, err := blob.GetBytes(ctx, s.Blobs, ad.BlobKey)
	return &ad, b, err
}

// DeleteAdapter removes the adapter, its endpoint row and its blob.
func (s *Service) DeleteAdapter(ctx context.Context, id uuid.UUID) error {
	ad, err := s.DB.GetAdapter(ctx, id)
	if err != nil {
		return ErrNotFound
	}
	if ad.EndpointID != nil {
		_ = s.DB.DeleteEndpoint(ctx, *ad.EndpointID)
		s.reload(ctx)
	}
	if err := s.DB.DeleteAdapter(ctx, id); err != nil {
		return err
	}
	_ = s.Blobs.Delete(ctx, ad.BlobKey)
	return nil
}

// Promote enables an adapter's endpoint so the router can use it, after
// the eval gate: the adapter must have been evaluated and scored at
// least its base, unless force. Unpromote disables it again.
func (s *Service) Promote(ctx context.Context, id uuid.UUID, force bool) error {
	ad, err := s.DB.GetAdapter(ctx, id)
	if err != nil {
		return ErrNotFound
	}
	if ad.EndpointID == nil {
		return fmt.Errorf("%w: the adapter has no endpoint (its job named no base endpoint); register one in Admin", ErrInvalid)
	}
	if !force {
		if ad.EvalScore == nil || ad.BaselineScore == nil {
			return fmt.Errorf("%w: evaluate the adapter first (or force)", ErrNotReady)
		}
		if *ad.EvalScore < *ad.BaselineScore {
			return fmt.Errorf("%w: the adapter scored %.2f against the base's %.2f (or force)", ErrNotReady, *ad.EvalScore, *ad.BaselineScore)
		}
	}
	if err := s.DB.SetEndpointEnabled(ctx, store.SetEndpointEnabledParams{ID: *ad.EndpointID, Enabled: true}); err != nil {
		return err
	}
	if err := s.DB.SetAdapterPromoted(ctx, store.SetAdapterPromotedParams{ID: id, Promoted: true}); err != nil {
		return err
	}
	s.reload(ctx)
	return nil
}

// Unpromote disables the adapter's endpoint.
func (s *Service) Unpromote(ctx context.Context, id uuid.UUID) error {
	ad, err := s.DB.GetAdapter(ctx, id)
	if err != nil {
		return ErrNotFound
	}
	if ad.EndpointID != nil {
		_ = s.DB.SetEndpointEnabled(ctx, store.SetEndpointEnabledParams{ID: *ad.EndpointID, Enabled: false})
		s.reload(ctx)
	}
	return s.DB.SetAdapterPromoted(ctx, store.SetAdapterPromotedParams{ID: id, Promoted: false})
}

func (s *Service) reload(ctx context.Context) {
	if s.Reload != nil {
		if err := s.Reload(ctx); err != nil {
			s.log().Warn("training: registry reload", "err", err)
		}
	}
}

// tail keeps the last n bytes of a log.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
