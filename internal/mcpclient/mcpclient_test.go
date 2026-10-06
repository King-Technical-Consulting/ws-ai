package mcpclient

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jking323/ws/internal/agent"
)

// An in-process MCP server with two tools, served over Streamable HTTP.
func fakeServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "fake-infisical", Version: "0"}, nil)
	type getIn struct {
		Name string `json:"name"`
	}
	type getOut struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	mcp.AddTool(srv, &mcp.Tool{Name: "get-secret", Description: "Get one secret"}, func(ctx context.Context, req *mcp.CallToolRequest, in getIn) (*mcp.CallToolResult, getOut, error) {
		if in.Name == "" {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "name required"}}}, getOut{}, nil
		}
		return nil, getOut{Name: in.Name, Value: "****"}, nil
	})
	mcp.AddTool(srv, &mcp.Tool{Name: "list-secrets", Description: "List secrets"}, func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "A, B, C"}}}, nil, nil
	})
	srv.AddTool(&mcp.Tool{Name: "delete-secret", Description: "Delete", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "deleted"}}}, nil
	})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	return httptest.NewServer(h)
}

func TestConnectAndCall(t *testing.T) {
	ts := fakeServer(t)
	defer ts.Close()
	cfg := ServerConfig{
		Name: "infisical", URL: ts.URL, Policy: agent.PolicyAsk,
		Policies:   map[string]agent.Policy{"list-secrets": agent.PolicyAuto, "delete-secret": agent.PolicyDeny},
		Idempotent: []string{"list-secrets", "get-secret"},
	}
	s, tools, err := Connect(context.Background(), cfg, slog.Default(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	byName := map[string]agent.Tool{}
	for _, tl := range tools {
		byName[tl.Def().Name] = tl
	}
	if len(byName) != 3 {
		t.Fatalf("tools = %v", byName)
	}
	get, ok := byName["mcp__infisical__get-secret"]
	if !ok {
		t.Fatalf("missing namespaced tool; have %v", byName)
	}
	if get.DefaultPolicy() != agent.PolicyAsk || !get.Idempotent() {
		t.Errorf("get-secret policy/idempotent = %v %v", get.DefaultPolicy(), get.Idempotent())
	}
	if byName["mcp__infisical__list-secrets"].DefaultPolicy() != agent.PolicyAuto {
		t.Errorf("per-tool policy override not applied")
	}
	if byName["mcp__infisical__delete-secret"].DefaultPolicy() != agent.PolicyDeny || byName["mcp__infisical__delete-secret"].Idempotent() {
		t.Errorf("delete-secret should be deny and non-idempotent")
	}
	// Schema comes from the server (generated from getIn).
	var schema map[string]any
	_ = json.Unmarshal(get.Def().InputSchema, &schema)
	if props, _ := schema["properties"].(map[string]any); props["name"] == nil {
		t.Errorf("input schema missing name: %s", get.Def().InputSchema)
	}

	res, err := get.Call(context.Background(), agent.ToolCtx{}, json.RawMessage(`{"name":"OPENROUTER_API_KEY"}`))
	if err != nil || res.IsError {
		t.Fatalf("call: %v %+v", err, res)
	}
	if !strings.Contains(res.Text, "OPENROUTER_API_KEY") || !strings.Contains(res.Text, "****") {
		t.Errorf("text = %q", res.Text)
	}
	if res.Data == nil {
		t.Errorf("structured content should be carried as Data")
	}
	res, _ = get.Call(context.Background(), agent.ToolCtx{}, json.RawMessage(`{}`))
	// The SDK validates arguments against the schema before sending, so the
	// error can come from either side; both must surface as IsError.
	if !res.IsError || !strings.Contains(res.Text, "name") {
		t.Errorf("IsError must propagate: %+v", res)
	}
	res, _ = byName["mcp__infisical__list-secrets"].Call(context.Background(), agent.ToolCtx{}, nil)
	if res.Text != "A, B, C" {
		t.Errorf("list = %q", res.Text)
	}
}

func TestAllowDenyAndFile(t *testing.T) {
	ts := fakeServer(t)
	defer ts.Close()
	cfg := ServerConfig{Name: "x", URL: ts.URL, Allow: []string{"get-secret", "list-secrets"}, Deny: []string{"list-secrets"}}
	s, tools, err := Connect(context.Background(), cfg, slog.Default(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if len(tools) != 1 || tools[0].Def().Name != "mcp__x__get-secret" {
		t.Errorf("allow/deny: %v", tools)
	}

	dir := t.TempDir()
	p := filepath.Join(dir, "mcp.yaml")
	_ = os.WriteFile(p, []byte("servers:\n  - name: infisical\n    url: http://mcp:8000/mcp\n    policy: ask\n    policies: {list-secrets: auto}\n  - name: Bad Name\n    url: x\n"), 0o644)
	if _, err := LoadFile(p); err == nil || !strings.Contains(err.Error(), "name must match") {
		t.Errorf("invalid server name must be rejected: %v", err)
	}
	_ = os.WriteFile(p, []byte("servers:\n  - name: infisical\n    url: http://mcp:8000/mcp\n"), 0o644)
	f, err := LoadFile(p)
	if err != nil || len(f.Servers) != 1 {
		t.Errorf("load: %v %+v", err, f)
	}
	if f, err := LoadFile(filepath.Join(dir, "missing.yaml")); err != nil || len(f.Servers) != 0 {
		t.Errorf("missing file must be empty config: %v", err)
	}
	if ToolName("infisical", "get secret/with:odd") != "mcp__infisical__get_secret_with_odd" {
		t.Errorf("ToolName = %q", ToolName("infisical", "get secret/with:odd"))
	}

	// Unreachable server: Connect fails within the wait; ConnectAll skips it.
	_, _, err = Connect(context.Background(), ServerConfig{Name: "dead", URL: "http://127.0.0.1:1/mcp"}, slog.Default(), 1*time.Second)
	if err == nil {
		t.Errorf("dead server must fail")
	}
	all, closeAll := ConnectAll(context.Background(), &File{Servers: []ServerConfig{{Name: "dead", URL: "http://127.0.0.1:1/mcp"}, {Name: "ok", URL: ts.URL}}}, nil, slog.Default(), 1*time.Second)
	defer closeAll()
	if len(all) != 3 {
		t.Errorf("ConnectAll should skip the dead server and keep the live one: %d tools", len(all))
	}
}
