package aistream

import (
	"regexp"
	"strings"
	"testing"
)

var regexpDigits = regexp.MustCompile(`\d+\.\d+\.\d+\.\d+|:\d{2,5}`)

func TestScrubErrorText(t *testing.T) {
	in := `gateway: all 1 candidates failed: openaicompat: Post "http://192.168.1.20:8000/v1/chat/completions": dial tcp 192.168.1.20:8000: connect: connection refused`
	out := scrubErrorText(in)
	if strings.Contains(out, "http://") || strings.Contains(out, "/v1/chat") || strings.Contains(out, "192.168") {
		t.Fatalf("URL left in %q", out)
	}
	if !strings.Contains(out, "all 1 candidates failed") || !strings.Contains(out, "connection refused") {
		t.Fatalf("the reason was lost: %q", out)
	}
	for _, in := range []string{"dial tcp 10.0.0.5:8000: connect: connection refused", "lookup llama on 127.0.0.11:53: no such host", "upstream 10.1.2.3 refused"} {
		if out := scrubErrorText(in); strings.ContainsAny(out, "0123456789") && strings.Contains(out, ".") && regexpDigits.MatchString(out) {
			t.Errorf("address left in %q -> %q", in, out)
		}
	}
	for _, in := range []string{"dial tcp [::1]:8000: connect: connection refused", "dial tcp [fd00:1234::5]:8000: i/o timeout", "dial tcp: lookup llama on 100.100.100.100:53: no such host"} {
		if out := scrubErrorText(in); strings.Contains(out, "::") || strings.Contains(out, "fd00") || strings.Contains(out, "100.100") || strings.Contains(out, "llama") {
			t.Errorf("address left in %q -> %q", in, out)
		}
	}
	// Ordinary words are not eaten.
	for _, in := range []string{"DNS lookup timed out after 5s", "key lookup failed: not found"} {
		if got := scrubErrorText(in); got != in {
			t.Errorf("plain text changed: %q -> %q", in, got)
		}
	}
	if got := scrubErrorText("rate limited"); got != "rate limited" {
		t.Fatalf("plain text changed: %q", got)
	}
}
