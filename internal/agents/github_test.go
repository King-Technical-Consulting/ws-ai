package agents

import (
	"strings"
	"testing"
)

const pushPayload = `{"ref":"refs/heads/main","forced":false,"compare":"https://github.com/acme/ws/compare/abc...def",
"repository":{"full_name":"acme/ws"},"pusher":{"name":"jeremy"},"sender":{"login":"jeremy"},
"commits":[{"id":"abcdef1234567","message":"Fix the thing\n\nLonger body","author":{"name":"Jeremy"},"added":["a.go"],"modified":["b.go"],"removed":[]},
{"id":"1234567abcdef","message":"Second","author":{"name":"Jeremy"}}],
"head_commit":{"id":"1234567abcdef","message":"Second"}}`

func TestRenderGitHubPush(t *testing.T) {
	out, ok := RenderGitHubEvent(RepoPushSpec{}, "push", pushPayload)
	if !ok {
		t.Fatal("push should render")
	}
	for _, want := range []string{"GitHub push to acme/ws on main by jeremy: 2 commit(s).", "- abcdef1 Fix the thing (Jeremy)", "files: a.go, b.go", "- 1234567 Second", "Compare: https://github.com/acme/ws/compare/abc...def", "Head: 1234567abcdef"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// Filters: ping, another event, another repo, another branch, patterns.
	if _, ok := RenderGitHubEvent(RepoPushSpec{}, "ping", `{"zen":"x"}`); ok {
		t.Error("ping should be ignored")
	}
	if _, ok := RenderGitHubEvent(RepoPushSpec{}, "issues", `{}`); ok {
		t.Error("events default to push only")
	}
	if _, ok := RenderGitHubEvent(RepoPushSpec{Repo: "acme/other"}, "push", pushPayload); ok {
		t.Error("another repository should be ignored")
	}
	if _, ok := RenderGitHubEvent(RepoPushSpec{Repo: "ACME/ws"}, "push", pushPayload); !ok {
		t.Error("repository names compare case-insensitively")
	}
	if _, ok := RenderGitHubEvent(RepoPushSpec{Branches: []string{"release/*"}}, "push", pushPayload); ok {
		t.Error("main is not release/*")
	}
	if _, ok := RenderGitHubEvent(RepoPushSpec{Branches: []string{"dev", "main"}}, "push", pushPayload); !ok {
		t.Error("main is listed")
	}
	if _, ok := RenderGitHubEvent(RepoPushSpec{Events: []string{"*"}}, "deployment", `{"action":"created","sender":{"login":"bot"},"repository":{"full_name":"acme/ws"}}`); !ok {
		t.Error("* takes every event")
	}
}

func TestRenderGitHubOtherEvents(t *testing.T) {
	spec := RepoPushSpec{Events: []string{"pull_request", "issues", "issue_comment", "release", "watch"}}
	out, ok := RenderGitHubEvent(spec, "pull_request", `{"action":"opened","repository":{"full_name":"acme/ws"},"pull_request":{"number":7,"title":"Add X","body":"Why","html_url":"https://github.com/acme/ws/pull/7","user":{"login":"ann"},"head":{"ref":"feat"},"base":{"ref":"main"}}}`)
	if !ok || !strings.Contains(out, `pull request opened in acme/ws: #7 "Add X" by ann, feat → main`) || !strings.Contains(out, "Description:\nWhy") {
		t.Errorf("pull_request = %q ok=%v", out, ok)
	}
	// Pull requests filter on their base branch.
	if _, ok := RenderGitHubEvent(RepoPushSpec{Events: []string{"pull_request"}, Branches: []string{"dev"}}, "pull_request", `{"pull_request":{"base":{"ref":"main"}}}`); ok {
		t.Error("base main is not dev")
	}
	out, ok = RenderGitHubEvent(spec, "issues", `{"action":"labeled","repository":{"full_name":"acme/ws"},"issue":{"number":3,"title":"Bug","body":"Steps","html_url":"u","user":{"login":"bob"}}}`)
	if !ok || !strings.Contains(out, `issue labeled in acme/ws: #3 "Bug" by bob`) || !strings.Contains(out, "Body:\nSteps") {
		t.Errorf("issues = %q", out)
	}
	out, ok = RenderGitHubEvent(spec, "issue_comment", `{"action":"created","repository":{"full_name":"acme/ws"},"issue":{"number":3,"title":"Bug"},"comment":{"body":"me too","html_url":"c","user":{"login":"cat"}}}`)
	if !ok || !strings.Contains(out, "comment created in acme/ws on #3 \"Bug\" by cat:\nme too") {
		t.Errorf("issue_comment = %q", out)
	}
	out, ok = RenderGitHubEvent(spec, "release", `{"action":"published","repository":{"full_name":"acme/ws"},"sender":{"login":"dan"},"release":{"tag_name":"v1","name":"One","html_url":"r"}}`)
	if !ok || !strings.Contains(out, `release published in acme/ws: v1 "One" by dan`) {
		t.Errorf("release = %q", out)
	}
	out, ok = RenderGitHubEvent(spec, "watch", `{"action":"started","repository":{"full_name":"acme/ws"},"sender":{"login":"eve"}}`)
	if !ok || !strings.Contains(out, "GitHub watch event in acme/ws (started) by eve") || !strings.Contains(out, "Payload:") {
		t.Errorf("watch = %q", out)
	}
	// Garbage still renders generically and never exceeds the input cap.
	big := strings.Repeat("x", 2*MaxInputBytes)
	out, ok = RenderGitHubEvent(RepoPushSpec{Events: []string{"*"}}, "custom", big)
	if !ok || len(out) > MaxInputBytes {
		t.Errorf("big payload: ok=%v len=%d", ok, len(out))
	}
}
