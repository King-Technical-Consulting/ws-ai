// Package github holds the GitHub App integration: installation tokens for
// the egress proxy (so sandboxes push without ever holding a credential)
// and pull-request creation from the worker.
package github

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	gh "github.com/google/go-github/v88/github"
)

// Config is read from the environment.
type Config struct {
	AppID      int64
	PrivateKey []byte // PEM
	Slug       string // for the install URL: https://github.com/apps/<slug>/installations/new
	// APIBase overrides https://api.github.com (tests, GHES).
	APIBase string
}

// LoadKey reads the private key from GITHUB_APP_PRIVATE_KEY (PEM, with
// literal "\n" or base64 accepted) or GITHUB_APP_PRIVATE_KEY_FILE.
func LoadKey(inline, file string) ([]byte, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("github app key file: %w", err)
		}
		return b, nil
	}
	s := strings.TrimSpace(inline)
	if s == "" {
		return nil, nil
	}
	s = strings.ReplaceAll(s, `\n`, "\n")
	if !strings.Contains(s, "-----BEGIN") {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, errors.New("github app key: not PEM and not base64")
		}
		return b, nil
	}
	return []byte(s), nil
}

// App is a configured GitHub App.
type App struct {
	cfg  Config
	apps *ghinstallation.AppsTransport
	http *http.Client

	mu     sync.Mutex
	insts  map[string]*ghinstallation.Transport // key: installation|repo
	byOwnr map[string]cached                    // owner -> installation id
}

type cached struct {
	id int64
	at time.Time
}

// New builds the App. Returns nil, nil when not configured.
func New(cfg Config) (*App, error) {
	if cfg.AppID == 0 || len(cfg.PrivateKey) == 0 {
		return nil, nil
	}
	atr, err := ghinstallation.NewAppsTransport(http.DefaultTransport, cfg.AppID, cfg.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("github app: %w", err)
	}
	if cfg.APIBase != "" {
		atr.BaseURL = strings.TrimRight(cfg.APIBase, "/")
	}
	return &App{cfg: cfg, apps: atr, http: &http.Client{Timeout: 30 * time.Second}, insts: map[string]*ghinstallation.Transport{}, byOwnr: map[string]cached{}}, nil
}

// Slug returns the app slug (may be empty).
func (a *App) Slug() string { return a.cfg.Slug }

// InstallURL is where a user installs the app on their account or org.
func (a *App) InstallURL() string {
	if a.cfg.Slug == "" {
		return ""
	}
	return "https://github.com/apps/" + a.cfg.Slug + "/installations/new"
}

func (a *App) client(tr http.RoundTripper) *gh.Client {
	opts := []gh.ClientOptionsFunc{gh.WithHTTPClient(&http.Client{Transport: tr, Timeout: 30 * time.Second})}
	if a.cfg.APIBase != "" {
		base := strings.TrimRight(a.cfg.APIBase, "/") + "/"
		opts = append(opts, gh.WithURLs(&base, &base))
	}
	c, err := gh.NewClient(opts...)
	if err != nil {
		// Only misconfigured URLs fail; fall back to the public API.
		c, _ = gh.NewClient(gh.WithHTTPClient(&http.Client{Transport: tr, Timeout: 30 * time.Second}))
	}
	return c
}

// Installation is one place the app is installed.
type Installation struct {
	ID        int64  `json:"id"`
	Account   string `json:"account"`
	Type      string `json:"type"` // User | Organization
	Repos     string `json:"repository_selection"`
	HTMLURL   string `json:"html_url"`
	Suspended bool   `json:"suspended"`
}

// Installations lists where the app is installed.
func (a *App) Installations(ctx context.Context) ([]Installation, error) {
	c := a.client(a.apps)
	var out []Installation
	opt := &gh.ListOptions{PerPage: 100}
	for {
		list, resp, err := c.Apps.ListInstallations(ctx, opt)
		if err != nil {
			return nil, fmt.Errorf("github app: list installations: %w", err)
		}
		for _, in := range list {
			i := Installation{ID: in.GetID(), Repos: in.GetRepositorySelection(), HTMLURL: in.GetHTMLURL(), Suspended: in.SuspendedAt != nil}
			if in.Account != nil {
				i.Account = in.Account.GetLogin()
				i.Type = in.Account.GetType()
			}
			out = append(out, i)
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}

// InstallationForOwner finds the installation covering a repo owner.
func (a *App) InstallationForOwner(ctx context.Context, owner string) (int64, error) {
	owner = strings.ToLower(owner)
	a.mu.Lock()
	if c, ok := a.byOwnr[owner]; ok && time.Since(c.at) < 5*time.Minute {
		a.mu.Unlock()
		return c.id, nil
	}
	a.mu.Unlock()
	insts, err := a.Installations(ctx)
	if err != nil {
		return 0, err
	}
	for _, in := range insts {
		if strings.ToLower(in.Account) == owner && !in.Suspended {
			a.mu.Lock()
			a.byOwnr[owner] = cached{id: in.ID, at: time.Now()}
			a.mu.Unlock()
			return in.ID, nil
		}
	}
	return 0, fmt.Errorf("the GitHub App is not installed on %q", owner)
}

func (a *App) transport(installationID int64, repo string) *ghinstallation.Transport {
	key := fmt.Sprintf("%d|%s", installationID, repo)
	a.mu.Lock()
	defer a.mu.Unlock()
	if t, ok := a.insts[key]; ok {
		return t
	}
	t := ghinstallation.NewFromAppsTransport(a.apps, installationID)
	if a.cfg.APIBase != "" {
		t.BaseURL = strings.TrimRight(a.cfg.APIBase, "/")
	}
	if repo != "" {
		t.InstallationTokenOptions = &gh.InstallationTokenOptions{Repositories: []string{repo}}
	}
	a.insts[key] = t
	return t
}

// Token returns a short-lived installation token, scoped to one repo when
// repo is given. ghinstallation caches and refreshes it.
func (a *App) Token(ctx context.Context, installationID int64, repo string) (string, error) {
	return a.transport(installationID, repo).Token(ctx)
}

// ParseRepoURL splits https://github.com/owner/repo(.git) into parts.
func ParseRepoURL(raw string) (owner, repo string, err error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", "", fmt.Errorf("not a repo URL: %q", raw)
	}
	if !strings.EqualFold(u.Host, "github.com") && !strings.EqualFold(u.Host, "www.github.com") {
		return "", "", fmt.Errorf("only github.com repos are supported, got %s", u.Host)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("not an owner/repo URL: %q", raw)
	}
	return parts[0], strings.TrimSuffix(parts[1], ".git"), nil
}

// PullRequest is what CreatePR returns.
type PullRequest struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
}

// CreatePR opens a pull request as the installation.
func (a *App) CreatePR(ctx context.Context, installationID int64, owner, repo, head, base, title, body string, draft bool) (PullRequest, error) {
	c := a.client(a.transport(installationID, repo))
	if base == "" {
		r, _, err := c.Repositories.Get(ctx, owner, repo)
		if err != nil {
			return PullRequest{}, fmt.Errorf("github: repo: %w", err)
		}
		base = r.GetDefaultBranch()
	}
	pr, _, err := c.PullRequests.Create(ctx, owner, repo, &gh.NewPullRequest{
		Title: gh.Ptr(title), Head: gh.Ptr(head), Base: gh.Ptr(base), Body: gh.Ptr(body), Draft: gh.Ptr(draft),
	})
	if err != nil {
		return PullRequest{}, fmt.Errorf("github: create pr: %w", err)
	}
	return PullRequest{Number: pr.GetNumber(), URL: pr.GetHTMLURL()}, nil
}

// DefaultBranch asks GitHub for a repo's default branch.
func (a *App) DefaultBranch(ctx context.Context, installationID int64, owner, repo string) (string, error) {
	c := a.client(a.transport(installationID, repo))
	r, _, err := c.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	return r.GetDefaultBranch(), nil
}

// basic is the Authorization value git expects for a token.
func basic(token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
}
