package httpx

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
)

// catalogCache keeps provider catalogs for a few minutes; they're large and
// change rarely.
var catalogCache = struct {
	mu   sync.Mutex
	data map[string]struct {
		at     time.Time
		models []gateway.CatalogModel
	}
}{data: map[string]struct {
	at     time.Time
	models []gateway.CatalogModel
}{}}

// handleProviderCatalog lists the models a provider offers, flagging the
// ones that already exist as endpoints. Only OpenRouter has a catalog
// today; OpenAI-compatible local servers return /v1/models with no
// pricing, so they're handled by the seed instead.
func (s *Server) handleProviderCatalog(w http.ResponseWriter, r *http.Request) {
	pid := chi.URLParam(r, "id")
	p, ok := s.GW.Registry.Provider(pid)
	if !ok {
		writeErr(w, 404, "provider not configured on this box (no API key?)")
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	refresh := r.URL.Query().Get("refresh") == "1"

	catalogCache.mu.Lock()
	entry, cached := catalogCache.data[pid]
	catalogCache.mu.Unlock()
	var models []gateway.CatalogModel
	if cached && !refresh && time.Since(entry.at) < 10*time.Minute {
		models = entry.models
	} else {
		var err error
		switch {
		case strings.Contains(p.BaseURL, "openrouter.ai"):
			models, err = gateway.OpenRouterCatalog(r.Context(), p, nil)
		default:
			writeErr(w, 400, "no catalog for this provider; add endpoints via config/endpoints.yaml")
			return
		}
		if err != nil {
			writeErr(w, 502, err.Error())
			return
		}
		catalogCache.mu.Lock()
		catalogCache.data[pid] = struct {
			at     time.Time
			models []gateway.CatalogModel
		}{time.Now(), models}
		catalogCache.mu.Unlock()
	}
	out := make([]gateway.CatalogModel, 0, 64)
	for _, m := range models {
		if q != "" && !strings.Contains(strings.ToLower(m.ID), q) && !strings.Contains(strings.ToLower(m.Name), q) {
			continue
		}
		_, m.Added = s.GW.Registry.Endpoint(m.EndpointID)
		out = append(out, m)
		if len(out) >= 200 {
			break
		}
	}
	writeJSON(w, 200, map[string]any{"provider": pid, "count": len(models), "models": out})
}

// handleProviderRoutes lists the upstreams serving one model behind an
// aggregator (?model=author/slug), each addable as its own pinned endpoint.
func (s *Server) handleProviderRoutes(w http.ResponseWriter, r *http.Request) {
	pid := chi.URLParam(r, "id")
	p, ok := s.GW.Registry.Provider(pid)
	if !ok {
		writeErr(w, 404, "provider not configured on this box (no API key?)")
		return
	}
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	if model == "" {
		writeErr(w, 400, "model is required")
		return
	}
	if !strings.Contains(p.BaseURL, "openrouter.ai") {
		writeErr(w, 400, "this provider has no per-model routes")
		return
	}
	routes, err := gateway.OpenRouterRoutes(r.Context(), p, nil, model)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	for i := range routes {
		_, routes[i].Added = s.GW.Registry.Endpoint(routes[i].EndpointID)
	}
	writeJSON(w, 200, map[string]any{"provider": pid, "model": model, "routes": routes})
}

// handleUpsertEndpoint adds or updates an endpoint and reloads the
// registry so the model is routable immediately in this process; other
// processes pick it up on their periodic reload.
func (s *Server) handleUpsertEndpoint(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID              string               `json:"id"`
		ProviderID      string               `json:"provider_id"`
		ModelName       string               `json:"model_name"`
		DisplayName     string               `json:"display_name"`
		Capabilities    gateway.Capabilities `json:"capabilities"`
		Pricing         gateway.Pricing      `json:"pricing"`
		ThroughputClass string               `json:"throughput_class"`
		LatencyClass    string               `json:"latency_class"`
		Local           bool                 `json:"local"`
		Enabled         *bool                `json:"enabled"`
		ExtraBody       map[string]any       `json:"extra_body"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	if in.ProviderID == "" || in.ModelName == "" {
		writeErr(w, 400, "provider_id and model_name are required")
		return
	}
	if _, ok := s.GW.Registry.Provider(in.ProviderID); !ok {
		writeErr(w, 400, "unknown provider "+in.ProviderID)
		return
	}
	if in.ID == "" {
		in.ID = in.ProviderID + "/" + in.ModelName
	}
	if strings.ContainsAny(in.ID, " \t\n") {
		writeErr(w, 400, "id must not contain whitespace")
		return
	}
	if in.DisplayName == "" {
		in.DisplayName = in.ModelName
	}
	if m := in.Capabilities.Media; m != nil {
		if s.Media == nil || !s.Media.KnownEngine(m.Engine) {
			writeErr(w, 400, "unknown media engine "+m.Engine)
			return
		}
		if !(m.Image || m.ImageEdit || m.Video || m.ImageToVideo) {
			writeErr(w, 400, "a media endpoint must make images or video")
			return
		}
		for i, sz := range m.Sizes {
			m.Sizes[i] = strings.TrimSpace(sz)
		}
		if m.MaxImages > 4 {
			m.MaxImages = 4
		}
	}
	if in.ThroughputClass == "" {
		in.ThroughputClass = "medium"
	}
	if in.LatencyClass == "" {
		in.LatencyClass = "normal"
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	caps, _ := json.Marshal(in.Capabilities)
	pricing, _ := json.Marshal(in.Pricing)
	extra := []byte("{}")
	if len(in.ExtraBody) > 0 {
		extra, _ = json.Marshal(in.ExtraBody)
	}
	if err := s.DB.UpsertEndpoint(r.Context(), store.UpsertEndpointParams{
		ID: in.ID, ProviderID: in.ProviderID, ModelName: in.ModelName, DisplayName: in.DisplayName,
		Capabilities: caps, Pricing: pricing, ThroughputClass: in.ThroughputClass, LatencyClass: in.LatencyClass,
		IsLocal: in.Local, Enabled: enabled, ExtraBody: extra,
	}); err != nil {
		writeErr(w, 500, "db: "+err.Error())
		return
	}
	// UpsertEndpoint deliberately doesn't touch `enabled` on conflict (admin
	// toggles survive reseeding), so set it explicitly for UI edits.
	_ = s.DB.SetEndpointEnabled(r.Context(), store.SetEndpointEnabledParams{ID: in.ID, Enabled: enabled})
	if err := s.DB.LoadRegistry(r.Context(), s.GW.Registry); err != nil {
		s.Log.Warn("reload registry", "err", err)
	}
	ep, _ := s.GW.Registry.Endpoint(in.ID)
	writeJSON(w, 200, ep)
}

func (s *Server) handleDeleteEndpoint(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "*")
	if id == "" {
		writeErr(w, 400, "id required")
		return
	}
	if err := s.DB.DeleteEndpoint(r.Context(), id); err != nil {
		writeErr(w, 500, "db")
		return
	}
	_ = s.DB.LoadRegistry(r.Context(), s.GW.Registry)
	writeJSON(w, 200, map[string]string{"status": "ok"})
}
