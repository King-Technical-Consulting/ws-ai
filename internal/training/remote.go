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
	"net/url"
	"strings"
	"time"
)

// trainerClient speaks the trainer image's HTTP contract
// (infra/training/serve.py), under a base URL:
//
//	GET  /status            {state: idle|running|done|failed, progress, log, error}
//	POST /train             {base_model, adapter_name, config, train, eval}  -> 202
//	GET  /adapter           the adapter as a gzipped tar, once done
//	POST /cancel            stop a running trainer
//
// The rented runner and the remote runner both drive a trainer through
// it; what differs is who provides the box.
type trainerClient struct {
	// Key returns the bearer key the trainer requires.
	Key    func() string
	Client *http.Client
	// Poll is the status interval (default 10s).
	Poll time.Duration
	Log  *slog.Logger
}

func (c *trainerClient) poll() time.Duration {
	if c.Poll <= 0 {
		return 10 * time.Second
	}
	return c.Poll
}

// drive posts the job to the trainer at base, follows its status until
// the run ends and fetches the adapter. log is the log so far (the
// caller's preamble); the returned log carries the trainer's tail. The
// reason names how it ended, for the caller's own bookkeeping (a rented
// machine's stop reason). touch, when not nil, is called on every status
// the trainer answers while the run goes.
func (c *trainerClient) drive(ctx context.Context, base string, spec RunSpec, progress func(float64, string), log string, touch func()) (adapter []byte, fullLog, reason string, err error) {
	body, _ := json.Marshal(map[string]any{
		"base_model": spec.BaseModel, "adapter_name": spec.AdapterName, "config": spec.Config,
		"train": string(spec.Train), "eval": string(spec.Eval),
	})
	if err := c.do(ctx, http.MethodPost, base+"/train", body, nil); err != nil {
		return nil, log, "fine-tune failed to start", fmt.Errorf("training: start the trainer: %w", err)
	}
	tick := time.NewTicker(c.poll())
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
			_ = c.do(cctx, http.MethodPost, base+"/cancel", nil, nil)
			cancel()
			return nil, log, "fine-tune cancelled", ctx.Err()
		case <-tick.C:
		}
		var st trainerStatus
		if err := c.do(ctx, http.MethodGet, base+"/status", nil, &st); err != nil {
			c.log().Debug("training: trainer status", "err", err)
			continue
		}
		if touch != nil {
			touch()
		}
		full := log + st.Log
		switch st.State {
		case "running":
			if progress != nil {
				progress(st.Progress, full)
			}
		case "done":
			var out bytes.Buffer
			if err := c.do(ctx, http.MethodGet, base+"/adapter", nil, &out); err != nil {
				return nil, full, "adapter download failed", fmt.Errorf("training: fetch the adapter: %w", err)
			}
			if progress != nil {
				progress(1, full)
			}
			return out.Bytes(), full, "fine-tune finished", nil
		case "failed":
			msg := st.Error
			if msg == "" {
				msg = "the trainer failed"
			}
			return nil, full, "fine-tune failed", errors.New("training: " + msg)
		case "idle":
			return nil, full, "fine-tune failed", errors.New("training: the trainer lost the job (restarted?)")
		}
	}
}

type trainerStatus struct {
	State    string  `json:"state"`
	Progress float64 `json:"progress"`
	Log      string  `json:"log"`
	Error    string  `json:"error"`
}

// do sends a request with the key. out is a *bytes.Buffer for raw bytes,
// a JSON target, or nil.
func (c *trainerClient) do(ctx context.Context, method, url string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return err
	}
	if c.Key != nil {
		if k := c.Key(); k != "" {
			req.Header.Set("Authorization", "Bearer "+k)
		}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := c.Client
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

func (c *trainerClient) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

// RemoteRunner trains on a box the operator runs the trainer on: the
// trainer image in serve mode (or serve.py by itself, around the MLX
// script on a Mac) reachable at URL, usually over the tailnet. Nothing
// is rented or started: the box is expected to be there, and a job
// posted while it is busy fails with the trainer's 409. This is the
// target for a ws that has no GPU of its own (the worker on a small
// box, the GPU on the Orin, the Spark or the Mac).
type RemoteRunner struct {
	URL string
	// Key returns the key the trainer requires (WS_FINETUNE_KEY).
	Key    func() string
	Client *http.Client
	Poll   time.Duration
	Log    *slog.Logger
}

// Host is the URL's host, for the page's label.
func (r *RemoteRunner) Host() string {
	if u, err := url.Parse(r.URL); err == nil && u.Host != "" {
		return u.Host
	}
	return r.URL
}

// Run implements Runner.
func (r *RemoteRunner) Run(ctx context.Context, spec RunSpec, progress func(float64, string)) ([]byte, string, error) {
	if r == nil || strings.TrimSpace(r.URL) == "" {
		return nil, "", ErrNoRunner
	}
	base := strings.TrimRight(r.URL, "/")
	c := &trainerClient{Key: r.Key, Client: r.Client, Poll: r.Poll, Log: r.Log}
	log := "trainer at " + r.Host() + "\n"
	if progress != nil {
		progress(0, log)
	}
	adapter, full, _, err := c.drive(ctx, base, spec, progress, log, nil)
	return adapter, full, err
}
