package artifacts

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jking323/ws/internal/gateway"
)

func TestParseDesignContext(t *testing.T) {
	if d, err := ParseDesignContext(nil); d != nil || err != nil {
		t.Errorf("nil: %v %v", d, err)
	}
	if d, err := ParseDesignContext(json.RawMessage(`{}`)); d != nil || err != nil {
		t.Errorf("empty object: %v %v", d, err)
	}
	if _, err := ParseDesignContext(json.RawMessage(`{"library":"bootstrap"}`)); err == nil {
		t.Error("unknown library accepted")
	}
	if _, err := ParseDesignContext(json.RawMessage(`{"colour":"red"}`)); err == nil {
		t.Error("unknown field accepted")
	}
	if _, err := ParseDesignContext(json.RawMessage(`{"notes":"` + strings.Repeat("x", MaxDesignContext) + `"}`)); err == nil {
		t.Error("oversized context accepted")
	}
	d, err := ParseDesignContext(json.RawMessage(`{"library":"tailwind","colors":{"primary":"#b85a1a","background":"#faf8f5"},"type":{"body":"Inter"},"radius":"8px","components":["button","card"],"notes":"warm, quiet"}`))
	if err != nil || d == nil {
		t.Fatalf("parse: %v", err)
	}
	p := d.Prompt()
	for _, want := range []string{"cdn.tailwindcss.com", "Colors: background #faf8f5, primary #b85a1a", "Type: body Inter", "Corner radius: 8px", "Component catalog: button, card", "Notes: warm, quiet", "CSS custom property"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
	// Deterministic: the same context renders the same block.
	if d.Prompt() != p {
		t.Error("prompt not deterministic")
	}
	if s := DesignSystemPrompt(nil); !strings.Contains(s, "kind=design") || strings.Contains(s, "Design system to follow") {
		t.Errorf("prompt without context = %q", s)
	}
	if s := DesignSystemPrompt(d); !strings.Contains(s, "Design system to follow") {
		t.Error("prompt with context lacks the block")
	}
}

func TestNormalizeDesign(t *testing.T) {
	if b, err := normalizeDesign(KindHTML, json.RawMessage(`{"library":"plain"}`)); b != nil || err != nil {
		t.Errorf("html drops the context: %s %v", b, err)
	}
	if _, err := normalizeDesign(KindDesign, json.RawMessage(`{"library":"nope"}`)); err == nil {
		t.Error("bad context accepted on a design")
	}
	b, err := normalizeDesign(KindDesign, json.RawMessage(` {"radius":"4px","library":""} `))
	if err != nil || string(b) != `{"radius":"4px"}` {
		t.Errorf("normalized = %s err=%v", b, err)
	}
	if b, _ := normalizeDesign(KindDesign, json.RawMessage(`{}`)); b != nil {
		t.Errorf("empty context should be nil, got %s", b)
	}
}

func TestExtractHTML(t *testing.T) {
	doc := "<!DOCTYPE html><html><body>x</body></html>"
	cases := map[string]string{
		"": "",
		"Here you go:\n```html\n" + doc + "\n```\nDone.": doc,
		"```\n" + doc + "\n```":                          doc,
		"Sure.\n" + doc + "\nAnything else?":             doc,
		"<div>fragment</div>":                            "<div>fragment</div>",
		"I cannot do that.":                              "",
		"```html\n```\n" + doc:                           doc, // empty fence, then the document
	}
	for in, want := range cases {
		if got := ExtractHTML(in); got != want {
			t.Errorf("ExtractHTML(%q) = %q want %q", in, got, want)
		}
	}
}

func TestVariantRequest(t *testing.T) {
	d := &DesignContext{Library: "plain", Radius: "0"}
	req := variantRequest("auto", d, "Landing", "<html>x</html>", "", 2, 3, gateway.Metadata{UserID: "u"})
	if req.Model != "auto" || req.Temperature == nil || *req.Temperature != 1 || req.Metadata.UserID != "u" {
		t.Errorf("request = %+v", req)
	}
	if !strings.Contains(req.System, "plain CSS") || !strings.Contains(req.System, "no Markdown fence") {
		t.Errorf("system = %q", req.System)
	}
	u := req.Messages[0].Parts[0].Text
	if !strings.Contains(u, "Variant 2 of 3") || !strings.Contains(u, "<html>x</html>") || !strings.Contains(u, "distinct take") {
		t.Errorf("user = %q", u)
	}
	req = variantRequest("auto", nil, "L", "c", "darker, denser", 1, 1, gateway.Metadata{})
	if !strings.Contains(req.Messages[0].Parts[0].Text, "Make it darker, denser.") {
		t.Errorf("instruction not used: %q", req.Messages[0].Parts[0].Text)
	}
}
