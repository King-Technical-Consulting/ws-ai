package auth

import (
	"strings"
	"testing"
)

// Both bodies carry the link and the expiry; the HTML escapes what it is
// given and never relies on a stylesheet.
func TestMailBodies(t *testing.T) {
	m := mail{Subject: "s", Title: "Sign in to ws", Intro: "a <b>line</b>", Action: "Sign in", Link: "https://ws.example/login/magic/abc?x=1&y=2", Expires: "15 minutes"}
	txt, h := m.text(), m.html()
	for _, want := range []string{m.Link, "15 minutes", "Sign in to ws", "a <b>line</b>"} {
		if !strings.Contains(txt, want) {
			t.Errorf("text lacks %q:\n%s", want, txt)
		}
	}
	for _, want := range []string{`href="https://ws.example/login/magic/abc?x=1&amp;y=2"`, "15 minutes", "a &lt;b&gt;line&lt;/b&gt;", "Georgia", "#b85a1a"} {
		if !strings.Contains(h, want) {
			t.Errorf("html lacks %q", want)
		}
	}
	if strings.Contains(h, "<style") || strings.Contains(h, "<b>line</b>") {
		t.Error("html must inline its styles and escape its text")
	}
}
