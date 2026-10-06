package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jking323/ws/internal/agent"
	"github.com/jking323/ws/internal/ccrouter"
	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
)

// fakeStore is two users' worth of workspace in memory.
type fakeStore struct {
	mu       sync.Mutex
	projects map[uuid.UUID]store.Project
	convs    map[uuid.UUID]store.Conversation
	msgs     map[uuid.UUID][]store.Message
	runs     map[uuid.UUID]store.AgentRun
}

func newFake() *fakeStore {
	return &fakeStore{projects: map[uuid.UUID]store.Project{}, convs: map[uuid.UUID]store.Conversation{}, msgs: map[uuid.UUID][]store.Message{}, runs: map[uuid.UUID]store.AgentRun{}}
}

func (f *fakeStore) ListProjectsForUser(_ context.Context, userID uuid.UUID) ([]store.Project, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.Project
	for _, p := range f.projects {
		if p.OwnerID == userID {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *fakeStore) GetProject(_ context.Context, id uuid.UUID) (store.Project, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.projects[id]
	if !ok {
		return store.Project{}, errors.New("no rows")
	}
	return p, nil
}

func (f *fakeStore) UserCanAccessProject(_ context.Context, a store.UserCanAccessProjectParams) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.projects[a.ID]
	return ok && p.OwnerID == a.UserID, nil
}

func (f *fakeStore) ListConversationsForProject(_ context.Context, a store.ListConversationsForProjectParams) ([]store.Conversation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.Conversation
	for _, c := range f.convs {
		if c.ProjectID == a.ProjectID {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeStore) ListRecentConversationsForUser(_ context.Context, a store.ListRecentConversationsForUserParams) ([]store.Conversation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.Conversation
	for _, c := range f.convs {
		if c.UserID == a.UserID {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeStore) GetConversation(_ context.Context, id uuid.UUID) (store.Conversation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.convs[id]
	if !ok {
		return store.Conversation{}, errors.New("no rows")
	}
	return c, nil
}

func (f *fakeStore) CreateConversation(_ context.Context, a store.CreateConversationParams) (store.Conversation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := store.Conversation{ID: uuid.New(), ProjectID: a.ProjectID, UserID: a.UserID, Title: a.Title, Mode: a.Mode, ModelSelector: a.ModelSelector, UpdatedAt: time.Now()}
	f.convs[c.ID] = c
	return c, nil
}

func (f *fakeStore) NextMessageSeq(_ context.Context, id uuid.UUID) (int32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int32(len(f.msgs[id]) + 1), nil
}

func (f *fakeStore) InsertMessage(_ context.Context, a store.InsertMessageParams) (store.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := store.Message{ID: uuid.New(), ConversationID: a.ConversationID, Seq: a.Seq, Role: a.Role, Parts: a.Parts}
	f.msgs[a.ConversationID] = append(f.msgs[a.ConversationID], m)
	return m, nil
}

func (f *fakeStore) ListMessages(_ context.Context, id uuid.UUID) ([]store.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.Message(nil), f.msgs[id]...), nil
}

func (f *fakeStore) GetRun(_ context.Context, id uuid.UUID) (store.AgentRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runs[id]
	if !ok {
		return store.AgentRun{}, errors.New("no rows")
	}
	return r, nil
}

func (f *fakeStore) LatestRunForConversation(_ context.Context, id uuid.UUID) (store.AgentRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.runs {
		if r.ConversationID == id {
			return r, nil
		}
	}
	return store.AgentRun{}, errors.New("no rows")
}

// Runs implementation: records the run in the fake store.
func (f *fakeStore) Create(_ context.Context, p agent.StartParams) (*store.AgentRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	req, _ := json.Marshal(p.Request)
	r := store.AgentRun{ID: uuid.New(), ConversationID: p.ConversationID, UserID: uuid.NullUUID{UUID: p.UserID, Valid: true}, Status: "queued", Request: req}
	f.runs[r.ID] = r
	return &r, nil
}

type fakeGW struct {
	mu   sync.Mutex
	last *gateway.Request
}

func (g *fakeGW) Complete(_ context.Context, req *gateway.Request) (*gateway.Response, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.last = req
	if strings.Contains(req.Messages[0].Parts[0].Text, "fail") {
		return nil, errors.New("no route")
	}
	return &gateway.Response{EndpointID: "local-llama/gpt-oss-20b", Model: "gpt-oss-20b", Parts: []gateway.Part{gateway.TextPart("hello from "), gateway.TextPart(req.Model)},
		Usage: gateway.Usage{InputTokens: 7, OutputTokens: 3}, FinishReason: gateway.FinishStop}, nil
}

type harness struct {
	db     *fakeStore
	gw     *fakeGW
	srv    *Server
	ts     *httptest.Server
	owner  Principal
	other  Principal
	queued []uuid.UUID
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{db: newFake(), gw: &fakeGW{}}
	h.owner = Principal{UserID: uuid.New(), APIKeyID: uuid.NullUUID{UUID: uuid.New(), Valid: true}, Owner: true, Selector: "cheap"}
	h.other = Principal{UserID: uuid.New(), APIKeyID: uuid.NullUUID{UUID: uuid.New(), Valid: true}}
	h.srv = &Server{
		DB: h.db, GW: h.gw, Runtime: h.db,
		Enqueue: func(_ context.Context, id uuid.UUID) error { h.queued = append(h.queued, id); return nil },
		Models: func() ([]Model, map[string][]string) {
			return []Model{{ID: "local-llama/gpt-oss-20b", Provider: "local-llama", Local: true}}, map[string][]string{"cheap": {"local-llama/gpt-oss-20b"}}
		},
		Spawn:   &ccrouter.Tool{IsOwner: func(context.Context, uuid.UUID) (bool, error) { return true, nil }},
		Version: "test",
	}
	// Stand-in for the httpx layer: the principal comes from a header.
	handler := h.srv.Handler()
	h.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		switch r.Header.Get("X-Test-User") {
		case "owner":
			ctx = WithPrincipal(ctx, h.owner)
		case "other":
			ctx = WithPrincipal(ctx, h.other)
		}
		handler.ServeHTTP(w, r.WithContext(ctx))
	}))
	t.Cleanup(h.ts.Close)
	return h
}

type headerRT struct {
	user string
	next http.RoundTripper
}

func (h headerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("X-Test-User", h.user)
	return h.next.RoundTrip(r)
}

func (h *harness) session(t *testing.T, user string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	tr := &mcp.StreamableClientTransport{Endpoint: h.ts.URL, HTTPClient: &http.Client{Transport: headerRT{user: user, next: http.DefaultTransport}}}
	cs, err := client.Connect(context.Background(), tr, nil)
	if err != nil {
		t.Fatalf("connect as %s: %v", user, err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, out any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if !res.IsError && out != nil {
		var text string
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				text += tc.Text
			}
		}
		if err := json.Unmarshal([]byte(text), out); err != nil {
			t.Fatalf("%s: decode %q: %v", name, text, err)
		}
	}
	return res
}

func errText(res *mcp.CallToolResult) string {
	var s string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			s += tc.Text
		}
	}
	return s
}

func TestUnauthenticated(t *testing.T) {
	h := newHarness(t)
	res, err := http.Post(h.ts.URL, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 401 {
		t.Errorf("no principal: status %d", res.StatusCode)
	}
}

func TestToolsAndScoping(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	proj := store.Project{ID: uuid.New(), OwnerID: h.owner.UserID, Name: "ws", Kind: "code"}
	h.db.projects[proj.ID] = proj
	theirs := store.Project{ID: uuid.New(), OwnerID: h.other.UserID, Name: "private", Kind: "chat"}
	h.db.projects[theirs.ID] = theirs
	conv, _ := h.db.CreateConversation(ctx, store.CreateConversationParams{ProjectID: proj.ID, UserID: h.owner.UserID, Title: "fix the build", Mode: "code", ModelSelector: "auto"})
	user, _ := json.Marshal([]gateway.Part{gateway.TextPart("why does the build fail?")})
	_, _ = h.db.InsertMessage(ctx, store.InsertMessageParams{ConversationID: conv.ID, Seq: 1, Role: "user", Parts: user})
	asst, _ := json.Marshal([]gateway.Part{gateway.TextPart("Looking."), {Kind: gateway.PartToolCall, ToolCallID: "c1", ToolName: "bash", Args: json.RawMessage(`{"cmd":"go build ./..."}`)}})
	_, _ = h.db.InsertMessage(ctx, store.InsertMessageParams{ConversationID: conv.ID, Seq: 2, Role: "assistant", Parts: asst})
	tool, _ := json.Marshal([]gateway.Part{{Kind: gateway.PartToolResult, ToolCallID: "c1", Content: []gateway.Part{gateway.TextPart("ok " + strings.Repeat("x", 2000))}}})
	_, _ = h.db.InsertMessage(ctx, store.InsertMessageParams{ConversationID: conv.ID, Seq: 3, Role: "tool", Parts: tool})
	asst2, _ := json.Marshal([]gateway.Part{gateway.TextPart("It builds now.")})
	_, _ = h.db.InsertMessage(ctx, store.InsertMessageParams{ConversationID: conv.ID, Seq: 4, Role: "assistant", Parts: asst2})

	cs := h.session(t, "owner")
	tl, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range tl.Tools {
		names[tool.Name] = true
	}
	for _, want := range []string{"ws_projects", "ws_conversations", "ws_conversation", "ws_models", "ws_ask", "ws_run", "ws_run_status", "ws_spawn_job"} {
		if !names[want] {
			t.Errorf("tool %s missing from %v", want, names)
		}
	}

	var ps projectsOut
	call(t, cs, "ws_projects", nil, &ps)
	if len(ps.Projects) != 1 || ps.Projects[0].ID != proj.ID.String() || ps.Projects[0].Kind != "code" {
		t.Errorf("projects = %+v (must be the caller's only)", ps)
	}

	var cv conversationsOut
	call(t, cs, "ws_conversations", map[string]any{"project_id": proj.ID.String()}, &cv)
	if len(cv.Conversations) != 1 || cv.Conversations[0].Title != "fix the build" {
		t.Errorf("conversations = %+v", cv)
	}
	if res := call(t, cs, "ws_conversations", map[string]any{"project_id": theirs.ID.String()}, nil); !res.IsError {
		t.Error("another user's project must not list")
	}
	call(t, cs, "ws_conversations", nil, &cv)
	if len(cv.Conversations) != 1 {
		t.Errorf("recent conversations = %+v", cv)
	}

	var full conversationFull
	call(t, cs, "ws_conversation", map[string]any{"conversation_id": conv.ID.String()}, &full)
	if len(full.Messages) != 2 || full.Messages[0].Role != "user" || full.Messages[1].Role != "assistant" {
		t.Fatalf("messages = %+v", full.Messages)
	}
	a := full.Messages[1].Text
	if !strings.Contains(a, "Looking.") || !strings.Contains(a, "[tool bash") || !strings.Contains(a, "go build") || !strings.Contains(a, "It builds now.") {
		t.Errorf("assistant text = %q", a)
	}
	if len(a) > 1500 {
		t.Errorf("tool output not clipped: %d bytes", len(a))
	}
	call(t, cs, "ws_conversation", map[string]any{"conversation_id": conv.ID.String(), "last": 1}, &full)
	if len(full.Messages) != 1 || full.Messages[0].Role != "assistant" {
		t.Errorf("last=1 = %+v", full.Messages)
	}

	var ms modelsOut
	call(t, cs, "ws_models", nil, &ms)
	if len(ms.Models) != 1 || ms.Aliases["cheap"] == nil || len(ms.Aliases["auto"]) != 0 {
		t.Errorf("models = %+v", ms)
	}

	// ws_ask: the key's policy is the default model; the ledger metadata names the caller.
	var ao askOut
	call(t, cs, "ws_ask", map[string]any{"prompt": "hi"}, &ao)
	if ao.Text != "hello from cheap" || ao.EndpointID != "local-llama/gpt-oss-20b" || ao.InputTokens != 7 || ao.OutputTokens != 3 || ao.FinishReason != "stop" {
		t.Errorf("ask = %+v", ao)
	}
	if h.gw.last.Model != "cheap" || h.gw.last.Metadata.UserID != h.owner.UserID.String() || h.gw.last.Metadata.APIKeyID != h.owner.APIKeyID.UUID.String() || !h.gw.last.Metadata.External || h.gw.last.MaxTokens != 4096 {
		t.Errorf("ask request = %+v", h.gw.last)
	}
	call(t, cs, "ws_ask", map[string]any{"prompt": "hi", "model": "local-llama/gpt-oss-20b", "system": "be brief", "max_tokens": 50}, &ao)
	if h.gw.last.Model != "local-llama/gpt-oss-20b" || h.gw.last.System != "be brief" || h.gw.last.MaxTokens != 50 {
		t.Errorf("ask request with model = %+v", h.gw.last)
	}
	if res := call(t, cs, "ws_ask", map[string]any{"prompt": "please fail"}, nil); !res.IsError || !strings.Contains(errText(res), "no route") {
		t.Errorf("gateway error must be a tool error: %+v", res)
	}
	if res := call(t, cs, "ws_ask", map[string]any{"prompt": " "}, nil); !res.IsError {
		t.Error("empty prompt accepted")
	}

	// ws_run creates a conversation with the prompt and queues a run in the project's mode.
	var ro runOut
	call(t, cs, "ws_run", map[string]any{"project_id": proj.ID.String(), "prompt": "add a test for the parser"}, &ro)
	if ro.Status != "queued" || len(h.queued) != 1 || h.queued[0].String() != ro.RunID {
		t.Errorf("run = %+v queued %v", ro, h.queued)
	}
	newConv := h.db.convs[uuid.MustParse(ro.ConversationID)]
	if newConv.Mode != "code" || newConv.Title != "add a test for the parser" || newConv.ModelSelector != "auto" || len(h.db.msgs[newConv.ID]) != 1 {
		t.Errorf("run conversation = %+v msgs %d", newConv, len(h.db.msgs[newConv.ID]))
	}
	var req agent.Request
	_ = json.Unmarshal(h.db.runs[uuid.MustParse(ro.RunID)].Request, &req)
	if req.TaskClass != gateway.TaskCode || req.Selector != "auto" {
		t.Errorf("run request = %+v", req)
	}
	if res := call(t, cs, "ws_run", map[string]any{"project_id": theirs.ID.String(), "prompt": "x"}, nil); !res.IsError {
		t.Error("run in another user's project accepted")
	}

	var st runStatusOut
	call(t, cs, "ws_run_status", map[string]any{"run_id": ro.RunID}, &st)
	if st.Status != "queued" || st.ConversationID != ro.ConversationID || st.LastAssistant != "" {
		t.Errorf("status = %+v", st)
	}
	// A run in a conversation the caller cannot see is not found.
	hidden, _ := h.db.CreateConversation(ctx, store.CreateConversationParams{ProjectID: theirs.ID, UserID: h.other.UserID})
	hr, _ := h.db.Create(ctx, agent.StartParams{ConversationID: hidden.ID, UserID: h.other.UserID})
	if res := call(t, cs, "ws_run_status", map[string]any{"run_id": hr.ID.String()}, nil); !res.IsError {
		t.Error("another user's run visible")
	}

	// ws_spawn_job: dry run through the router.
	var so ccrouter.Output
	call(t, cs, "ws_spawn_job", map[string]any{"prompt": "refactor internal/a/x.go and internal/b/y.go to share a helper", "cwd": "~/code/ws", "dry_run": true}, &so)
	if so.Decision.Lane != "claude-subscription" || !so.DryRun || so.Dispatched {
		t.Errorf("spawn = %+v", so)
	}
	if res := call(t, cs, "ws_spawn_job", map[string]any{"prompt": "x", "conversation_id": hidden.ID.String()}, nil); !res.IsError {
		t.Error("spawn with another user's conversation accepted")
	}

	// The other user sees none of the owner's workspace.
	other := h.session(t, "other")
	call(t, other, "ws_projects", nil, &ps)
	if len(ps.Projects) != 1 || ps.Projects[0].ID != theirs.ID.String() {
		t.Errorf("other's projects = %+v", ps)
	}
	if res := call(t, other, "ws_conversation", map[string]any{"conversation_id": conv.ID.String()}, nil); !res.IsError {
		t.Error("other user read the owner's conversation")
	}
}
