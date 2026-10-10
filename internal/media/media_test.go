package media

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jking323/ws/internal/agent"
	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
	"github.com/jking323/ws/internal/store/blob"
)

// fakeStore mirrors the media queries' semantics in memory.
type fakeStore struct {
	mu   sync.Mutex
	jobs map[uuid.UUID]store.MediaJob
	atts map[uuid.UUID]store.Attachment
	conv map[uuid.UUID]store.Conversation
}

func newFake() *fakeStore {
	return &fakeStore{jobs: map[uuid.UUID]store.MediaJob{}, atts: map[uuid.UUID]store.Attachment{}, conv: map[uuid.UUID]store.Conversation{}}
}

func (f *fakeStore) CreateMediaJob(_ context.Context, p store.CreateMediaJobParams) (store.MediaJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j := store.MediaJob{ID: uuid.New(), UserID: p.UserID, ProjectID: p.ProjectID, ConversationID: p.ConversationID, Kind: p.Kind,
		Selector: p.Selector, Inputs: p.Inputs, Status: StatusQueued, OutputAttachmentIds: []uuid.UUID{}, CreatedAt: time.Now()}
	f.jobs[j.ID] = j
	return j, nil
}

func (f *fakeStore) GetMediaJob(_ context.Context, id uuid.UUID) (store.MediaJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[id]
	if !ok {
		return store.MediaJob{}, pgx.ErrNoRows
	}
	return j, nil
}

func (f *fakeStore) ClaimMediaJob(_ context.Context, id uuid.UUID) (store.MediaJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[id]
	if !ok || j.Status != StatusQueued {
		return store.MediaJob{}, pgx.ErrNoRows
	}
	now := time.Now()
	j.Status, j.StartedAt = StatusRunning, &now
	f.jobs[id] = j
	return j, nil
}

func (f *fakeStore) SetMediaJobProgress(_ context.Context, p store.SetMediaJobProgressParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	j := f.jobs[p.ID]
	if j.Status == StatusRunning {
		j.Progress = p.Progress
		if p.ProviderJobID != nil {
			j.ProviderJobID = p.ProviderJobID
		}
		f.jobs[p.ID] = j
	}
	return nil
}

func (f *fakeStore) FinishMediaJob(_ context.Context, p store.FinishMediaJobParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	j := f.jobs[p.ID]
	now := time.Now()
	j.Status, j.Progress, j.EndpointID, j.OutputAttachmentIds, j.CostUsd, j.EndedAt = StatusDone, 1, p.EndpointID, p.OutputAttachmentIds, p.CostUsd, &now
	f.jobs[p.ID] = j
	return nil
}

func (f *fakeStore) FailMediaJob(_ context.Context, p store.FailMediaJobParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	j := f.jobs[p.ID]
	now := time.Now()
	j.Status, j.Error, j.EndedAt = StatusFailed, p.Error, &now
	if p.EndpointID != nil {
		j.EndpointID = p.EndpointID
	}
	f.jobs[p.ID] = j
	return nil
}

func (f *fakeStore) CreateAttachment(_ context.Context, p store.CreateAttachmentParams) (store.Attachment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := store.Attachment{ID: uuid.New(), UserID: p.UserID, BlobKey: p.BlobKey, Mime: p.Mime, Bytes: p.Bytes, Sha256: p.Sha256, Filename: p.Filename, Width: p.Width, Height: p.Height, CreatedAt: time.Now()}
	f.atts[a.ID] = a
	return a, nil
}

func (f *fakeStore) ListAttachmentsByIDs(_ context.Context, ids []uuid.UUID) ([]store.Attachment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.Attachment
	for _, id := range ids {
		if a, ok := f.atts[id]; ok {
			out = append(out, a)
		}
	}
	return out, nil
}

func (f *fakeStore) GetAttachment(_ context.Context, id uuid.UUID) (store.Attachment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.atts[id]
	if !ok {
		return store.Attachment{}, pgx.ErrNoRows
	}
	return a, nil
}

func (f *fakeStore) AttachmentMediaProjects(_ context.Context, id uuid.UUID) ([]uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []uuid.UUID
	for _, j := range f.jobs {
		for _, oid := range j.OutputAttachmentIds {
			if oid == id {
				out = append(out, j.ProjectID)
			}
		}
	}
	return out, nil
}

func (f *fakeStore) GetConversation(_ context.Context, id uuid.UUID) (store.Conversation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.conv[id]
	if !ok {
		return store.Conversation{}, pgx.ErrNoRows
	}
	return c, nil
}

// fakeEngine scripts one engine's behaviour.
type fakeEngine struct {
	err   error
	calls int
	last  *Request
}

func (e *fakeEngine) Generate(_ context.Context, _ *gateway.Provider, ep *gateway.Endpoint, req *Request, progress Progress) (*Result, error) {
	e.calls++
	cp := *req
	e.last = &cp
	if e.err != nil {
		return nil, e.err
	}
	progress(0.5, "prov-1")
	res := &Result{}
	if req.Kind == KindVideo {
		res.Outputs = append(res.Outputs, Output{Data: []byte("not really mp4"), MIME: "video/mp4", Seconds: float64(req.Seconds)})
		return res, nil
	}
	for i := 0; i < req.N; i++ {
		res.Outputs = append(res.Outputs, Decode(pngBytes(8+i, 4)))
	}
	return res, nil
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

// wait returns the records once n have arrived (the gateway writes them
// from a goroutine) or fails the test.
func (m *memRecorder) wait(t *testing.T, n int) []gateway.UsageRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		m.mu.Lock()
		recs := append([]gateway.UsageRecord(nil), m.recs...)
		m.mu.Unlock()
		if len(recs) >= n {
			return recs
		}
		if time.Now().After(deadline) {
			t.Fatalf("ledger: want %d records, have %d", n, len(recs))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func pngBytes(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		img.Set(x, 0, color.RGBA{R: uint8(x), A: 255})
	}
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	return b.Bytes()
}

func testGateway(rec gateway.UsageRecorder, eps ...*gateway.Endpoint) *gateway.Gateway {
	reg := gateway.NewRegistry()
	eps = append(eps, &gateway.Endpoint{ID: "anthropic/opus", ProviderID: "anthropic", ModelName: "opus", Enabled: true, Capabilities: gateway.Capabilities{Tools: true}})
	reg.Replace([]*gateway.Provider{{ID: "openai", Kind: gateway.ProviderOpenAICompat, BaseURL: "http://x"}, {ID: "fal", Kind: gateway.ProviderOpenAICompat, BaseURL: "http://y"}, {ID: "anthropic", Kind: gateway.ProviderAnthropic}}, eps)
	policy, _ := gateway.ParsePolicy("name: t\nrules:\n  - match: { task_class: [image] }\n    prefer: [openai/img, fal/img]\n")
	return gateway.New(reg, gateway.NewRouter(reg, []gateway.Policy{policy}), rec, nil)
}

func imageEndpoint(id, engine string, perImage float64) *gateway.Endpoint {
	return &gateway.Endpoint{ID: id, ProviderID: strings.Split(id, "/")[0], ModelName: "m", Enabled: true,
		Capabilities: gateway.Capabilities{Media: &gateway.MediaCaps{Engine: engine, Image: true, Sizes: []string{"1024x1024", "auto"}, MaxImages: 2}},
		Pricing:      gateway.Pricing{PerImage: perImage}}
}

func newService(t *testing.T, rec *memRecorder, engines map[string]Engine, eps ...*gateway.Endpoint) (*Service, *fakeStore) {
	t.Helper()
	fs, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	db := newFake()
	svc := &Service{DB: db, Blobs: fs, GW: testGateway(rec, eps...), Engines: engines}
	return svc, db
}

func TestCreateValidatesAndEstimates(t *testing.T) {
	svc, _ := newService(t, &memRecorder{}, map[string]Engine{"fake": &fakeEngine{}}, imageEndpoint("openai/img", "fake", 0.04))
	ctx := context.Background()
	uid, pid := uuid.New(), uuid.New()
	if _, err := svc.Create(ctx, CreateParams{UserID: uid, ProjectID: pid, Inputs: Inputs{Prompt: "  "}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty prompt: %v", err)
	}
	if _, err := svc.Create(ctx, CreateParams{UserID: uid, ProjectID: pid, Inputs: Inputs{Prompt: "cat", N: 3}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("n over the endpoint's max should fail: %v", err)
	}
	if _, err := svc.Create(ctx, CreateParams{UserID: uid, ProjectID: pid, Inputs: Inputs{Prompt: "cat", Size: "9x9"}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown size should fail: %v", err)
	}
	if _, err := svc.Create(ctx, CreateParams{UserID: uid, ProjectID: pid, Kind: KindVideo, Inputs: Inputs{Prompt: "cat"}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("video with no video endpoint should fail: %v", err)
	} else if !strings.Contains(err.Error(), "no video endpoint is configured") || strings.Contains(err.Error(), "router:") {
		t.Errorf("the person should read the router's plain reason: %v", err)
	}
	if _, err := svc.Create(ctx, CreateParams{UserID: uid, ProjectID: pid, Selector: "anthropic/opus", Inputs: Inputs{Prompt: "cat"}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("text endpoint as selector should fail: %v", err)
	}
	job, err := svc.Create(ctx, CreateParams{UserID: uid, ProjectID: pid, Inputs: Inputs{Prompt: "a cat", N: 2, Size: "AUTO"}})
	if err != nil {
		t.Fatal(err)
	}
	var in Inputs
	_ = json.Unmarshal(job.Inputs, &in)
	if job.Status != StatusQueued || job.Kind != KindImage || job.Selector != "auto" || in.N != 2 || in.EstimateUSD != 0.08 {
		t.Errorf("job = %+v inputs = %+v", job, in)
	}
}

func TestRunStoresOutputsAndLedger(t *testing.T) {
	rec := &memRecorder{}
	eng := &fakeEngine{}
	svc, db := newService(t, rec, map[string]Engine{"fake": eng}, imageEndpoint("openai/img", "fake", 0.04))
	ctx := context.Background()
	job, err := svc.Create(ctx, CreateParams{UserID: uuid.New(), ProjectID: uuid.New(), Inputs: Inputs{Prompt: "a cat", N: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Run(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetMediaJob(ctx, job.ID)
	if got.Status != StatusDone || len(got.OutputAttachmentIds) != 2 || got.EndpointID == nil || *got.EndpointID != "openai/img" || got.CostUsd != 0.08 {
		t.Fatalf("job after run = %+v", got)
	}
	if got.ProviderJobID == nil || *got.ProviderJobID != "prov-1" {
		t.Errorf("provider job id = %v", got.ProviderJobID)
	}
	atts, err := svc.Outputs(ctx, &got)
	if err != nil || len(atts) != 2 {
		t.Fatalf("outputs = %v, %v", atts, err)
	}
	if atts[0].Mime != "image/png" || atts[0].Width == nil || *atts[0].Width != 8 || *atts[1].Width != 9 || !strings.HasSuffix(atts[0].Filename, "-1.png") {
		t.Errorf("attachments = %+v", atts)
	}
	b, err := blob.GetBytes(ctx, svc.Blobs, atts[0].BlobKey)
	if err != nil || len(b) != int(atts[0].Bytes) {
		t.Errorf("blob = %d bytes, %v", len(b), err)
	}
	recs := rec.wait(t, 1)
	if len(recs) != 1 || recs[0].EndpointID != "openai/img" || recs[0].CostUSD != 0.08 || recs[0].Metadata.TaskClass != gateway.TaskImage || recs[0].Decision.Chosen != "openai/img" {
		t.Errorf("ledger = %+v", recs)
	}
	// A second Run is a no-op: the job is not queued any more.
	if err := svc.Run(ctx, job.ID); err != nil || eng.calls != 1 {
		t.Errorf("rerun: err=%v calls=%d", err, eng.calls)
	}
}

func TestRunFailsOverThenFails(t *testing.T) {
	rec := &memRecorder{}
	bad := &fakeEngine{err: errors.New("overloaded")}
	good := &fakeEngine{}
	svc, db := newService(t, rec, map[string]Engine{"bad": bad, "good": good}, imageEndpoint("openai/img", "bad", 0.04), imageEndpoint("fal/img", "good", 0.02))
	ctx := context.Background()
	job, _ := svc.Create(ctx, CreateParams{UserID: uuid.New(), ProjectID: uuid.New(), Inputs: Inputs{Prompt: "x"}})
	_ = svc.Run(ctx, job.ID)
	got, _ := db.GetMediaJob(ctx, job.ID)
	if got.Status != StatusDone || *got.EndpointID != "fal/img" || got.CostUsd != 0.02 || bad.calls != 1 || good.calls != 1 {
		t.Errorf("failover: %+v bad=%d good=%d", got, bad.calls, good.calls)
	}
	if tried := rec.wait(t, 1)[0].Decision.Tried; len(tried) != 2 {
		t.Errorf("tried = %v", tried)
	}

	// Every engine failing marks the job failed with the last error and a
	// ledger row carrying it.
	good.err = errors.New("content policy")
	job2, _ := svc.Create(ctx, CreateParams{UserID: uuid.New(), ProjectID: uuid.New(), Inputs: Inputs{Prompt: "y"}})
	_ = svc.Run(ctx, job2.ID)
	got, _ = db.GetMediaJob(ctx, job2.ID)
	if got.Status != StatusFailed || got.Error == nil || !strings.Contains(*got.Error, "content policy") || *got.EndpointID != "fal/img" {
		t.Errorf("all failed: %+v", got)
	}
	last := rec.wait(t, 2)[1]
	if last.FinishReason != gateway.FinishError || last.Err == "" {
		t.Errorf("failure ledger = %+v", last)
	}
}

func TestRunSkipsEngineThisBuildLacks(t *testing.T) {
	svc, db := newService(t, &memRecorder{}, map[string]Engine{}, imageEndpoint("openai/img", "comfyui", 0))
	ctx := context.Background()
	job, _ := svc.Create(ctx, CreateParams{UserID: uuid.New(), ProjectID: uuid.New(), Inputs: Inputs{Prompt: "x"}})
	_ = svc.Run(ctx, job.ID)
	got, _ := db.GetMediaJob(ctx, job.ID)
	if got.Status != StatusFailed || !strings.Contains(*got.Error, "no engine") {
		t.Errorf("job = %+v", got)
	}
}

func TestRunRepairsAbandonedJob(t *testing.T) {
	svc, db := newService(t, &memRecorder{}, map[string]Engine{"fake": &fakeEngine{}}, imageEndpoint("openai/img", "fake", 0))
	ctx := context.Background()
	job, _ := svc.Create(ctx, CreateParams{UserID: uuid.New(), ProjectID: uuid.New(), Inputs: Inputs{Prompt: "x"}})
	claimed, _ := db.ClaimMediaJob(ctx, job.ID) // another worker took it and died
	old := claimed.StartedAt.Add(-20 * time.Minute)
	db.mu.Lock()
	j := db.jobs[job.ID]
	j.StartedAt = &old
	db.jobs[job.ID] = j
	db.mu.Unlock()
	if err := svc.Run(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetMediaJob(ctx, job.ID)
	if got.Status != StatusFailed || !strings.Contains(*got.Error, "abandoned") {
		t.Errorf("job = %+v", got)
	}
}

func TestPriceByTokensWhenReported(t *testing.T) {
	svc := &Service{}
	ep := &gateway.Endpoint{Pricing: gateway.Pricing{InputPerM: 5, OutputPerM: 40, PerImage: 0.04}}
	res := &Result{Outputs: []Output{{}, {}}, UsageKnown: true, Usage: gateway.Usage{InputTokens: 100, OutputTokens: 1000}}
	if c, _ := svc.price(ep, res); c < 0.0404 || c > 0.0406 {
		t.Errorf("token price = %v", c)
	}
	res.UsageKnown = false
	if c, _ := svc.price(ep, res); c != 0.08 {
		t.Errorf("per-image price = %v", c)
	}
	ep.Local = true
	if c, _ := svc.price(ep, res); c != 0 {
		t.Errorf("local price = %v", c)
	}
}

func TestToolWaitsAndReturnsImages(t *testing.T) {
	svc, db := newService(t, &memRecorder{}, map[string]Engine{"fake": &fakeEngine{}}, imageEndpoint("openai/img", "fake", 0.04))
	// Enqueue runs the job in the background, as the worker would.
	svc.Enqueue = func(ctx context.Context, id uuid.UUID) error {
		go func() { _ = svc.Run(context.Background(), id) }()
		return nil
	}
	conv := store.Conversation{ID: uuid.New(), ProjectID: uuid.New()}
	db.conv[conv.ID] = conv
	tool := &Tool{Svc: svc, Convs: db, Wait: 10 * time.Second, Poll: 10 * time.Millisecond}
	var emitted []string
	tc := agent.ToolCtx{ConversationID: conv.ID, UserID: uuid.New(), Emit: func(name string, _ any) { emitted = append(emitted, name) }}
	res, err := tool.Call(context.Background(), tc, json.RawMessage(`{"prompt":"a cat","n":1}`))
	if err != nil || res.IsError {
		t.Fatalf("tool: %v %+v", err, res)
	}
	// The UI reads the data part as JSON; decode it the same way.
	raw, _ := json.Marshal(res.Data)
	var data struct {
		Endpoint string  `json:"endpoint"`
		CostUSD  float64 `json:"cost_usd"`
		Images   []struct {
			URL   string `json:"url"`
			MIME  string `json:"mime"`
			Width int    `json:"width"`
		} `json:"images"`
	}
	_ = json.Unmarshal(raw, &data)
	if len(data.Images) != 1 || !strings.HasPrefix(data.Images[0].URL, "/api/attachments/") || data.Images[0].Width != 8 || data.Endpoint != "openai/img" || data.CostUSD != 0.04 || !strings.Contains(res.Text, "openai/img") {
		t.Errorf("result = %+v text=%q", data, res.Text)
	}
	if len(emitted) != 1 || emitted[0] != "media-job" {
		t.Errorf("emitted = %v", emitted)
	}
	// A bad prompt is an error result for the model, not a Go error.
	res, err = tool.Call(context.Background(), tc, json.RawMessage(`{"prompt":""}`))
	if err != nil || !res.IsError {
		t.Errorf("empty prompt: %v %+v", err, res)
	}
}

func TestOpenAIImagesEngine(t *testing.T) {
	var gotBody map[string]any
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/out.png" {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(pngBytes(3, 3))
			return
		}
		if r.URL.Path != "/v1/images/generations" || r.Method != http.MethodPost {
			http.Error(w, "wrong route "+r.URL.Path, 404)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		if gotBody["prompt"] == "forbidden" {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"message":"Your request was rejected by the safety system","code":"moderation_blocked"}}`))
			return
		}
		b64 := base64.StdEncoding.EncodeToString(pngBytes(5, 2))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"created": 1,
			"data":    []map[string]any{{"b64_json": b64}, {"url": srv0(r) + "/out.png", "revised_prompt": "a nicer cat"}},
			"usage":   map[string]int{"input_tokens": 12, "output_tokens": 1056},
		})
	}))
	defer srv.Close()
	p := &gateway.Provider{ID: "openai", Kind: gateway.ProviderOpenAICompat, BaseURL: srv.URL + "/v1", APIKey: "sk-test"}
	ep := &gateway.Endpoint{ID: "openai/gpt-image-1", ModelName: "gpt-image-1", ExtraBody: map[string]any{"moderation": "low"}}
	eng := &OpenAIImages{Client: srv.Client()}
	var progress []float64
	res, err := eng.Generate(context.Background(), p, ep, &Request{Kind: KindImage, Model: "gpt-image-1", Prompt: "a cat", N: 2, Size: "1024x1024", Quality: "high"}, func(f float64, _ string) { progress = append(progress, f) })
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer sk-test" || gotBody["model"] != "gpt-image-1" || gotBody["n"] != float64(2) || gotBody["size"] != "1024x1024" || gotBody["quality"] != "high" || gotBody["moderation"] != "low" {
		t.Errorf("request = %v auth=%q", gotBody, gotAuth)
	}
	if _, ok := gotBody["response_format"]; ok {
		t.Error("gpt-image-1 must not get response_format")
	}
	if len(res.Outputs) != 2 || res.Outputs[0].Width != 5 || res.Outputs[1].Width != 3 || res.Outputs[0].MIME != "image/png" {
		t.Errorf("outputs = %+v", res.Outputs)
	}
	if !res.UsageKnown || res.Usage.OutputTokens != 1056 || res.RevisedPrompt != "a nicer cat" {
		t.Errorf("usage = %+v known=%v revised=%q", res.Usage, res.UsageKnown, res.RevisedPrompt)
	}
	if len(progress) != 2 || progress[0] != 0 || progress[1] != 1 {
		t.Errorf("progress = %v", progress)
	}
	// Provider errors carry the API's message.
	_, err = eng.Generate(context.Background(), p, ep, &Request{Kind: KindImage, Model: "gpt-image-1", Prompt: "forbidden", N: 1}, nil)
	if err == nil || !strings.Contains(err.Error(), "HTTP 400") || !strings.Contains(err.Error(), "safety system") {
		t.Errorf("error = %v", err)
	}
	// DALL-E models are asked for base64 explicitly.
	_, _ = eng.Generate(context.Background(), p, ep, &Request{Kind: KindImage, Model: "dall-e-3", Prompt: "x", N: 1}, nil)
	if gotBody["response_format"] != "b64_json" {
		t.Errorf("dall-e request = %v", gotBody)
	}
	// Video is not this engine's.
	if _, err := eng.Generate(context.Background(), p, ep, &Request{Kind: KindVideo, Model: "x", Prompt: "x"}, nil); err == nil {
		t.Error("video should be refused")
	}
}

// srv0 rebuilds the test server's URL from the request.
func srv0(r *http.Request) string { return "http://" + r.Host }
