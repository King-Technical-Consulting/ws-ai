package agents

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"

	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
)

// memStore is an in-memory agent_memory with brute-force cosine search.
type memStore struct {
	*fakeStore
	mems    map[uuid.UUID]store.AgentMemory
	touched []uuid.UUID
}

func newMemStore() *memStore {
	return &memStore{fakeStore: newFake(), mems: map[uuid.UUID]store.AgentMemory{}}
}

func (m *memStore) ListMessages(_ context.Context, id uuid.UUID) ([]store.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.msgs[id], nil
}

func (m *memStore) UpsertMemory(_ context.Context, p store.UpsertMemoryParams) (store.AgentMemory, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, r := range m.mems {
		if r.AgentID == p.AgentID && string(r.ContentHash) == string(p.ContentHash) {
			if p.Importance > r.Importance {
				r.Importance = p.Importance
			}
			if p.Embedding != nil {
				r.Embedding = p.Embedding
			}
			m.mems[id] = r
			return r, nil
		}
	}
	r := store.AgentMemory{ID: uuid.New(), AgentID: p.AgentID, Kind: p.Kind, Content: p.Content, ContentHash: p.ContentHash, Embedding: p.Embedding, SourceRunID: p.SourceRunID, Importance: p.Importance}
	m.mems[r.ID] = r
	return r, nil
}

func (m *memStore) SearchMemories(_ context.Context, p store.SearchMemoriesParams) ([]store.SearchMemoriesRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	q := p.Column2.Slice()
	var out []store.SearchMemoriesRow
	for _, r := range m.mems {
		if r.AgentID != p.AgentID || r.Embedding == nil {
			continue
		}
		out = append(out, store.SearchMemoriesRow{ID: r.ID, AgentID: r.AgentID, Kind: r.Kind, Content: r.Content, Importance: r.Importance, Distance: cosineDistance(q, r.Embedding.Slice())})
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Distance < out[j-1].Distance; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	if len(out) > int(p.Limit) {
		out = out[:p.Limit]
	}
	return out, nil
}

func (m *memStore) TopMemoriesByImportance(_ context.Context, p store.TopMemoriesByImportanceParams) ([]store.AgentMemory, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.AgentMemory
	for _, r := range m.mems {
		if r.AgentID == p.AgentID {
			out = append(out, r)
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Importance > out[j-1].Importance; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	if len(out) > int(p.Limit) {
		out = out[:p.Limit]
	}
	return out, nil
}

func (m *memStore) TouchMemories(_ context.Context, ids []uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.touched = append(m.touched, ids...)
	return nil
}

func cosineDistance(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 1
	}
	return 1 - dot/(math.Sqrt(na)*math.Sqrt(nb))
}

// fakeGW scripts the reflection reply and embeds by a keyword table so
// nearness is predictable: a text gets a one-hot vector for the first
// keyword it contains.
type fakeGW struct {
	reply     string
	embedErr  error
	completes int
	embeds    int
	width     int
}

var keywords = []string{"server", "disk", "owner", "deploy"}

func (f *fakeGW) Complete(_ context.Context, req *gateway.Request) (*gateway.Response, error) {
	f.completes++
	if req.Metadata.TaskClass != gateway.TaskReflect {
		return nil, errors.New("wrong task class")
	}
	return &gateway.Response{EndpointID: "local/small", Parts: []gateway.Part{gateway.TextPart(f.reply)}}, nil
}

func (f *fakeGW) Embed(_ context.Context, req *gateway.EmbedRequest) (*gateway.EmbedResponse, error) {
	f.embeds++
	if f.embedErr != nil {
		return nil, f.embedErr
	}
	width := f.width
	if width == 0 {
		width = EmbedDims
	}
	out := &gateway.EmbedResponse{EndpointID: "local/embed"}
	for _, t := range req.Inputs {
		v := make([]float32, width)
		for i, k := range keywords {
			if strings.Contains(strings.ToLower(t), k) {
				v[i] = 1
				break
			}
		}
		out.Vectors = append(out.Vectors, v)
	}
	return out, nil
}

func TestReflectThenRecall(t *testing.T) {
	db := newMemStore()
	gw := &fakeGW{reply: `Here you go: {"facts":[{"content":"The server is called atlas and runs Debian.","importance":0.9},{"content":"Disk /dev/sda is 80% full.","importance":0.7},{"content":"","importance":1}],"preferences":[{"content":"The owner wants reports under 200 words.","importance":0.8}],"episode":{"content":"Checked logs; found two disk warnings; reported them.","importance":0.5}}`}
	mem := &Memory{DB: db, GW: gw}
	ctx := context.Background()
	ag := store.Agent{ID: uuid.New(), OwnerID: uuid.New(), Goal: "Keep things healthy.", ProjectID: store.NullUUID(uuid.New()), Enabled: true}
	db.agents[ag.ID] = ag
	conv := uuid.New()
	parts, _ := json.Marshal([]gateway.Part{gateway.TextPart("Check the server logs.")})
	_, _ = db.InsertMessage(ctx, store.InsertMessageParams{ConversationID: conv, Seq: 1, Role: "user", Parts: parts})
	reply, _ := json.Marshal([]gateway.Part{{Kind: gateway.PartToolCall, ToolCallID: "c1", ToolName: "bash", Args: json.RawMessage(`{"cmd":"df"}`)}})
	_, _ = db.InsertMessage(ctx, store.InsertMessageParams{ConversationID: conv, Seq: 2, Role: "assistant", Parts: reply})
	run := store.AgentRun{ID: uuid.New(), AgentID: store.NullUUID(ag.ID), ConversationID: conv, Status: "done"}
	db.runs[run.ID] = run

	if err := mem.Reflect(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if len(db.mems) != 4 || gw.completes != 1 || gw.embeds != 1 {
		t.Fatalf("memories = %d completes=%d embeds=%d", len(db.mems), gw.completes, gw.embeds)
	}
	kinds := map[string]int{}
	for _, m := range db.mems {
		kinds[m.Kind]++
		if m.Embedding == nil || m.SourceRunID.UUID != run.ID {
			t.Errorf("memory without embedding or run: %+v", m)
		}
	}
	if kinds[MemoryFact] != 2 || kinds[MemoryPreference] != 1 || kinds[MemoryEpisode] != 1 {
		t.Errorf("kinds = %v", kinds)
	}
	// Reflecting again dedupes on content and keeps the higher importance.
	gw.reply = `{"facts":[{"content":"Disk /dev/sda is 80% full.","importance":0.95}]}`
	_ = mem.Reflect(ctx, run.ID)
	if len(db.mems) != 4 {
		t.Errorf("dedupe failed: %d memories", len(db.mems))
	}
	for _, m := range db.mems {
		if strings.HasPrefix(m.Content, "Disk") && m.Importance != 0.95 {
			t.Errorf("importance not raised: %v", m.Importance)
		}
	}

	// Recall for a disk-related input puts the disk fact first, and the
	// importance-only tail follows.
	got, err := mem.Recall(ctx, ag, "Is the disk still filling up?")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || !strings.HasPrefix(got[0].Content, "Disk") {
		t.Fatalf("recall = %+v", got)
	}
	block := RenderRecall(got)
	if !strings.Contains(block, "[fact] Disk") || !strings.Contains(block, "[preference]") || len(db.touched) != len(got) {
		t.Errorf("block = %q touched=%d", block, len(db.touched))
	}
	// Embedding outage: importance only, no error.
	gw.embedErr = errors.New("embed down")
	got, err = mem.Recall(ctx, ag, "anything")
	if err != nil || len(got) != 4 {
		t.Errorf("degraded recall = %d, %v", len(got), err)
	}
	// Wrong width: stored without a vector, still recalled by importance.
	gw.embedErr, gw.width = nil, 1536
	gw.reply = `{"facts":[{"content":"Deploys happen on Fridays.","importance":0.6}]}`
	_ = mem.Reflect(ctx, run.ID)
	for _, m := range db.mems {
		if strings.HasPrefix(m.Content, "Deploys") && m.Embedding != nil {
			t.Error("wrong-width vector was stored")
		}
	}
	// Disabled memory: nothing recalled, nothing reflected.
	ag.MemoryConfig = json.RawMessage(`{"disabled":true}`)
	db.agents[ag.ID] = ag
	if got, _ := mem.Recall(ctx, ag, "x"); len(got) != 0 {
		t.Error("recall while disabled")
	}
	n := gw.completes
	_ = mem.Reflect(ctx, run.ID)
	if gw.completes != n {
		t.Error("reflect while disabled")
	}
}

func TestStartAppendsRecall(t *testing.T) {
	svc, db, rt, ag := newService(t)
	ms := newMemStore()
	ms.fakeStore = db
	v := pgvector.NewVector(make([]float32, EmbedDims))
	ms.mems[uuid.New()] = store.AgentMemory{ID: uuid.New(), AgentID: ag.ID, Kind: MemoryFact, Content: "The owner is Jeremy.", Importance: 0.9, Embedding: &v}
	svc.Memory = &Memory{DB: ms, GW: &fakeGW{}}
	if _, err := svc.Start(context.Background(), StartParams{AgentID: ag.ID}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rt.last.Request.System, "What you remember") || !strings.Contains(rt.last.Request.System, "The owner is Jeremy.") {
		t.Errorf("system = %q", rt.last.Request.System)
	}
}

func TestTranscriptTrimsFromFront(t *testing.T) {
	var rows []store.Message
	for i := 0; i < 50; i++ {
		p, _ := json.Marshal([]gateway.Part{gateway.TextPart(strings.Repeat("x", 100))})
		rows = append(rows, store.Message{Role: "user", Parts: p})
	}
	s := Transcript(rows, 1000)
	if !strings.HasPrefix(s, "[earlier part omitted]") || len(s) > 1100 {
		t.Errorf("transcript len=%d head=%q", len(s), s[:40])
	}
}
