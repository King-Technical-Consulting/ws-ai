package main

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
)

// runHealthChecks probes every provider every 30s and records status. For
// OpenAI-compatible providers it hits /models; for Anthropic it hits
// /v1/models. A provider-level failure marks all its endpoints down.
func runHealthChecks(ctx context.Context, db *store.DB, gw *gateway.Gateway, log *slog.Logger) {
	client := &http.Client{Timeout: 8 * time.Second}
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	check := func() {
		for _, p := range gw.Registry.Providers() {
			status, latency, errText := probe(ctx, client, p)
			for _, e := range gw.Registry.Endpoints() {
				if e.ProviderID != p.ID {
					continue
				}
				gw.Registry.SetHealth(e.ID, status, int(latency.Milliseconds()), 0, errText)
				var ep *string
				if errText != "" {
					ep = &errText
				}
				ms := int32(latency.Milliseconds())
				_ = db.UpdateEndpointHealth(ctx, store.UpdateEndpointHealthParams{
					ID: e.ID, HealthStatus: status, HealthError: ep, P50LatencyMs: &ms, ErrorRate: nil,
				})
			}
			log.Debug("health", "provider", p.ID, "status", status, "ms", latency.Milliseconds(), "err", errText)
		}
	}
	check()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			check()
		}
	}
}

func probe(ctx context.Context, client *http.Client, p *gateway.Provider) (string, time.Duration, string) {
	url := strings.TrimRight(p.BaseURL, "/")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url+"/models", nil)
	switch p.Kind {
	case gateway.ProviderAnthropic:
		req, _ = http.NewRequestWithContext(ctx, http.MethodGet, url+"/v1/models", nil)
		req.Header.Set("x-api-key", p.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	default:
		if p.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+p.APIKey)
		}
	}
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
	start := time.Now()
	resp, err := client.Do(req)
	lat := time.Since(start)
	if err != nil {
		return "down", lat, err.Error()
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		return "down", lat, "auth failed (" + resp.Status + ")"
	case resp.StatusCode >= 500:
		return "degraded", lat, resp.Status
	case resp.StatusCode >= 400:
		// Some servers 404 /models but still serve completions; treat as healthy.
		return "healthy", lat, ""
	}
	return "healthy", lat, ""
}
