// Package rental rents GPUs on demand (PLAN.md M9). A template
// (infra/rental/templates/*.yaml) says which machine to start on which
// provider, the image and model to run, the endpoint ws should register
// for it, and the limits. The controller starts a machine, registers an
// endpoint for it (disabled until the machine answers), probes it until
// it is ready, enables it so the router can use it, bills the hours to
// the ledger, stops it when idle or over its caps, and removes the
// endpoint again. Providers sit behind one interface; RunPod is first.
package rental

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
)

// Instance statuses (rental_instances.status).
const (
	StatusProvisioning = "provisioning" // asked the provider for a machine
	StatusWarming      = "warming"      // machine running, model not answering yet
	StatusReady        = "ready"        // endpoint enabled and routable
	StatusStopping     = "stopping"
	StatusStopped      = "stopped"
	StatusFailed       = "failed"
)

// Errors callers map to HTTP statuses.
var (
	ErrDisabled     = errors.New("rental: rentals are disabled (WS_RENTAL_DISABLED)")
	ErrNoTemplate   = errors.New("rental: unknown template")
	ErrNoProvider   = errors.New("rental: provider not configured")
	ErrBusy         = errors.New("rental: this template already has a machine")
	ErrDailyCap     = errors.New("rental: the daily cap on rented hours is reached")
	ErrNoAPIKey     = errors.New("rental: WS_RENTAL_API_KEY is not set; it is the key the rented endpoint will require")
	ErrNotRunning   = errors.New("rental: instance is not running")
	ErrNotAvailable = errors.New("rental: the provider has no such machine available right now")
)

// Template is one infra/rental/templates/*.yaml.
type Template struct {
	Name        string `yaml:"name" json:"name"`
	Provider    string `yaml:"provider" json:"provider"` // runpod
	Description string `yaml:"description" json:"description"`
	// GPU is the provider's GPU type id (RunPod: "NVIDIA H100 80GB HBM3").
	GPU      string `yaml:"gpu" json:"gpu"`
	GPUCount int    `yaml:"gpu_count" json:"gpu_count"`
	// Image runs the model; Args is its command line (vLLM flags). $VAR in
	// Args and Env is expanded from the process environment at start, so
	// secrets live in .env, never in the template. $WS_RENTAL_API_KEY is
	// the key the server must require.
	Image string            `yaml:"image" json:"image"`
	Args  string            `yaml:"args" json:"args"`
	Env   map[string]string `yaml:"env" json:"env"`
	Port  int               `yaml:"port" json:"port"`
	// Model is what the endpoint sends as the model name.
	Model      string `yaml:"model" json:"model"`
	DiskGB     int    `yaml:"disk_gb" json:"disk_gb"`
	VolumeGB   int    `yaml:"volume_gb" json:"volume_gb"`
	VolumePath string `yaml:"volume_path" json:"volume_path"`
	CloudType  string `yaml:"cloud_type" json:"cloud_type"` // secure | community
	// Limits.
	IdleTimeout   time.Duration `yaml:"idle_timeout" json:"idle_timeout"`     // stop after this long without a request (default 20m)
	MaxHours      float64       `yaml:"max_hours" json:"max_hours"`           // stop after this many hours in one go (default 6)
	WarmupTimeout time.Duration `yaml:"warmup_timeout" json:"warmup_timeout"` // give up if not ready by then (default 25m)
	// Endpoint is what ws registers while the machine is up.
	Endpoint TemplateEndpoint `yaml:"endpoint" json:"endpoint"`
	// HourlyUSD is the expected price, shown before starting; the
	// provider's actual price replaces it once the machine runs.
	HourlyUSD float64 `yaml:"hourly_usd" json:"hourly_usd"`
}

// TemplateEndpoint is the endpoints row a template registers.
type TemplateEndpoint struct {
	ID           string               `yaml:"id" json:"id"` // e.g. rental/gpt-oss-120b-h100
	DisplayName  string               `yaml:"display_name" json:"display_name"`
	Capabilities gateway.Capabilities `yaml:"capabilities" json:"capabilities"`
	// Pricing is optional: 0 means the per-hour rows carry the cost and
	// per-token rows are free; an amortized estimate lets the cost-aware
	// router compare the machine with hosted models.
	Pricing gateway.Pricing `yaml:"pricing" json:"pricing"`
}

// ProviderID is the providers row an instance of a template serves through.
func (t Template) ProviderID() string { return "rental-" + t.Name }

var templateNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// LoadTemplates reads every *.yaml in dir.
func LoadTemplates(dir string) ([]Template, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	var out []Template
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var t Template
		if err := yaml.Unmarshal(b, &t); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		if t.Name == "" {
			t.Name = strings.TrimSuffix(filepath.Base(f), ".yaml")
		}
		if err := t.validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (t *Template) validate() error {
	if !templateNameRe.MatchString(t.Name) {
		return fmt.Errorf("name %q: lowercase letters, digits and dashes", t.Name)
	}
	if t.Provider == "" || t.GPU == "" || t.Image == "" || t.Model == "" || t.Endpoint.ID == "" {
		return errors.New("provider, gpu, image, model and endpoint.id are required")
	}
	if t.GPUCount <= 0 {
		t.GPUCount = 1
	}
	if t.Port <= 0 {
		t.Port = 8000
	}
	if t.DiskGB <= 0 {
		t.DiskGB = 40
	}
	if t.VolumePath == "" {
		t.VolumePath = "/workspace"
	}
	if t.CloudType == "" {
		t.CloudType = "secure"
	}
	if t.IdleTimeout <= 0 {
		t.IdleTimeout = 20 * time.Minute
	}
	if t.MaxHours <= 0 {
		t.MaxHours = 6
	}
	if t.WarmupTimeout <= 0 {
		t.WarmupTimeout = 25 * time.Minute
	}
	if t.Endpoint.DisplayName == "" {
		t.Endpoint.DisplayName = t.Model + " (rented " + t.GPU + ")"
	}
	return nil
}

// Expand substitutes $VAR and ${VAR} from getenv in s.
func Expand(s string, getenv func(string) string) string { return os.Expand(s, getenv) }

// StartSpec is what a provider starts.
type StartSpec struct {
	Name     string
	Template Template
	Args     string            // expanded
	Env      map[string]string // expanded
}

// Status is a provider's view of a machine.
type Status struct {
	// State is running | starting | exited | unknown.
	State string
	// BaseURL is the OpenAI-compatible URL (…/v1) once the port is exposed.
	BaseURL   string
	HourlyUSD float64
	Error     string
}

// Offer is a GPU type and its current price.
type Offer struct {
	GPU       string  `json:"gpu"`
	MemoryGB  int     `json:"memory_gb"`
	HourlyUSD float64 `json:"hourly_usd"`
	Available bool    `json:"available"`
}

// Provider is one rental API.
type Provider interface {
	Name() string
	Offers(ctx context.Context) ([]Offer, error)
	Start(ctx context.Context, spec StartSpec) (instanceID string, err error)
	Status(ctx context.Context, instanceID string) (Status, error)
	Stop(ctx context.Context, instanceID string) error
}

// Store is the slice of the store the controller uses (*store.DB implements it).
type Store interface {
	CreateRentalInstance(ctx context.Context, arg store.CreateRentalInstanceParams) (store.RentalInstance, error)
	GetRentalInstance(ctx context.Context, id uuid.UUID) (store.RentalInstance, error)
	ListRentalInstances(ctx context.Context, limit int32) ([]store.RentalInstance, error)
	ListOpenRentalInstances(ctx context.Context) ([]store.RentalInstance, error)
	OpenRentalInstanceForTemplate(ctx context.Context, template string) (store.RentalInstance, error)
	SetRentalStatus(ctx context.Context, arg store.SetRentalStatusParams) error
	SetRentalReady(ctx context.Context, arg store.SetRentalReadyParams) error
	SetRentalProviderID(ctx context.Context, arg store.SetRentalProviderIDParams) error
	SetRentalUsage(ctx context.Context, arg store.SetRentalUsageParams) error
	SumRentalHoursSince(ctx context.Context, since time.Time) (float64, error)
	LastUsageForEndpoint(ctx context.Context, endpointID *string) (time.Time, error)
	UpsertProvider(ctx context.Context, arg store.UpsertProviderParams) error
	UpsertEndpoint(ctx context.Context, arg store.UpsertEndpointParams) error
	SetEndpointEnabled(ctx context.Context, arg store.SetEndpointEnabledParams) error
	DeleteProvider(ctx context.Context, id string) error
}

// Controller drives instances.
type Controller struct {
	DB        Store
	Providers map[string]Provider
	Templates []Template
	// Recorder writes the per-hour ledger rows (gateway.Record).
	Recorder gateway.UsageRecorder
	// Reload reloads the gateway registry after a provider or endpoint
	// row changes (store.DB.LoadRegistry bound to the registry).
	Reload func(ctx context.Context) error
	// APIKeyEnv names the variable holding the key rented servers require
	// (default WS_RENTAL_API_KEY). The provider row references it by name.
	APIKeyEnv string
	// DailyCapHours refuses starts once today's hours reach it; 0 is no cap.
	DailyCapHours float64
	// Disabled is the kill switch: no starts, and every open instance is
	// stopped at the next reconcile.
	Disabled bool
	// Probe is the client that checks a machine's endpoint.
	Probe  *http.Client
	Getenv func(string) string
	Log    *slog.Logger
	Now    func() time.Time

	mu sync.Mutex // one reconcile at a time
}

// Template finds a template by name.
func (c *Controller) Template(name string) (Template, bool) {
	for _, t := range c.Templates {
		if t.Name == name {
			return t, true
		}
	}
	return Template{}, false
}

// Start rents a machine for a template and registers its endpoint
// (disabled until ready).
func (c *Controller) Start(ctx context.Context, templateName string, userID uuid.UUID) (*store.RentalInstance, error) {
	if c.Disabled {
		return nil, ErrDisabled
	}
	tpl, ok := c.Template(templateName)
	if !ok {
		return nil, ErrNoTemplate
	}
	prov, ok := c.Providers[tpl.Provider]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoProvider, tpl.Provider)
	}
	if c.getenv(c.apiKeyEnv()) == "" {
		return nil, ErrNoAPIKey
	}
	if _, err := c.DB.OpenRentalInstanceForTemplate(ctx, tpl.Name); err == nil {
		return nil, ErrBusy
	}
	if c.DailyCapHours > 0 {
		used, err := c.DB.SumRentalHoursSince(ctx, dayStart(c.now()))
		if err != nil {
			return nil, err
		}
		if used >= c.DailyCapHours {
			return nil, fmt.Errorf("%w: %.1f of %.1f hours", ErrDailyCap, used, c.DailyCapHours)
		}
	}
	env := map[string]string{}
	for k, v := range tpl.Env {
		env[k] = Expand(v, c.getenv)
	}
	spec := StartSpec{Name: "ws-" + tpl.Name + "-" + c.now().UTC().Format("0102-1504"), Template: tpl, Args: Expand(tpl.Args, c.getenv), Env: env}
	// Register the provider and endpoint first so an instance row never
	// points at nothing; the endpoint stays disabled until the probe passes.
	if err := c.register(ctx, tpl, "http://pending.invalid", false); err != nil {
		return nil, err
	}
	row, err := c.DB.CreateRentalInstance(ctx, store.CreateRentalInstanceParams{
		Provider: tpl.Provider, Template: tpl.Name, Gpu: tpl.GPU, EndpointID: tpl.Endpoint.ID, HourlyUsd: tpl.HourlyUSD, StartedBy: store.NullUUID(userID),
	})
	if err != nil {
		return nil, err
	}
	id, err := prov.Start(ctx, spec)
	if err != nil {
		msg := err.Error()
		_ = c.DB.SetRentalStatus(ctx, store.SetRentalStatusParams{ID: row.ID, Status: StatusFailed, Error: &msg})
		c.unregister(ctx, tpl)
		return nil, fmt.Errorf("rental: start on %s: %w", tpl.Provider, err)
	}
	_ = c.DB.SetRentalProviderID(ctx, store.SetRentalProviderIDParams{ID: row.ID, ProviderInstanceID: &id})
	c.log().Info("rental: started", "instance", row.ID, "template", tpl.Name, "provider", tpl.Provider, "provider_id", id, "gpu", tpl.GPU)
	row.ProviderInstanceID = &id
	return &row, nil
}

// Stop terminates an instance, bills the remainder and removes its
// endpoint. Idempotent.
func (c *Controller) Stop(ctx context.Context, id uuid.UUID, reason string) error {
	row, err := c.DB.GetRentalInstance(ctx, id)
	if err != nil {
		return err
	}
	return c.stop(ctx, row, reason)
}

func (c *Controller) stop(ctx context.Context, row store.RentalInstance, reason string) error {
	switch row.Status {
	case StatusStopped, StatusFailed:
		return nil
	}
	_ = c.DB.SetRentalStatus(ctx, store.SetRentalStatusParams{ID: row.ID, Status: StatusStopping, StopReason: &reason})
	if prov, ok := c.Providers[row.Provider]; ok && row.ProviderInstanceID != nil {
		if err := prov.Stop(ctx, *row.ProviderInstanceID); err != nil {
			msg := "stop: " + err.Error()
			_ = c.DB.SetRentalStatus(ctx, store.SetRentalStatusParams{ID: row.ID, Status: StatusStopping, Error: &msg})
			return fmt.Errorf("rental: stop on %s: %w", row.Provider, err)
		}
	}
	c.bill(ctx, row, true)
	_ = c.DB.SetRentalStatus(ctx, store.SetRentalStatusParams{ID: row.ID, Status: StatusStopped, StopReason: &reason})
	if tpl, ok := c.Template(row.Template); ok {
		c.unregister(ctx, tpl)
	} else {
		_ = c.DB.DeleteProvider(ctx, "rental-"+row.Template)
		c.reload(ctx)
	}
	c.log().Info("rental: stopped", "instance", row.ID, "template", row.Template, "reason", reason, "hours", row.HoursUsed)
	return nil
}

// StopAll stops every open instance (the kill switch).
func (c *Controller) StopAll(ctx context.Context, reason string) error {
	rows, err := c.DB.ListOpenRentalInstances(ctx)
	if err != nil {
		return err
	}
	var first error
	for _, r := range rows {
		if err := c.stop(ctx, r, reason); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Reconcile is the minute tick: it moves instances along, probes warming
// ones, bills hours, and stops the idle, the overdue and, with the kill
// switch on, everything.
func (c *Controller) Reconcile(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	rows, err := c.DB.ListOpenRentalInstances(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if err := c.reconcileOne(ctx, row); err != nil {
			c.log().Warn("rental: reconcile", "instance", row.ID, "err", err)
		}
	}
	return nil
}

func (c *Controller) reconcileOne(ctx context.Context, row store.RentalInstance) error {
	if c.Disabled {
		return c.stop(ctx, row, "kill switch")
	}
	tpl, ok := c.Template(row.Template)
	if !ok {
		return c.stop(ctx, row, "template removed")
	}
	prov, ok := c.Providers[row.Provider]
	if !ok || row.ProviderInstanceID == nil {
		return c.stop(ctx, row, "provider not configured")
	}
	now := c.now()
	st, err := prov.Status(ctx, *row.ProviderInstanceID)
	if err != nil {
		return err
	}
	switch st.State {
	case "exited":
		if row.Status == StatusStopping {
			return c.stop(ctx, row, "stopped")
		}
		msg := "the provider reports the machine exited"
		if st.Error != "" {
			msg += ": " + st.Error
		}
		c.bill(ctx, row, true)
		_ = c.DB.SetRentalStatus(ctx, store.SetRentalStatusParams{ID: row.ID, Status: StatusFailed, Error: &msg})
		c.unregister(ctx, tpl)
		return nil
	}
	switch row.Status {
	case StatusStopping:
		return c.stop(ctx, row, "stopped")
	case StatusProvisioning, StatusWarming:
		if now.Sub(row.StartedAt) > tpl.WarmupTimeout {
			return c.stop(ctx, row, fmt.Sprintf("not ready after %s", tpl.WarmupTimeout))
		}
		if st.State != "running" || st.BaseURL == "" {
			return nil
		}
		if row.Status == StatusProvisioning {
			_ = c.DB.SetRentalStatus(ctx, store.SetRentalStatusParams{ID: row.ID, Status: StatusWarming})
		}
		if err := c.probe(ctx, st.BaseURL, tpl.Model); err != nil {
			c.log().Debug("rental: not ready yet", "instance", row.ID, "err", err)
			return nil
		}
		if err := c.DB.SetRentalReady(ctx, store.SetRentalReadyParams{ID: row.ID, BaseUrl: &st.BaseURL, HourlyUsd: st.HourlyUSD}); err != nil {
			return err
		}
		if err := c.register(ctx, tpl, st.BaseURL, true); err != nil {
			return err
		}
		c.log().Info("rental: ready", "instance", row.ID, "template", tpl.Name, "base_url", st.BaseURL, "hourly_usd", st.HourlyUSD, "after", now.Sub(row.StartedAt).Round(time.Second))
		return nil
	case StatusReady:
		c.bill(ctx, row, false)
		hours := now.Sub(row.StartedAt).Hours()
		if hours >= tpl.MaxHours {
			return c.stop(ctx, row, fmt.Sprintf("max %.1f hours reached", tpl.MaxHours))
		}
		if c.DailyCapHours > 0 {
			if used, err := c.DB.SumRentalHoursSince(ctx, dayStart(now)); err == nil && used >= c.DailyCapHours {
				return c.stop(ctx, row, fmt.Sprintf("daily cap of %.1f hours reached", c.DailyCapHours))
			}
		}
		last := row.StartedAt
		if row.ReadyAt != nil {
			last = *row.ReadyAt
		}
		if t, err := c.DB.LastUsageForEndpoint(ctx, &row.EndpointID); err == nil && t.After(last) {
			last = t
		}
		if now.Sub(last) > tpl.IdleTimeout {
			return c.stop(ctx, row, fmt.Sprintf("idle for %s", tpl.IdleTimeout))
		}
	}
	return nil
}

// bill updates hours_used and writes one ledger row per whole hour since
// the last one; final also bills the remaining fraction.
func (c *Controller) bill(ctx context.Context, row store.RentalInstance, final bool) {
	now := c.now()
	hours := now.Sub(row.StartedAt).Hours()
	if row.Status == StatusStopped || row.Status == StatusFailed {
		return
	}
	billed := float64(row.BilledHours)
	rate := row.HourlyUsd
	write := func(h float64) {
		if c.Recorder == nil || h <= 0 {
			return
		}
		c.Recorder.Record(ctx, gateway.UsageRecord{
			Metadata:   gateway.Metadata{UserID: userOf(row), TaskClass: gateway.TaskRental},
			EndpointID: row.EndpointID, Model: row.Template,
			Decision:     gateway.Decision{Selector: row.Template, Reason: "rental", Chosen: row.EndpointID},
			CostUSD:      h * rate,
			Latency:      time.Duration(h * float64(time.Hour)),
			FinishReason: gateway.FinishStop,
		})
	}
	for billed+1 <= hours {
		write(1)
		billed++
	}
	if final && hours > billed {
		write(hours - billed)
		billed = hours
	}
	var last *time.Time
	if t, err := c.DB.LastUsageForEndpoint(ctx, &row.EndpointID); err == nil && t.Year() > 1971 {
		last = &t
	}
	_ = c.DB.SetRentalUsage(ctx, store.SetRentalUsageParams{ID: row.ID, HoursUsed: float32(math.Round(hours*100) / 100), BilledHours: float32(billed), LastRequestAt: last})
}

// probe checks that the machine answers /v1/models and a one-token
// completion with the rental key.
func (c *Controller) probe(ctx context.Context, baseURL, model string) error {
	client := c.Probe
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	key := c.getenv(c.apiKeyEnv())
	base := strings.TrimRight(baseURL, "/")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("/models: HTTP %d", resp.StatusCode)
	}
	body := strings.NewReader(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],"max_tokens":1}`, model))
	req, _ = http.NewRequestWithContext(ctx, http.MethodPost, base+"/chat/completions", body)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err = client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("/chat/completions: HTTP %d", resp.StatusCode)
	}
	return nil
}

// register upserts the provider and endpoint rows for a template.
func (c *Controller) register(ctx context.Context, tpl Template, baseURL string, enabled bool) error {
	keyEnv := c.apiKeyEnv()
	if err := c.DB.UpsertProvider(ctx, store.UpsertProviderParams{ID: tpl.ProviderID(), Kind: string(gateway.ProviderOpenAICompat), Name: "Rented: " + tpl.Name, BaseUrl: baseURL, ApiKeyEnv: &keyEnv, Headers: []byte("{}")}); err != nil {
		return err
	}
	caps, _ := jsonMarshal(tpl.Endpoint.Capabilities)
	pricing, _ := jsonMarshal(tpl.Endpoint.Pricing)
	if err := c.DB.UpsertEndpoint(ctx, store.UpsertEndpointParams{
		ID: tpl.Endpoint.ID, ProviderID: tpl.ProviderID(), ModelName: tpl.Model, DisplayName: tpl.Endpoint.DisplayName,
		Capabilities: caps, Pricing: pricing, ThroughputClass: "high", LatencyClass: "fast", IsLocal: false, Enabled: enabled, ExtraBody: []byte("{}"),
	}); err != nil {
		return err
	}
	if err := c.DB.SetEndpointEnabled(ctx, store.SetEndpointEnabledParams{ID: tpl.Endpoint.ID, Enabled: enabled}); err != nil {
		return err
	}
	c.reload(ctx)
	return nil
}

// unregister removes a template's provider (and its endpoint through the
// foreign key).
func (c *Controller) unregister(ctx context.Context, tpl Template) {
	_ = c.DB.SetEndpointEnabled(ctx, store.SetEndpointEnabledParams{ID: tpl.Endpoint.ID, Enabled: false})
	_ = c.DB.DeleteProvider(ctx, tpl.ProviderID())
	c.reload(ctx)
}

func (c *Controller) reload(ctx context.Context) {
	if c.Reload != nil {
		if err := c.Reload(ctx); err != nil {
			c.log().Warn("rental: reload registry", "err", err)
		}
	}
}

// Offers asks a provider for its GPU types and prices.
func (c *Controller) Offers(ctx context.Context, provider string) ([]Offer, error) {
	p, ok := c.Providers[provider]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoProvider, provider)
	}
	return p.Offers(ctx)
}

func (c *Controller) apiKeyEnv() string {
	if c.APIKeyEnv != "" {
		return c.APIKeyEnv
	}
	return "WS_RENTAL_API_KEY"
}

func (c *Controller) getenv(k string) string {
	if c.Getenv != nil {
		return c.Getenv(k)
	}
	return os.Getenv(k)
}

func (c *Controller) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Controller) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

func dayStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func userOf(row store.RentalInstance) string {
	if row.StartedBy.Valid {
		return row.StartedBy.UUID.String()
	}
	return ""
}
