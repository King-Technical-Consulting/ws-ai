package ccjobs

import (
	"strings"
	"testing"
)

// TestRoute is the spec §8 policy table: prompt features → lane.
func TestRoute(t *testing.T) {
	long := strings.Repeat("Please refactor the handler and add tests for every branch. ", 40)
	cases := []struct {
		name string
		in   RouteInput
		lane string
		rule string
	}{
		{"override claude", RouteInput{Prompt: "@claude tidy up the README wording"}, LaneSubscription, "override"},
		{"override local mid-sentence", RouteInput{Prompt: "summarize this @local please"}, LaneLocal, "override"},
		{"override api", RouteInput{Prompt: "@api prove that the algorithm terminates"}, LaneAPI, "override"},
		{"override openrouter", RouteInput{Prompt: "@openrouter translate to French: hello"}, LaneOpenRouter, "override"},
		{"email address is not an override", RouteInput{Prompt: "mail jeremy@api.example about the outage"}, LaneOpenRouter, "simple"},

		{"sensitive beats repo", RouteInput{Prompt: "fix the login bug; here is my password: hunter2", Cwd: "~/code/app"}, LaneLocal, "sensitive"},
		{"sensitive beats @openrouter", RouteInput{Prompt: "@openrouter rewrite this: my api key is sk-live-123"}, LaneLocal, "sensitive"},
		{"sensitive beats @claude", RouteInput{Prompt: "@claude rotate the secret in config"}, LaneLocal, "sensitive"},
		{"sensitive allows @api", RouteInput{Prompt: "@api is storing a password in plain text ever ok"}, LaneAPI, "override"},
		{"sensitive allows @local", RouteInput{Prompt: "@local summarize my medical results"}, LaneLocal, "override"},
		{"sensitive .env", RouteInput{Prompt: "what does this .env do? DB_URL=..."}, LaneLocal, "sensitive"},
		{"private key block", RouteInput{Prompt: "-----BEGIN RSA PRIVATE KEY----- MIIE..."}, LaneLocal, "sensitive"},

		{"cwd given", RouteInput{Prompt: "make the tests pass", Cwd: "/home/j/proj/app"}, LaneSubscription, "repo"},
		{"several file paths", RouteInput{Prompt: "move the helper from internal/a/x.go into pkg/b/y.go and update cmd/ws/main.go"}, LaneSubscription, "repo"},
		{"code fence plus edit verb", RouteInput{Prompt: "fix this:\n```go\nfunc main() { panic(1) }\n```"}, LaneSubscription, "repo"},
		{"long agentic prompt", RouteInput{Prompt: long}, LaneSubscription, "repo"},
		{"many edit verbs no paths", RouteInput{Prompt: "implement the endpoint, add a migration and run the tests in CI"}, LaneSubscription, "repo"},

		{"hard reasoning no repo", RouteInput{Prompt: "Compare the tradeoffs of CRDTs versus OT for a collaborative editor and explain why one scales better."}, LaneAPI, "reasoning"},
		{"proof", RouteInput{Prompt: "Prove that every bounded monotone sequence converges."}, LaneAPI, "reasoning"},

		{"short simple task", RouteInput{Prompt: "summarize: the meeting moved to Thursday and the budget was approved"}, LaneOpenRouter, "simple"},
		{"translate", RouteInput{Prompt: "translate to Spanish: where is the station?"}, LaneOpenRouter, "simple"},
		{"short chat", RouteInput{Prompt: "what's a good name for a cat"}, LaneOpenRouter, "simple"},
		{"latency critical", RouteInput{Prompt: "autocomplete this sentence: the quick brown", LatencyCritical: true}, LaneOpenRouter, "simple"},

		{"ambiguous medium prose", RouteInput{Prompt: strings.Repeat("Tell me about the history of the Hanseatic League and its trade routes across the Baltic. ", 4)}, LaneAPI, "default"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := Route(c.in)
			if d.Lane != c.lane || d.Rule != c.rule {
				t.Errorf("got %s via %s (%s); want %s via %s\nfeatures: %+v", d.Lane, d.Rule, d.Reason, c.lane, c.rule, d.Features)
			}
			if (d.Rule == "default") != d.Ambiguous {
				t.Errorf("ambiguous flag = %v for rule %s", d.Ambiguous, d.Rule)
			}
			if d.Reason == "" {
				t.Error("empty reason")
			}
		})
	}
}

// TestRouteClassified is the M4 policy over the classifier's JSON.
func TestRouteClassified(t *testing.T) {
	plain := Features{Chars: 300}
	cases := []struct {
		name string
		f    Features
		c    Classification
		lane string
		rule string
	}{
		{"model says sensitive", plain, Classification{TaskType: "writing", Sensitive: true}, LaneLocal, "classifier:sensitive"},
		{"rules say sensitive, model says no", Features{Sensitive: true}, Classification{TaskType: "chat", Difficulty: "easy"}, LaneLocal, "classifier:sensitive"},
		{"code needing tools", plain, Classification{TaskType: "code", NeedsTools: true}, LaneSubscription, "classifier:code"},
		{"code with long context", plain, Classification{TaskType: "code", LongContext: true, Difficulty: "hard"}, LaneSubscription, "classifier:code"},
		{"code snippet without tools", plain, Classification{TaskType: "code", Difficulty: "medium"}, LaneAPI, "classifier:default"},
		{"hard writing", plain, Classification{TaskType: "writing", Difficulty: "hard"}, LaneAPI, "classifier:hard"},
		{"analysis", plain, Classification{TaskType: "analysis", Difficulty: "medium"}, LaneAPI, "classifier:hard"},
		{"easy chat", plain, Classification{TaskType: "chat", Difficulty: "easy"}, LaneOpenRouter, "classifier:simple"},
		{"latency critical", plain, Classification{TaskType: "other", Difficulty: "medium", LatencyCritical: true}, LaneOpenRouter, "classifier:simple"},
		{"medium other", plain, Classification{TaskType: "other", Difficulty: "medium"}, LaneAPI, "classifier:default"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := RouteClassified(c.f, c.c)
			if d.Lane != c.lane || d.Rule != c.rule || d.Ambiguous {
				t.Errorf("got %s via %s; want %s via %s", d.Lane, d.Rule, c.lane, c.rule)
			}
			if d.Features.Classification == nil || *d.Features.Classification != c.c {
				t.Errorf("classification not carried in features: %+v", d.Features)
			}
		})
	}
}

func TestExtract(t *testing.T) {
	f := Extract(RouteInput{Prompt: "fix internal/ccjobs/launcher.go and ./cmd/wsj/main.go\n```go\nx := 1\n```\nthen run go test", Cwd: "~/ws"})
	if f.FilePaths != 2 || f.CodeFences != 1 || !f.HasCwd || f.Agentic < 2 || f.Lines != 5 {
		t.Errorf("features = %+v", f)
	}
	if f := Extract(RouteInput{Prompt: "https://example.com/a/b is down"}); f.FilePaths != 0 {
		// A URL is not a file path: it must not make a prompt look repo-shaped.
		t.Errorf("url features = %+v", f)
	}
	if PromptSHA256("a") != "ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb" {
		t.Error("sha256 mismatch")
	}
}

func TestChooseTarget(t *testing.T) {
	c := &Config{Default: "local", Targets: map[string]Target{
		"local":   {Type: "local", DefaultDir: "~/code"},
		"homelab": {Type: "ssh", Host: "homelab", DefaultDir: "~/proj", Repos: []string{"/srv/apps", "~/code/server-only"}},
	}}
	for cwd, want := range map[string]string{
		"":                        "local",
		"~/code/ws":               "local",
		"~/code":                  "local",
		"~/proj/app/":             "homelab",
		"/srv/apps/x":             "homelab",
		"~/code/server-only/api":  "homelab", // longest match wins over local's ~/code
		"/somewhere/else":         "local",
		"~/codex":                 "local", // prefix must end at a path boundary
		"/srv/applications/other": "local",
	} {
		if got := c.ChooseTarget(cwd); got != want {
			t.Errorf("ChooseTarget(%q) = %s, want %s", cwd, got, want)
		}
	}
}
