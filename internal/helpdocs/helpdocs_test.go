package helpdocs

import (
	"context"
	"encoding/json"
	"io/fs"
	"strings"
	"testing"

	ws "github.com/jking323/ws"
	"github.com/jking323/ws/internal/agent"
)

func load(t *testing.T) *Index {
	t.Helper()
	ix, err := Load(ws.HelpDocs)
	if err != nil {
		t.Fatal(err)
	}
	return ix
}

// TestEmbeddedMatchesPublicPages: the root package embeds exactly the pages
// docs-site/pages.json publishes, no more (an internal doc must never reach
// the binary) and no fewer (Load would fail at boot).
func TestEmbeddedMatchesPublicPages(t *testing.T) {
	raw, err := fs.ReadFile(ws.HelpDocs, "docs-site/pages.json")
	if err != nil {
		t.Fatal(err)
	}
	var pf pagesFile
	if err := json.Unmarshal(raw, &pf); err != nil {
		t.Fatal(err)
	}
	public := map[string]bool{}
	for _, p := range pf.Public {
		public[p.Src] = true
		if _, err := fs.Stat(ws.HelpDocs, p.Src); err != nil {
			t.Errorf("%s is public in docs-site/pages.json but not in the //go:embed list in helpdocs.go at the repository root; add it there", p.Src)
		}
	}
	_ = fs.WalkDir(ws.HelpDocs, ".", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".md") && !public[path] {
			t.Errorf("%s is embedded but not public in docs-site/pages.json; remove it from helpdocs.go", path)
		}
		return nil
	})
}

func TestLoadSplitsSections(t *testing.T) {
	ix := load(t)
	d := ix.Find("use/approvals")
	if d == nil {
		t.Fatal("use/approvals not found")
	}
	if len(d.Sections) < 2 {
		t.Fatalf("approvals guide split into %d sections", len(d.Sections))
	}
	if ix.Find("docs/guides/approvals.md") != d || ix.Find("Tool approvals") != d {
		t.Error("Find by source path or title did not return the same page")
	}
	for _, internal := range []string{"docs/PLAN.md", "docs/HANDOFF.md", "docs/COORDINATOR.md", "CLAUDE.md"} {
		if ix.Find(internal) != nil {
			t.Errorf("internal doc %s is in the help index", internal)
		}
	}
}

func TestSplitIgnoresHeadingsInCodeFences(t *testing.T) {
	s := split("T", "intro\n## One\nbody\n```sh\n# a shell comment\n```\n## Two\nmore\n")
	var heads []string
	for _, x := range s {
		heads = append(heads, x.Heading)
	}
	if got := strings.Join(heads, "|"); got != "T|One|Two" {
		t.Fatalf("headings %q", got)
	}
	if s[1].Anchor != "one" || !strings.Contains(s[1].Text, "# a shell comment") {
		t.Errorf("section One: %+v", s[1])
	}
}

func TestSearchRanksTheRightPage(t *testing.T) {
	ix := load(t)
	cases := map[string]string{
		"how do I answer a tool approval card": "use/approvals",
		"artifact versions":                    "use/artifacts",
		"choosing a model":                     "use/chat",
	}
	for q, want := range cases {
		hits := ix.Search(q, 3)
		if len(hits) == 0 {
			t.Errorf("%q: no hits", q)
			continue
		}
		found := false
		for _, h := range hits {
			if h.Doc.Slug == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%q: top hits %s, %s…, want %s among them", q, hits[0].Doc.Slug, hits[0].Section.Heading, want)
		}
		if hits[0].Snippet == "" {
			t.Errorf("%q: empty snippet", q)
		}
	}
	if hits := ix.Search("the and of", 5); len(hits) != 0 {
		t.Errorf("stop words alone matched %d sections", len(hits))
	}
}

func TestToolListSearchRead(t *testing.T) {
	tool := NewTool(load(t))
	call := func(args string) string {
		t.Helper()
		res, err := tool.Call(context.Background(), agentCtx(), json.RawMessage(args))
		if err != nil {
			t.Fatal(err)
		}
		return res.Text
	}
	if out := call(`{}`); !strings.Contains(out, "use/chat") || !strings.Contains(out, "Tool approvals") {
		t.Errorf("list: %s", out)
	}
	if out := call(`{"query":"tool approvals"}`); !strings.Contains(out, "page=use/approvals") {
		t.Errorf("search: %s", out)
	}
	if out := call(`{"page":"use/approvals"}`); !strings.HasPrefix(out, "Tool approvals (page=use/approvals)") {
		t.Errorf("read: %.200s", out)
	}
	if out := call(`{"page":"nope"}`); !strings.Contains(out, "No page") {
		t.Errorf("unknown page: %s", out)
	}
	ix := tool.Index
	d := ix.Find("use/chat")
	if out := call(`{"page":"use/chat","section":"` + d.Sections[1].Heading + `"}`); !strings.Contains(out, d.Sections[1].Heading) {
		t.Errorf("read section: %.200s", out)
	}
}

func agentCtx() agent.ToolCtx { return agent.ToolCtx{} }
