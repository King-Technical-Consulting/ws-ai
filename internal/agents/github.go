package agents

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
)

// RenderGitHubEvent turns a GitHub webhook delivery into the text an
// agent run starts with, or reports false when the trigger's spec does
// not want it: a ping, a repository or event the spec excludes, or a push
// to another branch. Unknown events are summarized generically with the
// payload clipped.
func RenderGitHubEvent(spec RepoPushSpec, event, body string) (string, bool) {
	event = strings.ToLower(strings.TrimSpace(event))
	if event == "" || event == "ping" {
		return "", false
	}
	events := spec.Events
	if len(events) == 0 {
		events = []string{"push"}
	}
	wanted := false
	for _, e := range events {
		if e == event || e == "*" {
			wanted = true
			break
		}
	}
	if !wanted {
		return "", false
	}
	var ev ghEvent
	_ = json.Unmarshal([]byte(body), &ev)
	if spec.Repo != "" && !strings.EqualFold(spec.Repo, ev.Repository.FullName) {
		return "", false
	}
	repo := ev.Repository.FullName
	if repo == "" {
		repo = "the repository"
	}
	var b strings.Builder
	switch event {
	case "push":
		branch := strings.TrimPrefix(ev.Ref, "refs/heads/")
		if strings.HasPrefix(ev.Ref, "refs/tags/") {
			branch = "tag " + strings.TrimPrefix(ev.Ref, "refs/tags/")
		}
		if !branchMatches(spec.Branches, strings.TrimPrefix(ev.Ref, "refs/heads/")) {
			return "", false
		}
		who := ev.Pusher.Name
		if who == "" {
			who = ev.Sender.Login
		}
		fmt.Fprintf(&b, "GitHub push to %s on %s by %s", repo, branch, who)
		if ev.Forced {
			b.WriteString(" (force push)")
		}
		if ev.Deleted {
			b.WriteString(" (branch deleted)")
		}
		fmt.Fprintf(&b, ": %d commit(s).\n", len(ev.Commits))
		for i, c := range ev.Commits {
			if i == 30 {
				fmt.Fprintf(&b, "- … and %d more\n", len(ev.Commits)-30)
				break
			}
			sha := c.ID
			if len(sha) > 7 {
				sha = sha[:7]
			}
			fmt.Fprintf(&b, "- %s %s (%s)\n", sha, firstLine(c.Message), c.Author.Name)
			if n := len(c.Added) + len(c.Modified) + len(c.Removed); n > 0 {
				files := append(append(append([]string{}, c.Added...), c.Modified...), c.Removed...)
				if len(files) > 12 {
					files = append(files[:12], fmt.Sprintf("… %d more", n-12))
				}
				fmt.Fprintf(&b, "  files: %s\n", strings.Join(files, ", "))
			}
		}
		if ev.Compare != "" {
			fmt.Fprintf(&b, "Compare: %s\n", ev.Compare)
		}
		if ev.HeadCommit != nil && ev.HeadCommit.ID != "" {
			fmt.Fprintf(&b, "Head: %s\n", ev.HeadCommit.ID)
		}
	case "pull_request":
		pr := ev.PullRequest
		if !branchMatches(spec.Branches, pr.Base.Ref) {
			return "", false
		}
		fmt.Fprintf(&b, "GitHub pull request %s in %s: #%d %q by %s, %s → %s.\n", ev.Action, repo, pr.Number, pr.Title, pr.User.Login, pr.Head.Ref, pr.Base.Ref)
		if pr.HTMLURL != "" {
			fmt.Fprintf(&b, "URL: %s\n", pr.HTMLURL)
		}
		if body := strings.TrimSpace(pr.Body); body != "" {
			fmt.Fprintf(&b, "Description:\n%s\n", clipText(body, 4000))
		}
	case "issues":
		is := ev.Issue
		fmt.Fprintf(&b, "GitHub issue %s in %s: #%d %q by %s.\n", ev.Action, repo, is.Number, is.Title, is.User.Login)
		if is.HTMLURL != "" {
			fmt.Fprintf(&b, "URL: %s\n", is.HTMLURL)
		}
		if body := strings.TrimSpace(is.Body); body != "" {
			fmt.Fprintf(&b, "Body:\n%s\n", clipText(body, 4000))
		}
	case "issue_comment", "pull_request_review_comment":
		fmt.Fprintf(&b, "GitHub comment %s in %s", ev.Action, repo)
		if ev.Issue.Number > 0 {
			fmt.Fprintf(&b, " on #%d %q", ev.Issue.Number, ev.Issue.Title)
		} else if ev.PullRequest.Number > 0 {
			fmt.Fprintf(&b, " on #%d %q", ev.PullRequest.Number, ev.PullRequest.Title)
		}
		fmt.Fprintf(&b, " by %s:\n%s\n", ev.Comment.User.Login, clipText(strings.TrimSpace(ev.Comment.Body), 4000))
		if ev.Comment.HTMLURL != "" {
			fmt.Fprintf(&b, "URL: %s\n", ev.Comment.HTMLURL)
		}
	case "release":
		fmt.Fprintf(&b, "GitHub release %s in %s: %s %q by %s.\n", ev.Action, repo, ev.Release.TagName, ev.Release.Name, ev.Sender.Login)
		if ev.Release.HTMLURL != "" {
			fmt.Fprintf(&b, "URL: %s\n", ev.Release.HTMLURL)
		}
	default:
		fmt.Fprintf(&b, "GitHub %s event in %s", event, repo)
		if ev.Action != "" {
			fmt.Fprintf(&b, " (%s)", ev.Action)
		}
		if ev.Sender.Login != "" {
			fmt.Fprintf(&b, " by %s", ev.Sender.Login)
		}
		b.WriteString(".\nPayload:\n")
		b.WriteString(clipText(strings.TrimSpace(body), 8000))
		b.WriteString("\n")
	}
	out := b.String()
	if len(out) > MaxInputBytes {
		out = out[:MaxInputBytes]
	}
	return out, true
}

// branchMatches reports whether a branch is on the list (names or
// path patterns); an empty list matches every branch.
func branchMatches(patterns []string, branch string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		if p == branch {
			return true
		}
		if ok, _ := path.Match(p, branch); ok {
			return true
		}
	}
	return false
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return clipText(s, 200)
}

func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ghEvent is the slice of GitHub's webhook payloads the renderer reads.
type ghEvent struct {
	Action     string `json:"action"`
	Ref        string `json:"ref"`
	Forced     bool   `json:"forced"`
	Deleted    bool   `json:"deleted"`
	Compare    string `json:"compare"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Pusher struct {
		Name string `json:"name"`
	} `json:"pusher"`
	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`
	Commits     []ghCommit `json:"commits"`
	HeadCommit  *ghCommit  `json:"head_commit"`
	PullRequest struct {
		Number  int    `json:"number"`
		Title   string `json:"title"`
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
		User    struct {
			Login string `json:"login"`
		} `json:"user"`
		Head struct {
			Ref string `json:"ref"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	} `json:"pull_request"`
	Issue struct {
		Number  int    `json:"number"`
		Title   string `json:"title"`
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
		User    struct {
			Login string `json:"login"`
		} `json:"user"`
	} `json:"issue"`
	Comment struct {
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
		User    struct {
			Login string `json:"login"`
		} `json:"user"`
	} `json:"comment"`
	Release struct {
		TagName string `json:"tag_name"`
		Name    string `json:"name"`
		HTMLURL string `json:"html_url"`
	} `json:"release"`
}

type ghCommit struct {
	ID      string `json:"id"`
	Message string `json:"message"`
	Author  struct {
		Name string `json:"name"`
	} `json:"author"`
	Added    []string `json:"added"`
	Modified []string `json:"modified"`
	Removed  []string `json:"removed"`
}
