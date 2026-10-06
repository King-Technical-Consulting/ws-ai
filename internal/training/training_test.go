package training

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
	"github.com/jking323/ws/internal/store/blob"
)

func msg(conv uuid.UUID, seq int64, role string, parts ...gateway.Part) store.Message {
	b, _ := json.Marshal(parts)
	return store.Message{ID: uuid.New(), ConversationID: conv, Seq: seq, Role: role, Parts: b}
}

func TestExamples(t *testing.T) {
	conv := store.Conversation{ID: uuid.New(), Mode: "chat"}
	m1 := msg(conv.ID, 1, "user", gateway.TextPart("hi"))
	m2 := msg(conv.ID, 2, "assistant", gateway.TextPart("hello"))
	model := "opus"
	m2.Model = &model
	m3 := msg(conv.ID, 3, "user", gateway.TextPart("look"), gateway.Part{Kind: gateway.PartImage, Data: []byte{1}})
	m4 := msg(conv.ID, 4, "assistant", gateway.Part{Kind: gateway.PartToolCall, ToolCallID: "c1", ToolName: "web_fetch", Args: json.RawMessage(`{"url":"x"}`)})
	m5 := msg(conv.ID, 5, "tool", gateway.Part{Kind: gateway.PartToolResult, ToolCallID: "c1", Content: []gateway.Part{gateway.TextPart("page")}})
	m6 := msg(conv.ID, 6, "assistant", gateway.TextPart("summary"), gateway.Part{Kind: gateway.PartReasoning, Text: "thinking"})
	cheap := "cheap"
	m6.Model = &cheap
	m7 := msg(conv.ID, 7, "assistant", gateway.TextPart("orphan first turn")) // no prefix: never an example by itself
	msgs := []store.Message{m7, m1, m2, m3, m4, m5, m6}
	msgs = msgs[1:] // drop the orphan to keep seq order simple
	rt := ratings{m2.ID: 1, m6.ID: -1}

	exs := Examples(conv, msgs, rt, Filters{})
	if len(exs) != 2 { // m2 (upvoted) and m4 (unrated tool call); m6 is downvoted
		t.Fatalf("examples = %d: %+v", len(exs), exs)
	}
	if exs[0].Meta.MessageID != m2.ID.String() || exs[0].Meta.Rating != 1 || exs[0].Meta.Model != "opus" || len(exs[0].Messages) != 2 || exs[0].Messages[1].Content != "hello" {
		t.Errorf("first example = %+v", exs[0])
	}
	second := exs[1]
	if last := second.Messages[len(second.Messages)-1]; len(last.ToolCalls) != 1 || last.ToolCalls[0].Function.Name != "web_fetch" || last.ToolCalls[0].Function.Arguments != `{"url":"x"}` {
		t.Errorf("tool call example = %+v", second)
	}
	if !strings.Contains(second.Messages[2].Content, "[image]") {
		t.Errorf("image placeholder missing: %+v", second.Messages[2])
	}
	// Upvoted only.
	if exs := Examples(conv, msgs, rt, Filters{MinRating: 1}); len(exs) != 1 || exs[0].Meta.MessageID != m2.ID.String() {
		t.Errorf("min_rating 1: %+v", exs)
	}
	// Everything, including the downvoted one; the tool result becomes a tool turn and reasoning is dropped.
	all := Examples(conv, msgs, rt, Filters{MinRating: -1})
	if len(all) != 3 {
		t.Fatalf("min_rating -1: %d", len(all))
	}
	last := all[2]
	if got := last.Messages[len(last.Messages)-1]; got.Content != "summary" || len(got.ToolCalls) != 0 {
		t.Errorf("reasoning should be dropped: %+v", got)
	}
	if tool := last.Messages[len(last.Messages)-2]; tool.Role != "tool" || tool.ToolCallID != "c1" || tool.Content != "page" {
		t.Errorf("tool turn = %+v", tool)
	}
	// Model filter matches the model or the endpoint id.
	if exs := Examples(conv, msgs, rt, Filters{MinRating: -1, Models: []string{"Cheap"}}); len(exs) != 1 || exs[0].Meta.Model != "cheap" {
		t.Errorf("model filter: %+v", exs)
	}
	// Prefix cap keeps the newest turns.
	if exs := Examples(conv, msgs, rt, Filters{MinRating: -1, MaxPrefix: 1}); len(exs[2].Messages) != 2 {
		t.Errorf("max_prefix 1: %d turns", len(exs[2].Messages))
	}
	// Round trip.
	back, err := ParseJSONL(JSONL(all))
	if err != nil || len(back) != 3 || back[1].Meta.MessageID != all[1].Meta.MessageID {
		t.Errorf("jsonl round trip: %v %d", err, len(back))
	}
}

func TestHoldout(t *testing.T) {
	in := 0
	for i := 0; i < 2000; i++ {
		if Holdout(uuid.New(), 10) {
			in++
		}
	}
	if in < 120 || in > 280 {
		t.Errorf("holdout 10%% put %d of 2000 aside", in)
	}
	id := uuid.New()
	if Holdout(id, 10) != Holdout(id, 10) || Holdout(id, 0) {
		t.Error("holdout must be stable and off at 0")
	}
}

func TestParseScore(t *testing.T) {
	for in, want := range map[string]float64{`{"score": 7, "reason": "fine"}`: 7, "Sure:\n```json\n{\"score\": 9.5}\n```": 9.5, `{"score": 14}`: 10, "8/10": 8, "score: 3": 3} {
		if got, ok := ParseScore(in); !ok || got != want {
			t.Errorf("ParseScore(%q) = %v,%v want %v", in, got, ok, want)
		}
	}
	if _, ok := ParseScore("no idea"); ok {
		t.Error("prose should not parse")
	}
}

// ---- service with fakes ----

type fakeStore struct {
	mu       sync.Mutex
	users    map[uuid.UUID]bool // consent
	convs    []store.Conversation
	msgs     map[uuid.UUID][]store.Message
	ratings  map[uuid.UUID][]store.MessageRating
	datasets map[uuid.UUID]store.Dataset
	jobs     map[uuid.UUID]store.FinetuneJob
	adapters map[uuid.UUID]store.Adapter
	eps      map[string]bool // enabled
}

func newFake() *fakeStore {
	return &fakeStore{users: map[uuid.UUID]bool{}, msgs: map[uuid.UUID][]store.Message{}, ratings: map[uuid.UUID][]store.MessageRating{}, datasets: map[uuid.UUID]store.Dataset{}, jobs: map[uuid.UUID]store.FinetuneJob{}, adapters: map[uuid.UUID]store.Adapter{}, eps: map[string]bool{}}
}

func (f *fakeStore) ListTrainingConversations(_ context.Context, p store.ListTrainingConversationsParams) ([]store.Conversation, error) {
	var out []store.Conversation
	for _, c := range f.convs {
		if !f.users[c.UserID] || c.ArchivedAt != nil || c.UpdatedAt.Before(p.Since) {
			continue
		}
		if len(p.Modes) > 0 && !containsFold(p.Modes, c.Mode) {
			continue
		}
		out = append(out, c)
		if len(out) >= int(p.MaxConversations) {
			break
		}
	}
	return out, nil
}
func (f *fakeStore) ListMessages(_ context.Context, id uuid.UUID) ([]store.Message, error) {
	return f.msgs[id], nil
}
func (f *fakeStore) ListRatingsForConversation(_ context.Context, id uuid.UUID) ([]store.MessageRating, error) {
	return f.ratings[id], nil
}
func (f *fakeStore) CreateDataset(_ context.Context, p store.CreateDatasetParams) (store.Dataset, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := store.Dataset{ID: uuid.New(), OwnerID: p.OwnerID, Name: p.Name, TaskClass: p.TaskClass, Filters: p.Filters, Status: StatusQueued, CreatedAt: time.Now()}
	f.datasets[d.ID] = d
	return d, nil
}
func (f *fakeStore) GetDataset(_ context.Context, id uuid.UUID) (store.Dataset, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.datasets[id]
	if !ok {
		return d, pgx.ErrNoRows
	}
	return d, nil
}
func (f *fakeStore) ListDatasets(_ context.Context, _ int32) ([]store.Dataset, error) {
	return nil, nil
}
func (f *fakeStore) SetDatasetStatus(_ context.Context, p store.SetDatasetStatusParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.datasets[p.ID]
	d.Status, d.Error = p.Status, p.Error
	f.datasets[p.ID] = d
	return nil
}
func (f *fakeStore) FinishDataset(_ context.Context, p store.FinishDatasetParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.datasets[p.ID]
	d.Status, d.BlobKey, d.EvalBlobKey, d.Bytes, d.Examples, d.EvalExamples = StatusReady, p.BlobKey, p.EvalBlobKey, p.Bytes, p.Examples, p.EvalExamples
	f.datasets[p.ID] = d
	return nil
}
func (f *fakeStore) DeleteDataset(_ context.Context, id uuid.UUID) error {
	delete(f.datasets, id)
	return nil
}
func (f *fakeStore) CreateFinetuneJob(_ context.Context, p store.CreateFinetuneJobParams) (store.FinetuneJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j := store.FinetuneJob{ID: uuid.New(), OwnerID: p.OwnerID, DatasetID: p.DatasetID, BaseModel: p.BaseModel, BaseEndpointID: p.BaseEndpointID, AdapterName: p.AdapterName, Config: p.Config, Status: StatusQueued}
	f.jobs[j.ID] = j
	return j, nil
}
func (f *fakeStore) GetFinetuneJob(_ context.Context, id uuid.UUID) (store.FinetuneJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[id]
	if !ok {
		return j, pgx.ErrNoRows
	}
	return j, nil
}
func (f *fakeStore) ListFinetuneJobs(_ context.Context, _ int32) ([]store.FinetuneJob, error) {
	return nil, nil
}
func (f *fakeStore) ClaimFinetuneJob(_ context.Context, id uuid.UUID) (store.FinetuneJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[id]
	if !ok || j.Status != StatusQueued {
		return j, pgx.ErrNoRows
	}
	j.Status = StatusRunning
	f.jobs[id] = j
	return j, nil
}
func (f *fakeStore) SetFinetuneProgress(_ context.Context, p store.SetFinetuneProgressParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	j := f.jobs[p.ID]
	j.Progress, j.Log = p.Progress, p.Log
	f.jobs[p.ID] = j
	return nil
}
func (f *fakeStore) FinishFinetuneJob(_ context.Context, p store.FinishFinetuneJobParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	j := f.jobs[p.ID]
	j.Status, j.Error, j.AdapterID, j.Log = p.Status, p.Error, p.AdapterID, p.Log
	f.jobs[p.ID] = j
	return nil
}
func (f *fakeStore) CancelFinetuneJob(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	j := f.jobs[id]
	j.Status = StatusCancelled
	f.jobs[id] = j
	return nil
}
func (f *fakeStore) CreateAdapter(_ context.Context, p store.CreateAdapterParams) (store.Adapter, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := store.Adapter{ID: uuid.New(), Name: p.Name, BaseModel: p.BaseModel, BaseEndpointID: p.BaseEndpointID, FinetuneJobID: p.FinetuneJobID, BlobKey: p.BlobKey, Bytes: p.Bytes, EndpointID: p.EndpointID}
	f.adapters[a.ID] = a
	return a, nil
}
func (f *fakeStore) GetAdapter(_ context.Context, id uuid.UUID) (store.Adapter, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.adapters[id]
	if !ok {
		return a, pgx.ErrNoRows
	}
	return a, nil
}
func (f *fakeStore) ListAdapters(_ context.Context, _ int32) ([]store.Adapter, error) {
	return nil, nil
}
func (f *fakeStore) SetAdapterEval(_ context.Context, p store.SetAdapterEvalParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.adapters[p.ID]
	a.EvalScore, a.BaselineScore, a.Eval = p.EvalScore, p.BaselineScore, p.Eval
	f.adapters[p.ID] = a
	return nil
}
func (f *fakeStore) SetAdapterPromoted(_ context.Context, p store.SetAdapterPromotedParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.adapters[p.ID]
	a.Promoted = p.Promoted
	f.adapters[p.ID] = a
	return nil
}
func (f *fakeStore) DeleteAdapter(_ context.Context, id uuid.UUID) error {
	delete(f.adapters, id)
	return nil
}
func (f *fakeStore) UpsertEndpoint(_ context.Context, p store.UpsertEndpointParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.eps[p.ID] = p.Enabled
	return nil
}
func (f *fakeStore) SetEndpointEnabled(_ context.Context, p store.SetEndpointEnabledParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.eps[p.ID] = p.Enabled
	return nil
}
func (f *fakeStore) DeleteEndpoint(_ context.Context, id string) error { delete(f.eps, id); return nil }

type fakeRunner struct {
	err  error
	spec RunSpec
}

func (r *fakeRunner) Run(_ context.Context, spec RunSpec, progress func(float64, string)) ([]byte, string, error) {
	r.spec = spec
	if progress != nil {
		progress(0.5, "progress 0.5\n")
	}
	if r.err != nil {
		return nil, "boom", r.err
	}
	return []byte("tar"), "done", nil
}

// fakeGW answers eval calls: the adapter endpoint echoes the reference
// (a perfect answer), the base says something else, and the judge
// scores the candidate 10 when it matches the reference, else 4.
type fakeGW struct {
	refs map[string]string // prompt text → reference
}

func (g *fakeGW) Complete(_ context.Context, req *gateway.Request) (*gateway.Response, error) {
	text := req.Messages[len(req.Messages)-1].Parts[0].Text
	if req.Metadata.TaskClass == gateway.TaskClassify {
		i := strings.Index(text, "Reference answer:\n")
		j := strings.Index(text, "\n\nCandidate answer:\n")
		k := strings.LastIndex(text, "\n\nScore the candidate.")
		ref := text[i+len("Reference answer:\n") : j]
		cand := text[j+len("\n\nCandidate answer:\n") : k]
		if ref == cand {
			return &gateway.Response{Parts: []gateway.Part{gateway.TextPart(`{"score": 10}`)}}, nil
		}
		return &gateway.Response{Parts: []gateway.Part{gateway.TextPart(`{"score": 4}`)}}, nil
	}
	if strings.HasPrefix(req.Model, "lora/") {
		return &gateway.Response{Parts: []gateway.Part{gateway.TextPart(g.refs[text])}}, nil
	}
	return &gateway.Response{Parts: []gateway.Part{gateway.TextPart("something else")}}, nil
}

func newService(t *testing.T, db *fakeStore) *Service {
	t.Helper()
	fs, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := &gateway.Endpoint{ID: "local/qwen", ProviderID: "local", ModelName: "Qwen/Qwen3-8B", DisplayName: "Qwen", Local: true, Capabilities: gateway.Capabilities{Tools: true, ContextWindow: 32768}}
	return &Service{DB: db, Blobs: fs, Endpoint: func(id string) (*gateway.Endpoint, bool) {
		if id == base.ID {
			return base, true
		}
		return nil, false
	}}
}

func TestDatasetBuildFinetuneEvalPromote(t *testing.T) {
	db := newFake()
	svc := newService(t, db)
	ctx := context.Background()
	owner, member := uuid.New(), uuid.New()
	db.users[owner] = true // member has not opted in
	refs := map[string]string{}
	// 30 conversations: the member's are never exported; the owner's each
	// hold one rated answer.
	for i := 0; i < 30; i++ {
		uid := owner
		if i%3 == 0 {
			uid = member
		}
		c := store.Conversation{ID: uuid.New(), UserID: uid, Mode: "chat", UpdatedAt: time.Now()}
		db.convs = append(db.convs, c)
		q := "question " + c.ID.String()[:8]
		a := "answer " + c.ID.String()[:8]
		refs[q] = a
		m1 := msg(c.ID, 1, "user", gateway.TextPart(q))
		m2 := msg(c.ID, 2, "assistant", gateway.TextPart(a))
		db.msgs[c.ID] = []store.Message{m1, m2}
		db.ratings[c.ID] = []store.MessageRating{{MessageID: m2.ID, UserID: uid, Score: 1}}
	}
	if _, err := svc.CreateDataset(ctx, owner, DatasetSpec{Name: " "}); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty name: %v", err)
	}
	if _, err := svc.CreateDataset(ctx, owner, DatasetSpec{Name: "x", Filters: Filters{Modes: []string{"nope"}}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad mode: %v", err)
	}
	ds, err := svc.CreateDataset(ctx, owner, DatasetSpec{Name: "chat upvotes", Filters: Filters{MinRating: 1, HoldoutPct: 30}})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Build(ctx, ds.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetDataset(ctx, ds.ID)
	if got.Status != StatusReady || got.Examples+got.EvalExamples != 20 || got.EvalExamples == 0 || got.BlobKey == nil || got.EvalBlobKey == nil {
		t.Fatalf("dataset = %+v", got)
	}
	train, _ := svc.DatasetFile(ctx, ds.ID, false)
	exs, _ := ParseJSONL(train)
	if len(exs) != int(got.Examples) || exs[0].Messages[0].Role != "user" {
		t.Errorf("train file = %d examples", len(exs))
	}

	// A fine-tune needs a runner, a ready dataset and a valid name.
	if _, err := svc.StartFinetune(ctx, owner, FinetuneSpec{DatasetID: ds.ID, AdapterName: "chat-v1", BaseEndpointID: "local/qwen"}); !errors.Is(err, ErrNoRunner) {
		t.Errorf("no runner: %v", err)
	}
	runner := &fakeRunner{}
	svc.Runner = runner
	if _, err := svc.StartFinetune(ctx, owner, FinetuneSpec{DatasetID: ds.ID, AdapterName: "Bad Name!", BaseEndpointID: "local/qwen"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad name: %v", err)
	}
	if _, err := svc.StartFinetune(ctx, owner, FinetuneSpec{DatasetID: ds.ID, AdapterName: "chat-v1", BaseEndpointID: "local/none"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown base: %v", err)
	}
	job, err := svc.StartFinetune(ctx, owner, FinetuneSpec{DatasetID: ds.ID, AdapterName: "Chat-v1", BaseEndpointID: "local/qwen", Config: FinetuneConfig{Epochs: 3}})
	if err != nil {
		t.Fatal(err)
	}
	if job.BaseModel != "Qwen/Qwen3-8B" || job.AdapterName != "chat-v1" {
		t.Errorf("job = %+v", job)
	}
	if err := svc.RunFinetune(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	j, _ := db.GetFinetuneJob(ctx, job.ID)
	if j.Status != StatusDone || !j.AdapterID.Valid || runner.spec.Config.Epochs != 3 || runner.spec.Config.Rank != 16 || len(runner.spec.Train) == 0 || len(runner.spec.Eval) == 0 {
		t.Fatalf("job after run = %+v spec=%+v", j, runner.spec.Config)
	}
	ad, _ := db.GetAdapter(ctx, j.AdapterID.UUID)
	if ad.EndpointID == nil || *ad.EndpointID != "lora/chat-v1" || db.eps["lora/chat-v1"] {
		t.Errorf("adapter = %+v eps=%v", ad, db.eps)
	}
	_, tarball, err := svc.AdapterFile(ctx, ad.ID)
	if err != nil || string(tarball) != "tar" {
		t.Errorf("adapter file: %s %v", tarball, err)
	}
	// Promote needs the eval gate.
	if err := svc.Promote(ctx, ad.ID, false); !errors.Is(err, ErrNotReady) {
		t.Errorf("promote before eval: %v", err)
	}
	svc.GW = &fakeGW{refs: refs}
	svc.EvalLimit = 5
	if err := svc.Evaluate(ctx, ad.ID); err != nil {
		t.Fatal(err)
	}
	ad, _ = db.GetAdapter(ctx, ad.ID)
	if ad.EvalScore == nil || *ad.EvalScore != 1 || ad.BaselineScore == nil || *ad.BaselineScore != 0.4 {
		t.Fatalf("eval = %+v", ad)
	}
	var res EvalResult
	_ = json.Unmarshal(ad.Eval, &res)
	if res.Examples != min(5, int(got.EvalExamples)) || res.AdapterWins != res.Examples {
		t.Errorf("eval result = %+v", res)
	}
	if err := svc.Promote(ctx, ad.ID, false); err != nil {
		t.Fatal(err)
	}
	ad, _ = db.GetAdapter(ctx, ad.ID)
	if !ad.Promoted || !db.eps["lora/chat-v1"] {
		t.Errorf("after promote = %+v eps=%v", ad, db.eps)
	}
	if err := svc.Unpromote(ctx, ad.ID); err != nil || db.eps["lora/chat-v1"] {
		t.Errorf("unpromote: %v eps=%v", err, db.eps)
	}
	// A failed trainer fails the job with its log.
	runner.err = errors.New("cuda out of memory")
	job2, _ := svc.StartFinetune(ctx, owner, FinetuneSpec{DatasetID: ds.ID, AdapterName: "chat-v2", BaseEndpointID: "local/qwen"})
	_ = svc.RunFinetune(ctx, job2.ID)
	if j2, _ := db.GetFinetuneJob(ctx, job2.ID); j2.Status != StatusFailed || j2.Error == nil || !strings.Contains(*j2.Error, "out of memory") || j2.Log != "boom" {
		t.Errorf("failed job = %+v", j2)
	}
	if err := svc.DeleteAdapter(ctx, ad.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := db.eps["lora/chat-v1"]; ok {
		t.Error("deleting the adapter should drop its endpoint")
	}
	// A dataset nobody matches fails with a reason.
	empty, _ := svc.CreateDataset(ctx, owner, DatasetSpec{Name: "none", Filters: Filters{Modes: []string{"code"}}})
	_ = svc.Build(ctx, empty.ID)
	if e, _ := db.GetDataset(ctx, empty.ID); e.Status != StatusFailed || e.Error == nil || !strings.Contains(*e.Error, "no examples") {
		t.Errorf("empty dataset = %+v", e)
	}
}
