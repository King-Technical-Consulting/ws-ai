package rental

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
)

type fakeStore struct {
	mu        sync.Mutex
	rows      map[uuid.UUID]store.RentalInstance
	providers map[string]store.UpsertProviderParams
	endpoints map[string]store.UpsertEndpointParams
	enabled   map[string]bool
	lastUse   map[string]time.Time
	reloads   int
}

func newFake() *fakeStore {
	return &fakeStore{rows: map[uuid.UUID]store.RentalInstance{}, providers: map[string]store.UpsertProviderParams{}, endpoints: map[string]store.UpsertEndpointParams{}, enabled: map[string]bool{}, lastUse: map[string]time.Time{}}
}

func (f *fakeStore) CreateRentalInstance(_ context.Context, p store.CreateRentalInstanceParams) (store.RentalInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := store.RentalInstance{ID: uuid.New(), Provider: p.Provider, Template: p.Template, Gpu: p.Gpu, ProviderInstanceID: p.ProviderInstanceID, EndpointID: p.EndpointID, HourlyUsd: p.HourlyUsd, Status: StatusProvisioning, StartedBy: p.StartedBy, StartedAt: time.Now()}
	f.rows[r.ID] = r
	return r, nil
}
func (f *fakeStore) GetRentalInstance(_ context.Context, id uuid.UUID) (store.RentalInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rows[id]
	if !ok {
		return r, pgx.ErrNoRows
	}
	return r, nil
}
func (f *fakeStore) ListRentalInstances(_ context.Context, limit int32) ([]store.RentalInstance, error) {
	return f.ListOpenRentalInstances(context.Background())
}
func (f *fakeStore) ListOpenRentalInstances(_ context.Context) ([]store.RentalInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.RentalInstance
	for _, r := range f.rows {
		switch r.Status {
		case StatusProvisioning, StatusWarming, StatusReady, StatusStopping:
			out = append(out, r)
		}
	}
	return out, nil
}
func (f *fakeStore) OpenRentalInstanceForTemplate(ctx context.Context, tpl string) (store.RentalInstance, error) {
	rows, _ := f.ListOpenRentalInstances(ctx)
	for _, r := range rows {
		if r.Template == tpl {
			return r, nil
		}
	}
	return store.RentalInstance{}, pgx.ErrNoRows
}
func (f *fakeStore) SetRentalStatus(_ context.Context, p store.SetRentalStatusParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.rows[p.ID]
	r.Status = p.Status
	if p.Error != nil {
		r.Error = p.Error
	}
	if p.StopReason != nil {
		r.StopReason = p.StopReason
	}
	if (p.Status == StatusStopped || p.Status == StatusFailed) && r.StoppedAt == nil {
		now := time.Now()
		r.StoppedAt = &now
	}
	f.rows[p.ID] = r
	return nil
}
func (f *fakeStore) SetRentalReady(_ context.Context, p store.SetRentalReadyParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.rows[p.ID]
	now := time.Now()
	r.Status, r.BaseUrl, r.ReadyAt = StatusReady, p.BaseUrl, &now
	if p.HourlyUsd > 0 {
		r.HourlyUsd = p.HourlyUsd
	}
	f.rows[p.ID] = r
	return nil
}
func (f *fakeStore) SetRentalProviderID(_ context.Context, p store.SetRentalProviderIDParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.rows[p.ID]
	r.ProviderInstanceID = p.ProviderInstanceID
	f.rows[p.ID] = r
	return nil
}
func (f *fakeStore) SetRentalUsage(_ context.Context, p store.SetRentalUsageParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.rows[p.ID]
	r.HoursUsed, r.BilledHours = p.HoursUsed, p.BilledHours
	if p.LastRequestAt != nil {
		r.LastRequestAt = p.LastRequestAt
	}
	f.rows[p.ID] = r
	return nil
}
func (f *fakeStore) SumRentalHoursSince(_ context.Context, since time.Time) (float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var sum float64
	for _, r := range f.rows {
		sum += float64(r.HoursUsed)
	}
	return sum, nil
}
func (f *fakeStore) LastUsageForEndpoint(_ context.Context, id *string) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.lastUse[*id]; ok {
		return t, nil
	}
	return time.Unix(0, 0), nil
}
func (f *fakeStore) UpsertProvider(_ context.Context, p store.UpsertProviderParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.providers[p.ID] = p
	return nil
}
func (f *fakeStore) UpsertEndpoint(_ context.Context, p store.UpsertEndpointParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.endpoints[p.ID]; !ok {
		f.enabled[p.ID] = p.Enabled
	}
	f.endpoints[p.ID] = p
	return nil
}
func (f *fakeStore) SetEndpointEnabled(_ context.Context, p store.SetEndpointEnabledParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enabled[p.ID] = p.Enabled
	return nil
}
func (f *fakeStore) DeleteProvider(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.providers, id)
	for eid, e := range f.endpoints {
		if e.ProviderID == id {
			delete(f.endpoints, eid)
			delete(f.enabled, eid)
		}
	}
	return nil
}

// fakeProvider scripts a machine's life.
type fakeProvider struct {
	mu       sync.Mutex
	started  []StartSpec
	state    map[string]Status
	stopped  []string
	startErr error
}

func newProvider() *fakeProvider { return &fakeProvider{state: map[string]Status{}} }

func (p *fakeProvider) Name() string { return "fake" }
func (p *fakeProvider) Offers(context.Context) ([]Offer, error) {
	return []Offer{{GPU: "H100", MemoryGB: 80, HourlyUSD: 2.5, Available: true}}, nil
}
func (p *fakeProvider) Start(_ context.Context, spec StartSpec) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.startErr != nil {
		return "", p.startErr
	}
	p.started = append(p.started, spec)
	id := "pod-" + string(rune('a'+len(p.started)))
	p.state[id] = Status{State: "starting"}
	return id, nil
}
func (p *fakeProvider) Status(_ context.Context, id string) (Status, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.state[id]
	if !ok {
		return Status{State: "exited", Error: "gone"}, nil
	}
	return st, nil
}
func (p *fakeProvider) Stop(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopped = append(p.stopped, id)
	p.state[id] = Status{State: "exited"}
	return nil
}
func (p *fakeProvider) set(id string, st Status) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state[id] = st
}

type memRecorder struct {
	mu   sync.Mutex
	recs []gateway.UsageRecord
}

func (m *memRecorder) Record(_ context.Context, r gateway.UsageRecord) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recs = append(m.recs, r)
}

// modelServer answers like vLLM when up is true.
func modelServer(t *testing.T, up *bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer rk-test" {
			w.WriteHeader(401)
			return
		}
		if !*up {
			w.WriteHeader(503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"m"}]}`))
		case "/v1/chat/completions":
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"x"}}]}`))
		default:
			w.WriteHeader(404)
		}
	}))
}

func testTemplate() Template {
	t := Template{Name: "h100-test", Provider: "fake", GPU: "H100", Image: "vllm", Model: "m", Args: "--api-key $WS_RENTAL_API_KEY", Env: map[string]string{"HF_TOKEN": "$HF_TOKEN"},
		IdleTimeout: 20 * time.Minute, MaxHours: 6, WarmupTimeout: 25 * time.Minute, HourlyUSD: 2.5,
		Endpoint: TemplateEndpoint{ID: "rental/test", DisplayName: "test", Capabilities: gateway.Capabilities{ContextWindow: 1000, Tools: true}}}
	_ = t.validate()
	return t
}

// testEnv is what a controller test gets.
type testEnv struct {
	c    *Controller
	db   *fakeStore
	prov *fakeProvider
	rec  *memRecorder
	up   *bool
	now  *time.Time
	url  string
}

// running tells the fake provider the instance's machine is up at the
// test model server.
func (e *testEnv) running(row *store.RentalInstance) {
	e.prov.set(*row.ProviderInstanceID, Status{State: "running", BaseURL: e.url + "/v1", HourlyUSD: 2.49})
}

func newEnv(t *testing.T) *testEnv {
	c, db, prov, rec, up, now, url := newController(t)
	return &testEnv{c: c, db: db, prov: prov, rec: rec, up: up, now: now, url: url}
}

func newController(t *testing.T) (*Controller, *fakeStore, *fakeProvider, *memRecorder, *bool, *time.Time, string) {
	t.Helper()
	db, prov, rec := newFake(), newProvider(), &memRecorder{}
	up := false
	srv := modelServer(t, &up)
	t.Cleanup(srv.Close)
	now := time.Now()
	env := map[string]string{"WS_RENTAL_API_KEY": "rk-test", "HF_TOKEN": "hf-secret"}
	c := &Controller{DB: db, Providers: map[string]Provider{"fake": prov}, Templates: []Template{testTemplate()}, Recorder: rec,
		Reload: func(context.Context) error { db.reloads++; return nil },
		Probe:  srv.Client(), Getenv: func(k string) string { return env[k] }, Now: func() time.Time { return now }, DailyCapHours: 8}
	return c, db, prov, rec, &up, &now, srv.URL
}

func TestStartRegistersDisabledEndpointAndExpandsSecrets(t *testing.T) {
	c, db, prov, _, _, _, _ := newController(t)
	ctx := context.Background()
	row, err := c.Start(ctx, "h100-test", uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != StatusProvisioning || row.ProviderInstanceID == nil || *row.ProviderInstanceID != "pod-b" {
		t.Errorf("row = %+v", row)
	}
	if len(prov.started) != 1 || prov.started[0].Args != "--api-key rk-test" || prov.started[0].Env["HF_TOKEN"] != "hf-secret" {
		t.Errorf("start spec = %+v", prov.started)
	}
	p := db.providers["rental-h100-test"]
	if p.ApiKeyEnv == nil || *p.ApiKeyEnv != "WS_RENTAL_API_KEY" || p.BaseUrl != "http://pending.invalid" {
		t.Errorf("provider row = %+v", p)
	}
	if db.enabled["rental/test"] {
		t.Error("endpoint enabled before the machine is ready")
	}
	if _, err := c.Start(ctx, "h100-test", uuid.New()); !errors.Is(err, ErrBusy) {
		t.Errorf("second start: %v", err)
	}
	if _, err := c.Start(ctx, "nope", uuid.New()); !errors.Is(err, ErrNoTemplate) {
		t.Errorf("unknown template: %v", err)
	}
	c.Disabled = true
	if _, err := c.Start(ctx, "h100-test", uuid.New()); !errors.Is(err, ErrDisabled) {
		t.Errorf("disabled: %v", err)
	}
}

func TestStartFailsWithoutKey(t *testing.T) {
	c, _, _, _, _, _, _ := newController(t)
	c.Getenv = func(string) string { return "" }
	if _, err := c.Start(context.Background(), "h100-test", uuid.New()); !errors.Is(err, ErrNoAPIKey) {
		t.Errorf("no key: %v", err)
	}
}

func TestReconcileWarmsReadyBillsAndStopsIdle(t *testing.T) {
	e := newEnv(t)
	c, db, prov, rec, up, now := e.c, e.db, e.prov, e.rec, e.up, e.now
	ctx := context.Background()
	row, _ := c.Start(ctx, "h100-test", uuid.New())
	e.running(row)

	// Running but the model is not answering yet: warming, still disabled.
	_ = c.Reconcile(ctx)
	got, _ := db.GetRentalInstance(ctx, row.ID)
	if got.Status != StatusWarming || db.enabled["rental/test"] {
		t.Fatalf("after first tick: %+v enabled=%v", got, db.enabled["rental/test"])
	}
	// The model answers: ready, endpoint enabled on the real URL with the
	// provider's price.
	*up = true
	_ = c.Reconcile(ctx)
	got, _ = db.GetRentalInstance(ctx, row.ID)
	if got.Status != StatusReady || !db.enabled["rental/test"] || got.HourlyUsd != 2.49 || !strings.HasSuffix(db.providers["rental-h100-test"].BaseUrl, "/v1") {
		t.Fatalf("after ready: %+v enabled=%v provider=%+v", got, db.enabled["rental/test"], db.providers["rental-h100-test"])
	}
	// Two and a half hours later with recent traffic: two hourly ledger
	// rows, still up.
	*now = now.Add(150 * time.Minute)
	db.lastUse["rental/test"] = now.Add(-5 * time.Minute)
	_ = c.Reconcile(ctx)
	got, _ = db.GetRentalInstance(ctx, row.ID)
	rec.mu.Lock()
	n := len(rec.recs)
	rec.mu.Unlock()
	if got.Status != StatusReady || n != 2 || got.BilledHours != 2 || got.HoursUsed < 2.4 || got.LastRequestAt == nil {
		t.Fatalf("after 2.5h: %+v ledger rows=%d", got, n)
	}
	rec.mu.Lock()
	r0 := rec.recs[0]
	rec.mu.Unlock()
	if r0.Metadata.TaskClass != gateway.TaskRental || r0.CostUSD != 2.49 || r0.EndpointID != "rental/test" {
		t.Errorf("ledger row = %+v", r0)
	}
	// Idle past the timeout: stopped, remainder billed, endpoint gone.
	*now = now.Add(30 * time.Minute)
	_ = c.Reconcile(ctx)
	got, _ = db.GetRentalInstance(ctx, row.ID)
	rec.mu.Lock()
	n = len(rec.recs)
	last := rec.recs[len(rec.recs)-1]
	rec.mu.Unlock()
	if got.Status != StatusStopped || got.StopReason == nil || !strings.Contains(*got.StopReason, "idle") || len(prov.stopped) != 1 {
		t.Fatalf("after idle: %+v stopped=%v", got, prov.stopped)
	}
	if n != 3 || last.CostUSD <= 0 || last.CostUSD >= 2.49 {
		t.Errorf("final billing: rows=%d last=%+v", n, last)
	}
	if _, ok := db.providers["rental-h100-test"]; ok || db.enabled["rental/test"] {
		t.Error("provider and endpoint should be gone after stop")
	}
	// Stopping again is a no-op.
	if err := c.Stop(ctx, row.ID, "again"); err != nil || len(prov.stopped) != 1 {
		t.Errorf("second stop: %v stopped=%v", err, prov.stopped)
	}
}

func TestReconcileStopsOnWarmupTimeout(t *testing.T) {
	e := newEnv(t)
	c, db, now := e.c, e.db, e.now
	ctx := context.Background()
	row, _ := c.Start(ctx, "h100-test", uuid.New())
	e.running(row) // running, but the model never answers (up stays false)
	*now = now.Add(26 * time.Minute)
	_ = c.Reconcile(ctx)
	got, _ := db.GetRentalInstance(ctx, row.ID)
	if got.Status != StatusStopped || got.StopReason == nil || !strings.Contains(*got.StopReason, "not ready") {
		t.Fatalf("warmup timeout: %+v", got)
	}
}

func TestReconcileStopsOnMaxHours(t *testing.T) {
	e := newEnv(t)
	c, db, up, now := e.c, e.db, e.up, e.now
	ctx := context.Background()
	*up = true
	row, _ := c.Start(ctx, "h100-test", uuid.New())
	e.running(row)
	_ = c.Reconcile(ctx) // ready
	*now = now.Add(6*time.Hour + time.Minute)
	db.lastUse["rental/test"] = now.Add(-time.Minute) // busy, but over the per-run cap
	_ = c.Reconcile(ctx)
	got, _ := db.GetRentalInstance(ctx, row.ID)
	if got.Status != StatusStopped || got.StopReason == nil || !strings.Contains(*got.StopReason, "max 6.0 hours") || got.BilledHours < 6 {
		t.Fatalf("max hours: %+v", got)
	}
}

func TestKillSwitchStopsEverything(t *testing.T) {
	e := newEnv(t)
	c, db, prov, up := e.c, e.db, e.prov, e.up
	ctx := context.Background()
	*up = true
	row, _ := c.Start(ctx, "h100-test", uuid.New())
	e.running(row)
	_ = c.Reconcile(ctx)
	c.Disabled = true
	_ = c.Reconcile(ctx)
	got, _ := db.GetRentalInstance(ctx, row.ID)
	if got.Status != StatusStopped || !strings.Contains(*got.StopReason, "kill switch") || len(prov.stopped) != 1 {
		t.Fatalf("kill switch: %+v stopped=%v", got, prov.stopped)
	}
}

func TestProviderExitMarksFailed(t *testing.T) {
	c, db, prov, _, _, _, _ := newController(t)
	ctx := context.Background()
	row, _ := c.Start(ctx, "h100-test", uuid.New())
	prov.set(*row.ProviderInstanceID, Status{State: "exited", Error: "OOM"})
	_ = c.Reconcile(ctx)
	got, _ := db.GetRentalInstance(ctx, row.ID)
	if got.Status != StatusFailed || got.Error == nil || !strings.Contains(*got.Error, "OOM") || db.enabled["rental/test"] {
		t.Errorf("exited: %+v", got)
	}
}

func TestStartProviderFailureCleansUp(t *testing.T) {
	c, db, prov, _, _, _, _ := newController(t)
	prov.startErr = errors.New("no capacity")
	if _, err := c.Start(context.Background(), "h100-test", uuid.New()); err == nil || !strings.Contains(err.Error(), "no capacity") {
		t.Fatalf("err = %v", err)
	}
	if _, ok := db.providers["rental-h100-test"]; ok {
		t.Error("provider row left behind")
	}
	for _, r := range db.rows {
		if r.Status != StatusFailed {
			t.Errorf("row = %+v", r)
		}
	}
}

func TestDailyCap(t *testing.T) {
	c, db, _, _, _, _, _ := newController(t)
	c.DailyCapHours = 1
	db.rows[uuid.New()] = store.RentalInstance{ID: uuid.New(), Template: "other", Status: StatusStopped, HoursUsed: 1.5, StartedAt: time.Now()}
	if _, err := c.Start(context.Background(), "h100-test", uuid.New()); !errors.Is(err, ErrDailyCap) {
		t.Errorf("cap: %v", err)
	}
}

func TestLoadTemplates(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "a.yaml"), []byte("provider: runpod\ngpu: H100\nimage: vllm\nmodel: m\nidle_timeout: 5m\nendpoint:\n  id: rental/a\n  capabilities: { context_window: 4096, tools: true }\n"), 0o600)
	tpls, err := LoadTemplates(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(tpls) != 1 || tpls[0].Name != "a" || tpls[0].IdleTimeout != 5*time.Minute || tpls[0].MaxHours != 6 || tpls[0].Port != 8000 || tpls[0].Endpoint.Capabilities.ContextWindow != 4096 || tpls[0].ProviderID() != "rental-a" {
		t.Errorf("templates = %+v", tpls)
	}
	_ = os.WriteFile(filepath.Join(dir, "Bad Name.yaml"), []byte("provider: runpod\ngpu: H100\nimage: vllm\nmodel: m\nendpoint: {id: x}\n"), 0o600)
	if _, err := LoadTemplates(dir); err == nil {
		t.Error("bad name accepted")
	}
	// The shipped templates parse.
	shipped, err := LoadTemplates(filepath.Join("..", "..", "..", "infra", "rental", "templates"))
	if err != nil || len(shipped) < 2 {
		t.Errorf("shipped templates: %d, %v", len(shipped), err)
	}
	for _, s := range shipped {
		if !strings.Contains(s.Args, "$WS_RENTAL_API_KEY") {
			t.Errorf("%s: args do not require the rental key", s.Name)
		}
	}
}

func TestRunPodAdapter(t *testing.T) {
	var method, path string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		if r.Header.Get("Authorization") != "Bearer rp-key" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && path == "/v1/gputypes":
			_, _ = w.Write([]byte(`[{"id":"NVIDIA H100 80GB HBM3","displayName":"H100 80GB","memoryInGb":80,"secureCloud":true,"securePrice":2.69},{"id":"NVIDIA A40","memoryInGb":48,"communityCloud":true,"communityPrice":0.4}]`))
		case r.Method == "POST" && path == "/v1/pods":
			_ = json.NewDecoder(r.Body).Decode(&body)
			_, _ = w.Write([]byte(`{"id":"abc123","desiredStatus":"RUNNING","costPerHr":2.69}`))
		case r.Method == "GET" && path == "/v1/pods/abc123":
			_, _ = w.Write([]byte(`{"id":"abc123","desiredStatus":"RUNNING","costPerHr":2.69,"runtime":{"uptimeInSeconds":120,"ports":[{"privatePort":8000,"publicPort":31234,"type":"http","ip":"1.2.3.4"}]}}`))
		case r.Method == "GET" && path == "/v1/pods/new":
			_, _ = w.Write([]byte(`{"id":"new","desiredStatus":"RUNNING","costPerHr":2.69}`))
		case r.Method == "GET" && path == "/v1/pods/gone":
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"pod not found"}`))
		case r.Method == "DELETE" && path == "/v1/pods/abc123":
			w.WriteHeader(204)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	rp := &RunPod{APIKey: "rp-key", BaseURL: srv.URL + "/v1", Client: srv.Client()}
	ctx := context.Background()
	offers, err := rp.Offers(ctx)
	if err != nil || len(offers) != 2 || offers[1].HourlyUSD != 2.69 || !offers[0].Available || offers[0].HourlyUSD != 0.4 {
		t.Fatalf("offers = %+v, %v", offers, err)
	}
	tpl := testTemplate()
	tpl.CloudType, tpl.VolumeGB = "secure", 150
	id, err := rp.Start(ctx, StartSpec{Name: "ws-x", Template: tpl, Args: "--model m --api-key k", Env: map[string]string{"HF_TOKEN": "h"}})
	if err != nil || id != "abc123" {
		t.Fatalf("start = %q, %v", id, err)
	}
	if body["imageName"] != "vllm" || body["cloudType"] != "SECURE" || body["volumeInGb"] != float64(150) || body["env"].(map[string]any)["HF_TOKEN"] != "h" {
		t.Errorf("pod body = %v", body)
	}
	if cmd := body["dockerStartCmd"].([]any); len(cmd) != 4 || cmd[0] != "--model" {
		t.Errorf("dockerStartCmd = %v", cmd)
	}
	if ports := body["ports"].([]any); ports[0] != "8000/http" {
		t.Errorf("ports = %v", ports)
	}
	st, err := rp.Status(ctx, "abc123")
	if err != nil || st.State != "running" || st.BaseURL != "https://abc123-8000.proxy.runpod.net/v1" || st.HourlyUSD != 2.69 {
		t.Errorf("status = %+v, %v", st, err)
	}
	if st, _ := rp.Status(ctx, "new"); st.State != "starting" || st.BaseURL != "" {
		t.Errorf("new status = %+v", st)
	}
	if st, err := rp.Status(ctx, "gone"); err != nil || st.State != "exited" {
		t.Errorf("gone status = %+v, %v", st, err)
	}
	if err := rp.Stop(ctx, "abc123"); err != nil || method != "DELETE" {
		t.Errorf("stop: %v (%s)", err, method)
	}
	rp.APIKey = "bad"
	if _, err := rp.Offers(ctx); err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Errorf("bad key: %v", err)
	}
}
