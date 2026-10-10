package gateway

import (
	"fmt"
	"os"
	"sort"
	"sync"

	"gopkg.in/yaml.v3"
)

// Registry is the in-memory view of providers and endpoints. The store
// package loads it from Postgres and refreshes it on NOTIFY; the YAML seed
// populates Postgres on boot.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]*Provider
	endpoints map[string]*Endpoint
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{providers: map[string]*Provider{}, endpoints: map[string]*Endpoint{}}
}

// Replace swaps the whole contents atomically.
func (r *Registry) Replace(providers []*Provider, endpoints []*Endpoint) {
	pm := make(map[string]*Provider, len(providers))
	for _, p := range providers {
		pm[p.ID] = p
	}
	em := make(map[string]*Endpoint, len(endpoints))
	for _, e := range endpoints {
		em[e.ID] = e
	}
	r.mu.Lock()
	r.providers, r.endpoints = pm, em
	r.mu.Unlock()
}

// Provider looks up a provider.
func (r *Registry) Provider(id string) (*Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[id]
	return p, ok
}

// Endpoint looks up an endpoint.
func (r *Registry) Endpoint(id string) (*Endpoint, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.endpoints[id]
	return e, ok
}

// Endpoints returns all endpoints sorted by ID.
func (r *Registry) Endpoints() []*Endpoint {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Endpoint, 0, len(r.endpoints))
	for _, e := range r.endpoints {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Providers returns all providers sorted by ID.
func (r *Registry) Providers() []*Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Provider, 0, len(r.providers))
	for _, p := range r.providers {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// A published *Endpoint is read without a lock by the router, the HTTP
// handlers and json.Marshal, so it must never change. The two fields that
// do change at runtime, Enabled and Health, are updated by storing a
// modified copy under the registry's lock: a reader that already holds a
// pointer keeps a consistent snapshot, and the next lookup sees the new one.

// SetHealth records an endpoint's health (called by the health checker).
func (r *Registry) SetHealth(id string, status string, p50ms int, errRate float64, errText string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.endpoints[id]; ok {
		n := *e
		n.Health = Health{Status: status, P50LatencyMS: p50ms, ErrorRate: errRate, Error: errText}
		r.endpoints[id] = &n
	}
}

// SetEnabled switches an endpoint on or off. It reports whether the
// endpoint exists. A request that already picked the endpoint finishes on
// it; the toggle has never cancelled running requests.
func (r *Registry) SetEnabled(id string, enabled bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.endpoints[id]
	if !ok {
		return false
	}
	n := *e
	n.Enabled = enabled
	r.endpoints[id] = &n
	return true
}

// Health is the runtime health of an endpoint.
type Health struct {
	Status       string  `json:"status"` // unknown | healthy | degraded | down
	P50LatencyMS int     `json:"p50_latency_ms"`
	ErrorRate    float64 `json:"error_rate"`
	Error        string  `json:"error,omitempty"`
}

// ---- YAML seed ----

// SeedFile is the on-disk shape of config/endpoints.yaml.
type SeedFile struct {
	Providers []SeedProvider `yaml:"providers"`
	Endpoints []SeedEndpoint `yaml:"endpoints"`
}

// SeedProvider is one provider entry.
type SeedProvider struct {
	ID         string            `yaml:"id"`
	Kind       ProviderKind      `yaml:"kind"`
	Name       string            `yaml:"name"`
	BaseURL    string            `yaml:"base_url"`
	BaseURLEnv string            `yaml:"base_url_env"`
	APIKeyEnv  string            `yaml:"api_key_env"`
	Headers    map[string]string `yaml:"headers"`
}

// SeedEndpoint is one endpoint entry.
type SeedEndpoint struct {
	ID              string       `yaml:"id"`
	Provider        string       `yaml:"provider"`
	Model           string       `yaml:"model"`
	DisplayName     string       `yaml:"display_name"`
	Capabilities    Capabilities `yaml:"capabilities"`
	Pricing         Pricing      `yaml:"pricing"`
	ThroughputClass string       `yaml:"throughput_class"`
	LatencyClass    string       `yaml:"latency_class"`
	Local           bool         `yaml:"local"`
	Enabled         *bool        `yaml:"enabled"`
	// ExtraBody is merged into every request to this endpoint; see
	// Endpoint.ExtraBody.
	ExtraBody map[string]any `yaml:"extra_body"`
}

// LoadSeed parses the YAML file.
func LoadSeed(path string) (*SeedFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sf SeedFile
	if err := yaml.Unmarshal(b, &sf); err != nil {
		return nil, fmt.Errorf("endpoints.yaml: %w", err)
	}
	for _, p := range sf.Providers {
		if p.ID == "" || p.Kind == "" {
			return nil, fmt.Errorf("endpoints.yaml: provider missing id or kind")
		}
		if p.Kind != ProviderAnthropic && p.Kind != ProviderOpenAICompat {
			return nil, fmt.Errorf("endpoints.yaml: provider %s: unknown kind %q", p.ID, p.Kind)
		}
	}
	ids := map[string]bool{}
	for _, p := range sf.Providers {
		ids[p.ID] = true
	}
	for _, e := range sf.Endpoints {
		if !ids[e.Provider] {
			return nil, fmt.Errorf("endpoints.yaml: endpoint %s references unknown provider %q", e.ID, e.Provider)
		}
	}
	return &sf, nil
}

// Resolve turns seed entries into runtime values, reading env for keys and
// base URLs. Providers whose base_url_env is unset are skipped along with
// their endpoints, so one file serves every hardware target. A hosted
// provider whose key env is unset stays, with no key: people who save their
// own key for it can use it, nobody else can (keys.go).
func (sf *SeedFile) Resolve(getenv func(string) string) ([]*Provider, []*Endpoint) {
	var provs []*Provider
	have := map[string]bool{}
	for _, sp := range sf.Providers {
		base := sp.BaseURL
		if sp.BaseURLEnv != "" {
			base = getenv(sp.BaseURLEnv)
			if base == "" {
				continue
			}
		}
		p := &Provider{ID: sp.ID, Kind: sp.Kind, Name: sp.Name, BaseURL: base, Headers: sp.Headers, Hosted: sp.APIKeyEnv != ""}
		if sp.APIKeyEnv != "" {
			p.APIKey = getenv(sp.APIKeyEnv)
		}
		provs = append(provs, p)
		have[p.ID] = true
	}
	var eps []*Endpoint
	for _, se := range sf.Endpoints {
		if !have[se.Provider] {
			continue
		}
		enabled := true
		if se.Enabled != nil {
			enabled = *se.Enabled
		}
		eps = append(eps, &Endpoint{
			ID:              se.ID,
			ProviderID:      se.Provider,
			ModelName:       se.Model,
			DisplayName:     se.DisplayName,
			Capabilities:    se.Capabilities,
			Pricing:         se.Pricing,
			ThroughputClass: se.ThroughputClass,
			LatencyClass:    se.LatencyClass,
			Local:           se.Local,
			Enabled:         enabled,
		})
	}
	return provs, eps
}
