// Package helpdocs is ws's in-app help: the public documentation, embedded
// at build time (the root package's HelpDocs, the pages docs-site/pages.json
// publishes), split into sections and searched by keyword, so the assistant
// can explain ws from the docs that match the running build. Internal docs
// are never embedded. The ws_help tool (tool.go) is how a model reads it.
package helpdocs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"sort"
	"strings"
	"unicode"
)

// Doc is one public page.
type Doc struct {
	Src         string // repository path, e.g. docs/guides/chat.md
	Slug        string // the docs site path, e.g. use/chat
	Title       string
	Description string
	Sections    []Section
}

// Section is the text under one heading (the text before the first heading
// is a section titled after the page).
type Section struct {
	Heading string
	Anchor  string
	Text    string
	terms   map[string]int
}

// Hit is one search result.
type Hit struct {
	Doc     *Doc
	Section *Section
	Score   float64
	Snippet string
}

// Index is the loaded documentation.
type Index struct {
	Docs []*Doc
	df   map[string]int // sections containing each term
	n    int            // number of sections
}

type pagesFile struct {
	Public []struct {
		Src         string `json:"src"`
		Dest        string `json:"dest"`
		Title       string `json:"title"`
		Description string `json:"description"`
	} `json:"public"`
}

// Load reads docs-site/pages.json from fsys and every public page it lists.
func Load(fsys fs.FS) (*Index, error) {
	raw, err := fs.ReadFile(fsys, "docs-site/pages.json")
	if err != nil {
		return nil, fmt.Errorf("helpdocs: %w", err)
	}
	var pf pagesFile
	if err := json.Unmarshal(raw, &pf); err != nil {
		return nil, fmt.Errorf("helpdocs: pages.json: %w", err)
	}
	if len(pf.Public) == 0 {
		return nil, errors.New("helpdocs: pages.json lists no public pages")
	}
	ix := &Index{df: map[string]int{}}
	for _, p := range pf.Public {
		body, err := fs.ReadFile(fsys, p.Src)
		if err != nil {
			return nil, fmt.Errorf("helpdocs: %s is public in pages.json but not embedded: %w", p.Src, err)
		}
		d := &Doc{Src: p.Src, Slug: p.Dest, Title: p.Title, Description: p.Description}
		d.Sections = split(p.Title, string(body))
		for i := range d.Sections {
			s := &d.Sections[i]
			s.terms = counts(s.Heading + " " + s.Text)
			for t := range s.terms {
				ix.df[t]++
			}
			ix.n++
		}
		ix.Docs = append(ix.Docs, d)
	}
	return ix, nil
}

// split cuts a Markdown page at its headings, ignoring lines inside code
// fences.
func split(title, body string) []Section {
	var out []Section
	cur := Section{Heading: title, Anchor: ""}
	var b strings.Builder
	fence := false
	flush := func() {
		cur.Text = strings.TrimSpace(b.String())
		if cur.Text != "" || cur.Anchor != "" {
			out = append(out, cur)
		}
		b.Reset()
	}
	for _, line := range strings.Split(body, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "```") {
			fence = !fence
		}
		if !fence && strings.HasPrefix(line, "#") {
			h := strings.TrimSpace(strings.TrimLeft(line, "#"))
			if h != "" {
				flush()
				cur = Section{Heading: h, Anchor: anchor(h)}
				continue
			}
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	flush()
	return out
}

// anchor is the GitHub-style fragment for a heading.
func anchor(h string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(h) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
	}
	return b.String()
}

var stop = map[string]bool{
	"the": true, "and": true, "for": true, "are": true, "you": true, "how": true, "what": true, "can": true,
	"does": true, "with": true, "this": true, "that": true, "from": true, "into": true, "your": true, "its": true,
	"is": true, "it": true, "of": true, "to": true, "in": true, "on": true, "or": true, "an": true, "be": true,
	"do": true, "if": true, "my": true, "me": true, "as": true, "at": true, "by": true, "ws": true, "use": true,
	"get": true, "set": true, "about": true, "where": true, "when": true, "which": true, "who": true, "why": true,
}

// terms lowercases and splits text into searchable words, dropping very
// short and very common ones and a trailing plural "s".
func terms(text string) []string {
	f := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
	})
	out := f[:0]
	for _, w := range f {
		if len(w) < 2 || stop[w] {
			continue
		}
		if len(w) > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") {
			w = strings.TrimSuffix(w, "s")
		}
		out = append(out, w)
	}
	return out
}

func counts(text string) map[string]int {
	m := map[string]int{}
	for _, t := range terms(text) {
		m[t]++
	}
	return m
}

// Search returns up to n sections ranked for the query: term frequency
// (capped) weighted by rarity, with extra weight for a term in the heading
// or the page title.
func (ix *Index) Search(query string, n int) []Hit {
	q := terms(query)
	if len(q) == 0 || n <= 0 {
		return nil
	}
	var hits []Hit
	for _, d := range ix.Docs {
		title := counts(d.Title + " " + d.Description)
		for i := range d.Sections {
			s := &d.Sections[i]
			head := counts(s.Heading)
			var score float64
			for _, t := range q {
				tf := s.terms[t]
				if tf == 0 && title[t] == 0 {
					continue
				}
				idf := math.Log(1 + float64(ix.n)/float64(1+ix.df[t]))
				score += idf * math.Min(float64(tf), 5)
				if head[t] > 0 {
					score += 3 * idf
				}
				if title[t] > 0 {
					score += 1.5 * idf
				}
			}
			if score > 0 {
				hits = append(hits, Hit{Doc: d, Section: s, Score: score})
			}
		}
	}
	sort.SliceStable(hits, func(a, b int) bool { return hits[a].Score > hits[b].Score })
	if len(hits) > n {
		hits = hits[:n]
	}
	for i := range hits {
		hits[i].Snippet = snippet(hits[i].Section.Text, q, 280)
	}
	return hits
}

// snippet is about max characters of text around the first query term.
func snippet(text string, q []string, max int) string {
	flat := strings.Join(strings.Fields(text), " ")
	low := strings.ToLower(flat)
	at := -1
	for _, t := range q {
		if i := strings.Index(low, t); i >= 0 && (at < 0 || i < at) {
			at = i
		}
	}
	start := 0
	if at > max/3 {
		start = at - max/3
	}
	r := []rune(flat)
	// start is a byte offset; move it to a rune boundary.
	start = len([]rune(flat[:start]))
	end := start + max
	if end > len(r) {
		end = len(r)
	}
	s := string(r[start:end])
	if start > 0 {
		s = "…" + s
	}
	if end < len(r) {
		s += "…"
	}
	return s
}

// Find looks a page up by its slug, its repository path or its title,
// ignoring case.
func (ix *Index) Find(page string) *Doc {
	p := strings.ToLower(strings.Trim(strings.TrimSpace(page), "/"))
	for _, d := range ix.Docs {
		if p == strings.ToLower(d.Slug) || p == strings.ToLower(d.Src) || p == strings.ToLower(d.Title) {
			return d
		}
	}
	return nil
}

// Section looks a section up by its anchor or heading, ignoring case; a
// heading prefix matches when it is unambiguous.
func (d *Doc) Section(name string) *Section {
	n := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(name), "#"))
	var prefix []*Section
	for i := range d.Sections {
		s := &d.Sections[i]
		h := strings.ToLower(s.Heading)
		if n == s.Anchor || n == h {
			return s
		}
		if strings.HasPrefix(h, n) {
			prefix = append(prefix, s)
		}
	}
	if len(prefix) == 1 {
		return prefix[0]
	}
	return nil
}
