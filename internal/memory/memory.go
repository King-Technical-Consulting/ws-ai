// Package memory is what ws keeps about one member between chats (PLAN M13,
// cut 3): memories recalled into the system prompt by similarity under a
// small budget, the personal-context card, and the reflection that tidies
// them. Everything is per member and filtered by user id; agents keep their
// own memory in internal/agents.
package memory

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
	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"

	"github.com/jking323/ws/internal/agents"
	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
)

// Memory kinds (user_memory.kind) and sources.
const (
	KindFact       = "fact"
	KindPreference = "preference"
	KindDecision   = "decision"

	SourceChat       = "chat"
	SourceReflection = "reflection"
	SourceManual     = "manual"
)

// Limits.
const (
	// EmbedDims is the width of user_memory.embedding, as agent_memory's.
	EmbedDims = 768
	// MaxContent bounds one memory's text.
	MaxContent = 500
	// MaxPerUser bounds how many memories a member can keep; a manual or
	// chat write past it is refused, reflection merges instead of adding.
	MaxPerUser = 500
	// ContextChars is the budget for the card plus recalled memories in the
	// system prompt (about 600 tokens).
	ContextChars = 2400
	// mergeDistance is the cosine distance under which a new memory is the
	// same thing as an existing one.
	mergeDistance = 0.12
	// staleAfter and decayFactor demote memories nobody has used.
	staleAfter  = 60 * 24 * time.Hour
	decayFactor = 0.8
)

// ErrInvalid is a rejected input; the message is safe to show.
var ErrInvalid = errors.New("memory: invalid")

// ErrLimit is returned when the member is at MaxPerUser.
var ErrLimit = fmt.Errorf("%w: you have %d memories, the most ws keeps; delete some first", ErrInvalid, MaxPerUser)

// Store is the slice of the store the service uses (*store.DB).
type Store interface {
	UpsertUserMemory(ctx context.Context, arg store.UpsertUserMemoryParams) (store.UserMemory, error)
	ListUserMemories(ctx context.Context, arg store.ListUserMemoriesParams) ([]store.UserMemory, error)
	CountUserMemories(ctx context.Context, userID uuid.UUID) (int64, error)
	GetUserMemory(ctx context.Context, arg store.GetUserMemoryParams) (store.UserMemory, error)
	UpdateUserMemory(ctx context.Context, arg store.UpdateUserMemoryParams) (store.UserMemory, error)
	DeleteUserMemory(ctx context.Context, arg store.DeleteUserMemoryParams) error
	DeleteUserMemories(ctx context.Context, userID uuid.UUID) error
	SearchUserMemories(ctx context.Context, arg store.SearchUserMemoriesParams) ([]store.SearchUserMemoriesRow, error)
	TopUserMemories(ctx context.Context, arg store.TopUserMemoriesParams) ([]store.UserMemory, error)
	TouchUserMemories(ctx context.Context, arg store.TouchUserMemoriesParams) error
	DecayUserMemories(ctx context.Context, arg store.DecayUserMemoriesParams) (int64, error)
	GetMemorySettings(ctx context.Context, userID uuid.UUID) (store.MemorySetting, error)
	UpsertMemorySettings(ctx context.Context, arg store.UpsertMemorySettingsParams) (store.MemorySetting, error)
	MarkMemoryReflected(ctx context.Context, arg store.MarkMemoryReflectedParams) error
	ListReflectingMembers(ctx context.Context) ([]store.MemorySetting, error)
	ListConversationsActiveSince(ctx context.Context, arg store.ListConversationsActiveSinceParams) ([]store.ListConversationsActiveSinceRow, error)
	ListMessages(ctx context.Context, conversationID uuid.UUID) ([]store.Message, error)
}

// Completer is what the service needs from the gateway.
type Completer interface {
	Complete(ctx context.Context, req *gateway.Request) (*gateway.Response, error)
	Embed(ctx context.Context, req *gateway.EmbedRequest) (*gateway.EmbedResponse, error)
}

// Service reads and writes a member's memory. Model and embedding calls go
// through the gateway under the member's identity so the ledger and budgets
// see them: reflection as task class reflect, embeddings as embed.
type Service struct {
	DB  Store
	GW  Completer
	Log *slog.Logger
	Now func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Profile is the personal-context card: a short structured description the
// model keeps about the person, shown as one editable card and injected
// ahead of the memories.
type Profile struct {
	Name      string   `json:"name,omitempty"`       // what to call them
	Role      string   `json:"role,omitempty"`       // what they do
	Projects  []string `json:"projects,omitempty"`   // what they work on
	Tools     []string `json:"tools,omitempty"`      // languages, tools, stack
	Tone      string   `json:"tone,omitempty"`       // how they like to be answered
	ThisWeek  string   `json:"this_week,omitempty"`  // what they are on right now
	AddressAs string   `json:"address_as,omitempty"` // how to address them, if not Name
}

// Empty reports whether the card holds nothing.
func (p Profile) Empty() bool {
	return p.Name == "" && p.Role == "" && len(p.Projects) == 0 && len(p.Tools) == 0 && p.Tone == "" && p.ThisWeek == "" && p.AddressAs == ""
}

// clean trims and bounds every field so a card stays a card.
func (p Profile) clean() Profile {
	cl := func(s string, n int) string {
		s = strings.TrimSpace(s)
		if len(s) > n {
			s = s[:n]
		}
		return s
	}
	list := func(in []string) []string {
		var out []string
		for _, v := range in {
			if v = cl(v, 80); v != "" && len(out) < 12 {
				out = append(out, v)
			}
		}
		return out
	}
	return Profile{
		Name: cl(p.Name, 80), AddressAs: cl(p.AddressAs, 80), Role: cl(p.Role, 120), Projects: list(p.Projects),
		Tools: list(p.Tools), Tone: cl(p.Tone, 200), ThisWeek: cl(p.ThisWeek, 300),
	}
}

// Settings are one member's switches and card.
type Settings struct {
	Enabled         bool       `json:"enabled"`
	ReflectEnabled  bool       `json:"reflect_enabled"`
	ReflectAt       string     `json:"reflect_at"` // HH:MM in Timezone
	Timezone        string     `json:"timezone"`
	Profile         Profile    `json:"profile"`
	LastReflectedAt *time.Time `json:"last_reflected_at"`
}

// DefaultSettings is what a member who never opened the page has.
func DefaultSettings() Settings {
	return Settings{Enabled: true, ReflectAt: "03:00", Timezone: "UTC"}
}

// Settings returns the member's settings, the defaults when there is no row.
func (s *Service) Settings(ctx context.Context, userID uuid.UUID) (Settings, error) {
	row, err := s.DB.GetMemorySettings(ctx, userID)
	if err != nil {
		if isNoRows(err) {
			return DefaultSettings(), nil
		}
		return Settings{}, err
	}
	out := Settings{Enabled: row.Enabled, ReflectEnabled: row.ReflectEnabled, ReflectAt: row.ReflectAt, Timezone: row.Timezone, LastReflectedAt: row.LastReflectedAt}
	_ = json.Unmarshal(row.Profile, &out.Profile)
	return out, nil
}

// SaveSettings validates and stores the member's settings.
func (s *Service) SaveSettings(ctx context.Context, userID uuid.UUID, in Settings) (Settings, error) {
	if _, err := time.Parse("15:04", in.ReflectAt); err != nil {
		return Settings{}, fmt.Errorf("%w: time of day must be HH:MM", ErrInvalid)
	}
	if _, err := time.LoadLocation(in.Timezone); err != nil || in.Timezone == "" {
		return Settings{}, fmt.Errorf("%w: unknown time zone %q", ErrInvalid, in.Timezone)
	}
	in.Profile = in.Profile.clean()
	pj, _ := json.Marshal(in.Profile)
	row, err := s.DB.UpsertMemorySettings(ctx, store.UpsertMemorySettingsParams{
		UserID: userID, Enabled: in.Enabled, ReflectEnabled: in.ReflectEnabled, ReflectAt: in.ReflectAt, Timezone: in.Timezone, Profile: pj,
	})
	if err != nil {
		return Settings{}, err
	}
	in.LastReflectedAt = row.LastReflectedAt
	return in, nil
}

// Memory is one stored memory as the page and the model see it.
type Memory struct {
	ID         uuid.UUID  `json:"id"`
	Kind       string     `json:"kind"`
	Content    string     `json:"content"`
	Source     string     `json:"source"`
	Importance float32    `json:"importance"`
	LastUsedAt *time.Time `json:"last_used_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

func toMemory(r store.UserMemory) Memory {
	return Memory{ID: r.ID, Kind: r.Kind, Content: r.Content, Source: r.Source, Importance: r.Importance, LastUsedAt: r.LastUsedAt, CreatedAt: r.CreatedAt}
}

// Recalled is a memory chosen for a turn.
type Recalled struct {
	ID         uuid.UUID `json:"id"`
	Kind       string    `json:"kind"`
	Content    string    `json:"content"`
	importance float32
	distance   float64
}

// Block is what a chat turn gets: text for the system prompt, and the
// memories it names so the UI can show why the model knows something.
type Block struct {
	Text     string
	Recalled []Recalled
	Card     bool
}

// Context builds the personal block for a turn that starts with input:
// the card first, then the memories nearest to the input and the most
// important ones, until the character budget is spent. A member who turned
// memory off gets an empty block. Recalled memories are marked used.
func (s *Service) Context(ctx context.Context, userID uuid.UUID, input string) (Block, error) {
	set, err := s.Settings(ctx, userID)
	if err != nil || !set.Enabled {
		return Block{}, err
	}
	picked, err := s.recall(ctx, userID, input, 12, 4)
	if err != nil {
		s.log().Warn("memory: recall failed, continuing without", "err", err)
	}
	return renderBlock(set.Profile, picked, ContextChars), nil
}

func renderBlock(p Profile, picked []Recalled, budget int) Block {
	var b strings.Builder
	var blk Block
	if !p.Empty() {
		b.WriteString("About the person you are talking to (their own card; trust it unless they say otherwise):\n")
		line := func(label, v string) {
			if v != "" {
				fmt.Fprintf(&b, "- %s: %s\n", label, v)
			}
		}
		line("Name", p.Name)
		line("Address them as", p.AddressAs)
		line("Role", p.Role)
		if len(p.Projects) > 0 {
			line("Projects", strings.Join(p.Projects, "; "))
		}
		if len(p.Tools) > 0 {
			line("Tools", strings.Join(p.Tools, ", "))
		}
		line("Tone", p.Tone)
		line("This week", p.ThisWeek)
		blk.Card = true
	}
	if len(picked) > 0 {
		head := "What you remember about them from earlier chats (recalled for this message; mention that you remembered it when it shapes your answer):\n"
		var lines strings.Builder
		for _, r := range picked {
			l := fmt.Sprintf("- [%s] %s\n", r.Kind, strings.TrimSpace(r.Content))
			if b.Len()+len(head)+lines.Len()+len(l) > budget {
				break
			}
			lines.WriteString(l)
			blk.Recalled = append(blk.Recalled, r)
		}
		if lines.Len() > 0 {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(head)
			b.WriteString(lines.String())
		}
	}
	blk.Text = strings.TrimRight(b.String(), "\n")
	return blk
}

// recall picks the k nearest memories to query plus the top n by importance,
// nearest first; an embedding failure degrades to importance only.
func (s *Service) recall(ctx context.Context, userID uuid.UUID, query string, k, topN int) ([]Recalled, error) {
	picked := map[uuid.UUID]Recalled{}
	if q := strings.TrimSpace(query); q != "" {
		vec, err := s.embed(ctx, userID, []string{q})
		if err != nil {
			s.log().Warn("memory: recall embed failed, importance only", "err", err)
		} else if len(vec) == 1 && vec[0] != nil {
			rows, err := s.DB.SearchUserMemories(ctx, store.SearchUserMemoriesParams{UserID: userID, Column2: *vec[0], Limit: int32(k)})
			if err != nil {
				return nil, err
			}
			for _, r := range rows {
				picked[r.ID] = Recalled{ID: r.ID, Kind: r.Kind, Content: r.Content, importance: r.Importance, distance: r.Distance}
			}
		}
	}
	top, err := s.DB.TopUserMemories(ctx, store.TopUserMemoriesParams{UserID: userID, Limit: int32(topN)})
	if err != nil {
		return nil, err
	}
	for _, r := range top {
		if _, ok := picked[r.ID]; !ok {
			picked[r.ID] = Recalled{ID: r.ID, Kind: r.Kind, Content: r.Content, importance: r.Importance, distance: 2}
		}
	}
	out := make([]Recalled, 0, len(picked))
	for _, r := range picked {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].distance != out[j].distance {
			return out[i].distance < out[j].distance
		}
		return out[i].importance > out[j].importance
	})
	return out, nil
}

// MarkUsed stamps the memories a turn actually carried.
func (s *Service) MarkUsed(ctx context.Context, userID uuid.UUID, rec []Recalled) {
	if len(rec) == 0 {
		return
	}
	ids := make([]uuid.UUID, len(rec))
	for i, r := range rec {
		ids[i] = r.ID
	}
	if err := s.DB.TouchUserMemories(ctx, store.TouchUserMemoriesParams{Ids: ids, UserID: userID}); err != nil {
		s.log().Warn("memory: touch", "err", err)
	}
}

// Add stores one memory. Kind "" is a fact; importance outside (0, 1] is
// 0.7 for something the person asked to keep.
func (s *Service) Add(ctx context.Context, userID uuid.UUID, kind, content, source string, importance float32, conv uuid.NullUUID) (*Memory, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, fmt.Errorf("%w: content is required", ErrInvalid)
	}
	if len(content) > MaxContent {
		return nil, fmt.Errorf("%w: a memory is at most %d characters", ErrInvalid, MaxContent)
	}
	kind, err := normKind(kind)
	if err != nil {
		return nil, err
	}
	if importance <= 0 || importance > 1 {
		importance = 0.7
	}
	hash := hashOf(content)
	if n, err := s.DB.CountUserMemories(ctx, userID); err == nil && n >= MaxPerUser {
		return nil, ErrLimit
	}
	vecs, err := s.embed(ctx, userID, []string{content})
	if err != nil {
		s.log().Warn("memory: embed failed, storing without a vector", "err", err)
		vecs = make([]*pgvector.Vector, 1)
	}
	row, err := s.DB.UpsertUserMemory(ctx, store.UpsertUserMemoryParams{
		UserID: userID, Kind: kind, Content: content, ContentHash: hash, Embedding: vecs[0],
		SourceConversationID: conv, Source: source, Importance: importance,
	})
	if err != nil {
		return nil, err
	}
	m := toMemory(row)
	return &m, nil
}

// Update edits a memory's text, kind or importance.
func (s *Service) Update(ctx context.Context, userID, id uuid.UUID, kind, content string, importance float32) (*Memory, error) {
	content = strings.TrimSpace(content)
	if content == "" || len(content) > MaxContent {
		return nil, fmt.Errorf("%w: content is 1 to %d characters", ErrInvalid, MaxContent)
	}
	kind, err := normKind(kind)
	if err != nil {
		return nil, err
	}
	if importance <= 0 || importance > 1 {
		return nil, fmt.Errorf("%w: importance is above 0 and at most 1", ErrInvalid)
	}
	vecs, err := s.embed(ctx, userID, []string{content})
	if err != nil {
		vecs = make([]*pgvector.Vector, 1)
	}
	row, err := s.DB.UpdateUserMemory(ctx, store.UpdateUserMemoryParams{
		ID: id, UserID: userID, Content: content, ContentHash: hashOf(content), Kind: kind, Importance: importance, Embedding: vecs[0],
	})
	if err != nil {
		if isNoRows(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	m := toMemory(row)
	return &m, nil
}

// ErrNotFound is a memory that does not exist or is another member's.
var ErrNotFound = errors.New("memory: not found")

// Delete removes one memory.
func (s *Service) Delete(ctx context.Context, userID, id uuid.UUID) error {
	if _, err := s.DB.GetUserMemory(ctx, store.GetUserMemoryParams{ID: id, UserID: userID}); err != nil {
		if isNoRows(err) {
			return ErrNotFound
		}
		return err
	}
	return s.DB.DeleteUserMemory(ctx, store.DeleteUserMemoryParams{ID: id, UserID: userID})
}

// Forget removes every memory the member has (the card stays).
func (s *Service) Forget(ctx context.Context, userID uuid.UUID) error {
	return s.DB.DeleteUserMemories(ctx, userID)
}

// List returns a page of the member's memories, most important first.
func (s *Service) List(ctx context.Context, userID uuid.UUID, limit, offset int) ([]Memory, int64, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := s.DB.ListUserMemories(ctx, store.ListUserMemoriesParams{UserID: userID, Limit: int32(limit), Offset: int32(offset)})
	if err != nil {
		return nil, 0, err
	}
	n, err := s.DB.CountUserMemories(ctx, userID)
	if err != nil {
		return nil, 0, err
	}
	out := make([]Memory, len(rows))
	for i, r := range rows {
		out[i] = toMemory(r)
	}
	return out, n, nil
}

func normKind(kind string) (string, error) {
	switch kind {
	case "":
		return KindFact, nil
	case KindFact, KindPreference, KindDecision:
		return kind, nil
	case "episode":
		return KindFact, nil
	}
	return "", fmt.Errorf("%w: kind is fact, preference or decision", ErrInvalid)
}

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

func hashOf(content string) []byte {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(content))))
	return sum[:]
}

// embed returns one vector per text, nil where the width is wrong.
func (s *Service) embed(ctx context.Context, userID uuid.UUID, texts []string) ([]*pgvector.Vector, error) {
	resp, err := s.GW.Embed(ctx, &gateway.EmbedRequest{Model: "auto", Inputs: texts, Metadata: gateway.Metadata{UserID: userID.String()}})
	if err != nil {
		return nil, err
	}
	if len(resp.Vectors) != len(texts) {
		return nil, errors.New("memory: embedding count mismatch")
	}
	out := make([]*pgvector.Vector, len(texts))
	for i, v := range resp.Vectors {
		if len(v) != EmbedDims {
			s.log().Warn("memory: embedding width is not the column's", "endpoint", resp.EndpointID, "got", len(v), "want", EmbedDims)
			continue
		}
		pv := pgvector.NewVector(v)
		out[i] = &pv
	}
	return out, nil
}

// ---- the agents.PersonalMemory seam: remember and recall in a chat ----

// RememberFor implements agents.PersonalMemory.
func (s *Service) RememberFor(ctx context.Context, userID uuid.UUID, kind, content string, importance float32, conv uuid.UUID) error {
	set, err := s.Settings(ctx, userID)
	if err != nil {
		return err
	}
	if !set.Enabled {
		return errors.New("memory is turned off; the person can turn it on under Settings, memory")
	}
	_, err = s.Add(ctx, userID, kind, content, SourceChat, importance, uuid.NullUUID{UUID: conv, Valid: conv != uuid.Nil})
	return err
}

// SearchFor implements agents.PersonalMemory: rendered lines, nearest first.
func (s *Service) SearchFor(ctx context.Context, userID uuid.UUID, query string, k int) ([]string, error) {
	set, err := s.Settings(ctx, userID)
	if err != nil {
		return nil, err
	}
	if !set.Enabled {
		return nil, errors.New("memory is turned off")
	}
	if k <= 0 || k > 50 {
		k = 8
	}
	rec, err := s.recall(ctx, userID, query, k, 0)
	if err != nil {
		return nil, err
	}
	if len(rec) > k {
		rec = rec[:k]
	}
	s.MarkUsed(ctx, userID, rec)
	out := make([]string, len(rec))
	for i, r := range rec {
		out[i] = "[" + r.Kind + "] " + r.Content
	}
	return out, nil
}

var _ agents.PersonalMemory = (*Service)(nil)

// ContextText is Context as text only, for callers that build their own
// system prompt (scheduled prompts); a failure yields no text.
func (s *Service) ContextText(ctx context.Context, userID uuid.UUID, input string) string {
	blk, err := s.Context(ctx, userID, input)
	if err != nil {
		return ""
	}
	s.MarkUsed(ctx, userID, blk.Recalled)
	return blk.Text
}
