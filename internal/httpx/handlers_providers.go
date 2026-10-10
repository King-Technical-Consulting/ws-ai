package httpx

import (
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
)

// providerView is a providers row as Admin sees it: the key is named,
// never shown, and Configured says whether this box resolved it.
type providerView struct {
	ID         string            `json:"id"`
	Kind       string            `json:"kind"`
	Name       string            `json:"name"`
	BaseURL    string            `json:"base_url"`     // literal URL, or "" when it comes from a variable
	BaseURLEnv string            `json:"base_url_env"` // the variable, when the URL comes from one
	APIKeyEnv  string            `json:"api_key_env"`
	Headers    map[string]string `json:"headers"`
	Configured bool              `json:"configured"`
	Reason     string            `json:"reason,omitempty"` // why it is not configured
}

func viewProvider(p store.Provider, reg *gateway.Registry) providerView {
	v := providerView{ID: p.ID, Kind: p.Kind, Name: p.Name, BaseURL: p.BaseUrl, Headers: map[string]string{}}
	if strings.HasPrefix(p.BaseUrl, "env:") {
		v.BaseURLEnv, v.BaseURL = strings.TrimPrefix(p.BaseUrl, "env:"), ""
	}
	if p.ApiKeyEnv != nil {
		v.APIKeyEnv = *p.ApiKeyEnv
	}
	_ = json.Unmarshal(p.Headers, &v.Headers)
	rp, loaded := reg.Provider(p.ID)
	// A hosted provider loaded without a server key is registered but not
	// configured for the box: only people with their own key reach it.
	v.Configured = loaded && !(rp.Hosted && rp.APIKey == "")
	if !v.Configured {
		switch {
		case v.BaseURLEnv != "" && os.Getenv(v.BaseURLEnv) == "":
			v.Reason = v.BaseURLEnv + " is not set on this box"
		case v.APIKeyEnv != "" && os.Getenv(v.APIKeyEnv) == "":
			v.Reason = v.APIKeyEnv + " is not set on this box; only people who saved their own key can use it"
		default:
			v.Reason = "not loaded yet; the registry reloads within 30 s"
		}
	}
	return v
}

var providerIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
var envNameRe = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)

// handleUpsertProvider adds or edits a provider (a model runner: the base
// URL and the name of the variable that holds its key). The key value
// never passes through here; it lives in .env or Infisical. The seed
// re-applies config/endpoints.yaml on boot, so a provider that is also in
// the seed takes the seed's values again after a restart.
func (s *Server) handleUpsertProvider(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID         string            `json:"id"`
		Kind       string            `json:"kind"`
		Name       string            `json:"name"`
		BaseURL    string            `json:"base_url"`
		BaseURLEnv string            `json:"base_url_env"`
		APIKeyEnv  string            `json:"api_key_env"`
		Headers    map[string]string `json:"headers"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	in.ID = strings.TrimSpace(in.ID)
	if !providerIDRe.MatchString(in.ID) {
		writeErr(w, 400, "id: lowercase letters, digits, dots, dashes and underscores")
		return
	}
	switch gateway.ProviderKind(in.Kind) {
	case gateway.ProviderAnthropic, gateway.ProviderOpenAICompat:
	default:
		writeErr(w, 400, "kind must be anthropic or openai_compat")
		return
	}
	in.BaseURL, in.BaseURLEnv, in.APIKeyEnv = strings.TrimSpace(in.BaseURL), strings.TrimSpace(in.BaseURLEnv), strings.TrimSpace(in.APIKeyEnv)
	base := in.BaseURL
	switch {
	case in.BaseURLEnv != "":
		if !envNameRe.MatchString(in.BaseURLEnv) {
			writeErr(w, 400, "base_url_env must be an environment variable name")
			return
		}
		base = "env:" + in.BaseURLEnv
	case strings.HasPrefix(base, "http://") || strings.HasPrefix(base, "https://"):
	default:
		writeErr(w, 400, "base_url must start with http:// or https://, or set base_url_env")
		return
	}
	var keyEnv *string
	if in.APIKeyEnv != "" {
		if !envNameRe.MatchString(in.APIKeyEnv) {
			writeErr(w, 400, "api_key_env must be an environment variable name")
			return
		}
		keyEnv = &in.APIKeyEnv
	}
	if in.Name == "" {
		in.Name = in.ID
	}
	if in.Headers == nil {
		in.Headers = map[string]string{}
	}
	hdr, _ := json.Marshal(in.Headers)
	if err := s.DB.UpsertProvider(r.Context(), store.UpsertProviderParams{ID: in.ID, Kind: in.Kind, Name: in.Name, BaseUrl: base, ApiKeyEnv: keyEnv, Headers: hdr}); err != nil {
		writeErr(w, 500, "db: "+err.Error())
		return
	}
	if err := s.DB.LoadRegistry(r.Context(), s.GW.Registry); err != nil {
		s.Log.Warn("reload registry", "err", err)
	}
	rows, _ := s.DB.ListProviders(r.Context())
	for _, p := range rows {
		if p.ID == in.ID {
			writeJSON(w, 200, viewProvider(p, s.GW.Registry))
			return
		}
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// handleDeleteProvider removes a provider and, through the foreign key,
// its endpoints.
func (s *Server) handleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeErr(w, 400, "id required")
		return
	}
	if err := s.DB.DeleteProvider(r.Context(), id); err != nil {
		writeErr(w, 500, "db: "+err.Error())
		return
	}
	_ = s.DB.LoadRegistry(r.Context(), s.GW.Registry)
	writeJSON(w, 200, map[string]string{"status": "ok"})
}
