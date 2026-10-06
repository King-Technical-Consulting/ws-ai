package artifacts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/gateway"
)

// DesignContext is the design system a design artifact follows (PLAN M6):
// the tokens and the component library. It lives on the conversation
// (settings.design_context, edited from the chat's design panel) and is
// copied onto every version of a design artifact, so a version can be
// re-rendered or varied later with the system it was made under.
type DesignContext struct {
	// Library is tailwind (loaded from the CDN the artifact origin allows),
	// shadcn (Tailwind with shadcn/ui conventions, component styles
	// inlined) or plain (hand-written CSS). Empty means the model's choice.
	Library string `json:"library,omitempty"`
	// Colors maps a token name (primary, background, foreground, accent,
	// muted, border, ...) to a CSS color.
	Colors map[string]string `json:"colors,omitempty"`
	// Type maps heading, body and mono to font stacks, and scale to a
	// type scale description.
	Type map[string]string `json:"type,omitempty"`
	// Spacing and Radius are the base unit and corner radius, as CSS.
	Spacing string `json:"spacing,omitempty"`
	Radius  string `json:"radius,omitempty"`
	// Components names the catalog the mockup should be built from.
	Components []string `json:"components,omitempty"`
	// Notes is free text: brand voice, what to avoid, accessibility rules.
	Notes string `json:"notes,omitempty"`
}

// MaxDesignContext caps the stored JSON.
const MaxDesignContext = 16 << 10

// ParseDesignContext validates a design_context document. nil in, nil
// out; an empty object is nil too.
func ParseDesignContext(raw json.RawMessage) (*DesignContext, error) {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if len(raw) > MaxDesignContext {
		return nil, errors.New("design_context is too large")
	}
	var d DesignContext
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("design_context: %w", err)
	}
	switch d.Library {
	case "", "tailwind", "shadcn", "plain":
	default:
		return nil, fmt.Errorf("design_context: library must be tailwind, shadcn or plain, not %q", d.Library)
	}
	if d.IsZero() {
		return nil, nil
	}
	return &d, nil
}

// IsZero reports an empty context.
func (d *DesignContext) IsZero() bool {
	return d == nil || (d.Library == "" && len(d.Colors) == 0 && len(d.Type) == 0 && d.Spacing == "" && d.Radius == "" && len(d.Components) == 0 && strings.TrimSpace(d.Notes) == "")
}

// Prompt renders the context as the block the design system prompt
// carries. Deterministic (sorted keys) so the prefix caches.
func (d *DesignContext) Prompt() string {
	if d.IsZero() {
		return ""
	}
	var b strings.Builder
	b.WriteString("Design system to follow:\n")
	switch d.Library {
	case "tailwind":
		b.WriteString("- Library: Tailwind CSS, loaded with <script src=\"https://cdn.tailwindcss.com\"></script>; configure the tokens below in tailwind.config on the page.\n")
	case "shadcn":
		b.WriteString("- Library: Tailwind CSS (CDN) with shadcn/ui conventions: the shadcn class and variable names, component styles written inline since there is no bundler.\n")
	case "plain":
		b.WriteString("- Library: plain CSS in a <style> block, no frameworks.\n")
	}
	if len(d.Colors) > 0 {
		b.WriteString("- Colors: " + joinMap(d.Colors) + "\n")
	}
	if len(d.Type) > 0 {
		b.WriteString("- Type: " + joinMap(d.Type) + "\n")
	}
	if d.Spacing != "" {
		b.WriteString("- Spacing unit: " + d.Spacing + "\n")
	}
	if d.Radius != "" {
		b.WriteString("- Corner radius: " + d.Radius + "\n")
	}
	if len(d.Components) > 0 {
		b.WriteString("- Component catalog: " + strings.Join(d.Components, ", ") + "; build the page from these, named as such in class names or comments.\n")
	}
	if n := strings.TrimSpace(d.Notes); n != "" {
		b.WriteString("- Notes: " + n + "\n")
	}
	b.WriteString("Declare every token as a CSS custom property on :root and use only those tokens for color, type, spacing and radius.")
	return b.String()
}

func joinMap(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+" "+m[k])
	}
	return strings.Join(parts, ", ")
}

// DesignSystemPrompt is what a design-mode conversation starts with: the
// base chat prompt gets this appended, with the context block when the
// conversation has one.
func DesignSystemPrompt(d *DesignContext) string {
	s := " You help design user interfaces. When asked for a mockup or a screen, produce one complete, self-contained HTML document (inline CSS and JS, no external assets except the Tailwind CDN when the design system asks for it) with create_artifact kind=design, and pass the design system as design_context so every version keeps it. Revise with update_artifact: each call is a new version the user can compare and export. When asked for alternatives, make them clearly different in layout and hierarchy, not in color."
	if p := d.Prompt(); p != "" {
		s += "\n\n" + p
	}
	return s
}

// ---- variants: N parallel generations from one version ----

// Completer is the slice of the gateway variants use.
type Completer interface {
	Complete(ctx context.Context, req *gateway.Request) (*gateway.Response, error)
}

// VariantParams asks for N alternatives of an artifact's version.
type VariantParams struct {
	ArtifactID uuid.UUID
	Version    int // 0: current
	N          int // 1..MaxVariants, default 3
	// Instruction says what should differ; empty asks for distinct takes.
	Instruction string
	// Selector picks the model; empty uses the conversation's.
	Selector string
	Meta     gateway.Metadata
}

// MaxVariants caps one request; each variant is a model call.
const MaxVariants = 4

// Variants generates N alternatives of a design artifact in parallel and
// stores each as a new design artifact in the same conversation, titled
// after the original. Variants that fail are reported, not fatal, as long
// as one succeeds.
func (s *Service) Variants(ctx context.Context, gw Completer, p VariantParams) ([]*Ref, []error, error) {
	if gw == nil {
		return nil, nil, errors.New("artifacts: no gateway")
	}
	if p.N <= 0 {
		p.N = 3
	}
	if p.N > MaxVariants {
		return nil, nil, fmt.Errorf("artifacts: at most %d variants", MaxVariants)
	}
	if len(p.Instruction) > 2000 {
		return nil, nil, errors.New("artifacts: instruction too long")
	}
	a, _, v, _, err := s.Get(ctx, p.ArtifactID, p.Version)
	if err != nil {
		return nil, nil, err
	}
	if k := Kind(a.Kind); k != KindDesign && k != KindHTML {
		return nil, nil, fmt.Errorf("artifacts: variants need an html or design artifact, not %s", a.Kind)
	}
	content := ""
	if v.Content != nil {
		content = *v.Content
	}
	design, _ := ParseDesignContext(v.DesignContext)
	selector := p.Selector
	if selector == "" {
		if conv, err := s.DB.GetArtifactConversation(ctx, a.ID); err == nil && conv.ModelSelector != "" {
			selector = conv.ModelSelector
		}
	}
	if selector == "" {
		selector = "auto"
	}
	type out struct {
		html string
		err  error
	}
	results := make([]out, p.N)
	var wg sync.WaitGroup
	for i := 0; i < p.N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := variantRequest(selector, design, a.Title, content, p.Instruction, i+1, p.N, p.Meta)
			res, err := gw.Complete(ctx, req)
			if err != nil {
				results[i] = out{err: err}
				return
			}
			html := ExtractHTML(res.Text())
			if html == "" {
				results[i] = out{err: errors.New("the model returned no HTML document")}
				return
			}
			if len(html) > MaxContent {
				results[i] = out{err: errors.New("variant too large")}
				return
			}
			results[i] = out{html: html}
		}(i)
	}
	wg.Wait()
	var refs []*Ref
	var errs []error
	for i, r := range results {
		if r.err != nil {
			errs = append(errs, fmt.Errorf("variant %d: %w", i+1, r.err))
			continue
		}
		ref, err := s.Create(ctx, a.ConversationID, KindDesign, fmt.Sprintf("%s · variant %d", a.Title, i+1), "", r.html, v.DesignContext, uuid.NullUUID{})
		if err != nil {
			errs = append(errs, fmt.Errorf("variant %d: %w", i+1, err))
			continue
		}
		refs = append(refs, ref)
	}
	if len(refs) == 0 {
		if len(errs) > 0 {
			return nil, errs, fmt.Errorf("artifacts: no variant succeeded: %w", errs[0])
		}
		return nil, nil, errors.New("artifacts: no variants")
	}
	return refs, errs, nil
}

// variantRequest is one variant's model call: the design system prompt,
// the current document and the ask, with the temperature up so the N
// calls differ.
func variantRequest(selector string, design *DesignContext, title, content, instruction string, i, n int, meta gateway.Metadata) *gateway.Request {
	system := "You are a user interface designer." + DesignSystemPrompt(design) +
		" You are producing one alternative of an existing design. Reply with the complete HTML document and nothing else: no explanation, no Markdown fence."
	ask := strings.TrimSpace(instruction)
	if ask == "" {
		ask = "a distinct take with a different layout and visual hierarchy, keeping the content and the design system"
	}
	user := fmt.Sprintf("Design: %s\nVariant %d of %d. Make it %s.\n\nCurrent document:\n%s", title, i, n, ask, content)
	temp := 1.0
	return &gateway.Request{
		Model:       selector,
		System:      system,
		Messages:    []gateway.Message{{Role: gateway.RoleUser, Parts: []gateway.Part{{Kind: gateway.PartText, Text: user}}}},
		MaxTokens:   16000,
		Temperature: &temp,
		Metadata:    meta,
	}
}

// ExtractHTML pulls the HTML document out of a model reply: a fenced
// block if there is one, else from the doctype or <html> to the closing
// tag, else the reply itself when it looks like markup.
func ExtractHTML(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if i := strings.Index(text, "```"); i >= 0 {
		rest := text[i+3:]
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			rest = rest[nl+1:]
		}
		if j := strings.Index(rest, "```"); j >= 0 {
			rest = rest[:j]
		}
		if s := strings.TrimSpace(rest); s != "" && strings.HasPrefix(s, "<") {
			return s
		}
	}
	lower := strings.ToLower(text)
	start := strings.Index(lower, "<!doctype")
	if start < 0 {
		start = strings.Index(lower, "<html")
	}
	if start >= 0 {
		end := strings.LastIndex(lower, "</html>")
		if end > start {
			return text[start : end+len("</html>")]
		}
		return text[start:]
	}
	if strings.HasPrefix(text, "<") {
		return text
	}
	return ""
}
