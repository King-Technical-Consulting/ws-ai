package training

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
)

// specGW answers the flesh-out call with a canned reply and records it.
type specGW struct {
	mu     sync.Mutex
	reply  string
	finish gateway.FinishReason
	calls  []*gateway.Request
	err    error
}

func (g *specGW) Complete(_ context.Context, req *gateway.Request) (*gateway.Response, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, req)
	if g.err != nil {
		return nil, g.err
	}
	return &gateway.Response{Model: "test-model", FinishReason: g.finish, Parts: []gateway.Part{gateway.TextPart(g.reply)}}, nil
}

func specReply(ds ...string) string {
	maps := []map[string]string{}
	for _, d := range ds {
		maps = append(maps, map[string]string{"dataset_id": d, "input": "question", "output": "answer", "system": "", "split": "", "note": "named like it"})
	}
	b, _ := json.Marshal(map[string]any{
		"system_prompt":   "You answer questions about whales.",
		"mappings":        maps,
		"rubric":          "A good answer names the species.",
		"hyperparameters": map[string]any{"epochs": 3, "learning_rate": 0.0002, "rank": 16, "max_seq_len": 1024},
		"data_notes":      "Few examples.",
	})
	return string(b)
}

func recipeService(t *testing.T) (*fakeStore, *Service, *specGW, *[]bool) {
	t.Helper()
	db := newFake()
	svc := newService(t, db)
	gw := &specGW{finish: gateway.FinishStop}
	svc.GW = gw
	var replaces []bool
	svc.EnqueueSpec = func(_ context.Context, _ uuid.UUID, replace bool) error {
		replaces = append(replaces, replace)
		return nil
	}
	return db, svc, gw, &replaces
}

func readySet(t *testing.T, svc *Service, db *fakeStore) uuid.UUID {
	t.Helper()
	ds, err := svc.CreateUploaded(context.Background(), uuid.New(), UploadSpec{Name: "whales", SourceName: "w.csv"}, strings.NewReader(csvOf(20)))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Build(context.Background(), ds.ID); err != nil {
		t.Fatal(err)
	}
	return ds.ID
}

func TestRecipeValidation(t *testing.T) {
	db, svc, _, _ := recipeService(t)
	ctx := context.Background()
	owner := uuid.New()
	for name, in := range map[string]RecipeInput{
		"no name":      {BaseModel: "Qwen/Qwen2.5-0.5B-Instruct"},
		"no base":      {Name: "r"},
		"long":         {Name: "r", BaseModel: "b", Instructions: strings.Repeat("x", maxInstructions+1)},
		"bad dataset":  {Name: "r", BaseModel: "b", DatasetIDs: []uuid.UUID{uuid.New()}},
		"bad endpoint": {Name: "r", BaseModel: "b", BaseEndpointID: "nope"},
		"bad target":   {Name: "r", BaseModel: "b", Target: "mars"},
	} {
		if _, err := svc.CreateRecipe(ctx, owner, in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	bare, err := svc.CreateRecipe(ctx, owner, RecipeInput{Name: "bare", BaseModel: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if bare.DatasetIds == nil {
		t.Error("a recipe with no datasets must send an empty list, not nil (NULL violates the column's NOT NULL)")
	}
	id := readySet(t, svc, db)
	r, err := svc.CreateRecipe(ctx, owner, RecipeInput{Name: " whales ", BaseModel: "Qwen/Qwen2.5-0.5B-Instruct", DatasetIDs: []uuid.UUID{id, id}, Instructions: "Identify whales."})
	if err != nil {
		t.Fatal(err)
	}
	if r.Name != "whales" || len(r.DatasetIds) != 1 || r.SpecModel != "auto" || r.SpecStatus != "none" {
		t.Errorf("recipe = %+v", r)
	}
	if _, err := svc.UpdateRecipe(ctx, uuid.New(), RecipeInput{Name: "x", BaseModel: "b"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("update unknown: %v", err)
	}
}

func TestFleshOutWritesTheDraftAndKeepsEdits(t *testing.T) {
	db, svc, gw, replaces := recipeService(t)
	ctx := context.Background()
	dsID := readySet(t, svc, db)
	r, _ := svc.CreateRecipe(ctx, uuid.New(), RecipeInput{Name: "whales", BaseModel: "Qwen/Qwen2.5-0.5B-Instruct", DatasetIDs: []uuid.UUID{dsID}, Instructions: "Identify whales from questions."})

	if _, err := svc.FleshOut(ctx, r.ID, "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.FleshOut(ctx, r.ID, "", false); !errors.Is(err, ErrNotReady) {
		t.Errorf("second flesh out while queued: %v", err)
	}
	gw.reply = specReply(dsID.String(), uuid.NewString()) // the second dataset is not attached
	if err := svc.RunSpec(ctx, r.ID, false); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetRecipe(ctx, r.ID)
	if got.SpecStatus != "ready" || got.SpecUsedModel == nil || *got.SpecUsedModel != "test-model" {
		t.Fatalf("recipe = %+v", got)
	}
	var sp Spec
	_ = json.Unmarshal(got.Spec, &sp)
	if sp.SystemPrompt != "You answer questions about whales." || len(sp.Mappings) != 1 || sp.Mappings[0].DatasetID != dsID.String() || sp.Hyperparameters.Epochs != 3 {
		t.Errorf("spec = %+v", sp)
	}
	if !strings.Contains(sp.DataNotes, "dropped 1 mapping") {
		t.Errorf("data notes = %q", sp.DataNotes)
	}

	// What the model was sent: the instructions, the sample, the class, the schema.
	req := gw.calls[0]
	user := req.Messages[0].Parts[0].Text
	if req.Metadata.TaskClass != gateway.TaskTraining || req.Model != "auto" || req.JSON == nil || len(req.JSON.Schema) == 0 || req.System != fleshOutPrompt {
		t.Errorf("request = %+v", req)
	}
	for _, want := range []string{"Identify whales from questions.", "Qwen/Qwen2.5-0.5B-Instruct", "columns found in the upload: id, question, answer, notes", "what is 0, exactly?", dsID.String()} {
		if !strings.Contains(user, want) {
			t.Errorf("the model was not sent %q:\n%s", want, user)
		}
	}
	if strings.Count(user, "USER:") > sampleCount {
		t.Errorf("more than %d samples sent", sampleCount)
	}

	// The owner edits the system prompt; a second flesh out keeps it.
	edit := map[string]json.RawMessage{"system_prompt": json.RawMessage(`"My own prompt."`)}
	if _, err := svc.SetSpec(ctx, r.ID, edit); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetSpec(ctx, r.ID, map[string]json.RawMessage{"rubric": json.RawMessage(`"A good answer names the species."`)}); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetRecipe(ctx, r.ID)
	if len(got.Edited) != 1 || got.Edited[0] != "system_prompt" {
		t.Errorf("edited = %v (an edit that changes nothing must not count)", got.Edited)
	}
	gw.reply = strings.ReplaceAll(specReply(dsID.String()), "whales", "dolphins")
	if _, err := svc.FleshOut(ctx, r.ID, "fast", false); err != nil {
		t.Fatal(err)
	}
	if err := svc.RunSpec(ctx, r.ID, false); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetRecipe(ctx, r.ID)
	_ = json.Unmarshal(got.Spec, &sp)
	if sp.SystemPrompt != "My own prompt." || !strings.Contains(sp.Rubric, "names the species") || got.SpecModel != "fast" || gw.calls[1].Model != "fast" {
		t.Errorf("after rerun: prompt %q rubric %q model %q", sp.SystemPrompt, sp.Rubric, got.SpecModel)
	}
	var draft Spec
	_ = json.Unmarshal(got.SpecDraft, &draft)
	if draft.SystemPrompt != "You answer questions about dolphins." {
		t.Errorf("the draft should hold the model's own text: %q", draft.SystemPrompt)
	}

	// Replace drops the edits.
	if _, err := svc.FleshOut(ctx, r.ID, "", true); err != nil {
		t.Fatal(err)
	}
	if !(*replaces)[len(*replaces)-1] {
		t.Error("replace was not passed to the job")
	}
	if err := svc.RunSpec(ctx, r.ID, true); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetRecipe(ctx, r.ID)
	_ = json.Unmarshal(got.Spec, &sp)
	if sp.SystemPrompt != "You answer questions about dolphins." || len(got.Edited) != 0 {
		t.Errorf("after replace: %q edited %v", sp.SystemPrompt, got.Edited)
	}
}

func TestFleshOutFailures(t *testing.T) {
	db, svc, gw, _ := recipeService(t)
	ctx := context.Background()
	r, _ := svc.CreateRecipe(ctx, uuid.New(), RecipeInput{Name: "r", BaseModel: "b"})
	if _, err := svc.FleshOut(ctx, r.ID, "", false); !errors.Is(err, ErrInvalid) {
		t.Errorf("no instructions: %v", err)
	}
	if _, err := svc.FleshOut(ctx, uuid.New(), "", false); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
	r, _ = svc.UpdateRecipe(ctx, r.ID, RecipeInput{Name: "r", BaseModel: "b", Instructions: "do it"})
	for name, c := range map[string]struct {
		reply  string
		finish gateway.FinishReason
		err    error
		want   string
	}{
		"prose":    {reply: "I cannot help with that.", finish: gateway.FinishStop, want: "not a usable spec"},
		"empty":    {reply: `{"mappings": []}`, finish: gateway.FinishStop, want: "neither a system prompt nor a rubric"},
		"cut off":  {reply: `{"system_prompt": "You ans`, finish: gateway.FinishLength, want: "not a usable spec"},
		"gw error": {err: errors.New("no usable endpoint"), want: "no usable endpoint"},
	} {
		gw.reply, gw.finish, gw.err = c.reply, c.finish, c.err
		if _, err := svc.FleshOut(ctx, r.ID, "", false); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := svc.RunSpec(ctx, r.ID, false); err != nil {
			t.Fatal(err)
		}
		got, _ := db.GetRecipe(ctx, r.ID)
		if got.SpecStatus != "failed" || got.SpecError == nil || !strings.Contains(*got.SpecError, c.want) {
			t.Errorf("%s: status %s err %v", name, got.SpecStatus, got.SpecError)
		}
	}
	// A cut-off reply gets a second try with more room.
	gw.calls = nil
	gw.reply, gw.finish, gw.err = `{"system_prompt": "You ans`, gateway.FinishLength, nil
	_, _ = svc.FleshOut(ctx, r.ID, "", false)
	_ = svc.RunSpec(ctx, r.ID, false)
	if len(gw.calls) != 2 || gw.calls[1].MaxTokens <= gw.calls[0].MaxTokens {
		t.Errorf("calls = %d", len(gw.calls))
	}
}

func TestParseSpecClampsAndTolerates(t *testing.T) {
	sp, err := ParseSpec("Here you go:\n```json\n"+`{"system_prompt":"hi","mappings":[],"rubric":"r","hyperparameters":{"epochs":99,"learning_rate":5,"rank":0,"max_seq_len":3},"data_notes":""}`+"\n```", nil)
	if err != nil {
		t.Fatal(err)
	}
	h := sp.Hyperparameters
	if h.Epochs != 10 || h.LearningRate != 2e-4 || h.Rank != 16 || h.MaxSeqLen != 128 {
		t.Errorf("hyperparameters = %+v", h)
	}
	if sp.Mappings == nil {
		t.Error("mappings must be an empty list, not null")
	}
}

func TestSetSpecValidation(t *testing.T) {
	db, svc, _, _ := recipeService(t)
	ctx := context.Background()
	r, _ := svc.CreateRecipe(ctx, uuid.New(), RecipeInput{Name: "r", BaseModel: "b"})
	for name, edit := range map[string]map[string]json.RawMessage{
		"unknown key":   {"nope": json.RawMessage(`"x"`)},
		"wrong type":    {"system_prompt": json.RawMessage(`42`)},
		"bad hyper":     {"hyperparameters": json.RawMessage(`{"epochs":0,"learning_rate":0.0002,"rank":8,"max_seq_len":1024}`)},
		"unknown field": {"hyperparameters": json.RawMessage(`{"epochs":1,"learning_rate":0.0002,"rank":8,"max_seq_len":1024,"zzz":1}`)},
	} {
		if _, err := svc.SetSpec(ctx, r.ID, edit); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	ok := map[string]json.RawMessage{
		"hyperparameters": json.RawMessage(`{"epochs":2,"learning_rate":0.0001,"rank":8,"max_seq_len":1024}`),
		"mappings":        json.RawMessage(`[{"dataset_id":"x","input":"q","output":"a","system":"","split":"","note":""}]`),
	}
	if _, err := svc.SetSpec(ctx, r.ID, ok); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetRecipe(ctx, r.ID)
	if len(got.Edited) != 2 {
		t.Errorf("edited = %v", got.Edited)
	}
	if _, err := svc.SetSpec(ctx, uuid.New(), ok); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown recipe: %v", err)
	}
}

func TestSmokeNote(t *testing.T) {
	if smokeNote(SmokeEvalExamples) != "" || smokeNote(50) != "" {
		t.Error("an eval of 30 or more is not a smoke result")
	}
	n := smokeNote(4)
	if !strings.Contains(n, "smoke result") || !strings.Contains(n, "only 4 held-out examples") || !strings.Contains(n, "25 points") {
		t.Errorf("note = %q", n)
	}
	if smokeNote(0) == "" {
		t.Error("no examples at all is a smoke result too")
	}
	if evalExamples([]byte(`{"examples": 4, "judge": "x"}`)) != 4 || evalExamples(nil) != 0 {
		t.Error("evalExamples")
	}
}

// The gate's refusal names a small eval; a sound one is refused without the note.
func TestPromoteGateSaysWhenTheEvalIsSmall(t *testing.T) {
	db, svc, _, _ := recipeService(t)
	ctx := context.Background()
	mk := func(examples int) uuid.UUID {
		a, _ := db.CreateAdapter(ctx, store.CreateAdapterParams{Name: "a", BaseModel: "b", BlobKey: "k", EndpointID: strPtr("lora/a")})
		lo, hi := float32(0.33), float32(0.5)
		raw, _ := json.Marshal(map[string]any{"examples": examples})
		_ = db.SetAdapterEval(ctx, store.SetAdapterEvalParams{ID: a.ID, EvalScore: &lo, BaselineScore: &hi, Eval: raw})
		return a.ID
	}
	err := svc.Promote(ctx, mk(4), false, "")
	if !errors.Is(err, ErrNotReady) || !strings.Contains(err.Error(), "smoke result") || !strings.Contains(err.Error(), "only 4 held-out examples") {
		t.Errorf("small eval: %v", err)
	}
	err = svc.Promote(ctx, mk(50), false, "")
	if !errors.Is(err, ErrNotReady) || strings.Contains(err.Error(), "smoke") {
		t.Errorf("sound eval: %v", err)
	}
}
