package store

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jking323/ws/internal/gateway"
)

// SeedRegistry upserts providers and endpoints from the YAML seed into
// Postgres. Endpoint "enabled" is only set on first insert so admin toggles
// survive restarts.
func (d *DB) SeedRegistry(ctx context.Context, sf *gateway.SeedFile) error {
	for _, p := range sf.Providers {
		hdr, _ := json.Marshal(p.Headers)
		if hdr == nil || string(hdr) == "null" {
			hdr = []byte("{}")
		}
		base := p.BaseURL
		if p.BaseURLEnv != "" {
			base = "env:" + p.BaseURLEnv
		}
		var keyEnv *string
		if p.APIKeyEnv != "" {
			keyEnv = &p.APIKeyEnv
		}
		if err := d.UpsertProvider(ctx, UpsertProviderParams{ID: p.ID, Kind: string(p.Kind), Name: p.Name, BaseUrl: base, ApiKeyEnv: keyEnv, Headers: hdr}); err != nil {
			return err
		}
	}
	for _, e := range sf.Endpoints {
		caps, _ := json.Marshal(e.Capabilities)
		pricing, _ := json.Marshal(e.Pricing)
		enabled := true
		if e.Enabled != nil {
			enabled = *e.Enabled
		}
		tc, lc := e.ThroughputClass, e.LatencyClass
		if tc == "" {
			tc = "medium"
		}
		if lc == "" {
			lc = "normal"
		}
		extra := []byte("{}")
		if len(e.ExtraBody) > 0 {
			extra, _ = json.Marshal(e.ExtraBody)
		}
		if err := d.UpsertEndpoint(ctx, UpsertEndpointParams{
			ID: e.ID, ProviderID: e.Provider, ModelName: e.Model, DisplayName: e.DisplayName,
			Capabilities: caps, Pricing: pricing, ThroughputClass: tc, LatencyClass: lc, IsLocal: e.Local, Enabled: enabled,
			ExtraBody: extra,
		}); err != nil {
			return err
		}
		// Only what the file says: an omitted tier or routable leaves what
		// the owner set in Admin.
		if e.Tier != "" || e.Routable != nil {
			cur, err := d.GetEndpoint(ctx, e.ID)
			if err != nil {
				return err
			}
			tier, routable := cur.Tier, cur.Routable
			if e.Tier != "" {
				tier = e.Tier
			}
			if e.Routable != nil {
				routable = *e.Routable
			}
			if err := d.SetEndpointRouting(ctx, SetEndpointRoutingParams{ID: e.ID, Tier: tier, Routable: routable}); err != nil {
				return err
			}
		}
	}
	return nil
}

// LoadRegistry reads providers and endpoints from Postgres into the
// in-memory registry, resolving env references for keys and base URLs.
// Providers whose base URL env is unset are skipped with their endpoints.
func (d *DB) LoadRegistry(ctx context.Context, reg *gateway.Registry) error {
	provRows, err := d.ListProviders(ctx)
	if err != nil {
		return err
	}
	var provs []*gateway.Provider
	var noKey []string
	have := map[string]bool{}
	for _, r := range provRows {
		base := r.BaseUrl
		if len(base) > 4 && base[:4] == "env:" {
			base = os.Getenv(base[4:])
			if base == "" {
				continue
			}
		}
		p := &gateway.Provider{ID: r.ID, Kind: gateway.ProviderKind(r.Kind), Name: r.Name, BaseURL: base}
		if r.ApiKeyEnv != nil && *r.ApiKeyEnv != "" {
			p.Hosted = true
			p.APIKey = os.Getenv(*r.ApiKeyEnv)
			if p.APIKey == "" {
				// No server key: the provider stays registered and serves only
				// people who saved a key of their own for it (gateway/keys.go).
				noKey = append(noKey, r.ID+" ("+*r.ApiKeyEnv+")")
			}
		}
		_ = json.Unmarshal(r.Headers, &p.Headers)
		provs = append(provs, p)
		have[p.ID] = true
	}
	logNoServerKey(noKey)
	epRows, err := d.ListEndpoints(ctx)
	if err != nil {
		return err
	}
	var eps []*gateway.Endpoint
	for _, r := range epRows {
		if !have[r.ProviderID] {
			continue
		}
		e := &gateway.Endpoint{
			ID: r.ID, ProviderID: r.ProviderID, ModelName: r.ModelName, DisplayName: r.DisplayName,
			ThroughputClass: r.ThroughputClass, LatencyClass: r.LatencyClass, Local: r.IsLocal, Enabled: r.Enabled,
			Tier: r.Tier, NotRoutable: !r.Routable,
			Health: gateway.Health{Status: r.HealthStatus},
		}
		_ = json.Unmarshal(r.Capabilities, &e.Capabilities)
		_ = json.Unmarshal(r.Pricing, &e.Pricing)
		if len(r.ExtraBody) > 2 {
			_ = json.Unmarshal(r.ExtraBody, &e.ExtraBody)
		}
		e.LiftDefaultSystem()
		if r.P50LatencyMs != nil {
			e.Health.P50LatencyMS = int(*r.P50LatencyMs)
		}
		if r.ErrorRate != nil {
			e.Health.ErrorRate = float64(*r.ErrorRate)
		}
		if r.HealthError != nil {
			e.Health.Error = *r.HealthError
		}
		eps = append(eps, e)
	}
	reg.Replace(provs, eps)
	return nil
}

// LoadPolicies reads enabled routing policies from Postgres.
func (d *DB) LoadPolicies(ctx context.Context) ([]gateway.Policy, error) {
	rows, err := d.ListRoutingPolicies(ctx)
	if err != nil {
		return nil, err
	}
	var out []gateway.Policy
	for _, r := range rows {
		p, err := gateway.ParsePolicy(r.Yaml)
		if err != nil {
			slog.Warn("skipping bad routing policy", "name", r.Name, "err", err)
			continue
		}
		p.Name = r.Name
		p.Priority = int(r.Priority)
		out = append(out, p)
	}
	return out, nil
}

// SeedPolicies upserts policies from disk into Postgres.
func (d *DB) SeedPolicies(ctx context.Context, dir string) error {
	ps, err := gateway.LoadPolicyDir(dir)
	if err != nil {
		return err
	}
	for _, p := range ps {
		y, err := yamlOf(p)
		if err != nil {
			return err
		}
		if _, err := d.UpsertRoutingPolicy(ctx, UpsertRoutingPolicyParams{Name: p.Name, Yaml: y, Priority: int32(p.Priority), Enabled: true}); err != nil {
			return err
		}
	}
	return nil
}

// UsageRecorder implements gateway.UsageRecorder on Postgres.
type UsageRecorder struct {
	DB  *DB
	Log *slog.Logger
}

// Record implements gateway.UsageRecorder.
func (u *UsageRecorder) Record(ctx context.Context, rec gateway.UsageRecord) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	md := rec.Metadata
	params := InsertUsageParams{
		UserID:           parseNullUUID(md.UserID),
		ConversationID:   parseNullUUID(md.ConversationID),
		AgentID:          parseNullUUID(md.AgentID),
		AgentRunID:       parseNullUUID(md.AgentRunID),
		ApiKeyID:         parseNullUUID(md.APIKeyID),
		EndpointID:       nilIfEmpty(rec.EndpointID),
		Model:            nilIfEmpty(rec.Model),
		TaskClass:        nilIfEmpty(string(md.TaskClass)),
		PolicyName:       nilIfEmpty(rec.Decision.Policy),
		Decision:         gateway.DecisionJSON(rec.Decision),
		InputTokens:      int32(rec.Usage.InputTokens),
		OutputTokens:     int32(rec.Usage.OutputTokens),
		CacheReadTokens:  int32(rec.Usage.CacheReadTokens),
		CacheWriteTokens: int32(rec.Usage.CacheWriteTokens),
		CostUsd:          rec.CostUSD,
		LatencyMs:        ptrInt32(int32(rec.Latency.Milliseconds())),
		TtftMs:           ptrInt32(int32(rec.TTFT.Milliseconds())),
		FinishReason:     nilIfEmpty(string(rec.FinishReason)),
		Error:            nilIfEmpty(rec.Err),
		SessionID:        nilIfEmpty(md.SessionID),
		OwnKey:           rec.OwnKey,
	}
	if _, err := u.DB.InsertUsage(ctx, params); err != nil {
		log := u.Log
		if log == nil {
			log = slog.Default()
		}
		log.Error("usage ledger insert failed", "err", err)
	}
}

func parseNullUUID(s string) uuid.NullUUID {
	if s == "" {
		return uuid.NullUUID{}
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.NullUUID{}
	}
	return uuid.NullUUID{UUID: id, Valid: true}
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func ptrInt32(v int32) *int32 { return &v }

var (
	noKeyMu   sync.Mutex
	noKeyLast string
	noKeySeen bool
)

// logNoServerKey says which hosted providers have no server key, once and
// again only when that set changes: the registry reloads every half minute
// and a line per provider each time buried the lines that matter.
func logNoServerKey(missing []string) {
	sort.Strings(missing)
	key := strings.Join(missing, ", ")
	noKeyMu.Lock()
	defer noKeyMu.Unlock()
	if noKeySeen && key == noKeyLast {
		return
	}
	noKeySeen, noKeyLast = true, key
	if key == "" {
		slog.Info("every hosted provider has a server key")
		return
	}
	slog.Info("hosted providers without a server key (own keys only)", "providers", key)
}

// RouteCapKey is the server_settings key of the daily ceiling on
// verdict-routing classifier spend, in US dollars.
const RouteCapKey = "route_classifier_max_usd_day"

// RouteClassifierCap returns the owner's daily ceiling, or def when the
// owner has not set one. A stored value of 0 or less means no ceiling.
func (d *DB) RouteClassifierCap(ctx context.Context, def float64) float64 {
	row, err := d.GetServerSetting(ctx, RouteCapKey)
	if err != nil {
		return def
	}
	var v float64
	if json.Unmarshal(row.Value, &v) != nil {
		return def
	}
	return v
}

// SetRouteClassifierCap stores the owner's daily ceiling.
func (d *DB) SetRouteClassifierCap(ctx context.Context, usd float64) error {
	b, _ := json.Marshal(usd)
	return d.SetServerSetting(ctx, SetServerSettingParams{Key: RouteCapKey, Value: b})
}

// RouteSpendToday is what verdict calls have cost since UTC midnight.
func (d *DB) RouteSpendToday(ctx context.Context) (float64, error) {
	now := time.Now().UTC()
	return d.SumRouteSpendSince(ctx, time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC))
}
