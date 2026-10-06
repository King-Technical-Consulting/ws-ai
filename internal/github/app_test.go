package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
}

// fakeGitHub answers the three calls the integration makes.
func fakeGitHub(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path+" auth="+authKind(r.Header.Get("Authorization")))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/app/installations":
			_, _ = w.Write([]byte(`[{"id":42,"account":{"login":"jking323","type":"User"},"repository_selection":"selected","html_url":"https://github.com/settings/installations/42"},
			                      {"id":7,"account":{"login":"someorg","type":"Organization"},"repository_selection":"all","suspended_at":"2026-01-01T00:00:00Z"}]`))
		case r.URL.Path == "/app/installations/42/access_tokens":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if repos, _ := body["repositories"].([]any); len(repos) != 1 || repos[0] != "ws" {
				t.Errorf("token request not repo-scoped: %v", body)
			}
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"token":"ghs_test_token","expires_at":"2999-01-01T00:00:00Z"}`))
		case r.URL.Path == "/repos/jking323/ws":
			_, _ = w.Write([]byte(`{"full_name":"jking323/ws","default_branch":"master"}`))
		case r.URL.Path == "/repos/jking323/ws/pulls" && r.Method == "POST":
			var pr map[string]any
			_ = json.NewDecoder(r.Body).Decode(&pr)
			if pr["head"] != "ws/fix-thing" || pr["base"] != "master" || pr["title"] != "Fix thing" {
				t.Errorf("pr body = %v", pr)
			}
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"number":12,"html_url":"https://github.com/jking323/ws/pull/12"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	return srv, &seen
}

func authKind(h string) string {
	switch {
	case strings.HasPrefix(h, "Bearer ") && strings.Count(h, ".") == 2:
		return "app-jwt"
	case strings.HasPrefix(h, "token ghs_"), strings.HasPrefix(h, "Bearer ghs_"):
		return "installation"
	case h == "":
		return "none"
	}
	return "other"
}

func TestAppFlow(t *testing.T) {
	srv, seen := fakeGitHub(t)
	defer srv.Close()
	app, err := New(Config{AppID: 1234, PrivateKey: testKey(t), Slug: "ws-dev", APIBase: srv.URL})
	if err != nil || app == nil {
		t.Fatal(err)
	}
	if app.InstallURL() != "https://github.com/apps/ws-dev/installations/new" {
		t.Errorf("install url = %q", app.InstallURL())
	}
	ctx := context.Background()

	id, err := app.InstallationForOwner(ctx, "JKing323")
	if err != nil || id != 42 {
		t.Fatalf("installation = %d, %v", id, err)
	}
	if _, err := app.InstallationForOwner(ctx, "someorg"); err == nil {
		t.Errorf("suspended installation must not be used")
	}
	if _, err := app.InstallationForOwner(ctx, "nobody"); err == nil {
		t.Errorf("unknown owner must error")
	}

	tok, err := app.Token(ctx, 42, "ws")
	if err != nil || tok != "ghs_test_token" {
		t.Fatalf("token = %q, %v", tok, err)
	}
	// git expects "x-access-token:<token>" as Basic auth. Computed here rather
	// than pasted so secret scanners don't flag a fake token.
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+tok))
	if basic(tok) != want {
		t.Errorf("basic = %q, want %q", basic(tok), want)
	}

	pr, err := app.CreatePR(ctx, 42, "jking323", "ws", "ws/fix-thing", "", "Fix thing", "body", false)
	if err != nil || pr.Number != 12 || !strings.HasSuffix(pr.URL, "/pull/12") {
		t.Fatalf("pr = %+v, %v", pr, err)
	}

	// The app JWT is used only for app-level calls; repo calls use the
	// installation token.
	joined := strings.Join(*seen, "\n")
	if !strings.Contains(joined, "GET /app/installations auth=app-jwt") {
		t.Errorf("installations not fetched with the app JWT:\n%s", joined)
	}
	if !strings.Contains(joined, "POST /app/installations/42/access_tokens auth=app-jwt") {
		t.Errorf("token not minted with the app JWT:\n%s", joined)
	}
	if !strings.Contains(joined, "POST /repos/jking323/ws/pulls auth=installation") {
		t.Errorf("PR not created with the installation token:\n%s", joined)
	}
}

func TestParseRepoURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/jking323/ws":         "jking323/ws",
		"https://github.com/jking323/ws.git":     "jking323/ws",
		"https://www.github.com/a/b/tree/main/x": "a/b",
		"http://github.com/a/b/":                 "a/b",
	} {
		o, r, err := ParseRepoURL(in)
		if err != nil || o+"/"+r != want {
			t.Errorf("ParseRepoURL(%q) = %s/%s, %v", in, o, r, err)
		}
	}
	for _, bad := range []string{"https://gitlab.com/a/b", "https://github.com/onlyowner", "nonsense", ""} {
		if _, _, err := ParseRepoURL(bad); err == nil {
			t.Errorf("ParseRepoURL(%q) should fail", bad)
		}
	}
}

func TestLoadKey(t *testing.T) {
	pemKey := testKey(t)
	if k, err := LoadKey(string(pemKey), ""); err != nil || strings.TrimSpace(string(k)) != strings.TrimSpace(string(pemKey)) {
		t.Errorf("pem passthrough failed: %v", err)
	}
	escaped := strings.ReplaceAll(string(pemKey), "\n", `\n`)
	if k, err := LoadKey(escaped, ""); err != nil || strings.TrimSpace(string(k)) != strings.TrimSpace(string(pemKey)) {
		t.Errorf("escaped newlines not restored: %v", err)
	}
	if k, err := LoadKey("", ""); err != nil || k != nil {
		t.Errorf("empty must be nil, nil")
	}
	if _, err := LoadKey("not a key", ""); err == nil {
		t.Errorf("garbage must error")
	}
	if app, err := New(Config{}); err != nil || app != nil {
		t.Errorf("unconfigured New must be nil, nil")
	}
}
