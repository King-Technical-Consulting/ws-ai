package httpx

import "testing"

func TestEndpointEnabledID(t *testing.T) {
	for _, c := range []struct {
		in, id string
		ok     bool
	}{
		{"local-llama/gpt-oss-20b/enabled", "local-llama/gpt-oss-20b", true},
		{"openai/gpt-5/enabled", "openai/gpt-5", true},
		{"plain/enabled", "plain", true},
		{"/enabled", "", false},
		{"enabled", "", false},
		{"local-llama/gpt-oss-20b", "", false},
	} {
		id, ok := endpointEnabledID(c.in)
		if ok != c.ok || (ok && id != c.id) {
			t.Errorf("endpointEnabledID(%q) = %q, %v; want %q, %v", c.in, id, ok, c.id, c.ok)
		}
	}
}

func TestValidInviteAddress(t *testing.T) {
	for in, want := range map[string]bool{
		"a@example.com":                true,
		" a@example.com ":              true,
		"not-an-email":                 false,
		"Name <a@example.com>":         false,
		"a@example.com, b@example.com": false,
		"@example.com":                 false,
		"":                             false,
	} {
		if got := validInviteAddress(in); got != want {
			t.Errorf("validInviteAddress(%q) = %v, want %v", in, got, want)
		}
	}
}
