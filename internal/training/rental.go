package training

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/fleet/rental"
	"github.com/jking323/ws/internal/store"
)

// Renter is the slice of the rental controller the RentalRunner drives
// (*rental.Controller implements it).
type Renter interface {
	Start(ctx context.Context, template string, userID uuid.UUID) (*store.RentalInstance, error)
	Instance(ctx context.Context, id uuid.UUID) (store.RentalInstance, error)
	Stop(ctx context.Context, id uuid.UUID, reason string) error
	Touch(ctx context.Context, id uuid.UUID) error
}

// RentalRunner trains on a rented GPU (PLAN M9 + M10): it starts a
// trainer-kind rental template (the trainer image in serve mode), waits
// for the rental controller to report it ready, posts the dataset and
// the config to it, polls its status for progress and the log, fetches
// the adapter and stops the machine. The machine is reachable through
// the provider's HTTPS proxy and requires the rental key, as a served
// model would; the dataset and HF_TOKEN are all the box ever sees.
//
// The trainer's HTTP contract (infra/training/serve.py), under the
// machine's base URL:
//
//	GET  /status            {state: idle|running|done|failed, progress, log, error}
//	POST /train             {base_model, adapter_name, config, train, eval}  -> 202
//	GET  /adapter           the adapter as a gzipped tar, once done
//	POST /cancel            stop a running trainer
type RentalRunner struct {
	Rental   Renter
	Template string
	// Key returns the rental key (WS_RENTAL_API_KEY) the trainer requires.
	Key    func() string
	Client *http.Client
	// Poll is the status interval (default 10s).
	Poll time.Duration
	// ReadyTimeout caps the wait for the machine (default 30m; the
	// template's warmup_timeout usually fires first).
	ReadyTimeout time.Duration
	Log          *slog.Logger
}

// Run implements Runner.
func (r *RentalRunner) Run(ctx context.Context, spec RunSpec, progress func(float64, string)) ([]byte, string, error) {
	if r.Rental == nil || r.Template == "" {
		return nil, "", ErrNoRunner
	}
	inst, err := r.Rental.Start(ctx, r.Template, spec.OwnerID)
	if err != nil {
		return nil, "", fmt.Errorf("training: rent a trainer: %w", err)
	}
	log := fmt.Sprintf("rented %s on %s (%s)\n", r.Template, inst.Provider, inst.Gpu)
	if progress != nil {
		progress(0, log)
	}
	reason := "fine-tune finished"
	defer func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		if err := r.Rental.Stop(sctx, inst.ID, reason); err != nil {
			r.log().Warn("training: stop rented trainer", "instance", inst.ID, "err", err)
		}
	}()
	base, err := r.waitReady(ctx, inst.ID)
	if err != nil {
		reason = "trainer not ready"
		return nil, log, err
	}
	log += "trainer ready at " + base + "\n"
	body, _ := json.Marshal(map[string]any{
		"base_model": spec.BaseModel, "adapter_name": spec.AdapterName, "config": spec.Config,
		"train": string(spec.Train), "eval": string(spec.Eval),
	})
	if err := r.do(ctx, http.MethodPost, base+"/train", body, nil); err != nil {
		reason = "fine-tune failed to start"
		return nil, log, fmt.Errorf("training: start the trainer: %w", err)
	}
	poll := r.Poll
	if poll <= 0 {
		poll = 10 * time.Second
	}
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			reason = "fine-tune cancelled"
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
			_ = r.do(cctx, http.MethodPost, base+"/cancel", nil, nil)
			cancel()
			return nil, log, ctx.Err()
		case <-tick.C:
		}
		var st trainerStatus
		if err := r.do(ctx, http.MethodGet, base+"/status", nil, &st); err != nil {
			r.log().Debug("training: trainer status", "err", err)
			continue
		}
		_ = r.Rental.Touch(ctx, inst.ID)
		full := log + st.Log
		switch st.State {
		case "running":
			if progress != nil {
				progress(st.Progress, full)
			}
		case "done":
			var adapter bytes.Buffer
			if err := r.do(ctx, http.MethodGet, base+"/adapter", nil, &adapter); err != nil {
				reason = "adapter download failed"
				return nil, full, fmt.Errorf("training: fetch the adapter: %w", err)
			}
			if progress != nil {
				progress(1, full)
			}
			return adapter.Bytes(), full, nil
		case "failed":
			reason = "fine-tune failed"
			msg := st.Error
			if msg == "" {
				msg = "the trainer failed"
			}
			return nil, full, errors.New("training: " + msg)
		case "idle":
			reason = "fine-tune failed"
			return nil, full, errors.New("training: the trainer lost the job (restarted?)")
		}
	}
}

type trainerStatus struct {
	State    string  `json:"state"`
	Progress float64 `json:"progress"`
	Log      string  `json:"log"`
	Error    string  `json:"error"`
}

// waitReady polls the instance row until the rental controller's
// reconcile marks it ready (base URL known, the trainer answering), or
// the controller stopped it.
func (r *RentalRunner) waitReady(ctx context.Context, id uuid.UUID) (string, error) {
	timeout := r.ReadyTimeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	poll := r.Poll
	if poll <= 0 {
		poll = 10 * time.Second
	}
	for {
		inst, err := r.Rental.Instance(ctx, id)
		if err != nil {
			return "", err
		}
		switch inst.Status {
		case rental.StatusReady:
			if inst.BaseUrl != nil && *inst.BaseUrl != "" {
				return strings.TrimRight(*inst.BaseUrl, "/"), nil
			}
		case rental.StatusStopped, rental.StatusFailed, rental.StatusStopping:
			msg := inst.Status
			if inst.Error != nil && *inst.Error != "" {
				msg += ": " + *inst.Error
			} else if inst.StopReason != nil && *inst.StopReason != "" {
				msg += ": " + *inst.StopReason
			}
			return "", fmt.Errorf("training: the rented trainer %s", msg)
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("training: the rented trainer was not ready after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(poll):
		}
	}
}

// do sends a request with the rental key. out is a *bytes.Buffer for raw
// bytes, a JSON target, or nil.
func (r *RentalRunner) do(ctx context.Context, method, url string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return err
	}
	if r.Key != nil {
		req.Header.Set("Authorization", "Bearer "+r.Key())
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("%s: HTTP %d: %s", strings.TrimPrefix(url, "http"), resp.StatusCode, strings.TrimSpace(string(b)))
	}
	switch o := out.(type) {
	case nil:
		return nil
	case *bytes.Buffer:
		_, err := io.Copy(o, io.LimitReader(resp.Body, 4<<30))
		return err
	default:
		return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
	}
}

func (r *RentalRunner) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

// ---- targets ----

// Target is a place a fine-tune job can run.
type Target struct {
	ID    string `json:"id"`    // local | rental
	Label string `json:"label"` // for the page
}

// Target ids.
const (
	TargetLocal  = "local"
	TargetRental = "rental"
)

// Targets is a Runner that picks the local Docker runner or the rented
// one per job (FinetuneConfig.Target). Either may be nil; the default is
// local when it exists, else rental.
type Targets struct {
	Local  Runner
	Rental Runner
	// RentalLabel names the rental template on the page.
	RentalLabel string
}

// List reports the targets that exist, the default first.
func (t *Targets) List() []Target {
	var out []Target
	if t.Local != nil {
		out = append(out, Target{ID: TargetLocal, Label: "this worker's GPU"})
	}
	if t.Rental != nil {
		label := "rented GPU"
		if t.RentalLabel != "" {
			label += " (" + t.RentalLabel + ")"
		}
		out = append(out, Target{ID: TargetRental, Label: label})
	}
	return out
}

// Pick returns the runner for a target ("" is the default).
func (t *Targets) Pick(target string) (Runner, error) {
	switch target {
	case "":
		if t.Local != nil {
			return t.Local, nil
		}
		if t.Rental != nil {
			return t.Rental, nil
		}
		return nil, ErrNoRunner
	case TargetLocal:
		if t.Local == nil {
			return nil, fmt.Errorf("%w: no trainer image on this worker (WS_FINETUNE_IMAGE)", ErrInvalid)
		}
		return t.Local, nil
	case TargetRental:
		if t.Rental == nil {
			return nil, fmt.Errorf("%w: no rented trainer (WS_FINETUNE_TEMPLATE and a rental provider)", ErrInvalid)
		}
		return t.Rental, nil
	}
	return nil, fmt.Errorf("%w: unknown target %q", ErrInvalid, target)
}

// Run implements Runner.
func (t *Targets) Run(ctx context.Context, spec RunSpec, progress func(float64, string)) ([]byte, string, error) {
	r, err := t.Pick(spec.Config.Target)
	if err != nil {
		return nil, "", err
	}
	return r.Run(ctx, spec, progress)
}

func hasTarget(ts []Target, id string) bool {
	for _, t := range ts {
		if t.ID == id {
			return true
		}
	}
	return false
}

// TargetLister is what a Runner implements to offer a choice of targets.
type TargetLister interface {
	List() []Target
	Pick(target string) (Runner, error)
}

// Targets reports where this deployment can run a fine-tune job: what
// the config declares (Available), else what the runner offers.
func (s *Service) Targets() []Target {
	if len(s.Available) > 0 {
		return s.Available
	}
	switch r := s.Runner.(type) {
	case nil:
		return nil
	case TargetLister:
		return r.List()
	default:
		return []Target{{ID: TargetLocal, Label: "this worker's GPU"}}
	}
}
