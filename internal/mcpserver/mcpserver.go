// Package mcpserver is ws as a Model Context Protocol server (PLAN.md
// M5): Claude Code, Cursor or any MCP client connects to /mcp with a ws
// API key and gets ws's own capabilities as tools: the caller's projects
// and conversations, the routable models, a one-shot completion through
// the gateway (routed and billed like any other call), starting an agent
// run in a project and reading its status, and the task router's
// spawn_job. Everything is scoped to the key's user; nothing here is
// reachable with a browser session.
//
// The transport is Streamable HTTP in stateless mode (one POST per
// request, no session id), which is what the ws MCP client speaks too.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jking323/ws/internal/agent"
	"github.com/jking323/ws/internal/ccrouter"
	"github.com/jking323/ws/internal/chat"
	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
)

// Principal is the caller as the HTTP layer authenticated it.
type Principal struct {
	UserID   uuid.UUID
	APIKeyID uuid.NullUUID
	Owner    bool
	// Selector is the key's default model policy ("" means auto).
	Selector string
}

type ctxKey int

const principalKey ctxKey = 1

// WithPrincipal attaches the caller to a request context; the handler
// refuses a request without one.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey, p)
}

func principalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey).(Principal)
	return p, ok && p.UserID != uuid.Nil
}

// Store is the slice of the store the tools read and write (*store.DB implements it).
type Store interface {
	ListProjectsForUser(ctx context.Context, userID uuid.UUID) ([]store.Project, error)
	GetProject(ctx context.Context, id uuid.UUID) (store.Project, error)
	UserCanAccessProject(ctx context.Context, arg store.UserCanAccessProjectParams) (bool, error)
	ListConversationsForProject(ctx context.Context, arg store.ListConversationsForProjectParams) ([]store.Conversation, error)
	ListRecentConversationsForUser(ctx context.Context, arg store.ListRecentConversationsForUserParams) ([]store.Conversation, error)
	GetConversation(ctx context.Context, id uuid.UUID) (store.Conversation, error)
	CreateConversation(ctx context.Context, arg store.CreateConversationParams) (store.Conversation, error)
	NextMessageSeq(ctx context.Context, conversationID uuid.UUID) (int32, error)
	InsertMessage(ctx context.Context, arg store.InsertMessageParams) (store.Message, error)
	ListMessages(ctx context.Context, conversationID uuid.UUID) ([]store.Message, error)
	GetRun(ctx context.Context, id uuid.UUID) (store.AgentRun, error)
	LatestRunForConversation(ctx context.Context, conversationID uuid.UUID) (store.AgentRun, error)
}

// Completer is the gateway's one-shot call.
type Completer interface {
	Complete(ctx context.Context, req *gateway.Request) (*gateway.Response, error)
}

// Runs creates agent runs (*agent.Runtime implements it).
type Runs interface {
	Create(ctx context.Context, p agent.StartParams) (*store.AgentRun, error)
}

// Model is one routable endpoint as ws_models reports it.
type Model struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name,omitempty"`
	Provider    string `json:"provider"`
	Local       bool   `json:"local"`
	Health      string `json:"health,omitempty"`
}

// Server builds the MCP server for each request.
type Server struct {
	DB      Store
	GW      Completer
	Runtime Runs
	// Enqueue hands a created run to the worker; nil means ws_run is unavailable.
	Enqueue func(ctx context.Context, runID uuid.UUID) error
	// Models lists the routable endpoints and the policy aliases.
	Models func() ([]Model, map[string][]string)
	// Spawn is the task router's tool; nil leaves ws_spawn_job out.
	Spawn *ccrouter.Tool
	// Version is reported in the server's implementation info.
	Version string
	Log     *slog.Logger
	// AskTimeout bounds ws_ask (default 5 minutes).
	AskTimeout time.Duration
}

// Handler serves Streamable HTTP. The request context must carry a
// Principal (WithPrincipal); without one the request is refused.
func (s *Server) Handler() http.Handler {
	h := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		p, ok := principalFrom(r.Context())
		if !ok {
			return nil
		}
		return s.build(p)
	}, &mcp.StreamableHTTPOptions{
		Stateless: true,
		// The HTTP layer requires a bearer API key on every request and
		// refuses cookies, so a rebinding page could not act as the user.
		// Behind the reverse proxy the peer address is often loopback.
		DisableLocalhostProtection: true,
		Logger:                     s.Log,
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := principalFrom(r.Context()); !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"a ws API key is required (Authorization: Bearer ws_...)"}`))
			return
		}
		h.ServeHTTP(w, r)
	})
}

func (s *Server) build(p Principal) *mcp.Server {
	version := s.Version
	if version == "" {
		version = "dev"
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "ws", Title: "ws workspace", Version: version}, &mcp.ServerOptions{
		Instructions: "ws is a self-hosted AI workspace. Use ws_models to see what it can route to, ws_ask for a one-shot answer from a model of your choice " +
			"(local or hosted, billed to this key), ws_run to start an agent run in a project and ws_run_status to follow it, " +
			"ws_projects / ws_conversations / ws_conversation to read the workspace, and ws_spawn_job to hand a task to the task router.",
	})
	t := &tools{s: s, p: p}
	mcp.AddTool(srv, &mcp.Tool{Name: "ws_projects", Description: "List the caller's projects (chat, code, design, images).",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, t.projects)
	mcp.AddTool(srv, &mcp.Tool{Name: "ws_conversations", Description: "List conversations: a project's, or the caller's most recent across projects.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, t.conversations)
	mcp.AddTool(srv, &mcp.Tool{Name: "ws_conversation", Description: "Read a conversation: its messages as text (tool calls summarized) and the latest run's status.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, t.conversation)
	mcp.AddTool(srv, &mcp.Tool{Name: "ws_models", Description: "List the models ws can route to and the policy aliases (auto, best, cheap, local, code).",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, t.models)
	mcp.AddTool(srv, &mcp.Tool{Name: "ws_ask", Description: "One-shot completion through the ws gateway: pick a model or alias (default: this key's policy), get the text back. " +
		"Routed, failed over and recorded in the ledger like any ws call. No tools, no memory."}, t.ask)
	mcp.AddTool(srv, &mcp.Tool{Name: "ws_run", Description: "Start an agent run in a project: a new conversation with the prompt as its first message, queued on the ws worker " +
		"with the project's tools (sandbox for code projects). Returns ids; follow with ws_run_status."}, t.run)
	mcp.AddTool(srv, &mcp.Tool{Name: "ws_run_status", Description: "Status of an agent run started with ws_run (or any run the caller may see), with the latest assistant text.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, t.runStatus)
	if s.Spawn != nil {
		mcp.AddTool(srv, &mcp.Tool{Name: "ws_spawn_job", Description: "Hand a task to the ws task router (docs/CLAUDE_CODE_JOBS.md): it picks a lane " +
			"(claude-subscription tmux session, api, openrouter, local), logs the decision and dispatches when dispatch is enabled on the host. " +
			"Returns the decision and a handle, never the task's output. dry_run only decides. The api/openrouter/local lanes run in a conversation, " +
			"so pass conversation_id for them."}, t.spawn)
	}
	return srv
}

type tools struct {
	s *Server
	p Principal
}

// fail is a tool-level error the model sees (not a protocol error).
func fail(format string, a ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, a...)}}}
}

func (t *tools) canAccess(ctx context.Context, projectID uuid.UUID) bool {
	ok, err := t.s.DB.UserCanAccessProject(ctx, store.UserCanAccessProjectParams{ID: projectID, UserID: t.p.UserID})
	return err == nil && ok
}

// loadConversation returns a conversation the caller may read.
func (t *tools) loadConversation(ctx context.Context, id string) (store.Conversation, *mcp.CallToolResult) {
	cid, err := uuid.Parse(id)
	if err != nil {
		return store.Conversation{}, fail("conversation_id must be a uuid")
	}
	conv, err := t.s.DB.GetConversation(ctx, cid)
	if err != nil || !t.canAccess(ctx, conv.ProjectID) {
		return store.Conversation{}, fail("no such conversation")
	}
	return conv, nil
}

// ---- projects, conversations ----

type projectOut struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	RepoURL   string    `json:"repo_url,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

type projectsOut struct {
	Projects []projectOut `json:"projects"`
}

func (t *tools) projects(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, projectsOut, error) {
	rows, err := t.s.DB.ListProjectsForUser(ctx, t.p.UserID)
	if err != nil {
		return fail("db: %v", err), projectsOut{}, nil
	}
	out := projectsOut{Projects: []projectOut{}}
	for _, r := range rows {
		p := projectOut{ID: r.ID.String(), Name: r.Name, Kind: r.Kind, UpdatedAt: r.UpdatedAt}
		if r.RepoUrl != nil {
			p.RepoURL = *r.RepoUrl
		}
		out.Projects = append(out.Projects, p)
	}
	return nil, out, nil
}

type conversationsIn struct {
	ProjectID string `json:"project_id,omitempty" jsonschema:"A project id from ws_projects; omit for the caller's most recent conversations across projects"`
	Limit     int    `json:"limit,omitempty" jsonschema:"At most this many (default 20, max 100)"`
}

type conversationOut struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	Title     string    `json:"title"`
	Mode      string    `json:"mode"`
	Model     string    `json:"model"`
	UpdatedAt time.Time `json:"updated_at"`
}

type conversationsOut struct {
	Conversations []conversationOut `json:"conversations"`
}

func convOut(c store.Conversation) conversationOut {
	return conversationOut{ID: c.ID.String(), ProjectID: c.ProjectID.String(), Title: c.Title, Mode: c.Mode, Model: c.ModelSelector, UpdatedAt: c.UpdatedAt}
}

func (t *tools) conversations(ctx context.Context, _ *mcp.CallToolRequest, in conversationsIn) (*mcp.CallToolResult, conversationsOut, error) {
	limit := in.Limit
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	var rows []store.Conversation
	var err error
	if in.ProjectID != "" {
		pid, perr := uuid.Parse(in.ProjectID)
		if perr != nil || !t.canAccess(ctx, pid) {
			return fail("no such project"), conversationsOut{}, nil
		}
		rows, err = t.s.DB.ListConversationsForProject(ctx, store.ListConversationsForProjectParams{ProjectID: pid, Limit: int32(limit)})
	} else {
		rows, err = t.s.DB.ListRecentConversationsForUser(ctx, store.ListRecentConversationsForUserParams{UserID: t.p.UserID, Limit: int32(limit)})
	}
	if err != nil {
		return fail("db: %v", err), conversationsOut{}, nil
	}
	out := conversationsOut{Conversations: []conversationOut{}}
	for _, c := range rows {
		out.Conversations = append(out.Conversations, convOut(c))
	}
	return nil, out, nil
}

type conversationIn struct {
	ConversationID string `json:"conversation_id" jsonschema:"The conversation id"`
	Last           int    `json:"last,omitempty" jsonschema:"Only the last N messages (default all, max 200)"`
}

type messageOut struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

type conversationFull struct {
	Conversation conversationOut `json:"conversation"`
	RunStatus    string          `json:"run_status,omitempty"`
	Messages     []messageOut    `json:"messages"`
}

func (t *tools) conversation(ctx context.Context, _ *mcp.CallToolRequest, in conversationIn) (*mcp.CallToolResult, conversationFull, error) {
	conv, ferr := t.loadConversation(ctx, in.ConversationID)
	if ferr != nil {
		return ferr, conversationFull{}, nil
	}
	rows, err := t.s.DB.ListMessages(ctx, conv.ID)
	if err != nil {
		return fail("db: %v", err), conversationFull{}, nil
	}
	out := conversationFull{Conversation: convOut(conv), Messages: renderMessages(rows)}
	if in.Last > 0 && in.Last < len(out.Messages) {
		if in.Last > 200 {
			in.Last = 200
		}
		out.Messages = out.Messages[len(out.Messages)-in.Last:]
	}
	if run, err := t.s.DB.LatestRunForConversation(ctx, conv.ID); err == nil {
		out.RunStatus = run.Status
	}
	return nil, out, nil
}

// renderMessages flattens stored rows to role + text: text parts
// verbatim, tool calls and results as one-line summaries, reasoning
// left out. Tool results are clipped so one large output cannot swamp
// the reply.
func renderMessages(rows []store.Message) []messageOut {
	out := []messageOut{}
	for _, m := range chat.MergeUI(rows, nil) {
		var b strings.Builder
		for _, p := range m.Parts {
			typ, _ := p["type"].(string)
			text, _ := p["text"].(string)
			switch {
			case typ == "text" && text != "":
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(text)
			case strings.HasPrefix(typ, "tool-"):
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				name := strings.TrimPrefix(typ, "tool-")
				args, _ := json.Marshal(p["input"])
				fmt.Fprintf(&b, "[tool %s %s]", name, clip(string(args), 300))
				if out, ok := p["output"]; ok && out != nil {
					res, _ := json.Marshal(out)
					fmt.Fprintf(&b, " -> %s", clip(string(res), 600))
				}
			}
		}
		if b.Len() == 0 {
			continue
		}
		out = append(out, messageOut{Role: m.Role, Text: b.String()})
	}
	return out
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---- models, ask ----

type modelsOut struct {
	Models  []Model             `json:"models"`
	Aliases map[string][]string `json:"aliases"`
}

func (t *tools) models(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, modelsOut, error) {
	out := modelsOut{Models: []Model{}, Aliases: map[string][]string{"auto": nil}}
	if t.s.Models != nil {
		m, a := t.s.Models()
		if m != nil {
			out.Models = m
		}
		for k, v := range a {
			out.Aliases[k] = v
		}
	}
	return nil, out, nil
}

type askIn struct {
	Prompt    string  `json:"prompt" jsonschema:"The user message"`
	System    string  `json:"system,omitempty" jsonschema:"Optional system prompt"`
	Model     string  `json:"model,omitempty" jsonschema:"An endpoint id, provider/model, or alias from ws_models; default: this key's policy"`
	MaxTokens int     `json:"max_tokens,omitempty" jsonschema:"Output cap (default 4096)"`
	Temp      float64 `json:"temperature,omitempty" jsonschema:"Sampling temperature; omit for the model's default"`
}

type askOut struct {
	Text         string `json:"text"`
	Model        string `json:"model"`
	EndpointID   string `json:"endpoint_id"`
	FinishReason string `json:"finish_reason"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
}

func (t *tools) ask(ctx context.Context, _ *mcp.CallToolRequest, in askIn) (*mcp.CallToolResult, askOut, error) {
	if strings.TrimSpace(in.Prompt) == "" {
		return fail("prompt is required"), askOut{}, nil
	}
	if t.s.GW == nil {
		return fail("the gateway is not available here"), askOut{}, nil
	}
	model := in.Model
	if model == "" {
		model = t.p.Selector
	}
	if model == "" {
		model = "auto"
	}
	req := &gateway.Request{
		Model: model, System: in.System,
		Messages:  []gateway.Message{{Role: gateway.RoleUser, Parts: []gateway.Part{gateway.TextPart(in.Prompt)}}},
		MaxTokens: in.MaxTokens,
		Metadata:  gateway.Metadata{UserID: t.p.UserID.String(), TaskClass: gateway.TaskChat, External: true},
	}
	if req.MaxTokens <= 0 {
		req.MaxTokens = 4096
	}
	if in.Temp > 0 {
		temp := in.Temp
		req.Temperature = &temp
	}
	if t.p.APIKeyID.Valid {
		req.Metadata.APIKeyID = t.p.APIKeyID.UUID.String()
	}
	timeout := t.s.AskTimeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := t.s.GW.Complete(cctx, req)
	if err != nil {
		return fail("gateway: %v", err), askOut{}, nil
	}
	var b strings.Builder
	for _, p := range res.Parts {
		if p.Kind == gateway.PartText {
			b.WriteString(p.Text)
		}
	}
	return nil, askOut{Text: b.String(), Model: res.Model, EndpointID: res.EndpointID, FinishReason: string(res.FinishReason),
		InputTokens: res.Usage.InputTokens, OutputTokens: res.Usage.OutputTokens}, nil
}

// ---- runs ----

type runIn struct {
	ProjectID string `json:"project_id" jsonschema:"The project to run in (ws_projects)"`
	Prompt    string `json:"prompt" jsonschema:"The task; everything the agent needs must be in here or in the project"`
	Model     string `json:"model,omitempty" jsonschema:"Model or alias; default auto"`
	Title     string `json:"title,omitempty" jsonschema:"Conversation title; default the first words of the prompt"`
}

type runOut struct {
	ConversationID string `json:"conversation_id"`
	RunID          string `json:"run_id"`
	Status         string `json:"status"`
}

func (t *tools) run(ctx context.Context, _ *mcp.CallToolRequest, in runIn) (*mcp.CallToolResult, runOut, error) {
	if t.s.Runtime == nil || t.s.Enqueue == nil {
		return fail("agent runs are not available here"), runOut{}, nil
	}
	if strings.TrimSpace(in.Prompt) == "" {
		return fail("prompt is required"), runOut{}, nil
	}
	pid, err := uuid.Parse(in.ProjectID)
	if err != nil || !t.canAccess(ctx, pid) {
		return fail("no such project"), runOut{}, nil
	}
	proj, err := t.s.DB.GetProject(ctx, pid)
	if err != nil {
		return fail("no such project"), runOut{}, nil
	}
	mode := "chat"
	if proj.Kind == "code" {
		mode = "code"
	}
	selector := in.Model
	if selector == "" {
		selector = "auto"
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = titleFor(in.Prompt)
	}
	conv, err := t.s.DB.CreateConversation(ctx, store.CreateConversationParams{
		ProjectID: pid, UserID: t.p.UserID, Title: title, Mode: mode, ModelSelector: selector, Settings: json.RawMessage(`{}`),
	})
	if err != nil {
		return fail("create conversation: %v", err), runOut{}, nil
	}
	parts, _ := json.Marshal([]gateway.Part{gateway.TextPart(in.Prompt)})
	seq, err := t.s.DB.NextMessageSeq(ctx, conv.ID)
	if err != nil {
		return fail("db: %v", err), runOut{}, nil
	}
	if _, err := t.s.DB.InsertMessage(ctx, store.InsertMessageParams{ConversationID: conv.ID, Seq: int64(seq), Role: "user", Parts: parts}); err != nil {
		return fail("insert message: %v", err), runOut{}, nil
	}
	task := gateway.TaskChat
	if mode == "code" {
		task = gateway.TaskCode
	}
	run, err := t.s.Runtime.Create(ctx, agent.StartParams{
		ConversationID: conv.ID, UserID: t.p.UserID,
		Request: agent.Request{Selector: selector, TaskClass: task},
	})
	if err != nil {
		return fail("create run: %v", err), runOut{}, nil
	}
	if err := t.s.Enqueue(context.WithoutCancel(ctx), run.ID); err != nil {
		return fail("enqueue run: %v", err), runOut{}, nil
	}
	return nil, runOut{ConversationID: conv.ID.String(), RunID: run.ID.String(), Status: run.Status}, nil
}

type runStatusIn struct {
	RunID string `json:"run_id" jsonschema:"The run id from ws_run"`
}

type runStatusOut struct {
	RunID          string     `json:"run_id"`
	ConversationID string     `json:"conversation_id"`
	Status         string     `json:"status"`
	Steps          int        `json:"steps"`
	CostUSD        float64    `json:"cost_usd"`
	Error          string     `json:"error,omitempty"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	EndedAt        *time.Time `json:"ended_at,omitempty"`
	// LastAssistant is the newest assistant text in the conversation.
	LastAssistant string `json:"last_assistant,omitempty"`
}

func (t *tools) runStatus(ctx context.Context, _ *mcp.CallToolRequest, in runStatusIn) (*mcp.CallToolResult, runStatusOut, error) {
	rid, err := uuid.Parse(in.RunID)
	if err != nil {
		return fail("run_id must be a uuid"), runStatusOut{}, nil
	}
	run, err := t.s.DB.GetRun(ctx, rid)
	if err != nil {
		return fail("no such run"), runStatusOut{}, nil
	}
	conv, err := t.s.DB.GetConversation(ctx, run.ConversationID)
	if err != nil || !t.canAccess(ctx, conv.ProjectID) {
		return fail("no such run"), runStatusOut{}, nil
	}
	out := runStatusOut{RunID: run.ID.String(), ConversationID: conv.ID.String(), Status: run.Status, Steps: int(run.StepCount),
		CostUSD: run.CostUsd, StartedAt: run.StartedAt, EndedAt: run.EndedAt}
	if run.Error != nil {
		out.Error = *run.Error
	}
	if rows, err := t.s.DB.ListMessages(ctx, conv.ID); err == nil {
		msgs := renderMessages(rows)
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role == "assistant" {
				out.LastAssistant = msgs[i].Text
				break
			}
		}
	}
	return nil, out, nil
}

// ---- spawn_job ----

type spawnIn struct {
	Prompt         string `json:"prompt" jsonschema:"The task, written for the model that will run it"`
	Lane           string `json:"lane,omitempty" jsonschema:"Force a lane: claude-subscription, api, openrouter or local; omit to route by rules"`
	Cwd            string `json:"cwd,omitempty" jsonschema:"Working directory on the target for repo work"`
	Target         string `json:"target,omitempty" jsonschema:"Launcher target name for the subscription lane"`
	Model          string `json:"model,omitempty" jsonschema:"Model hint"`
	DryRun         bool   `json:"dry_run,omitempty" jsonschema:"Only decide and log; start nothing"`
	ConversationID string `json:"conversation_id,omitempty" jsonschema:"Conversation whose project hosts an api/openrouter/local run"`
}

func (t *tools) spawn(ctx context.Context, _ *mcp.CallToolRequest, in spawnIn) (*mcp.CallToolResult, *ccrouter.Output, error) {
	tc := agent.ToolCtx{UserID: t.p.UserID}
	if in.ConversationID != "" {
		conv, ferr := t.loadConversation(ctx, in.ConversationID)
		if ferr != nil {
			return ferr, nil, nil
		}
		tc.ConversationID = conv.ID
	}
	out, err := t.s.Spawn.Run(ctx, tc, ccrouter.Input{Prompt: in.Prompt, Lane: in.Lane, Cwd: in.Cwd, Target: in.Target, Model: in.Model, DryRun: in.DryRun})
	if err != nil {
		if in.ConversationID == "" && strings.Contains(err.Error(), "conversation") {
			err = errors.New(err.Error() + " (pass conversation_id for the api, openrouter and local lanes)")
		}
		return fail("%v", err), nil, nil
	}
	return nil, out, nil
}

// titleFor is the prompt's first words.
func titleFor(prompt string) string {
	title := strings.Join(strings.Fields(prompt), " ")
	runes := []rune(title)
	if len(runes) <= 60 {
		return title
	}
	return string(runes[:57]) + "..."
}
