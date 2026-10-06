package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/store"
)

// Host ties the App to ws projects and users: it resolves which
// installation covers a project's repo, hands the egress proxy a
// credential for that repo, and opens PRs. It also implements the sandbox
// package's GitHost interface. FallbackToken (GITHUB_TOKEN) is used when
// no App is configured.
type Host struct {
	App           *App // may be nil
	DB            *store.DB
	FallbackToken string
	Log           *slog.Logger

	mu    sync.Mutex
	creds map[uuid.UUID]credCache // project -> credential
}

type credCache struct {
	value string
	at    time.Time
}

// Configured reports whether PRs can be opened (App present).
func (h *Host) Configured() bool { return h != nil && h.App != nil }

// Identity returns the git author for a user.
func (h *Host) Identity(ctx context.Context, userID uuid.UUID) (string, string, error) {
	u, err := h.DB.GetUserByID(ctx, userID)
	if err != nil {
		return "", "", err
	}
	name := u.DisplayName
	if name == "" {
		name = u.Email
	}
	return name, u.Email, nil
}

// installationFor returns the installation id for a project, resolving and
// recording it from the repo owner on first use.
func (h *Host) installationFor(ctx context.Context, proj store.Project) (int64, string, string, error) {
	if proj.RepoUrl == nil || *proj.RepoUrl == "" {
		return 0, "", "", errors.New("project has no repo_url")
	}
	owner, repo, err := ParseRepoURL(*proj.RepoUrl)
	if err != nil {
		return 0, "", "", err
	}
	if proj.GithubInstallationID != nil && *proj.GithubInstallationID > 0 {
		return *proj.GithubInstallationID, owner, repo, nil
	}
	id, err := h.App.InstallationForOwner(ctx, owner)
	if err != nil {
		return 0, owner, repo, err
	}
	_ = h.DB.SetProjectInstallation(ctx, store.SetProjectInstallationParams{ID: proj.ID, GithubInstallationID: &id})
	return id, owner, repo, nil
}

// ResolveProject records the installation for a project if the App covers
// its repo. Best effort; returns the id or 0.
func (h *Host) ResolveProject(ctx context.Context, proj store.Project) int64 {
	if !h.Configured() {
		return 0
	}
	id, _, _, err := h.installationFor(ctx, proj)
	if err != nil {
		return 0
	}
	return id
}

// Credential returns the Authorization header value the egress proxy
// should inject for github.com on behalf of a project's sandbox, or "".
// Installation tokens are repo-scoped and last an hour; cached briefly.
func (h *Host) Credential(ctx context.Context, projectID uuid.UUID) string {
	h.mu.Lock()
	if c, ok := h.creds[projectID]; ok && time.Since(c.at) < 2*time.Minute {
		h.mu.Unlock()
		return c.value
	}
	h.mu.Unlock()

	value := ""
	if h.Configured() {
		if proj, err := h.DB.GetProject(ctx, projectID); err == nil {
			if inst, _, repo, err := h.installationFor(ctx, proj); err == nil {
				if tok, err := h.App.Token(ctx, inst, repo); err == nil {
					value = basic(tok)
				} else if h.Log != nil {
					h.Log.Warn("github installation token", "project", projectID, "err", err)
				}
			} else if h.Log != nil {
				h.Log.Info("github: no installation for project; falling back", "project", projectID, "err", err)
			}
		}
	}
	if value == "" && h.FallbackToken != "" {
		value = basic(h.FallbackToken)
	}
	h.mu.Lock()
	if h.creds == nil {
		h.creds = map[uuid.UUID]credCache{}
	}
	h.creds[projectID] = credCache{value: value, at: time.Now()}
	h.mu.Unlock()
	return value
}

// OpenPR creates a pull request for the project's repo.
func (h *Host) OpenPR(ctx context.Context, projectID uuid.UUID, head, base, title, body string, draft bool) (string, int, error) {
	if !h.Configured() {
		return "", 0, errors.New("GitHub App not configured")
	}
	proj, err := h.DB.GetProject(ctx, projectID)
	if err != nil {
		return "", 0, err
	}
	inst, owner, repo, err := h.installationFor(ctx, proj)
	if err != nil {
		return "", 0, fmt.Errorf("github: %w", err)
	}
	pr, err := h.App.CreatePR(ctx, inst, owner, repo, head, base, title, body, draft)
	if err != nil {
		return "", 0, err
	}
	return pr.URL, pr.Number, nil
}
