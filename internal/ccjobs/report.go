package ccjobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// WSConfig is the [ws] table of targets.toml: where wsj reports job
// handles so the read-only web tab (spec §6.5) can list them. Optional;
// without it wsj reports nothing. The key is a ws API key (Settings → API
// keys) belonging to the owner, which is ws's own credential and has
// nothing to do with Claude Code's login.
type WSConfig struct {
	// URL is the ws origin, e.g. https://ws.tailnet.ts.net. WSJ_WS_URL overrides.
	URL string `toml:"url"`
	// KeyFile holds the ws API key on one line ("~" allowed). WSJ_WS_KEY
	// overrides with the key itself.
	KeyFile string `toml:"key_file"`
}

// Report is what a launcher tells ws about a job: the handle and what the
// startup check saw. It is the body of POST /api/jobs/cc.
type Report struct {
	ID      string    `json:"id"`
	Target  string    `json:"target"`
	Session string    `json:"session"`
	Window  string    `json:"window"`
	Cwd     string    `json:"cwd"`
	Lane    string    `json:"lane"`
	Model   string    `json:"model,omitempty"`
	Started time.Time `json:"started"`
	// Status is "alive", or "dead" when the startup check saw the pane
	// exit (the window is kept by remain-on-exit either way).
	Status string `json:"status"`
}

// ReportFor builds the report for a launch.
func ReportFor(h Handle, job Job, dead bool) Report {
	status := "alive"
	if dead {
		status = "dead"
	}
	started := job.Created
	if started.IsZero() {
		started = time.Now().UTC()
	}
	return Report{ID: h.ID, Target: h.Target, Session: h.Session, Window: h.Window, Cwd: h.Cwd, Lane: LaneSubscription, Model: job.Model, Started: started, Status: status}
}

// ValidJobID reports whether id has the launcher's job id shape.
func ValidJobID(id string) bool { return jobIDRe.MatchString(id) }

// Reporter posts job handles to ws. Nothing but handles and liveness goes
// through it: no prompt, no output.
type Reporter struct {
	URL    string
	Key    string
	Client *http.Client
}

// Reporter returns the reporter for this config, or nil when no ws is
// configured (no [ws] url and no WSJ_WS_URL).
func (c *Config) Reporter() (*Reporter, error) {
	u := strings.TrimSpace(os.Getenv("WSJ_WS_URL"))
	if u == "" {
		u = strings.TrimSpace(c.WS.URL)
	}
	if u == "" {
		return nil, nil
	}
	pu, err := url.Parse(u)
	if err != nil || (pu.Scheme != "http" && pu.Scheme != "https") || pu.Host == "" {
		return nil, fmt.Errorf("ws url %q: want http(s)://host", u)
	}
	key := strings.TrimSpace(os.Getenv("WSJ_WS_KEY"))
	if key == "" {
		if c.WS.KeyFile == "" {
			return nil, errors.New("ws: set key_file in [ws] (a ws API key) or WSJ_WS_KEY")
		}
		b, err := os.ReadFile(ExpandHome(c.WS.KeyFile))
		if err != nil {
			return nil, fmt.Errorf("ws key_file: %w", err)
		}
		key = strings.TrimSpace(string(b))
	}
	if !strings.HasPrefix(key, "ws_") {
		return nil, errors.New("ws key does not look like a ws API key (ws_...)")
	}
	return &Reporter{URL: strings.TrimRight(u, "/"), Key: key, Client: &http.Client{Timeout: 15 * time.Second}}, nil
}

// Launched reports a new job.
func (r *Reporter) Launched(ctx context.Context, rep Report) error {
	body, err := json.Marshal(rep)
	if err != nil {
		return err
	}
	return r.do(ctx, http.MethodPost, "/api/jobs/cc", body)
}

// Killed reports that wsj removed the job's window.
func (r *Reporter) Killed(ctx context.Context, id string) error {
	if !ValidJobID(id) {
		return fmt.Errorf("bad job id %q", id)
	}
	return r.do(ctx, http.MethodDelete, "/api/jobs/cc/"+id, nil)
}

func (r *Reporter) do(ctx context.Context, method, path string, body []byte) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.URL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.Key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := r.Client
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("ws %s %s: %w", method, path, err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		msg := strings.TrimSpace(string(limited(res.Body, 512)))
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal([]byte(msg), &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return fmt.Errorf("ws %s %s: %s: %s", method, path, res.Status, msg)
	}
	return nil
}

func limited(r io.Reader, n int64) []byte {
	b, _ := io.ReadAll(io.LimitReader(r, n))
	return b
}
