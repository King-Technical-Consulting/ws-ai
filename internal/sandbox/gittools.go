package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/agent"
	"github.com/jking323/ws/internal/gateway"
)

// GitHost is what the git tools need from the GitHub integration: who the
// committing user is, and how to open a PR for a project. Pushes go through
// the egress proxy, which injects the installation token; nothing here
// touches credentials.
type GitHost interface {
	// Identity returns the git author for a user.
	Identity(ctx context.Context, userID uuid.UUID) (name, email string, err error)
	// OpenPR creates a pull request for the project's repo from head onto base.
	OpenPR(ctx context.Context, projectID uuid.UUID, head, base, title, body string, draft bool) (url string, number int, err error)
	// Configured reports whether PR creation is possible at all.
	Configured() bool
}

// GitTools returns git_status, git_diff, git_commit, git_push and open_pr.
func GitTools(m *Manager, resolve Resolver, host GitHost) []agent.Tool {
	return []agent.Tool{
		&gitStatusTool{m, resolve}, &gitDiffTool{m, resolve},
		&gitCommitTool{m, resolve, host}, &gitPushTool{m, resolve}, &openPRTool{m, resolve, host},
	}
}

// GitToolNames lists them, for ToolAllow lists.
var GitToolNames = []string{"git_status", "git_diff", "git_commit", "git_push", "open_pr"}

func gitExec(ctx context.Context, m *Manager, sb *Sandbox, timeout time.Duration, args ...string) (ExecResult, error) {
	return m.Exec(ctx, sb, ExecOptions{Cmd: append([]string{"git"}, args...), Timeout: timeout, MaxOutput: 256 << 10})
}

func gitFail(res ExecResult, what string) (agent.Result, error) {
	msg := strings.TrimSpace(res.Stderr)
	if msg == "" {
		msg = strings.TrimSpace(res.Stdout)
	}
	return errResult(fmt.Errorf("%s failed (exit %d): %s", what, res.ExitCode, msg))
}

// ---- git_status ----

type gitStatusTool struct {
	m       *Manager
	resolve Resolver
}

func (t *gitStatusTool) Def() gateway.ToolDef {
	return gateway.ToolDef{Name: "git_status", Description: "Show the working tree status: current branch, upstream, and changed/untracked files.", InputSchema: schema(`{"type":"object","properties":{}}`)}
}
func (t *gitStatusTool) DefaultPolicy() agent.Policy { return agent.PolicyAuto }
func (t *gitStatusTool) Idempotent() bool            { return true }
func (t *gitStatusTool) Call(ctx context.Context, tc agent.ToolCtx, _ json.RawMessage) (agent.Result, error) {
	sb, err := t.resolve(ctx, tc)
	if err != nil {
		return errResult(err)
	}
	res, err := gitExec(ctx, t.m, sb, 30*time.Second, "status", "--short", "--branch")
	if err != nil {
		return errResult(err)
	}
	if res.ExitCode != 0 {
		return gitFail(res, "git status")
	}
	out := strings.TrimSpace(res.Stdout)
	if lg, err := gitExec(ctx, t.m, sb, 20*time.Second, "log", "--oneline", "-5"); err == nil && lg.ExitCode == 0 {
		out += "\n\nrecent commits:\n" + strings.TrimSpace(lg.Stdout)
	}
	return textResult(out)
}

// ---- git_diff ----

type gitDiffTool struct {
	m       *Manager
	resolve Resolver
}

func (t *gitDiffTool) Def() gateway.ToolDef {
	return gateway.ToolDef{
		Name:        "git_diff",
		Description: "Show the diff of uncommitted changes (working tree vs HEAD by default). Pass staged=true for the index only, or a ref/range like \"main...HEAD\".",
		InputSchema: schema(`{"type":"object","properties":{"staged":{"type":"boolean"},"ref":{"type":"string","description":"A commit, branch, or range to diff against"},"path":{"type":"string","description":"Limit to a path"}}}`),
	}
}
func (t *gitDiffTool) DefaultPolicy() agent.Policy { return agent.PolicyAuto }
func (t *gitDiffTool) Idempotent() bool            { return true }
func (t *gitDiffTool) Call(ctx context.Context, tc agent.ToolCtx, args json.RawMessage) (agent.Result, error) {
	var in struct {
		Staged bool   `json:"staged"`
		Ref    string `json:"ref"`
		Path   string `json:"path"`
	}
	_ = json.Unmarshal(args, &in)
	sb, err := t.resolve(ctx, tc)
	if err != nil {
		return errResult(err)
	}
	a := []string{"diff", "--stat", "-p", "--no-color"}
	if in.Staged {
		a = append(a, "--staged")
	}
	if in.Ref != "" {
		if strings.HasPrefix(in.Ref, "-") {
			return errResult(errors.New("ref must not start with -"))
		}
		a = append(a, in.Ref)
	} else if !in.Staged {
		a = append(a, "HEAD")
	}
	if in.Path != "" {
		p, err := resolvePath(in.Path)
		if err != nil {
			return errResult(err)
		}
		a = append(a, "--", p)
	}
	res, err := gitExec(ctx, t.m, sb, 60*time.Second, a...)
	if err != nil {
		return errResult(err)
	}
	if res.ExitCode != 0 {
		return gitFail(res, "git diff")
	}
	if strings.TrimSpace(res.Stdout) == "" {
		// Untracked files don't show in a diff; say so instead of "no changes".
		if ut, err := gitExec(ctx, t.m, sb, 20*time.Second, "ls-files", "--others", "--exclude-standard"); err == nil && strings.TrimSpace(ut.Stdout) != "" {
			return textResult("no changes to tracked files; untracked (not in the diff until added):\n" + strings.TrimSpace(ut.Stdout))
		}
		return textResult("no changes")
	}
	return textResult(res.Stdout)
}

// ---- git_commit ----

type gitCommitTool struct {
	m       *Manager
	resolve Resolver
	host    GitHost
}

func (t *gitCommitTool) Def() gateway.ToolDef {
	return gateway.ToolDef{
		Name:        "git_commit",
		Description: "Stage files and commit. Stages everything (git add -A) unless paths are given. The user is the author; ws is the committer. Does not push.",
		InputSchema: schema(`{"type":"object","properties":{"message":{"type":"string"},"paths":{"type":"array","items":{"type":"string"},"description":"Paths to stage; default all changes"}},"required":["message"]}`),
	}
}
func (t *gitCommitTool) DefaultPolicy() agent.Policy { return agent.PolicyAuto }
func (t *gitCommitTool) Idempotent() bool            { return false }
func (t *gitCommitTool) Call(ctx context.Context, tc agent.ToolCtx, args json.RawMessage) (agent.Result, error) {
	var in struct {
		Message string   `json:"message"`
		Paths   []string `json:"paths"`
	}
	if err := json.Unmarshal(args, &in); err != nil || strings.TrimSpace(in.Message) == "" {
		return errResult(errors.New("message is required"))
	}
	sb, err := t.resolve(ctx, tc)
	if err != nil {
		return errResult(err)
	}
	add := []string{"add", "-A"}
	if len(in.Paths) > 0 {
		add = []string{"add", "--"}
		for _, p := range in.Paths {
			rp, err := resolvePath(p)
			if err != nil {
				return errResult(err)
			}
			add = append(add, rp)
		}
	}
	if res, err := gitExec(ctx, t.m, sb, 60*time.Second, add...); err != nil {
		return errResult(err)
	} else if res.ExitCode != 0 {
		return gitFail(res, "git add")
	}
	name, email := "ws user", "ws@localhost"
	if t.host != nil {
		if n, e, err := t.host.Identity(ctx, tc.UserID); err == nil {
			name, email = n, e
		}
	}
	env := []string{
		"GIT_AUTHOR_NAME=" + name, "GIT_AUTHOR_EMAIL=" + email,
		"GIT_COMMITTER_NAME=ws", "GIT_COMMITTER_EMAIL=ws@localhost",
	}
	res, err := t.m.Exec(ctx, sb, ExecOptions{Cmd: []string{"git", "commit", "-q", "-m", in.Message}, Env: env, Timeout: 60 * time.Second})
	if err != nil {
		return errResult(err)
	}
	if res.ExitCode != 0 {
		if strings.Contains(res.Stdout+res.Stderr, "nothing to commit") {
			return textResult("nothing to commit")
		}
		return gitFail(res, "git commit")
	}
	show, _ := gitExec(ctx, t.m, sb, 20*time.Second, "log", "-1", "--stat", "--format=%h %s")
	return agent.Result{Text: strings.TrimSpace(show.Stdout), Data: map[string]any{"author": name}}, nil
}

// ---- git_push ----

type gitPushTool struct {
	m       *Manager
	resolve Resolver
}

var branchRe = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

func (t *gitPushTool) Def() gateway.ToolDef {
	return gateway.ToolDef{
		Name:        "git_push",
		Description: "Push the current branch to origin (sets upstream). Credentials are injected by the platform; the repo must be one the GitHub App is installed on. Asks the user first.",
		InputSchema: schema(`{"type":"object","properties":{"branch":{"type":"string","description":"Branch to push; default current"},"force":{"type":"boolean","description":"Force-with-lease"}}}`),
	}
}
func (t *gitPushTool) DefaultPolicy() agent.Policy { return agent.PolicyAsk }
func (t *gitPushTool) Idempotent() bool            { return false }
func (t *gitPushTool) Call(ctx context.Context, tc agent.ToolCtx, args json.RawMessage) (agent.Result, error) {
	var in struct {
		Branch string `json:"branch"`
		Force  bool   `json:"force"`
	}
	_ = json.Unmarshal(args, &in)
	sb, err := t.resolve(ctx, tc)
	if err != nil {
		return errResult(err)
	}
	branch := in.Branch
	if branch == "" {
		cur, err := gitExec(ctx, t.m, sb, 20*time.Second, "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil || cur.ExitCode != 0 {
			return errResult(errors.New("could not determine the current branch"))
		}
		branch = strings.TrimSpace(cur.Stdout)
	}
	if !branchRe.MatchString(branch) || branch == "HEAD" {
		return errResult(fmt.Errorf("bad branch name %q", branch))
	}
	a := []string{"push", "-u", "origin", branch}
	if in.Force {
		a = []string{"push", "--force-with-lease", "-u", "origin", branch}
	}
	res, err := gitExec(ctx, t.m, sb, 3*time.Minute, a...)
	if err != nil {
		return errResult(err)
	}
	if res.ExitCode != 0 {
		return gitFail(res, "git push")
	}
	return agent.Result{Text: "pushed " + branch + " to origin\n" + strings.TrimSpace(res.Stderr), Data: map[string]any{"branch": branch}}, nil
}

// ---- open_pr ----

type openPRTool struct {
	m       *Manager
	resolve Resolver
	host    GitHost
}

func (t *openPRTool) Def() gateway.ToolDef {
	return gateway.ToolDef{
		Name:        "open_pr",
		Description: "Push the current branch (creating a ws/<slug> branch first if on the default branch) and open a GitHub pull request. Asks the user first. Returns the PR URL.",
		InputSchema: schema(`{"type":"object","properties":{"title":{"type":"string"},"body":{"type":"string","description":"Markdown body"},"base":{"type":"string","description":"Base branch; default the repo's default branch"},"branch":{"type":"string","description":"Head branch name; default ws/<slug of title>"},"draft":{"type":"boolean"}},"required":["title"]}`),
	}
}
func (t *openPRTool) DefaultPolicy() agent.Policy { return agent.PolicyAsk }
func (t *openPRTool) Idempotent() bool            { return false }
func (t *openPRTool) Call(ctx context.Context, tc agent.ToolCtx, args json.RawMessage) (agent.Result, error) {
	var in struct {
		Title  string `json:"title"`
		Body   string `json:"body"`
		Base   string `json:"base"`
		Branch string `json:"branch"`
		Draft  bool   `json:"draft"`
	}
	if err := json.Unmarshal(args, &in); err != nil || strings.TrimSpace(in.Title) == "" {
		return errResult(errors.New("title is required"))
	}
	if t.host == nil || !t.host.Configured() {
		return errResult(errors.New("the GitHub App is not configured on this ws; push with git_push and open the PR by hand"))
	}
	sb, err := t.resolve(ctx, tc)
	if err != nil {
		return errResult(err)
	}
	// Which branch are we on, and what's the default?
	cur, err := gitExec(ctx, t.m, sb, 20*time.Second, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || cur.ExitCode != 0 {
		return errResult(errors.New("could not determine the current branch"))
	}
	current := strings.TrimSpace(cur.Stdout)
	def := in.Base
	if def == "" {
		if d, err := gitExec(ctx, t.m, sb, 20*time.Second, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil && d.ExitCode == 0 {
			def = strings.TrimPrefix(strings.TrimSpace(d.Stdout), "origin/")
		}
	}
	head := in.Branch
	if head == "" {
		head = current
		if current == def || current == "HEAD" {
			head = "ws/" + slug(in.Title)
		}
	}
	if !branchRe.MatchString(head) {
		return errResult(fmt.Errorf("bad branch name %q", head))
	}
	if head != current {
		if res, err := gitExec(ctx, t.m, sb, 20*time.Second, "checkout", "-B", head); err != nil || res.ExitCode != 0 {
			return errResult(fmt.Errorf("could not create branch %s", head))
		}
	}
	// Anything uncommitted goes in a commit so the PR has it.
	if st, err := gitExec(ctx, t.m, sb, 20*time.Second, "status", "--porcelain"); err == nil && strings.TrimSpace(st.Stdout) != "" {
		name, email := "ws user", "ws@localhost"
		if n, e, err := t.host.Identity(ctx, tc.UserID); err == nil {
			name, email = n, e
		}
		_, _ = gitExec(ctx, t.m, sb, 30*time.Second, "add", "-A")
		res, err := t.m.Exec(ctx, sb, ExecOptions{Cmd: []string{"git", "commit", "-q", "-m", in.Title}, Timeout: 60 * time.Second,
			Env: []string{"GIT_AUTHOR_NAME=" + name, "GIT_AUTHOR_EMAIL=" + email, "GIT_COMMITTER_NAME=ws", "GIT_COMMITTER_EMAIL=ws@localhost"}})
		if err != nil || res.ExitCode != 0 {
			return errResult(errors.New("could not commit pending changes"))
		}
	}
	push, err := gitExec(ctx, t.m, sb, 3*time.Minute, "push", "-u", "origin", head)
	if err != nil {
		return errResult(err)
	}
	if push.ExitCode != 0 {
		return gitFail(push, "git push")
	}
	url, number, err := t.host.OpenPR(ctx, sb.ProjectID, head, in.Base, in.Title, in.Body, in.Draft)
	if err != nil {
		return errResult(err)
	}
	return agent.Result{Text: fmt.Sprintf("opened pull request #%d: %s", number, url), Data: map[string]any{"url": url, "number": number, "branch": head}}, nil
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	s = strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	if s == "" {
		s = "change"
	}
	return s
}
