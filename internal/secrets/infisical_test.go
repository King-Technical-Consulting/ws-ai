package secrets

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func fake(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/universal-auth/login":
			var in map[string]string
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in["clientId"] != "cid" || in["clientSecret"] != "csec" {
				w.WriteHeader(401)
				return
			}
			_, _ = w.Write([]byte(`{"accessToken":"tok123","expiresIn":3600,"tokenType":"Bearer"}`))
		case "/api/v4/secrets":
			if r.Header.Get("Authorization") != "Bearer tok123" {
				w.WriteHeader(401)
				return
			}
			q := r.URL.Query()
			if q.Get("projectId") != "proj" || q.Get("environment") != "prod" || q.Get("recursive") != "true" || q.Get("expandSecretReferences") != "true" {
				t.Errorf("query = %v", q)
			}
			_, _ = w.Write([]byte(`{"secrets":[
				{"secretKey":"OPENROUTER_API_KEY","secretValue":"sk-or-test","secretPath":"/"},
				{"secretKey":"GITHUB_APP_PRIVATE_KEY","secretValue":"-----BEGIN RSA PRIVATE KEY-----\nabc\n-----END RSA PRIVATE KEY-----","secretPath":"/github"},
				{"secretKey":"bad-name!","secretValue":"x","secretPath":"/"},
				{"secretKey":"WS_PINNED","secretValue":"from-infisical","secretPath":"/"}
			],"imports":[{"secretPath":"/","environment":"shared","secrets":[{"secretKey":"IMPORTED","secretValue":"base","secretPath":"/"},{"secretKey":"OPENROUTER_API_KEY","secretValue":"overridden-by-project","secretPath":"/"}]}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestApply(t *testing.T) {
	srv := fake(t)
	defer srv.Close()
	for _, k := range []string{"OPENROUTER_API_KEY", "GITHUB_APP_PRIVATE_KEY", "IMPORTED", "WS_PINNED"} {
		os.Unsetenv(k)
		defer os.Unsetenv(k)
	}
	t.Setenv("WS_PINNED", "local-pin")

	c := NewClient(InfisicalConfig{SiteURL: srv.URL, ClientID: "cid", ClientSecret: "csec", ProjectID: "proj", Environment: "prod", SecretPath: "/"}, slog.Default())
	set, err := c.Apply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(set) != 3 {
		t.Errorf("set = %v", set)
	}
	if os.Getenv("OPENROUTER_API_KEY") != "sk-or-test" {
		t.Errorf("project secret must override the import, got %q", os.Getenv("OPENROUTER_API_KEY"))
	}
	if os.Getenv("IMPORTED") != "base" {
		t.Errorf("imported secret missing")
	}
	if os.Getenv("WS_PINNED") != "local-pin" {
		t.Errorf("existing env must win without Overwrite, got %q", os.Getenv("WS_PINNED"))
	}
	if v := os.Getenv("GITHUB_APP_PRIVATE_KEY"); v == "" || v[:10] != "-----BEGIN" {
		t.Errorf("multiline value mangled: %q", v)
	}
	if _, ok := os.LookupEnv("bad-name!"); ok {
		t.Errorf("invalid env names must be skipped")
	}

	// Overwrite flips the pin.
	c2 := NewClient(InfisicalConfig{SiteURL: srv.URL, ClientID: "cid", ClientSecret: "csec", ProjectID: "proj", Environment: "prod", SecretPath: "/", Overwrite: true}, slog.Default())
	if _, err := c2.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("WS_PINNED") != "from-infisical" {
		t.Errorf("Overwrite must apply, got %q", os.Getenv("WS_PINNED"))
	}
}

func TestBadCredentials(t *testing.T) {
	srv := fake(t)
	defer srv.Close()
	c := NewClient(InfisicalConfig{SiteURL: srv.URL, ClientID: "cid", ClientSecret: "wrong", ProjectID: "proj", Environment: "prod"}, slog.Default())
	if _, err := c.Apply(context.Background()); err == nil {
		t.Fatal("bad credentials must error")
	}
}

func TestFromEnv(t *testing.T) {
	for _, k := range []string{"INFISICAL_CLIENT_ID", "INFISICAL_CLIENT_SECRET", "INFISICAL_PROJECT_ID", "INFISICAL_ENV", "INFISICAL_SITE_URL"} {
		os.Unsetenv(k)
	}
	if _, ok := FromEnv(); ok {
		t.Fatal("must be disabled without bootstrap vars")
	}
	t.Setenv("INFISICAL_CLIENT_ID", "a")
	t.Setenv("INFISICAL_CLIENT_SECRET", "b")
	t.Setenv("INFISICAL_PROJECT_ID", "c")
	cfg, ok := FromEnv()
	if !ok || cfg.Environment != "prod" || cfg.SiteURL != "https://app.infisical.com" || cfg.SecretPath != "/" {
		t.Errorf("defaults: %+v %v", cfg, ok)
	}
}
