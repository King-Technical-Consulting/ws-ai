package training

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/fleet/rental"
	"github.com/jking323/ws/internal/store"
	"github.com/jking323/ws/internal/store/blob"
)

// fakeRenter plays the rental controller: Start records an instance that
// becomes ready at the trainer server's URL on the next look.
type fakeRenter struct {
	mu      sync.Mutex
	url     string
	rows    map[uuid.UUID]store.RentalInstance
	stops   []string
	touches int
	failAt  string // status to report instead of ready
}

func (f *fakeRenter) Start(_ context.Context, tpl string, user uuid.UUID) (*store.RentalInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if tpl != "trainer-test" {
		return nil, rental.ErrNoTemplate
	}
	row := store.RentalInstance{ID: uuid.New(), Provider: "fake", Template: tpl, Gpu: "A100", Status: rental.StatusProvisioning, StartedBy: store.NullUUID(user)}
	f.rows[row.ID] = row
	return &row, nil
}
func (f *fakeRenter) Instance(_ context.Context, id uuid.UUID) (store.RentalInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row := f.rows[id]
	if f.failAt != "" {
		row.Status = f.failAt
		msg := "no capacity"
		row.Error = &msg
	} else {
		u := f.url + "/v1"
		row.Status, row.BaseUrl = rental.StatusReady, &u
	}
	f.rows[id] = row
	return row, nil
}
func (f *fakeRenter) Stop(_ context.Context, id uuid.UUID, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops = append(f.stops, reason)
	row := f.rows[id]
	row.Status = rental.StatusStopped
	f.rows[id] = row
	return nil
}
func (f *fakeRenter) Touch(context.Context, uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touches++
	return nil
}

// trainerServer plays infra/training/serve.py: a job posted to /train
// runs through "running" for a few status polls, then done or failed.
type trainerServer struct {
	mu      sync.Mutex
	state   string
	polls   int
	fail    bool
	got     map[string]any
	cancels int
}

func (s *trainerServer) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer rk-test" {
			w.WriteHeader(401)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/train":
			if s.state == "running" {
				w.WriteHeader(409)
				return
			}
			s.got = map[string]any{}
			_ = json.NewDecoder(r.Body).Decode(&s.got)
			s.state, s.polls = "running", 0
			w.WriteHeader(202)
		case "GET /v1/status":
			st := map[string]any{"state": s.state, "progress": 0.0, "log": ""}
			if s.state == "running" {
				s.polls++
				st["progress"] = 0.25 * float64(s.polls)
				st["log"] = strings.Repeat("progress\n", s.polls)
				if s.polls >= 3 {
					if s.fail {
						s.state, st["state"], st["error"] = "failed", "failed", "CUDA out of memory"
					} else {
						s.state, st["state"], st["progress"] = "done", "done", 1.0
					}
				}
			}
			_ = json.NewEncoder(w).Encode(st)
		case "GET /v1/adapter":
			if s.state != "done" {
				w.WriteHeader(409)
				return
			}
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write([]byte("adapter-tar"))
		case "POST /v1/cancel":
			s.cancels++
			s.state = "idle"
			w.WriteHeader(200)
		default:
			w.WriteHeader(404)
		}
	}
}

func newRentalRunner(t *testing.T) (*RentalRunner, *fakeRenter, *trainerServer) {
	t.Helper()
	ts := &trainerServer{state: "idle"}
	srv := httptest.NewServer(ts.handler(t))
	t.Cleanup(srv.Close)
	fr := &fakeRenter{url: srv.URL, rows: map[uuid.UUID]store.RentalInstance{}}
	r := &RentalRunner{Rental: fr, Template: "trainer-test", Key: func() string { return "rk-test" }, Client: srv.Client(), Poll: 5 * time.Millisecond, ReadyTimeout: time.Second}
	return r, fr, ts
}

func TestRentalRunnerTrainsAndStops(t *testing.T) {
	r, fr, ts := newRentalRunner(t)
	owner := uuid.New()
	spec := RunSpec{JobID: uuid.New(), OwnerID: owner, BaseModel: "Qwen/Qwen3-8B", AdapterName: "chat-v1", Config: FinetuneConfig{Epochs: 2, Rank: 16, Target: TargetRental}, Train: []byte(`{"messages":[]}` + "\n"), Eval: []byte("")}
	var fracs []float64
	adapter, log, err := r.Run(context.Background(), spec, func(f float64, _ string) { fracs = append(fracs, f) })
	if err != nil {
		t.Fatal(err)
	}
	if string(adapter) != "adapter-tar" || !strings.Contains(log, "rented trainer-test on fake (A100)") || !strings.Contains(log, "trainer ready at") {
		t.Errorf("adapter=%q log=%q", adapter, log)
	}
	if len(fracs) < 3 || fracs[0] != 0 || fracs[len(fracs)-1] != 1 {
		t.Errorf("progress = %v", fracs)
	}
	if ts.got["base_model"] != "Qwen/Qwen3-8B" || ts.got["adapter_name"] != "chat-v1" || ts.got["train"] != `{"messages":[]}`+"\n" {
		t.Errorf("trainer got %+v", ts.got)
	}
	if cfg, _ := ts.got["config"].(map[string]any); cfg["epochs"] != float64(2) || cfg["rank"] != float64(16) {
		t.Errorf("trainer config = %+v", ts.got["config"])
	}
	if len(fr.stops) != 1 || fr.stops[0] != "fine-tune finished" || fr.touches == 0 {
		t.Errorf("stops=%v touches=%d", fr.stops, fr.touches)
	}
	for _, row := range fr.rows {
		if !row.StartedBy.Valid || row.StartedBy.UUID != owner {
			t.Errorf("the machine was not billed to the job's owner: %+v", row.StartedBy)
		}
	}
}

func TestRentalRunnerFailuresStopTheMachine(t *testing.T) {
	// The trainer fails: the error carries its message, the machine stops.
	r, fr, ts := newRentalRunner(t)
	ts.fail = true
	_, log, err := r.Run(context.Background(), RunSpec{JobID: uuid.New(), Config: FinetuneConfig{}}, nil)
	if err == nil || !strings.Contains(err.Error(), "CUDA out of memory") || !strings.Contains(log, "progress") {
		t.Errorf("failed run: err=%v log=%q", err, log)
	}
	if len(fr.stops) != 1 || fr.stops[0] != "fine-tune failed" {
		t.Errorf("stops = %v", fr.stops)
	}
	// The rental controller never gets the machine ready: no train call,
	// the machine (whatever is left of it) is stopped.
	r, fr, ts = newRentalRunner(t)
	fr.failAt = rental.StatusFailed
	_, _, err = r.Run(context.Background(), RunSpec{JobID: uuid.New()}, nil)
	if err == nil || !strings.Contains(err.Error(), "failed: no capacity") || ts.got != nil || len(fr.stops) != 1 {
		t.Errorf("not ready: err=%v got=%v stops=%v", err, ts.got, fr.stops)
	}
	// An unknown template is refused before anything is rented.
	r, fr, _ = newRentalRunner(t)
	r.Template = "nope"
	if _, _, err := r.Run(context.Background(), RunSpec{}, nil); !errors.Is(err, rental.ErrNoTemplate) || len(fr.stops) != 0 {
		t.Errorf("unknown template: %v stops=%v", err, fr.stops)
	}
	// No renter at all.
	if _, _, err := (&RentalRunner{}).Run(context.Background(), RunSpec{}, nil); !errors.Is(err, ErrNoRunner) {
		t.Errorf("no renter: %v", err)
	}
}

func TestRentalRunnerCancelStopsTrainerAndMachine(t *testing.T) {
	r, fr, ts := newRentalRunner(t)
	ctx, cancel := context.WithCancel(context.Background())
	// A slow trainer: cancel after the first progress report.
	ts.mu.Lock()
	ts.fail = false
	ts.mu.Unlock()
	_, _, err := r.Run(ctx, RunSpec{JobID: uuid.New()}, func(f float64, _ string) {
		if f > 0 {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	ts.mu.Lock()
	cancels := ts.cancels
	ts.mu.Unlock()
	if cancels != 1 || len(fr.stops) != 1 || fr.stops[0] != "fine-tune cancelled" {
		t.Errorf("cancels=%d stops=%v", cancels, fr.stops)
	}
}

func TestTargetsPickAndList(t *testing.T) {
	local, rented := &fakeRunner{}, &fakeRunner{}
	both := &Targets{Local: local, Rental: rented, RentalLabel: "trainer-a100"}
	if l := both.List(); len(l) != 2 || l[0].ID != TargetLocal || l[1].ID != TargetRental || !strings.Contains(l[1].Label, "trainer-a100") {
		t.Errorf("list = %+v", l)
	}
	if r, _ := both.Pick(""); r != local {
		t.Error("default should be local")
	}
	if r, _ := both.Pick(TargetRental); r != rented {
		t.Error("rental pick")
	}
	if _, err := both.Pick("mac"); !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown target: %v", err)
	}
	only := &Targets{Rental: rented}
	if r, _ := only.Pick(""); r != rented {
		t.Error("default should fall back to rental")
	}
	if _, err := only.Pick(TargetLocal); !errors.Is(err, ErrInvalid) {
		t.Errorf("missing local: %v", err)
	}
	if _, err := (&Targets{}).Pick(""); !errors.Is(err, ErrNoRunner) {
		t.Errorf("nothing: %v", err)
	}
	// Run dispatches on the spec's target.
	if _, _, err := both.Run(context.Background(), RunSpec{Config: FinetuneConfig{Target: TargetRental}}, nil); err != nil || rented.spec.Config.Target != TargetRental || local.spec.Config.Target != "" {
		t.Errorf("dispatch: %v local=%+v rented=%+v", err, local.spec, rented.spec)
	}
	// The service reports the targets of whatever runner it has.
	s := &Service{}
	if s.Targets() != nil {
		t.Error("no runner should list nothing")
	}
	s.Runner = local
	if l := s.Targets(); len(l) != 1 || l[0].ID != TargetLocal {
		t.Errorf("plain runner = %+v", l)
	}
	s.Runner = both
	if l := s.Targets(); len(l) != 2 {
		t.Errorf("targets runner = %+v", l)
	}
}

func TestStartFinetuneValidatesTarget(t *testing.T) {
	db := newFake()
	svc := newService(t, db)
	svc.Runner = &Targets{Local: &fakeRunner{}}
	ctx := context.Background()
	owner := uuid.New()
	ds, _ := db.CreateDataset(ctx, store.CreateDatasetParams{OwnerID: owner, Name: "d", Filters: []byte("{}")})
	key := "k"
	_ = db.FinishDataset(ctx, store.FinishDatasetParams{ID: ds.ID, BlobKey: &key, Examples: 1})
	if _, err := svc.StartFinetune(ctx, owner, FinetuneSpec{DatasetID: ds.ID, BaseModel: "m", AdapterName: "a-1", Config: FinetuneConfig{Target: TargetRental}}); err == nil || !strings.Contains(err.Error(), "no rented trainer") {
		t.Errorf("rental without a renter: %v", err)
	}
	job, err := svc.StartFinetune(ctx, owner, FinetuneSpec{DatasetID: ds.ID, BaseModel: "m", AdapterName: "a-1", Config: FinetuneConfig{Target: TargetLocal}})
	if err != nil {
		t.Fatal(err)
	}
	var cfg FinetuneConfig
	_ = json.Unmarshal(job.Config, &cfg)
	if cfg.Target != TargetLocal {
		t.Errorf("stored config = %+v", cfg)
	}
	// The serve role has no runner, only the declared targets: it accepts
	// the job and stores the default target explicitly.
	serve := newService(t, db)
	serve.Available = []Target{{ID: TargetRental, Label: "rented GPU (trainer-a100)"}}
	if l := serve.Targets(); len(l) != 1 || l[0].ID != TargetRental {
		t.Errorf("declared targets = %+v", l)
	}
	job, err = serve.StartFinetune(ctx, owner, FinetuneSpec{DatasetID: ds.ID, BaseModel: "m", AdapterName: "a-2"})
	if err != nil {
		t.Fatalf("serve role: %v", err)
	}
	cfg = FinetuneConfig{}
	_ = json.Unmarshal(job.Config, &cfg)
	if cfg.Target != TargetRental {
		t.Errorf("default target not stored: %+v", cfg)
	}
	if _, err := serve.StartFinetune(ctx, owner, FinetuneSpec{DatasetID: ds.ID, BaseModel: "m", AdapterName: "a-3", Config: FinetuneConfig{Target: TargetLocal}}); err == nil || !strings.Contains(err.Error(), "no trainer image") {
		t.Errorf("undeclared local target: %v", err)
	}
	if _, err := serve.StartFinetune(ctx, owner, FinetuneSpec{DatasetID: ds.ID, BaseModel: "m", AdapterName: "a-4", Config: FinetuneConfig{Target: "mac"}}); err == nil || !strings.Contains(err.Error(), "unknown target") {
		t.Errorf("unknown target: %v", err)
	}
	// Nothing declared and no runner: refused up front, as before.
	if _, err := newService(t, db).StartFinetune(ctx, owner, FinetuneSpec{DatasetID: ds.ID, BaseModel: "m", AdapterName: "a-5"}); !errors.Is(err, ErrNoRunner) {
		t.Errorf("no targets: %v", err)
	}
}

func TestRunFinetuneCancelEndsTheRunner(t *testing.T) {
	db := newFake()
	svc := newService(t, db)
	old := CancelPoll
	CancelPoll = 5 * time.Millisecond
	t.Cleanup(func() { CancelPoll = old })
	ctx := context.Background()
	owner := uuid.New()
	ds, _ := db.CreateDataset(ctx, store.CreateDatasetParams{OwnerID: owner, Name: "d", Filters: []byte("{}")})
	tb := JSONL([]Example{{Messages: []ChatMessage{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "yo"}}}})
	key, err := blob.PutBytes(ctx, svc.Blobs, tb)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.FinishDataset(ctx, store.FinishDatasetParams{ID: ds.ID, BlobKey: &key, Examples: 1})
	// A runner that blocks until its context ends.
	blocking := &blockingRunner{started: make(chan struct{})}
	svc.Runner = blocking
	job, err := svc.StartFinetune(ctx, owner, FinetuneSpec{DatasetID: ds.ID, BaseModel: "m", AdapterName: "a-1"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- svc.RunFinetune(ctx, job.ID) }()
	<-blocking.started
	_ = svc.CancelFinetune(ctx, job.ID)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunFinetune did not return after the cancel")
	}
	if !errors.Is(blocking.err, context.Canceled) {
		t.Errorf("runner context: %v", blocking.err)
	}
	if got, _ := db.GetFinetuneJob(ctx, job.ID); got.Status != StatusCancelled {
		t.Errorf("job after cancel = %s", got.Status)
	}
}

type blockingRunner struct {
	started chan struct{}
	err     error
}

func (b *blockingRunner) Run(ctx context.Context, _ RunSpec, _ func(float64, string)) ([]byte, string, error) {
	close(b.started)
	<-ctx.Done()
	b.err = ctx.Err()
	return nil, "", ctx.Err()
}
