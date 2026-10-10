package gateway

import (
	"context"
	"errors"
)

// Strict bring-your-own-key routing. A hosted provider (one that takes an API
// key) serves a person only with a key that is theirs to use: their own, or
// the server's shared key if they are allowed it (the owner, or someone the
// owner granted it). Without either, the provider is not a candidate for
// them, and local models are unaffected. A request with no user (the
// server's own housekeeping calls) may use the shared keys.
//
// The decision is made once, in Prepare: candidates come out of it already
// carrying the right credentials, so every caller (chat, embeddings, media,
// the Claude passthrough) uses c.Provider as it always did and cannot reach
// for the shared key by mistake.

// ErrNeedsOwnKey is what a person sees when the only models that could serve
// them are hosted and they have no key for those providers.
var ErrNeedsOwnKey = errors.New("gateway: needs your own API key")

// KeyAccess is what one person may use. It carries no secret until Key is
// called, and then only the one asked for.
type KeyAccess struct {
	// Shared: the server's own provider keys may be used for this person.
	Shared bool
	// Own lists the providers the person has saved a key for.
	Own map[string]bool
	// Open returns the person's key for a provider; nil when there are none.
	Open func(providerID string) (string, bool)
}

// allows says whether a hosted provider may serve this person at all. A nil
// KeyAccess (no user, or no key store) turns enforcement off for the
// server's own calls, which still need a key the server has: a hosted
// provider configured without one is reachable only through a person's own
// key, and the shared grant is worth nothing on it.
func (k *KeyAccess) allows(p *Provider) bool {
	if !p.Hosted {
		return true
	}
	if k == nil {
		return p.APIKey != ""
	}
	return k.Own[p.ID] || (k.Shared && p.APIKey != "")
}

// KeySource looks up what a user may use. Implementations fail closed: on an
// error the gateway treats the person as having no keys.
type KeySource interface {
	Access(ctx context.Context, userID string) (*KeyAccess, error)
}

// credit rewrites the routed candidates so each carries the credentials the
// person is to use: a copy of the provider with their own key where they
// have one, the shared provider otherwise. The shared provider value is never
// modified.
func (k *KeyAccess) credit(cands []Candidate) []Candidate {
	out := make([]Candidate, 0, len(cands))
	for _, c := range cands {
		if k == nil {
			if !c.Provider.Hosted || c.Provider.APIKey != "" {
				out = append(out, c)
			}
			continue
		}
		if c.Provider.Hosted && k.Own[c.Provider.ID] && k.Open != nil {
			if key, ok := k.Open(c.Provider.ID); ok && key != "" {
				cp := *c.Provider
				cp.APIKey = key
				c.Provider = &cp
				c.OwnKey = true
				out = append(out, c)
				continue
			}
			// A saved key that cannot be opened (the server's secret
			// changed) is not a reason to fall back to the shared key for
			// someone who may not use it.
		}
		if c.Provider.Hosted && (!k.Shared || c.Provider.APIKey == "") {
			continue
		}
		out = append(out, c)
	}
	return out
}

// accessFor loads a user's key access. nil (no enforcement) for a request
// without a user or a gateway without a key source.
func (g *Gateway) accessFor(ctx context.Context, userID string) *KeyAccess {
	if g.Keys == nil || userID == "" {
		return nil
	}
	acc, err := g.Keys.Access(ctx, userID)
	if err != nil || acc == nil {
		g.Log.Warn("gateway: key lookup failed; treating the user as having no keys", "err", err)
		return &KeyAccess{}
	}
	return acc
}
