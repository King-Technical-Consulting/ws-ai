package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"

	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
)

// fakeDB is an in-memory Store. Search is by exact vector equality plus a
// tiny cosine, enough to drive recall and merge.
type fakeDB struct {
	mu       sync.Mutex
	mems     map[uuid.UUID]store.UserMemory
	settings map[uuid.UUID]store.MemorySetting
	convs    map[uuid.UUID][]store.ListConversationsActiveSinceRow
	msgs     map[uuid.UUID][]store.Message
	touched  []uuid.UUID
}

func newFakeDB() *fakeDB {
	return &fakeDB{mems: map[uuid.UUID]store.UserMemory{}, settings: map[uuid.UUID]store.MemorySetting{}, convs: map[uuid.UUID][]store.ListConversationsActiveSinceRow{}, msgs: map[uuid.UUID][]store.Message{}}
}

func (f *fakeDB) UpsertUserMemory(_ context.Context, a store.UpsertUserMemoryParams) (store.UserMemory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, m := range f.mems {
		if m.UserID == a.UserID && string(m.ContentHash) == string(a.ContentHash) {
			if a.Importance > m.Importance {
				m.Importance = a.Importance
			}
			f.mems[id] = m
			return m, nil
		}
	}
	m := store.UserMemory{ID: uuid.New(), UserID: a.UserID, Kind: a.Kind, Content: a.Content, ContentHash: a.ContentHash, Embedding: a.Embedding,
		SourceConversationID: a.SourceConversationID, Source: a.Source, Importance: a.Importance, CreatedAt: time.Now()}
	f.mems[m.ID] = m
	return m, nil
}
func (f *fakeDB) ListUserMemories(_ context.Context, a store.ListUserMemoriesParams) ([]store.UserMemory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.UserMemory
	for _, m := range f.mems {
		if m.UserID == a.UserID {
			out = append(out, m)
		}
	}
	return out, nil
}
func (f *fakeDB) CountUserMemories(_ context.Context, uid uuid.UUID) (int64, error) {
	l, _ := f.ListUserMemories(nil, store.ListUserMemoriesParams{UserID: uid})
	return int64(len(l)), nil
}
func (f *fakeDB) GetUserMemory(_ context.Context, a store.GetUserMemoryParams) (store.UserMemory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.mems[a.ID]
	if !ok || m.UserID != a.UserID {
		return store.UserMemory{}, pgx.ErrNoRows
	}
	return m, nil
}
func (f *fakeDB) UpdateUserMemory(_ context.Context, a store.UpdateUserMemoryParams) (store.UserMemory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.mems[a.ID]
	if !ok || m.UserID != a.UserID {
		return store.UserMemory{}, pgx.ErrNoRows
	}
	m.Content, m.ContentHash, m.Kind, m.Importance, m.Embedding = a.Content, a.ContentHash, a.Kind, a.Importance, a.Embedding
	f.mems[a.ID] = m
	return m, nil
}
func (f *fakeDB) DeleteUserMemory(_ context.Context, a store.DeleteUserMemoryParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m, ok := f.mems[a.ID]; ok && m.UserID == a.UserID {
		delete(f.mems, a.ID)
	}
	return nil
}
func (f *fakeDB) DeleteUserMemories(_ context.Context, uid uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, m := range f.mems {
		if m.UserID == uid {
			delete(f.mems, id)
		}
	}
	return nil
}
func (f *fakeDB) SearchUserMemories(_ context.Context, a store.SearchUserMemoriesParams) ([]store.SearchUserMemoriesRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.SearchUserMemoriesRow
	for _, m := range f.mems {
		if m.UserID != a.UserID || m.Embedding == nil {
			continue
		}
		d := 1 - cosine(m.Embedding.Slice(), a.Column2.Slice())
		out = append(out, store.SearchUserMemoriesRow{ID: m.ID, UserID: m.UserID, Kind: m.Kind, Content: m.Content, ContentHash: m.ContentHash,
			Embedding: m.Embedding, Importance: m.Importance, Distance: d})
	}
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j].Distance < out[i].Distance {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	if int(a.Limit) < len(out) {
		out = out[:a.Limit]
	}
	return out, nil
}
func (f *fakeDB) TopUserMemories(_ context.Context, a store.TopUserMemoriesParams) ([]store.UserMemory, error) {
	l, _ := f.ListUserMemories(nil, store.ListUserMemoriesParams{UserID: a.UserID})
	for i := range l {
		for j := i + 1; j < len(l); j++ {
			if l[j].Importance > l[i].Importance {
				l[i], l[j] = l[j], l[i]
			}
		}
	}
	if int(a.Limit) < len(l) {
		l = l[:a.Limit]
	}
	return l, nil
}
func (f *fakeDB) TouchUserMemories(_ context.Context, a store.TouchUserMemoriesParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touched = append(f.touched, a.Ids...)
	return nil
}
func (f *fakeDB) DecayUserMemories(_ context.Context, a store.DecayUserMemoriesParams) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for id, m := range f.mems {
		last := m.CreatedAt
		if m.LastUsedAt != nil {
			last = *m.LastUsedAt
		}
		if m.UserID == a.UserID && m.Importance > 0.05 && last.Before(a.Cutoff) {
			m.Importance *= a.Factor
			f.mems[id] = m
			n++
		}
	}
	return n, nil
}
func (f *fakeDB) GetMemorySettings(_ context.Context, uid uuid.UUID) (store.MemorySetting, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.settings[uid]
	if !ok {
		return store.MemorySetting{}, pgx.ErrNoRows
	}
	return s, nil
}
func (f *fakeDB) UpsertMemorySettings(_ context.Context, a store.UpsertMemorySettingsParams) (store.MemorySetting, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.settings[a.UserID]
	s.UserID, s.Enabled, s.ReflectEnabled, s.ReflectAt, s.Timezone, s.Profile = a.UserID, a.Enabled, a.ReflectEnabled, a.ReflectAt, a.Timezone, a.Profile
	f.settings[a.UserID] = s
	return s, nil
}
func (f *fakeDB) MarkMemoryReflected(_ context.Context, a store.MarkMemoryReflectedParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.settings[a.UserID]
	if !ok {
		s = store.MemorySetting{UserID: a.UserID, Enabled: true, ReflectAt: "03:00", Timezone: "UTC", Profile: json.RawMessage("{}")}
	}
	s.LastReflectedAt = a.LastReflectedAt
	f.settings[a.UserID] = s
	return nil
}
func (f *fakeDB) ListReflectingMembers(context.Context) ([]store.MemorySetting, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.MemorySetting
	for _, s := range f.settings {
		if s.Enabled && s.ReflectEnabled {
			out = append(out, s)
		}
	}
	return out, nil
}
func (f *fakeDB) ListConversationsActiveSince(_ context.Context, a store.ListConversationsActiveSinceParams) ([]store.ListConversationsActiveSinceRow, error) {
	return f.convs[a.UserID], nil
}
func (f *fakeDB) ListMessages(_ context.Context, id uuid.UUID) ([]store.Message, error) {
	return f.msgs[id], nil
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i] * b[i])
		na += float64(a[i] * a[i])
		nb += float64(b[i] * b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (sqrt(na) * sqrt(nb))
}

func sqrt(x float64) float64 {
	z := x
	for i := 0; i < 30; i++ {
		z = (z + x/z) / 2
	}
	return z
}

// fakeGW embeds a text as a one-hot-ish vector keyed by its first word, so
// "go tests" and "go builds" are near and "cooking" is far.
type fakeGW struct {
	reply     string
	embedErr  error
	completed int
	lastSys   string
}

func (g *fakeGW) Complete(_ context.Context, r *gateway.Request) (*gateway.Response, error) {
	g.completed++
	g.lastSys = r.System
	return &gateway.Response{Parts: []gateway.Part{gateway.TextPart(g.reply)}}, nil
}

func (g *fakeGW) Embed(_ context.Context, r *gateway.EmbedRequest) (*gateway.EmbedResponse, error) {
	if g.embedErr != nil {
		return nil, g.embedErr
	}
	out := make([][]float32, len(r.Inputs))
	for i, t := range r.Inputs {
		v := make([]float32, EmbedDims)
		w := strings.ToLower(strings.Fields(t + " x")[0])
		v[int(w[0])%EmbedDims] = 1
		out[i] = v
	}
	return &gateway.EmbedResponse{Vectors: out}, nil
}

func newSvc() (*Service, *fakeDB, *fakeGW) {
	db, gw := newFakeDB(), &fakeGW{}
	return &Service{DB: db, GW: gw}, db, gw
}

func TestAddValidatesAndDedupes(t *testing.T) {
	s, db, _ := newSvc()
	ctx := context.Background()
	u := uuid.New()
	if _, err := s.Add(ctx, u, "", "  ", SourceManual, 0, uuid.NullUUID{}); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty content: %v", err)
	}
	if _, err := s.Add(ctx, u, "mood", "x", SourceManual, 0, uuid.NullUUID{}); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad kind: %v", err)
	}
	if _, err := s.Add(ctx, u, "", strings.Repeat("a", MaxContent+1), SourceManual, 0, uuid.NullUUID{}); !errors.Is(err, ErrInvalid) {
		t.Errorf("too long: %v", err)
	}
	if _, err := s.Add(ctx, u, "episode", "Likes tea", SourceChat, 0, uuid.NullUUID{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(ctx, u, KindFact, "likes TEA", SourceChat, 0.9, uuid.NullUUID{}); err != nil {
		t.Fatal(err)
	}
	if n, _ := db.CountUserMemories(ctx, u); n != 1 {
		t.Errorf("same text in another case is one memory, got %d", n)
	}
}

func TestMemoryIsPerMember(t *testing.T) {
	s, _, _ := newSvc()
	ctx := context.Background()
	a, b := uuid.New(), uuid.New()
	m, err := s.Add(ctx, a, KindFact, "a's secret project is called Bluebird", SourceChat, 0.9, uuid.NullUUID{})
	if err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.List(ctx, b, 50, 0); len(got) != 0 {
		t.Errorf("b lists %d of a's memories", len(got))
	}
	if err := s.Delete(ctx, b, m.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("b deleting a's memory: %v", err)
	}
	if _, err := s.Update(ctx, b, m.ID, KindFact, "changed", 0.5); !errors.Is(err, ErrNotFound) {
		t.Errorf("b updating a's memory: %v", err)
	}
	blk, _ := s.Context(ctx, b, "Bluebird")
	if strings.Contains(blk.Text, "Bluebird") {
		t.Error("a's memory leaked into b's context")
	}
	if lines, _ := s.SearchFor(ctx, b, "Bluebird", 5); len(lines) != 0 {
		t.Errorf("b's recall found %v", lines)
	}
	blk, _ = s.Context(ctx, a, "Bluebird")
	if !strings.Contains(blk.Text, "Bluebird") {
		t.Error("a's own memory should be recalled")
	}
}

func TestContextOffMeansNothing(t *testing.T) {
	s, _, _ := newSvc()
	ctx := context.Background()
	u := uuid.New()
	_, _ = s.Add(ctx, u, KindFact, "Uses Postgres", SourceChat, 0.9, uuid.NullUUID{})
	set := DefaultSettings()
	set.Enabled = false
	set.Profile = Profile{Name: "Sam"}
	if _, err := s.SaveSettings(ctx, u, set); err != nil {
		t.Fatal(err)
	}
	blk, err := s.Context(ctx, u, "Postgres")
	if err != nil || blk.Text != "" {
		t.Errorf("memory off: %q %v", blk.Text, err)
	}
	if err := s.RememberFor(ctx, u, "", "x", 0, uuid.Nil); err == nil {
		t.Error("remember with memory off should say so")
	}
}

func TestContextCardFirstAndBudget(t *testing.T) {
	s, _, _ := newSvc()
	ctx := context.Background()
	u := uuid.New()
	set := DefaultSettings()
	set.Profile = Profile{Name: "Sam", Role: "platform engineer", Projects: []string{"ws"}}
	if _, err := s.SaveSettings(ctx, u, set); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		_, _ = s.Add(ctx, u, KindFact, fmt.Sprintf("fact number %d %s", i, strings.Repeat("x", 120)), SourceChat, 0.6, uuid.NullUUID{})
	}
	blk, err := s.Context(ctx, u, "fact")
	if err != nil {
		t.Fatal(err)
	}
	if !blk.Card || !strings.HasPrefix(blk.Text, "About the person") {
		t.Errorf("card should come first: %.80q", blk.Text)
	}
	if len(blk.Text) > ContextChars {
		t.Errorf("block is %d characters, budget %d", len(blk.Text), ContextChars)
	}
	if len(blk.Recalled) == 0 || len(blk.Recalled) >= 40 {
		t.Errorf("recalled %d, want some but not all", len(blk.Recalled))
	}
	if !strings.Contains(blk.Text, "Name: Sam") {
		t.Error("card fields missing")
	}
}

func TestSaveSettingsValidates(t *testing.T) {
	s, _, _ := newSvc()
	u := uuid.New()
	good := DefaultSettings()
	if _, err := s.SaveSettings(context.Background(), u, good); err != nil {
		t.Fatal(err)
	}
	bad := good
	bad.ReflectAt = "25:00"
	if _, err := s.SaveSettings(context.Background(), u, bad); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad time: %v", err)
	}
	bad = good
	bad.Timezone = "Mars/Olympus"
	if _, err := s.SaveSettings(context.Background(), u, bad); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad zone: %v", err)
	}
	long := good
	long.Profile = Profile{Role: strings.Repeat("r", 500), Projects: make([]string, 30)}
	for i := range long.Profile.Projects {
		long.Profile.Projects[i] = fmt.Sprintf("p%d", i)
	}
	out, err := s.SaveSettings(context.Background(), u, long)
	if err != nil || len(out.Profile.Role) != 120 || len(out.Profile.Projects) != 12 {
		t.Errorf("card not bounded: %v %d %d", err, len(out.Profile.Role), len(out.Profile.Projects))
	}
}

func TestIsDue(t *testing.T) {
	utc := time.UTC
	at := func(h, m int) time.Time { return time.Date(2026, 10, 10, h, m, 0, 0, utc) }
	yesterday := at(3, 5).Add(-24 * time.Hour)
	today := at(3, 5)
	cases := []struct {
		name string
		at   string
		tz   string
		last *time.Time
		now  time.Time
		want bool
	}{
		{"before the slot", "03:00", "UTC", nil, at(2, 59), false},
		{"after the slot, never reflected", "03:00", "UTC", nil, at(3, 1), true},
		{"after the slot, reflected yesterday", "03:00", "UTC", &yesterday, at(3, 1), true},
		{"already reflected since the slot", "03:00", "UTC", &today, at(9, 0), false},
		{"time zone: 03:00 in New York is 07:00 UTC", "03:00", "America/New_York", nil, at(6, 0), false},
		{"time zone, after", "03:00", "America/New_York", nil, at(8, 0), true},
		{"bad zone falls back to UTC", "03:00", "Nowhere/Land", nil, at(4, 0), true},
		{"bad time is never due", "nope", "UTC", nil, at(4, 0), false},
	}
	for _, c := range cases {
		if got := isDue(c.at, c.tz, c.last, c.now); got != c.want {
			t.Errorf("%s: due = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestReflectAddsMergesAndDemotes(t *testing.T) {
	s, db, gw := newSvc()
	ctx := context.Background()
	u := uuid.New()
	now := time.Date(2026, 10, 10, 3, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }

	// An existing memory that is near the new "go ..." fact, and one nobody used.
	old, _ := s.Add(ctx, u, KindFact, "go modules are vendored", SourceChat, 0.4, uuid.NullUUID{})
	stale, _ := s.Add(ctx, u, KindFact, "zebra stripes are black", SourceChat, 0.8, uuid.NullUUID{})
	st := db.mems[stale.ID]
	st.CreatedAt = now.Add(-90 * 24 * time.Hour)
	db.mems[stale.ID] = st

	conv := uuid.New()
	db.convs[u] = []store.ListConversationsActiveSinceRow{{ID: conv, Title: "t", UpdatedAt: now}}
	parts, _ := json.Marshal([]gateway.Part{gateway.TextPart("I always deploy with docker compose")})
	db.msgs[conv] = []store.Message{{Role: "user", Parts: parts}}
	gw.reply = `{"facts":[{"content":"go builds run in CI","importance":0.9},{"content":"docker compose is the deploy tool","importance":0.6}],"preferences":[{"content":"  ","importance":0.5}],"decisions":[]}`

	res, err := s.Reflect(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	if res.Conversations != 1 || res.Added != 1 || res.Merged != 1 || res.Demoted != 1 {
		t.Errorf("result = %+v", res)
	}
	if got := db.mems[old.ID].Importance; got != 0.9 {
		t.Errorf("merged memory importance = %v, want the higher 0.9", got)
	}
	if got := db.mems[stale.ID].Importance; got >= 0.8 {
		t.Errorf("stale memory not demoted: %v", got)
	}
	if n, _ := db.CountUserMemories(ctx, u); n != 3 {
		t.Errorf("memories = %d, want 3 (merge adds none, one new)", n)
	}
	if db.settings[u].LastReflectedAt == nil {
		t.Error("last_reflected_at not set")
	}
	if !strings.Contains(gw.lastSys, "THE PERSON") {
		t.Error("reflection prompt not sent")
	}
	// Nothing is deleted by reflection.
	if _, ok := db.mems[stale.ID]; !ok {
		t.Error("reflection deleted a memory")
	}
}

func TestReflectWithEmbeddingDownStillStores(t *testing.T) {
	s, db, gw := newSvc()
	gw.embedErr = errors.New("no embedding model")
	ctx := context.Background()
	u := uuid.New()
	conv := uuid.New()
	db.convs[u] = []store.ListConversationsActiveSinceRow{{ID: conv, Title: "t", UpdatedAt: time.Now()}}
	parts, _ := json.Marshal([]gateway.Part{gateway.TextPart("hello")})
	db.msgs[conv] = []store.Message{{Role: "user", Parts: parts}}
	gw.reply = `{"facts":[{"content":"works nights","importance":0.7}]}`
	res, err := s.Reflect(ctx, u)
	if err != nil || res.Added != 1 {
		t.Fatalf("reflect with embeddings down: %+v %v", res, err)
	}
	var m store.UserMemory
	for _, v := range db.mems {
		m = v
	}
	if m.Embedding != nil {
		t.Error("stored a vector from nowhere")
	}
	_ = pgvector.Vector{}
}

func TestTickEnqueuesOnlyDueMembers(t *testing.T) {
	s, db, _ := newSvc()
	ctx := context.Background()
	due, notDue, off := uuid.New(), uuid.New(), uuid.New()
	now := time.Date(2026, 10, 10, 4, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	recent := now.Add(-time.Hour)
	for _, c := range []struct {
		id   uuid.UUID
		on   bool
		last *time.Time
	}{{due, true, nil}, {notDue, true, &recent}, {off, false, nil}} {
		db.settings[c.id] = store.MemorySetting{UserID: c.id, Enabled: true, ReflectEnabled: c.on, ReflectAt: "03:00", Timezone: "UTC", LastReflectedAt: c.last}
	}
	var got []uuid.UUID
	if err := s.Tick(ctx, func(_ context.Context, id uuid.UUID) error { got = append(got, id); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != due {
		t.Errorf("enqueued %v, want only %v", got, due)
	}
}
