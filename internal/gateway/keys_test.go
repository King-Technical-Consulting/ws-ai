package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeKeys struct {
	acc *KeyAccess
	err error
}

func (f fakeKeys) Access(context.Context, string) (*KeyAccess, error) { return f.acc, f.err }

func keyRegistry() *Registry {
	reg := NewRegistry()
	reg.Replace(
		[]*Provider{
			{ID: "anthropic", Kind: ProviderAnthropic, Hosted: true, APIKey: "shared-key"},
			// Configured, hosted, no server key: only a person's own key reaches it.
			{ID: "nokey", Kind: ProviderOpenAICompat, Hosted: true},
			{ID: "local", Kind: ProviderOpenAICompat},
		},
		[]*Endpoint{
			{ID: "anthropic/haiku", ProviderID: "anthropic", ModelName: "haiku", Enabled: true,
				Capabilities: Capabilities{ContextWindow: 200_000, Tools: true}},
			{ID: "nokey/model", ProviderID: "nokey", ModelName: "model", Enabled: true,
				Capabilities: Capabilities{ContextWindow: 100_000, Tools: true}},
			{ID: "local/small", ProviderID: "local", ModelName: "small", Enabled: true, Local: true,
				Capabilities: Capabilities{ContextWindow: 32_000, Tools: true}},
		},
	)
	return reg
}

const keyPolicy = `
name: keys
rules:
  - match: {}
    prefer: [anthropic/haiku, local/small]
`

func keyGateway(t *testing.T, ks KeySource) *Gateway {
	t.Helper()
	p, err := ParsePolicy(keyPolicy)
	if err != nil {
		t.Fatal(err)
	}
	reg := keyRegistry()
	g := New(reg, NewRouter(reg, []Policy{p}), &memRecorder{recs: make(chan UsageRecord, 4)}, nil)
	g.Keys = ks
	return g
}

func prep(t *testing.T, g *Gateway, model, user string) ([]Candidate, error) {
	t.Helper()
	cands, _, err := g.Prepare(context.Background(), &Request{Model: model, Metadata: Metadata{UserID: user},
		Messages: []Message{{Role: RoleUser, Parts: []Part{TextPart("x")}}}})
	return cands, err
}

func ids(cs []Candidate) string {
	var s []string
	for _, c := range cs {
		s = append(s, c.Endpoint.ID)
	}
	return strings.Join(s, ",")
}

func TestKeysMemberWithoutKeyGetsLocalOnly(t *testing.T) {
	g := keyGateway(t, fakeKeys{acc: &KeyAccess{}})
	cands, err := prep(t, g, "auto", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if ids(cands) != "local/small" {
		t.Errorf("candidates = %s, want local only", ids(cands))
	}
}

func TestKeysMemberWithKeyUsesOwnKeyNotShared(t *testing.T) {
	acc := &KeyAccess{Own: map[string]bool{"anthropic": true},
		Open: func(id string) (string, bool) { return "member-key", id == "anthropic" }}
	g := keyGateway(t, fakeKeys{acc: acc})
	cands, err := prep(t, g, "auto", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if ids(cands) != "anthropic/haiku,local/small" {
		t.Fatalf("candidates = %s", ids(cands))
	}
	if cands[0].Provider.APIKey != "member-key" || !cands[0].OwnKey {
		t.Errorf("hosted candidate = key %q own=%v", cands[0].Provider.APIKey, cands[0].OwnKey)
	}
	if cands[1].OwnKey {
		t.Error("local candidate marked as own key")
	}
	if p, _ := g.Router.reg.Provider("anthropic"); p.APIKey != "shared-key" {
		t.Errorf("shared provider was modified: %q", p.APIKey)
	}
}

func TestKeysOwnerSharedAccessUsesSharedKey(t *testing.T) {
	g := keyGateway(t, fakeKeys{acc: &KeyAccess{Shared: true}})
	cands, err := prep(t, g, "auto", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if ids(cands) != "anthropic/haiku,local/small" || cands[0].Provider.APIKey != "shared-key" || cands[0].OwnKey {
		t.Errorf("candidates = %s key=%q own=%v", ids(cands), cands[0].Provider.APIKey, cands[0].OwnKey)
	}
}

func TestKeysUnreadableOwnKeyNeverFallsBackToShared(t *testing.T) {
	acc := &KeyAccess{Own: map[string]bool{"anthropic": true},
		Open: func(string) (string, bool) { return "", false }}
	g := keyGateway(t, fakeKeys{acc: acc})
	cands, err := prep(t, g, "auto", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if ids(cands) != "local/small" {
		t.Errorf("candidates = %s, want local only", ids(cands))
	}
}

func TestKeysLookupErrorFailsClosed(t *testing.T) {
	g := keyGateway(t, fakeKeys{err: errors.New("db down")})
	cands, err := prep(t, g, "auto", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if ids(cands) != "local/small" {
		t.Errorf("candidates = %s, want local only", ids(cands))
	}
}

func TestKeysNoUserIsNotEnforced(t *testing.T) {
	g := keyGateway(t, fakeKeys{acc: &KeyAccess{}})
	cands, err := prep(t, g, "auto", "")
	if err != nil {
		t.Fatal(err)
	}
	if ids(cands) != "anthropic/haiku,local/small" || cands[0].Provider.APIKey != "shared-key" {
		t.Errorf("candidates = %s", ids(cands))
	}
}

func TestKeysHostedOnlyAskForOwnKey(t *testing.T) {
	g := keyGateway(t, fakeKeys{acc: &KeyAccess{}})
	_, err := prep(t, g, "anthropic/haiku", "u1")
	if err == nil || !errors.Is(err, ErrNoRoute) {
		t.Fatalf("err = %v, want no route", err)
	}
	if !strings.Contains(err.Error(), "API key of yours") {
		t.Errorf("err = %v, should say the person needs their own key", err)
	}
}

func TestKeyAccessAllows(t *testing.T) {
	hosted := &Provider{ID: "h", Hosted: true, APIKey: "server-key"}
	keyless := &Provider{ID: "n", Hosted: true}
	local := &Provider{ID: "l"}
	var none *KeyAccess
	if !none.allows(hosted) {
		t.Error("nil access should allow")
	}
	if none.allows(keyless) {
		t.Error("nil access has no key for a keyless provider")
	}
	k := &KeyAccess{}
	if k.allows(hosted) || !k.allows(local) {
		t.Error("empty access: hosted denied, local allowed")
	}
	if !(&KeyAccess{Own: map[string]bool{"h": true}}).allows(hosted) {
		t.Error("own key should allow")
	}
	if !(&KeyAccess{Own: map[string]bool{"n": true}}).allows(keyless) || (&KeyAccess{Shared: true}).allows(keyless) {
		t.Error("a keyless provider: own key allows, the shared grant does not")
	}
}

// A hosted provider configured without a server key serves a person who saved
// their own key for it, and nobody else: not the owner through the shared
// grant, not the server's own calls.
func TestKeysKeylessProviderNeedsOwnKey(t *testing.T) {
	acc := &KeyAccess{Own: map[string]bool{"nokey": true},
		Open: func(id string) (string, bool) { return "member-nokey-key", id == "nokey" }}
	g := keyGateway(t, fakeKeys{acc: acc})
	cands, err := prep(t, g, "nokey/model", "u1")
	if err != nil || ids(cands) != "nokey/model" || !cands[0].OwnKey || cands[0].Provider.APIKey != "member-nokey-key" {
		t.Fatalf("own key on a keyless provider: %s %v %+v", ids(cands), err, cands)
	}

	owner := keyGateway(t, fakeKeys{acc: &KeyAccess{Shared: true}})
	if _, err := prep(t, owner, "nokey/model", "owner"); err == nil || !strings.Contains(err.Error(), "API key of yours") {
		t.Errorf("shared grant on a keyless provider should need an own key, got %v", err)
	}

	server := keyGateway(t, fakeKeys{acc: &KeyAccess{Shared: true}})
	if _, err := prep(t, server, "nokey/model", ""); err == nil {
		t.Errorf("a request without a user must not reach a keyless provider")
	}
}
