package store

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
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
			p.APIKey = os.Getenv(*r.ApiKeyEnv)
			if p.APIKey == "" {
				// A hosted provider that declares a key env but has none is not
				// configured on this box; skip it so the router never tries it.
				slog.Info("provider skipped: no API key in env", "provider", r.ID, "env", *r.ApiKeyEnv)
				continue
			}
		}
		_ = json.Unmarshal(r.Headers, &p.Headers)
		provs = append(provs, p)
		have[p.ID] = true
	}
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
			Health: gateway.Health{Status: r.HealthStatus},
		}
		_ = json.Unmarshal(r.Capabilities, &e.Capabilities)
		_ = json.Unmarshal(r.Pricing, &e.Pricing)
		if len(r.ExtraBody) > 2 {
			_ = json.Unmarshal(r.ExtraBody, &e.ExtraBody)
		}
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
