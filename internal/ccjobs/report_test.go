package ccjobs

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestReporter: handles go to ws as JSON with the key as a bearer; a
// rejection surfaces the server's message; no ws configured means nil.
func TestReporter(t *testing.T) {
	var got []struct {
		Method, Path, Auth, Body string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, struct{ Method, Path, Auth, Body string }{r.Method, r.URL.Path, r.Header.Get("Authorization"), string(b)})
		if strings.HasSuffix(r.URL.Path, "/nopenopeno") {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"no such job"}`))
			return
		}
		w.WriteHeader(201)
	}))
	defer srv.Close()

	tmp := t.TempDir()
	keyFile := filepath.Join(tmp, "ws.key")
	if err := os.WriteFile(keyFile, []byte("ws_testkey\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WSJ_WS_URL", "")
	t.Setenv("WSJ_WS_KEY", "")

	if r, err := (&Config{}).Reporter(); r != nil || err != nil {
		t.Fatalf("unconfigured: %v %v", r, err)
	}
	if _, err := (&Config{WS: WSConfig{URL: srv.URL}}).Reporter(); err == nil {
		t.Error("url without a key must be an error")
	}
	if _, err := (&Config{WS: WSConfig{URL: "ftp://x", KeyFile: keyFile}}).Reporter(); err == nil {
		t.Error("bad scheme must be an error")
	}
	r, err := (&Config{WS: WSConfig{URL: srv.URL + "/", KeyFile: keyFile}}).Reporter()
	if err != nil {
		t.Fatal(err)
	}
	if r.URL != srv.URL || r.Key != "ws_testkey" {
		t.Errorf("reporter = %+v", r)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	rep := ReportFor(Handle{ID: "abc123defg", Target: "homelab", Session: "subscription", Window: "@7", Cwd: "/home/j/proj"}, Job{Model: "opus", Created: started}, true)
	if err := r.Launched(ctx, rep); err != nil {
		t.Fatal(err)
	}
	if err := r.Killed(ctx, "abc123defg"); err != nil {
		t.Fatal(err)
	}
	if err := r.Killed(ctx, "nopenopeno"); err == nil || !strings.Contains(err.Error(), "no such job") {
		t.Errorf("rejected delete must carry the server message: %v", err)
	}
	if err := r.Killed(ctx, "../x"); err == nil {
		t.Error("bad id must not be sent")
	}
	if len(got) != 3 {
		t.Fatalf("requests = %+v", got)
	}
	if got[0].Method != "POST" || got[0].Path != "/api/jobs/cc" || got[0].Auth != "Bearer ws_testkey" {
		t.Errorf("launch request = %+v", got[0])
	}
	var sent Report
	if err := json.Unmarshal([]byte(got[0].Body), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.ID != "abc123defg" || sent.Target != "homelab" || sent.Window != "@7" || sent.Lane != LaneSubscription || sent.Model != "opus" || sent.Status != "dead" || !sent.Started.Equal(started) {
		t.Errorf("sent = %+v", sent)
	}
	if strings.Contains(got[0].Body, "prompt") {
		t.Errorf("a report must never carry a prompt: %s", got[0].Body)
	}
	if got[1].Method != "DELETE" || got[1].Path != "/api/jobs/cc/abc123defg" {
		t.Errorf("kill request = %+v", got[1])
	}

	// Env overrides the file.
	t.Setenv("WSJ_WS_URL", srv.URL)
	t.Setenv("WSJ_WS_KEY", "ws_env")
	r, err = (&Config{}).Reporter()
	if err != nil || r.Key != "ws_env" {
		t.Errorf("env reporter = %+v %v", r, err)
	}
}
