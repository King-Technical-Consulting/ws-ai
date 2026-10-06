package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jking323/ws/internal/gateway"
)

func TestRegistryDefsAndPolicies(t *testing.T) {
	r := NewRegistry(ReadBlobTool{}, &WebFetchTool{}, AskUserTool{})
	defs := r.Defs(nil)
	if len(defs) != 3 || defs[0].Name != "ask_user" || defs[2].Name != "web_fetch" {
		t.Errorf("defs should be all tools sorted by name: %v", names(defs))
	}
	if d := r.Defs([]string{"web_fetch"}); len(d) != 1 || d[0].Name != "web_fetch" {
		t.Errorf("allowlist not applied: %v", names(d))
	}
	if d := r.Defs([]string{NoTools}); len(d) != 0 {
		t.Errorf("none should give no tools: %v", names(d))
	}
	if d := r.Defs([]string{"web_fetch", NoTools}); len(d) != 0 {
		t.Errorf("none wins over listed tools: %v", names(d))
	}
	pol := r.Policies(map[string]Policy{"web_fetch": PolicyAsk})
	if pol["web_fetch"] != PolicyAsk || pol["read_blob"] != PolicyAuto {
		t.Errorf("policies = %v", pol)
	}
}

func names(defs []gateway.ToolDef) []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.Name)
	}
	return out
}

func TestWebFetchRefusesPrivateAndStripsHTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head><title>T</title><style>p{}</style></head><body><h1>Hello</h1><script>evil()</script><p>World &amp; more</p></body></html>`))
	}))
	defer srv.Close()
	tool := &WebFetchTool{Client: srv.Client()}

	// loopback is refused even though the server is right here
	res, _ := tool.Call(context.Background(), ToolCtx{}, json.RawMessage(`{"url":"`+srv.URL+`"}`))
	if !res.IsError || !strings.Contains(res.Text, "private") && !strings.Contains(res.Text, "local") {
		t.Errorf("loopback should be refused: %+v", res)
	}
	for _, u := range []string{"http://192.168.1.5/x", "http://10.0.0.1/", "http://100.100.100.100:8180/", "http://ws.home.arpa/", "ftp://example.com/"} {
		res, _ := tool.Call(context.Background(), ToolCtx{}, json.RawMessage(`{"url":"`+u+`"}`))
		if !res.IsError {
			t.Errorf("%s should be refused", u)
		}
	}
	// the html reducer
	txt := htmlToText(`<html><head><title>T</title><style>p{}</style></head><body><h1>Hello</h1><script>evil()</script><p>World &amp; more</p></body></html>`)
	if strings.Contains(txt, "evil") || strings.Contains(txt, "p{}") || !strings.Contains(txt, "Hello") || !strings.Contains(txt, "World & more") {
		t.Errorf("htmlToText = %q", txt)
	}
}

func TestExternalizeStub(t *testing.T) {
	big := strings.Repeat("y", MaxInlineResult+10)
	out, ext := externalize(context.Background(), nil, "t", big)
	if ext || len(out) > MaxInlineResult+200 {
		t.Error("without a blob store, output must be truncated not externalized")
	}
}
