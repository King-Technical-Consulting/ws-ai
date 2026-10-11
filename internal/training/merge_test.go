package training

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/gateway"
)

// wideCSV has a distinct "notes" column so a different mapping gives different examples.
func wideCSV(n int) string {
	var b strings.Builder
	b.WriteString("id,question,answer,notes\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "%d,what is %d?,it is %d,note about %d\n", i, i, i, i)
	}
	return b.String()
}

type recipeRig struct {
	db     *fakeStore
	svc    *Service
	runner *fakeRunner
	owner  uuid.UUID
	a, c   uuid.UUID // two ready uploaded datasets
	recipe uuid.UUID
}

const rigSystem = "You answer questions about whales."

func newRecipeRig(t *testing.T) *recipeRig {
	t.Helper()
	db := newFake()
	svc := newService(t, db)
	svc.Runner = &fakeRunner{}
	ctx := context.Background()
	owner := uuid.New()
	up := func(name, body string) uuid.UUID {
		ds, err := svc.CreateUploaded(ctx, owner, UploadSpec{Name: name, SourceName: name + ".csv"}, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.Build(ctx, ds.ID); err != nil {
			t.Fatal(err)
		}
		return ds.ID
	}
	a := up("a", wideCSV(30))
	c := up("c", wideCSV(30))
	rec, err := svc.CreateRecipe(ctx, owner, RecipeInput{Name: "whales", BaseModel: "Qwen/Qwen3-8B", BaseEndpointID: "local/qwen", Instructions: "x", DatasetIDs: []uuid.UUID{a, c}})
	if err != nil {
		t.Fatal(err)
	}
	spec := map[string]json.RawMessage{
		"system_prompt":   json.RawMessage(`"` + rigSystem + `"`),
		"rubric":          json.RawMessage(`"A good answer names the species."`),
		"hyperparameters": json.RawMessage(`{"epochs":3,"learning_rate":0.0001,"rank":8,"max_seq_len":1024}`),
		// dataset c is read again with the notes column as its input
		"mappings": json.RawMessage(`[{"dataset_id":"` + c.String() + `","input":"notes","output":"answer","system":"","split":"","note":""}]`),
	}
	if _, err := svc.SetSpec(ctx, rec.ID, spec); err != nil {
		t.Fatal(err)
	}
	return &recipeRig{db: db, svc: svc, runner: svc.Runner.(*fakeRunner), owner: owner, a: a, c: c, recipe: rec.ID}
}

func TestTrainFromRecipe(t *testing.T) {
	r := newRecipeRig(t)
	ctx := context.Background()
	job, err := r.svc.StartFinetune(ctx, r.owner, FinetuneSpec{RecipeID: r.recipe, AdapterName: "whale-v1"})
	if err != nil {
		t.Fatal(err)
	}
	if job.DatasetID.Valid || !job.RecipeID.Valid || job.BaseModel != "Qwen/Qwen3-8B" || job.BaseEndpointID == nil || *job.BaseEndpointID != "local/qwen" {
		t.Fatalf("job = %+v", job)
	}
	var cfg FinetuneConfig
	_ = json.Unmarshal(job.Config, &cfg)
	if cfg.Epochs != 3 || cfg.Rank != 8 || cfg.MaxSeqLen != 1024 || cfg.LearningRate != 0.0001 {
		t.Errorf("config from the spec's hyperparameters = %+v", cfg)
	}
	if err := r.svc.RunFinetune(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := r.db.GetFinetuneJob(ctx, job.ID)
	if got.Status != StatusDone || !got.DatasetID.Valid {
		t.Fatalf("job = %+v", got)
	}

	// The trainer saw ordinary chat JSONL with the recipe's system prompt first on every example.
	trainEx, err := ParseJSONL(r.runner.spec.Train)
	if err != nil {
		t.Fatal(err)
	}
	evalEx, _ := ParseJSONL(r.runner.spec.Eval)
	if len(trainEx)+len(evalEx) != 60 || len(trainEx) == 0 {
		t.Errorf("train %d eval %d, want 60 in all", len(trainEx), len(evalEx))
	}
	var fromNotes int
	for _, ex := range append(trainEx, evalEx...) {
		if ex.Messages[0].Role != "system" || ex.Messages[0].Content != rigSystem || len(ex.Messages) != 3 {
			t.Fatalf("example = %+v", ex.Messages)
		}
		if strings.HasPrefix(ex.Messages[1].Content, "note about") {
			fromNotes++
		}
	}
	if fromNotes != 30 {
		t.Errorf("%d examples came from the notes column, want all 30 of dataset c (read again under the spec's mapping)", fromNotes)
	}

	// The merged dataset is a dataset like any other.
	merged, _ := r.db.GetDataset(ctx, got.DatasetID.UUID)
	var rep MergeReport
	_ = json.Unmarshal(merged.Report, &rep)
	if merged.Kind != "merged" || merged.Status != StatusReady || int(merged.Examples+merged.EvalExamples) != 60 || len(rep.Sources) != 2 || rep.Sources[0].Remapped || !rep.Sources[1].Remapped || !rep.SystemPrompt {
		t.Errorf("merged = %+v report = %+v", merged, rep)
	}
	// The source datasets are untouched.
	a, _ := r.db.GetDataset(ctx, r.a)
	if a.Kind != KindUploaded {
		t.Errorf("source dataset changed: %+v", a)
	}

	// The adapter keeps what it was trained under, and its endpoint serves under it.
	ad := r.db.adapters[got.AdapterID.UUID]
	if !ad.RecipeID.Valid || ad.RecipeID.UUID != r.recipe || ad.SystemPrompt != rigSystem || !strings.Contains(ad.Rubric, "species") {
		t.Errorf("adapter = %+v", ad)
	}
	up := r.db.upserts["lora/whale-v1"]
	var extra map[string]any
	_ = json.Unmarshal(up.ExtraBody, &extra)
	if extra[gateway.DefaultSystemKey] != rigSystem {
		t.Errorf("adapter endpoint extra_body = %v", extra)
	}
}

func TestTrainFromRecipeRefusals(t *testing.T) {
	r := newRecipeRig(t)
	ctx := context.Background()
	for name, spec := range map[string]FinetuneSpec{
		"both":    {RecipeID: r.recipe, DatasetID: r.a, AdapterName: "x1"},
		"unknown": {RecipeID: uuid.New(), AdapterName: "x1"},
	} {
		if _, err := r.svc.StartFinetune(ctx, r.owner, spec); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// no spec yet
	bare, _ := r.svc.CreateRecipe(ctx, r.owner, RecipeInput{Name: "bare", BaseModel: "b", DatasetIDs: []uuid.UUID{r.a}})
	if _, err := r.svc.StartFinetune(ctx, r.owner, FinetuneSpec{RecipeID: bare.ID, AdapterName: "x1"}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "no spec") {
		t.Errorf("no spec: %v", err)
	}
	// a spec but no ready dataset
	nodata, _ := r.svc.CreateRecipe(ctx, r.owner, RecipeInput{Name: "nodata", BaseModel: "b"})
	_, _ = r.svc.SetSpec(ctx, nodata.ID, map[string]json.RawMessage{"system_prompt": json.RawMessage(`"hi"`)})
	if _, err := r.svc.StartFinetune(ctx, r.owner, FinetuneSpec{RecipeID: nodata.ID, AdapterName: "x1"}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "no ready dataset") {
		t.Errorf("no dataset: %v", err)
	}
	// the request's own config wins over the spec's suggestions
	job, err := r.svc.StartFinetune(ctx, r.owner, FinetuneSpec{RecipeID: r.recipe, AdapterName: "x2", Config: FinetuneConfig{Epochs: 7}})
	if err != nil {
		t.Fatal(err)
	}
	var cfg FinetuneConfig
	_ = json.Unmarshal(job.Config, &cfg)
	if cfg.Epochs != 7 || cfg.Rank != 8 {
		t.Errorf("config = %+v", cfg)
	}
	// a recipe deleted before the job runs fails the job with the reason
	r.db.DeleteRecipe(ctx, r.recipe)
	if err := r.svc.RunFinetune(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := r.db.GetFinetuneJob(ctx, job.ID)
	if got.Status != StatusFailed || got.Error == nil || !strings.Contains(*got.Error, "recipe was deleted") {
		t.Errorf("job = %+v", got)
	}
}

// recGW records the evaluation's calls: the answers, which prompt each saw,
// and the judge's instructions.
type recGW struct {
	mu     sync.Mutex
	system map[string]string // endpoint -> system prompt sent
	judge  []string
}

func (g *recGW) Complete(_ context.Context, req *gateway.Request) (*gateway.Response, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if req.Metadata.TaskClass == gateway.TaskClassify {
		g.judge = append(g.judge, req.System)
		return &gateway.Response{Parts: []gateway.Part{gateway.TextPart(`{"score": 7}`)}}, nil
	}
	if g.system == nil {
		g.system = map[string]string{}
	}
	g.system[req.Model] = req.System
	return &gateway.Response{Parts: []gateway.Part{gateway.TextPart("an answer")}}, nil
}

func TestEvalJudgesUnderTheRecipe(t *testing.T) {
	r := newRecipeRig(t)
	ctx := context.Background()
	job, _ := r.svc.StartFinetune(ctx, r.owner, FinetuneSpec{RecipeID: r.recipe, AdapterName: "whale-v1"})
	if err := r.svc.RunFinetune(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := r.db.GetFinetuneJob(ctx, job.ID)
	gw := &recGW{}
	r.svc.GW = gw
	if err := r.svc.Evaluate(ctx, got.AdapterID.UUID); err != nil {
		t.Fatal(err)
	}
	// Both sides answered under the recipe's prompt: fine-tuning is compared with prompting the base.
	if gw.system["lora/whale-v1"] != rigSystem || gw.system["local/qwen"] != rigSystem {
		t.Errorf("answer prompts = %v", gw.system)
	}
	if len(gw.judge) == 0 || !strings.Contains(gw.judge[0], "A good answer names the species.") || !strings.HasPrefix(gw.judge[0], judgeSystem) {
		t.Errorf("judge instructions = %q", gw.judge)
	}
	// A plain dataset adapter still gets the generic judge and no system prompt.
	if judgeSystemFor("") != judgeSystem {
		t.Error("an empty rubric must leave the judge's instructions alone")
	}
}

func TestWithSystem(t *testing.T) {
	ex := Example{Messages: []ChatMessage{{Role: "user", Content: "q"}, {Role: "assistant", Content: "a"}}}
	withSystem(&ex, "S")
	if len(ex.Messages) != 3 || ex.Messages[0].Content != "S" {
		t.Errorf("insert: %+v", ex.Messages)
	}
	withSystem(&ex, "T")
	if len(ex.Messages) != 3 || ex.Messages[0].Content != "T" {
		t.Errorf("replace: %+v", ex.Messages)
	}
	withSystem(&ex, "")
	if ex.Messages[0].Content != "T" {
		t.Errorf("empty must leave it: %+v", ex.Messages)
	}
}
