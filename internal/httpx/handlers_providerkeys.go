package httpx

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jking323/ws/internal/sealed"
	"github.com/jking323/ws/internal/store"
)

// Bring your own key: a person saves their own API key for a hosted
// provider on the Settings page. The routes take a browser session only
// (never an API key), the key is sealed before it is stored, and no route
// ever returns it: the list shows the last four characters.

const (
	minProviderKey = 8
	maxProviderKey = 512
)

type providerKeyView struct {
	Provider  string     `json:"provider"`
	Name      string     `json:"name"`
	HasKey    bool       `json:"has_key"`
	Last4     string     `json:"last4,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// handleListProviderKeys is GET /api/me/provider-keys: every hosted
// provider (one that takes an API key) and whether the caller has saved a
// key for it. "enabled" is false when the server has no WS_SECRETS_KEY, so
// the page can say saving is off instead of failing on the first save.
func (s *Server) handleListProviderKeys(w http.ResponseWriter, r *http.Request) {
	p := Principal(r.Context())
	provs, err := s.DB.ListProviders(r.Context())
	if err != nil {
		writeErr(w, 500, "db")
		return
	}
	mine, err := s.DB.ListUserProviderKeys(r.Context(), p.UserID)
	if err != nil {
		writeErr(w, 500, "db")
		return
	}
	have := map[string]store.ListUserProviderKeysRow{}
	for _, k := range mine {
		have[k.ProviderID] = k
	}
	out := []providerKeyView{}
	for _, pr := range provs {
		if pr.ApiKeyEnv == nil || *pr.ApiKeyEnv == "" {
			continue // a local or keyless provider has nothing to bring
		}
		v := providerKeyView{Provider: pr.ID, Name: pr.Name}
		if k, ok := have[pr.ID]; ok {
			v.HasKey, v.Last4 = true, k.Last4
			t := k.UpdatedAt
			v.UpdatedAt = &t
		}
		out = append(out, v)
	}
	writeJSON(w, 200, map[string]any{"enabled": s.Secrets != nil, "providers": out})
}

// handlePutProviderKey is PUT /api/me/provider-keys/{provider} {key}.
func (s *Server) handlePutProviderKey(w http.ResponseWriter, r *http.Request) {
	if s.Secrets == nil {
		writeErr(w, 503, "saving your own keys is off on this server: the owner has not set WS_SECRETS_KEY")
		return
	}
	p := Principal(r.Context())
	provID := chi.URLParam(r, "provider")
	prov, ok := s.hostedProvider(w, r, provID)
	if !ok {
		return
	}
	var in struct {
		Key string `json:"key"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	key := strings.TrimSpace(in.Key)
	if len(key) < minProviderKey || len(key) > maxProviderKey || strings.IndexFunc(key, func(c rune) bool { return unicode.IsSpace(c) || unicode.IsControl(c) }) >= 0 {
		writeErr(w, 400, "that does not look like an API key")
		return
	}
	sealed, err := s.Secrets.Seal([]byte(key), sealed.ProviderKeyAAD(p.UserID.String(), prov.ID))
	if err != nil {
		writeErr(w, 500, "could not seal the key")
		return
	}
	if err := s.DB.UpsertUserProviderKey(r.Context(), store.UpsertUserProviderKeyParams{UserID: p.UserID, ProviderID: prov.ID, Sealed: sealed, Last4: key[len(key)-4:]}); err != nil {
		writeErr(w, 500, "db")
		return
	}
	s.keysChanged(p.UserID.String())
	writeJSON(w, 200, providerKeyView{Provider: prov.ID, Name: prov.Name, HasKey: true, Last4: key[len(key)-4:]})
}

// handleDeleteProviderKey is DELETE /api/me/provider-keys/{provider}.
func (s *Server) handleDeleteProviderKey(w http.ResponseWriter, r *http.Request) {
	p := Principal(r.Context())
	prov, ok := s.hostedProvider(w, r, chi.URLParam(r, "provider"))
	if !ok {
		return
	}
	if err := s.DB.DeleteUserProviderKey(r.Context(), store.DeleteUserProviderKeyParams{UserID: p.UserID, ProviderID: prov.ID}); err != nil {
		writeErr(w, 500, "db")
		return
	}
	s.keysChanged(p.UserID.String())
	writeJSON(w, 200, providerKeyView{Provider: prov.ID, Name: prov.Name})
}

// keysChanged makes a saved, removed or granted key count at once rather
// than after the gateway's short cache expires.
func (s *Server) keysChanged(userID string) {
	if s.KeyStore != nil {
		s.KeyStore.Invalidate(userID)
	}
}

// hostedProvider loads a provider that takes an API key, or answers 404.
func (s *Server) hostedProvider(w http.ResponseWriter, r *http.Request, id string) (store.Provider, bool) {
	provs, err := s.DB.ListProviders(r.Context())
	if err != nil {
		writeErr(w, 500, "db")
		return store.Provider{}, false
	}
	for _, p := range provs {
		if p.ID == id && p.ApiKeyEnv != nil && *p.ApiKeyEnv != "" {
			return p, true
		}
	}
	writeErr(w, 404, "no such provider")
	return store.Provider{}, false
}

// handleSetSharedKeys lets the owner allow or stop one member's use of the
// server's shared provider keys. Without it a member reaches hosted models
// only with a key of their own.
func (s *Server) handleSetSharedKeys(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeErr(w, 400, "bad user id")
		return
	}
	var body struct {
		Shared bool `json:"shared"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	if err := s.DB.SetUserSharedProviderKeys(r.Context(), store.SetUserSharedProviderKeysParams{ID: id, SharedProviderKeys: body.Shared}); err != nil {
		writeErr(w, 500, "db")
		return
	}
	s.keysChanged(id.String())
	writeJSON(w, 200, map[string]bool{"shared": body.Shared})
}
