package agents

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"

	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
)

// Memory kinds (agent_memory.kind).
const (
	MemoryFact       = "fact"
	MemoryEpisode    = "episode"
	MemoryPreference = "preference"
)

// EmbedDims is the width of agent_memory.embedding (migration 0011). An
// embedding model that returns another width is stored without a vector
// and recalled by importance only.
const EmbedDims = 768

// MemoryConfig is agents.memory_config.
type MemoryConfig struct {
	// Disabled turns reflection and recall off for the agent.
	Disabled bool `json:"disabled,omitempty"`
	// K is how many nearest memories to recall (default 8); TopN how many
	// of the most important to add regardless (default 4).
	K    int `json:"k,omitempty"`
	TopN int `json:"top_n,omitempty"`
}

// MemoryStore is the slice of the store memory uses (*store.DB implements it).
type MemoryStore interface {
	GetAgent(ctx context.Context, id uuid.UUID) (store.Agent, error)
	GetRun(ctx context.Context, id uuid.UUID) (store.AgentRun, error)
	ListMessages(ctx context.Context, conversationID uuid.UUID) ([]store.Message, error)
	UpsertMemory(ctx context.Context, arg store.UpsertMemoryParams) (store.AgentMemory, error)
	SearchMemories(ctx context.Context, arg store.SearchMemoriesParams) ([]store.SearchMemoriesRow, error)
	TopMemoriesByImportance(ctx context.Context, arg store.TopMemoriesByImportanceParams) ([]store.AgentMemory, error)
	TouchMemories(ctx context.Context, ids []uuid.UUID) error
}

// Completer is what reflection needs from the gateway (*gateway.Gateway).
type Completer interface {
	Complete(ctx context.Context, req *gateway.Request) (*gateway.Response, error)
	Embed(ctx context.Context, req *gateway.EmbedRequest) (*gateway.EmbedResponse, error)
}

// Memory writes memories after a run (Reflect) and reads them before one
// (Recall). Both go through the gateway under the agent's identity, so
// the ledger and budgets see them: reflection as task class reflect,
// embeddings as embed.
type Memory struct {
	DB  MemoryStore
	GW  Completer
	Log *slog.Logger
	Now func() time.Time
}

// Recalled is one memory chosen for a run.
type Recalled struct {
	ID         uuid.UUID
	Kind       string
	Content    string
	Importance float32
	Distance   float64 // cosine distance, 0 when picked by importance only
}

// Recall returns the memories to show a run that starts with input, and
// marks them used. An embedding failure degrades to importance only.
func (m *Memory) Recall(ctx context.Context, ag store.Agent, input string) ([]Recalled, error) {
	cfg := memoryConfig(ag)
	if cfg.Disabled {
		return nil, nil
	}
	picked := map[uuid.UUID]Recalled{}
	query := strings.TrimSpace(ag.Goal + "\n" + input)
	if query != "" {
		vec, err := m.embed(ctx, ag, []string{query})
		if err != nil {
			m.log().Warn("memory: recall embed failed, importance only", "agent", ag.ID, "err", err)
		} else if len(vec) == 1 && vec[0] != nil {
			rows, err := m.DB.SearchMemories(ctx, store.SearchMemoriesParams{AgentID: ag.ID, Column2: *vec[0], Limit: int32(cfg.K)})
			if err != nil {
				return nil, err
			}
			for _, r := range rows {
				picked[r.ID] = Recalled{ID: r.ID, Kind: r.Kind, Content: r.Content, Importance: r.Importance, Distance: r.Distance}
			}
		}
	}
	top, err := m.DB.TopMemoriesByImportance(ctx, store.TopMemoriesByImportanceParams{AgentID: ag.ID, Limit: int32(cfg.TopN)})
	if err != nil {
		return nil, err
	}
	for _, r := range top {
		if _, ok := picked[r.ID]; !ok {
			picked[r.ID] = Recalled{ID: r.ID, Kind: r.Kind, Content: r.Content, Importance: r.Importance, Distance: 2}
		}
	}
	if len(picked) == 0 {
		return nil, nil
	}
	out := make([]Recalled, 0, len(picked))
	ids := make([]uuid.UUID, 0, len(picked))
	for _, r := range picked {
		out = append(out, r)
		ids = append(ids, r.ID)
	}
	// Nearest first; importance breaks ties and orders the importance-only tail.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Distance != out[j].Distance {
			return out[i].Distance < out[j].Distance
		}
		return out[i].Importance > out[j].Importance
	})
	if err := m.DB.TouchMemories(ctx, ids); err != nil {
		m.log().Warn("memory: touch", "err", err)
	}
	return out, nil
}

// Search returns the k memories nearest to a query (importance only when
// embedding fails), without the importance tail Recall adds, and marks
// them used. It backs the recall tool.
func (m *Memory) Search(ctx context.Context, ag store.Agent, query string, k int) ([]Recalled, error) {
	if k <= 0 {
		k = 8
	}
	if k > 50 {
		k = 50
	}
	var out []Recalled
	query = strings.TrimSpace(query)
	if query != "" {
		vec, err := m.embed(ctx, ag, []string{query})
		if err != nil {
			m.log().Warn("memory: search embed failed, importance only", "agent", ag.ID, "err", err)
		} else if len(vec) == 1 && vec[0] != nil {
			rows, err := m.DB.SearchMemories(ctx, store.SearchMemoriesParams{AgentID: ag.ID, Column2: *vec[0], Limit: int32(k)})
			if err != nil {
				return nil, err
			}
			for _, r := range rows {
				out = append(out, Recalled{ID: r.ID, Kind: r.Kind, Content: r.Content, Importance: r.Importance, Distance: r.Distance})
			}
		}
	}
	if len(out) == 0 {
		top, err := m.DB.TopMemoriesByImportance(ctx, store.TopMemoriesByImportanceParams{AgentID: ag.ID, Limit: int32(k)})
		if err != nil {
			return nil, err
		}
		for _, r := range top {
			out = append(out, Recalled{ID: r.ID, Kind: r.Kind, Content: r.Content, Importance: r.Importance, Distance: 2})
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	ids := make([]uuid.UUID, 0, len(out))
	for _, r := range out {
		ids = append(ids, r.ID)
	}
	if err := m.DB.TouchMemories(ctx, ids); err != nil {
		m.log().Warn("memory: touch", "err", err)
	}
	return out, nil
}

// Remember stores one memory the agent wrote itself, embedded when the
// embedding model answers. It backs the remember tool.
func (m *Memory) Remember(ctx context.Context, ag store.Agent, kind, content string, importance float32, runID uuid.NullUUID) (*store.AgentMemory, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, fmt.Errorf("%w: content is required", ErrInvalid)
	}
	if len(content) > MaxMemoryContent {
		content = content[:MaxMemoryContent]
	}
	switch kind {
	case "":
		kind = MemoryFact
	case MemoryFact, MemoryPreference, MemoryEpisode:
	default:
		return nil, fmt.Errorf("%w: kind is fact, preference or episode", ErrInvalid)
	}
	if importance <= 0 || importance > 1 {
		importance = 0.7
	}
	vecs, err := m.embed(ctx, ag, []string{content})
	if err != nil {
		m.log().Warn("memory: embed failed, storing without a vector", "agent", ag.ID, "err", err)
		vecs = make([]*pgvector.Vector, 1)
	}
	sum := sha256.Sum256([]byte(strings.ToLower(content)))
	row, err := m.DB.UpsertMemory(ctx, store.UpsertMemoryParams{
		AgentID: ag.ID, Kind: kind, Content: content, ContentHash: sum[:], Embedding: vecs[0],
		SourceRunID: runID, Importance: importance,
	})
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// RenderRecall is the block appended to the system prompt.
func RenderRecall(mem []Recalled) string {
	if len(mem) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("What you remember from earlier runs (facts, preferences and episodes you recorded; trust them unless the task shows otherwise):\n")
	for _, r := range mem {
		fmt.Fprintf(&b, "- [%s] %s\n", r.Kind, strings.TrimSpace(r.Content))
	}
	return b.String()
}

// reflection is what the model returns.
type reflection struct {
	Facts       []memoryItem `json:"facts"`
	Preferences []memoryItem `json:"preferences"`
	Episode     *memoryItem  `json:"episode"`
}

type memoryItem struct {
	Content    string  `json:"content"`
	Importance float32 `json:"importance"`
}

// MaxMemoryContent bounds one memory's text.
const MaxMemoryContent = 1000

// Reflect reads a finished run's transcript, asks a cheap model for the
// facts, preferences and a one-paragraph episode worth keeping, embeds
// them and upserts agent_memory. Idempotent per content.
func (m *Memory) Reflect(ctx context.Context, runID uuid.UUID) error {
	run, err := m.DB.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if !run.AgentID.Valid {
		return nil
	}
	ag, err := m.DB.GetAgent(ctx, run.AgentID.UUID)
	if err != nil {
		return err
	}
	if memoryConfig(ag).Disabled {
		return nil
	}
	rows, err := m.DB.ListMessages(ctx, run.ConversationID)
	if err != nil {
		return err
	}
	transcript := Transcript(rows, 14_000)
	if strings.TrimSpace(transcript) == "" {
		return nil
	}
	resp, err := m.GW.Complete(ctx, &gateway.Request{
		Model: "auto",
		System: `You maintain the long-term memory of an autonomous agent. From the run transcript, extract what the agent should remember for future runs. Reply with JSON only, matching:
{"facts":[{"content":string,"importance":number}],"preferences":[{"content":string,"importance":number}],"episode":{"content":string,"importance":number}}
Facts are stable, specific, verifiable statements about the world or the agent's environment (names, ids, URLs, numbers, what exists and where). Preferences are how the owner wants things done. The episode is one paragraph: what the run tried, what happened, what is still open. Importance is 0 to 1. Leave out anything already obvious from the goal, pleasantries, and transient detail. At most eight facts and four preferences.`,
		Messages:  []gateway.Message{{Role: gateway.RoleUser, Parts: []gateway.Part{gateway.TextPart("Agent goal: " + ag.Goal + "\n\nTranscript:\n" + transcript)}}},
		MaxTokens: 1500,
		JSON:      &gateway.JSONFormat{},
		Metadata:  gateway.Metadata{UserID: userOf(run), AgentID: ag.ID.String(), AgentRunID: run.ID.String(), ConversationID: run.ConversationID.String(), TaskClass: gateway.TaskReflect},
	})
	if err != nil {
		return fmt.Errorf("memory: reflect: %w", err)
	}
	var out reflection
	text := strings.TrimSpace(resp.Text())
	if i, j := strings.Index(text, "{"), strings.LastIndex(text, "}"); i >= 0 && j > i {
		_ = json.Unmarshal([]byte(text[i:j+1]), &out)
	}
	type item struct {
		kind string
		memoryItem
	}
	var items []item
	for _, f := range out.Facts {
		items = append(items, item{MemoryFact, f})
	}
	for _, p := range out.Preferences {
		items = append(items, item{MemoryPreference, p})
	}
	if out.Episode != nil {
		items = append(items, item{MemoryEpisode, *out.Episode})
	}
	var kept []item
	for _, it := range items {
		it.Content = strings.TrimSpace(it.Content)
		if it.Content == "" {
			continue
		}
		if len(it.Content) > MaxMemoryContent {
			it.Content = it.Content[:MaxMemoryContent]
		}
		if it.Importance <= 0 || it.Importance > 1 {
			it.Importance = 0.5
		}
		kept = append(kept, it)
	}
	if len(kept) == 0 {
		return nil
	}
	texts := make([]string, len(kept))
	for i, it := range kept {
		texts[i] = it.Content
	}
	vecs, err := m.embed(ctx, ag, texts)
	if err != nil {
		m.log().Warn("memory: embed failed, storing without vectors", "agent", ag.ID, "err", err)
		vecs = make([]*pgvector.Vector, len(kept))
	}
	n := 0
	for i, it := range kept {
		sum := sha256.Sum256([]byte(strings.ToLower(it.Content)))
		if _, err := m.DB.UpsertMemory(ctx, store.UpsertMemoryParams{
			AgentID: ag.ID, Kind: it.kind, Content: it.Content, ContentHash: sum[:], Embedding: vecs[i],
			SourceRunID: store.NullUUID(run.ID), Importance: it.Importance,
		}); err != nil {
			m.log().Warn("memory: upsert", "agent", ag.ID, "err", err)
			continue
		}
		n++
	}
	m.log().Info("memory: reflected", "agent", ag.ID, "run", run.ID, "memories", n, "endpoint", resp.EndpointID)
	return nil
}

// embed returns one vector per text, nil where the width is wrong.
func (m *Memory) embed(ctx context.Context, ag store.Agent, texts []string) ([]*pgvector.Vector, error) {
	resp, err := m.GW.Embed(ctx, &gateway.EmbedRequest{Model: "auto", Inputs: texts, Metadata: gateway.Metadata{UserID: ag.OwnerID.String(), AgentID: ag.ID.String()}})
	if err != nil {
		return nil, err
	}
	if len(resp.Vectors) != len(texts) {
		return nil, errors.New("memory: embedding count mismatch")
	}
	out := make([]*pgvector.Vector, len(texts))
	for i, v := range resp.Vectors {
		if len(v) != EmbedDims {
			m.log().Warn("memory: embedding width is not the column's", "endpoint", resp.EndpointID, "got", len(v), "want", EmbedDims)
			continue
		}
		pv := pgvector.NewVector(v)
		out[i] = &pv
	}
	return out, nil
}

// Transcript renders messages for a model to read, newest last, trimmed
// from the front to about max characters.
func Transcript(rows []store.Message, max int) string {
	var lines []string
	for _, r := range rows {
		var parts []gateway.Part
		_ = json.Unmarshal(r.Parts, &parts)
		for _, p := range parts {
			switch p.Kind {
			case gateway.PartText:
				if t := strings.TrimSpace(p.Text); t != "" {
					lines = append(lines, r.Role+": "+clip(t, 3000))
				}
			case gateway.PartToolCall:
				lines = append(lines, "assistant called "+p.ToolName+"("+clip(string(p.Args), 400)+")")
			case gateway.PartToolResult:
				for _, cp := range p.Content {
					if cp.Kind == gateway.PartText {
						lines = append(lines, "tool result: "+clip(cp.Text, 1200))
					}
				}
			}
		}
	}
	s := strings.Join(lines, "\n")
	if len(s) > max {
		s = "[earlier part omitted]\n" + s[len(s)-max:]
	}
	return s
}

func memoryConfig(ag store.Agent) MemoryConfig {
	var cfg MemoryConfig
	_ = json.Unmarshal(ag.MemoryConfig, &cfg)
	if cfg.K <= 0 {
		cfg.K = 8
	}
	if cfg.TopN <= 0 {
		cfg.TopN = 4
	}
	return cfg
}

func userOf(run store.AgentRun) string {
	if run.UserID.Valid {
		return run.UserID.UUID.String()
	}
	return ""
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func (m *Memory) log() *slog.Logger {
	if m.Log != nil {
		return m.Log
	}
	return slog.Default()
}
