package gateway

import "testing"

func TestResolveAlt(t *testing.T) {
	pdf := Part{Kind: PartFile, MIME: "application/pdf", Name: "a.pdf", Data: []byte("x")}
	alt := TextPart("extracted")
	alt.Alt = true
	parts := []Part{TextPart("see this"), pdf, alt}

	native := ResolveAlt(parts, true)
	if len(native) != 2 || native[1].Kind != PartFile {
		t.Errorf("native: %+v", native)
	}
	text := ResolveAlt(parts, false)
	if len(text) != 2 || text[1].Kind != PartText || text[1].Text != "extracted" || text[1].Alt {
		t.Errorf("text-only: %+v", text)
	}
	// A PDF with no stand-in is left for the adapter to deal with, as before.
	plain := []Part{pdf}
	if got := ResolveAlt(plain, false); len(got) != 1 {
		t.Errorf("no alt: %+v", got)
	}
	// Input is not modified.
	if !parts[2].Alt || len(parts) != 3 {
		t.Error("ResolveAlt changed its input")
	}
}
