package helpdocs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jking323/ws/internal/agent"
	"github.com/jking323/ws/internal/gateway"
)

// readCap is the most a single read returns; a longer page is cut and the
// result lists its sections so the model can read one.
const readCap = 12000

// Tool is ws_help: search the help, read a page or section, or list the
// pages. Read-only and local, so it runs without asking.
type Tool struct {
	Index *Index
}

// NewTool returns the ws_help tool over ix.
func NewTool(ix *Index) *Tool { return &Tool{Index: ix} }

func (t *Tool) Def() gateway.ToolDef {
	return gateway.ToolDef{
		Name: "ws_help",
		Description: "Look up ws's own documentation (this app: what it can do, how to use a feature, where a setting lives, how to run and configure it). " +
			"Give query to search; give page (and optionally section) to read; give neither to list the pages. " +
			"Answer questions about ws from what this returns and name the page you used.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"query":{"type":"string","description":"Words to search for, e.g. \"tool approvals\" or \"artifact origin\"."},` +
			`"page":{"type":"string","description":"A page slug from a search or the list, e.g. use/chat."},` +
			`"section":{"type":"string","description":"A heading or anchor within the page."}` +
			`},"additionalProperties":false}`),
	}
}

func (t *Tool) DefaultPolicy() agent.Policy { return agent.PolicyAuto }
func (t *Tool) Idempotent() bool            { return true }

type hitView struct {
	Page    string `json:"page"`
	Title   string `json:"title"`
	Section string `json:"section"`
	Anchor  string `json:"anchor,omitempty"`
	Snippet string `json:"snippet"`
}

func (t *Tool) Call(_ context.Context, _ agent.ToolCtx, args json.RawMessage) (agent.Result, error) {
	var in struct {
		Query   string `json:"query"`
		Page    string `json:"page"`
		Section string `json:"section"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return agent.ErrorResult(err), nil
		}
	}
	switch {
	case strings.TrimSpace(in.Page) != "":
		return t.read(in.Page, in.Section), nil
	case strings.TrimSpace(in.Query) != "":
		return t.search(in.Query), nil
	default:
		return t.list(), nil
	}
}

func (t *Tool) list() agent.Result {
	var b strings.Builder
	b.WriteString("ws documentation pages (read one with page=<slug>):\n")
	var pages []map[string]string
	for _, d := range t.Index.Docs {
		fmt.Fprintf(&b, "- %s (%s): %s\n", d.Title, d.Slug, d.Description)
		pages = append(pages, map[string]string{"page": d.Slug, "title": d.Title})
	}
	return agent.Result{Text: b.String(), Data: map[string]any{"pages": pages}}
}

func (t *Tool) search(q string) agent.Result {
	hits := t.Index.Search(q, 6)
	if len(hits) == 0 {
		return agent.Result{Text: fmt.Sprintf("No documentation matches %q. Call ws_help with no arguments to list the pages.", q), Data: map[string]any{"hits": []hitView{}}}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Documentation matching %q (read more with page and section):\n", q)
	views := make([]hitView, 0, len(hits))
	for _, h := range hits {
		v := hitView{Page: h.Doc.Slug, Title: h.Doc.Title, Section: h.Section.Heading, Anchor: h.Section.Anchor, Snippet: h.Snippet}
		views = append(views, v)
		fmt.Fprintf(&b, "\n[%s, page=%s, section=%q]\n%s\n", h.Doc.Title, h.Doc.Slug, h.Section.Heading, h.Snippet)
	}
	return agent.Result{Text: b.String(), Data: map[string]any{"hits": views}}
}

func (t *Tool) read(page, section string) agent.Result {
	d := t.Index.Find(page)
	if d == nil {
		return agent.Result{Text: fmt.Sprintf("No page %q. Call ws_help with no arguments to list the pages.", page), IsError: true}
	}
	var b strings.Builder
	if strings.TrimSpace(section) != "" {
		s := d.Section(section)
		if s == nil {
			fmt.Fprintf(&b, "No section %q in %s. Its sections:\n", section, d.Title)
			for _, s := range d.Sections {
				fmt.Fprintf(&b, "- %s\n", s.Heading)
			}
			return agent.Result{Text: b.String(), IsError: true}
		}
		fmt.Fprintf(&b, "%s (page=%s), section %q:\n\n%s", d.Title, d.Slug, s.Heading, s.Text)
		return agent.Result{Text: capText(b.String()), Data: map[string]any{"page": d.Slug, "section": s.Heading}}
	}
	fmt.Fprintf(&b, "%s (page=%s)\n", d.Title, d.Slug)
	for _, s := range d.Sections {
		fmt.Fprintf(&b, "\n## %s\n\n%s\n", s.Heading, s.Text)
	}
	text := b.String()
	if len(text) > readCap {
		var heads strings.Builder
		for _, s := range d.Sections {
			fmt.Fprintf(&heads, "- %s\n", s.Heading)
		}
		text = capText(text) + "\n\nThe page is longer than this. Read one section with section=<heading>. Its sections:\n" + heads.String()
	}
	return agent.Result{Text: text, Data: map[string]any{"page": d.Slug}}
}

func capText(s string) string {
	if len(s) <= readCap {
		return s
	}
	cut := s[:readCap]
	// Do not end inside a UTF-8 sequence.
	for len(cut) > 0 && cut[len(cut)-1]&0xC0 == 0x80 {
		cut = cut[:len(cut)-1]
	}
	if len(cut) > 0 && cut[len(cut)-1] >= 0xC0 {
		cut = cut[:len(cut)-1]
	}
	return cut + "…"
}
