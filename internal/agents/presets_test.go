package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jking323/ws/internal/agent"
)

func TestLoadPresets(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) { _ = os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(body), 0o600) }
	write("quiet", "title: Quiet\ntools: []\n")
	write("wide", "title: Wide\ntools: ['*']\nmodel: {task_class: code, reasoning: true}\nmax_steps: 20\n")
	write("some", "title: Some\ntools: [web_fetch, ' read_blob ', web_fetch, nope]\n")
	known := func(n string) bool { return n == "web_fetch" || n == "read_blob" }
	ps, warn, err := LoadPresets(dir, known)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 3 || ps[0].Name != "quiet" || ps[1].Name != "some" || ps[2].Name != "wide" {
		t.Fatalf("presets = %+v", ps)
	}
	// [] is "none" on the agent; ["*"] is the agent's empty list; names
	// are trimmed and unique; unknown ones warn but load.
	if q := ps[0]; len(q.Tools) != 1 || q.Tools[0] != agent.NoTools || q.Model.TaskClass != "chat" || q.Model.Selector != "auto" || q.MaxSteps != 50 {
		t.Errorf("quiet = %+v", q)
	}
	if w := ps[2]; len(w.Tools) != 0 || w.Tools == nil || w.Model.TaskClass != "code" || !w.Model.Reasoning || w.MaxSteps != 20 {
		t.Errorf("wide = %+v", w)
	}
	if s := ps[1]; strings.Join(s.Tools, ",") != "web_fetch,read_blob,nope" {
		t.Errorf("some = %+v", s)
	}
	if len(warn) != 1 || !strings.Contains(warn[0], "unknown tool nope") {
		t.Errorf("warn = %v", warn)
	}
	// Refusals.
	for name, body := range map[string]string{
		"no-tools-key": "title: X\n",
		"bad-class":    "title: X\ntools: []\nmodel: {task_class: rental}\n",
		"mixed":        "title: X\ntools: ['*', bash]\n",
		"no-title":     "tools: []\n",
		"Bad Name":     "title: X\ntools: []\n",
		"too-many":     "title: X\ntools: []\nmax_steps: 501\n",
	} {
		d := t.TempDir()
		_ = os.WriteFile(filepath.Join(d, name+".yaml"), []byte(body), 0o600)
		if _, _, err := LoadPresets(d, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestShippedPresets(t *testing.T) {
	ps, warn, err := LoadPresets(filepath.Join("..", "..", "config", "presets"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(warn) != 0 {
		t.Errorf("warn = %v", warn)
	}
	byName := map[string]Preset{}
	for _, p := range ps {
		byName[p.Name] = p
		if p.Description == "" || p.Prompt == "" {
			t.Errorf("%s: description and prompt should be set", p.Name)
		}
	}
	for _, want := range []string{"general", "code", "research", "quick"} {
		if _, ok := byName[want]; !ok {
			t.Errorf("preset %s missing", want)
		}
	}
	// Quick has no tools; Research reads untrusted pages and so has no
	// write tools and no memory writes; nothing ships with every tool
	// except General.
	if q := byName["quick"]; len(q.Tools) != 1 || q.Tools[0] != agent.NoTools {
		t.Errorf("quick tools = %v", q.Tools)
	}
	for _, tool := range byName["research"].Tools {
		switch tool {
		case "bash", "write_file", "edit_file", "git_push", "git_commit", "open_pr", "remember", "spawn_job":
			t.Errorf("research must not have the write tool %s", tool)
		}
	}
	for _, p := range ps {
		if len(p.Tools) == 0 && p.Name != "general" {
			t.Errorf("%s ships with every tool", p.Name)
		}
	}
}
