package artifacts

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestSignedURLRoundTrip(t *testing.T) {
	s := &Service{Secret: []byte("test-secret"), BaseURL: "http://art.local:8081", TTL: time.Hour}
	id := uuid.New()
	u := s.SignedURL(id)
	if !strings.HasPrefix(u, "http://art.local:8081/a/"+id.String()+"?t=") {
		t.Fatalf("url = %s", u)
	}
	tok := u[strings.Index(u, "?t=")+3:]
	if !s.Verify(id, tok) {
		t.Error("valid token rejected")
	}
	if s.Verify(uuid.New(), tok) {
		t.Error("token accepted for a different version id")
	}
	if s.Verify(id, tok+"x") {
		t.Error("tampered token accepted")
	}
	expired := &Service{Secret: []byte("test-secret"), TTL: -time.Minute}
	u2 := expired.SignedURL(id)
	if expired.Verify(id, u2[strings.Index(u2, "?t=")+3:]) {
		t.Error("expired token accepted")
	}
	other := &Service{Secret: []byte("other")}
	if other.Verify(id, tok) {
		t.Error("token accepted under a different secret")
	}
}

func TestCSPIsClosed(t *testing.T) {
	csp := CSP("https://app.example.com")
	for _, want := range []string{"default-src 'none'", "connect-src 'none'", "img-src data: blob:", "frame-ancestors https://app.example.com", "form-action 'none'", "base-uri 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP missing %q: %s", want, csp)
		}
	}
	if strings.Contains(csp, "img-src data: blob: https") {
		t.Error("img-src must not allow https (exfiltration beacons)")
	}
}

func TestRenderInjectsRuntimeAndEscapes(t *testing.T) {
	doc := Render(KindHTML, "x", "", "<html><head><title>t</title></head><body>hi</body></html>")
	if !strings.Contains(doc, `<head><meta charset="utf-8"><script>`) {
		t.Errorf("runtime not injected into head: %s", doc[:80])
	}
	frag := Render(KindHTML, "x", "", "<p>fragment</p>")
	if !strings.HasPrefix(frag, "<!doctype html>") || !strings.Contains(frag, "<p>fragment</p>") {
		t.Errorf("fragment not wrapped: %s", frag[:80])
	}
	code := Render(KindCode, "x", "go", "fmt.Println(\"<b>\")")
	if strings.Contains(code, "<b>") || !strings.Contains(code, "&lt;b&gt;") {
		t.Error("code content must be escaped")
	}
	mer := Render(KindMermaid, "x", "", "graph TD; A-->B")
	if !strings.Contains(mer, "cdnjs.cloudflare.com/ajax/libs/mermaid") || !strings.Contains(mer, "A--&gt;B") {
		t.Error("mermaid page wrong")
	}
	md := Render(KindMarkdown, "x", "", "# Title\n\n<script>alert(1)</script>\n\n**bold**")
	if strings.Contains(md, "<script>alert") {
		t.Error("raw html in markdown must be escaped or dropped")
	}
	if !strings.Contains(md, "<strong>bold</strong>") || !strings.Contains(md, "<h1") {
		t.Errorf("markdown not rendered: %s", md)
	}
}

func TestSanitizeSVG(t *testing.T) {
	out := sanitizeSVG(`<svg onload="alert(1)"><script>evil()</script><rect onclick="x()" width="1"/></svg>`)
	if strings.Contains(out, "<script") || strings.Contains(strings.ToLower(out), "onload") || strings.Contains(strings.ToLower(out), "onclick") {
		t.Errorf("svg not sanitized: %s", out)
	}
	if !strings.Contains(out, `width="1"`) {
		t.Errorf("legit attribute lost: %s", out)
	}
}
