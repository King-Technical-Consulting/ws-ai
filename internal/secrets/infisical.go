// Package secrets loads ws's secrets from Infisical at boot so .env holds
// only the bootstrap machine identity. It talks to the REST API directly:
// two endpoints, no SDK (the official Go SDK pulls in the Google and AWS
// auth trees for identity methods we don't use).
package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// InfisicalConfig is read from the environment (the bootstrap).
type InfisicalConfig struct {
	SiteURL      string // default https://app.infisical.com
	ClientID     string // INFISICAL_CLIENT_ID (machine identity, universal auth)
	ClientSecret string // INFISICAL_CLIENT_SECRET
	ProjectID    string // INFISICAL_PROJECT_ID
	Environment  string // INFISICAL_ENV, e.g. prod
	SecretPath   string // INFISICAL_PATH, default /
	Overwrite    bool   // INFISICAL_OVERWRITE=1: Infisical wins over values already in the environment
}

// FromEnv reads the bootstrap variables. Enabled reports whether the three
// required ones are present.
func FromEnv() (InfisicalConfig, bool) {
	c := InfisicalConfig{
		SiteURL:      strings.TrimRight(envOr("INFISICAL_SITE_URL", "https://app.infisical.com"), "/"),
		ClientID:     os.Getenv("INFISICAL_CLIENT_ID"),
		ClientSecret: os.Getenv("INFISICAL_CLIENT_SECRET"),
		ProjectID:    os.Getenv("INFISICAL_PROJECT_ID"),
		Environment:  envOr("INFISICAL_ENV", "prod"),
		SecretPath:   envOr("INFISICAL_PATH", "/"),
		Overwrite:    os.Getenv("INFISICAL_OVERWRITE") == "1",
	}
	return c, c.ClientID != "" && c.ClientSecret != "" && c.ProjectID != ""
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// Client fetches secrets with a machine identity.
type Client struct {
	cfg  InfisicalConfig
	http *http.Client
	log  *slog.Logger

	token    string
	tokenExp time.Time
}

// NewClient builds a client.
func NewClient(cfg InfisicalConfig, log *slog.Logger) *Client {
	return &Client{cfg: cfg, http: &http.Client{Timeout: 20 * time.Second}, log: log}
}

func (c *Client) login(ctx context.Context) error {
	if c.token != "" && time.Until(c.tokenExp) > 2*time.Minute {
		return nil
	}
	body, _ := json.Marshal(map[string]string{"clientId": c.cfg.ClientID, "clientSecret": c.cfg.ClientSecret})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.SiteURL+"/api/v1/auth/universal-auth/login", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("infisical login: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("infisical login: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		AccessToken string `json:"accessToken"`
		ExpiresIn   int    `json:"expiresIn"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.AccessToken == "" {
		return fmt.Errorf("infisical login: bad response")
	}
	c.token = out.AccessToken
	if out.ExpiresIn <= 0 {
		out.ExpiresIn = 3600
	}
	c.tokenExp = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)
	return nil
}

// Secret is one key/value from the project.
type Secret struct {
	Key   string
	Value string
	Path  string
}

// List returns the secrets at the configured path (recursive, references
// expanded, imports included).
func (c *Client) List(ctx context.Context) ([]Secret, error) {
	if err := c.login(ctx); err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("projectId", c.cfg.ProjectID)
	q.Set("environment", c.cfg.Environment)
	q.Set("secretPath", c.cfg.SecretPath)
	q.Set("expandSecretReferences", "true")
	q.Set("recursive", "true")
	q.Set("includeImports", "true")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.SiteURL+"/api/v4/secrets?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("infisical list: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 {
		c.token = "" // expired early; caller retries
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("infisical list: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		Secrets []struct {
			Key   string `json:"secretKey"`
			Value string `json:"secretValue"`
			Path  string `json:"secretPath"`
		} `json:"secrets"`
		Imports []struct {
			Secrets []struct {
				Key   string `json:"secretKey"`
				Value string `json:"secretValue"`
				Path  string `json:"secretPath"`
			} `json:"secrets"`
		} `json:"imports"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("infisical list: decode: %w", err)
	}
	// Imports first, then the project's own secrets, which win on the same
	// key; one entry per key in that order.
	index := map[string]int{}
	var secrets []Secret
	add := func(k, v, p string) {
		if i, ok := index[k]; ok {
			secrets[i] = Secret{Key: k, Value: v, Path: p}
			return
		}
		index[k] = len(secrets)
		secrets = append(secrets, Secret{Key: k, Value: v, Path: p})
	}
	for _, im := range out.Imports {
		for _, s := range im.Secrets {
			add(s.Key, s.Value, s.Path)
		}
	}
	for _, s := range out.Secrets {
		add(s.Key, s.Value, s.Path)
	}
	return secrets, nil
}

// Apply exports secrets into the process environment. Unless Overwrite is
// set, variables already present (from .env or the container env) win, so
// a box can pin a value locally. Returns the keys set.
func (c *Client) Apply(ctx context.Context) ([]string, error) {
	secrets, err := c.List(ctx)
	if err != nil {
		return nil, err
	}
	var set []string
	for _, s := range secrets {
		if s.Key == "" || !validEnvName(s.Key) {
			continue
		}
		if _, exists := os.LookupEnv(s.Key); exists && !c.cfg.Overwrite {
			continue
		}
		if err := os.Setenv(s.Key, s.Value); err == nil {
			set = append(set, s.Key)
		}
	}
	return set, nil
}

func validEnvName(k string) bool {
	for i, r := range k {
		switch {
		case r == '_', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return k != ""
}

// Load is the boot hook: if the bootstrap variables are present, pull the
// project's secrets into the environment before config is parsed, and keep
// refreshing them so rotated provider keys are picked up by the registry's
// periodic reload. Never fatal: a box that can't reach Infisical runs on
// whatever .env holds, loudly.
func Load(ctx context.Context, log *slog.Logger) {
	cfg, ok := FromEnv()
	if !ok {
		return
	}
	c := NewClient(cfg, log)
	set, err := c.Apply(ctx)
	if err != nil {
		log.Error("infisical: could not load secrets; continuing with the local environment", "site", cfg.SiteURL, "env", cfg.Environment, "err", err)
		return
	}
	log.Info("infisical: secrets loaded", "site", cfg.SiteURL, "project", cfg.ProjectID, "env", cfg.Environment, "path", cfg.SecretPath, "count", len(set))
	go func() {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				// Refreshes re-apply with Overwrite so rotations land; .env pins
				// still win on the first load, which is what "pin" means here.
				cc := NewClient(InfisicalConfig{SiteURL: cfg.SiteURL, ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, ProjectID: cfg.ProjectID, Environment: cfg.Environment, SecretPath: cfg.SecretPath, Overwrite: true}, log)
				cc.token, cc.tokenExp = c.token, c.tokenExp
				if _, err := cc.Apply(ctx); err != nil {
					log.Warn("infisical: refresh failed", "err", err)
				}
			}
		}
	}()
}
