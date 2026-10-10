package training

import (
	"context"
	"fmt"
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
	c := &trainerClient{Key: r.Key, Client: r.Client, Poll: r.Poll, Log: r.Log}
	adapter, full, why, err := c.drive(ctx, base, spec, progress, log, func() { _ = r.Rental.Touch(ctx, inst.ID) })
	reason = why
	return adapter, full, err
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

func (r *RentalRunner) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

// ---- targets ----

// Target is a place a fine-tune job can run.
type Target struct {
	ID    string `json:"id"`    // local | remote | rental
	Label string `json:"label"` // for the page
}

// Target ids.
const (
	TargetLocal  = "local"
	TargetRemote = "remote"
	TargetRental = "rental"
)

// Targets is a Runner that picks the local Docker runner, the trainer
// box at WS_FINETUNE_URL or the rented one per job (FinetuneConfig.Target).
// Any may be nil; the default is the first that exists in that order
// (a box already there costs nothing; renting is last).
type Targets struct {
	Local  Runner
	Remote Runner
	Rental Runner
	// RemoteLabel names the trainer box (its host) on the page;
	// RentalLabel names the rental template.
	RemoteLabel string
	RentalLabel string
}

// List reports the targets that exist, the default first.
func (t *Targets) List() []Target {
	var out []Target
	if t.Local != nil {
		out = append(out, Target{ID: TargetLocal, Label: "this worker's GPU"})
	}
	if t.Remote != nil {
		out = append(out, Target{ID: TargetRemote, Label: RemoteLabel(t.RemoteLabel)})
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
		if t.Remote != nil {
			return t.Remote, nil
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
	case TargetRemote:
		if t.Remote == nil {
			return nil, fmt.Errorf("%w: no trainer box (WS_FINETUNE_URL)", ErrInvalid)
		}
		return t.Remote, nil
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

// RemoteLabel is the page's label for the trainer box at host.
func RemoteLabel(host string) string {
	if host == "" {
		return "the trainer box"
	}
	return "the trainer at " + host
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
