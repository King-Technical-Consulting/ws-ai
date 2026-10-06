package gateway

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// EmbedRequest asks for embeddings of Inputs. Model is a selector like
// Request.Model; the task class is always embed, so only endpoints with
// the embeddings capability qualify.
type EmbedRequest struct {
	Model  string   `json:"model"`
	Inputs []string `json:"inputs"`
	// Dimensions asks the model for vectors of this width when it supports
	// it (OpenAI's text-embedding-3 models); 0 leaves the model's default.
	Dimensions int      `json:"dimensions,omitempty"`
	Metadata   Metadata `json:"metadata"`
}

// EmbedResponse is one vector per input, in order.
type EmbedResponse struct {
	EndpointID string
	Model      string
	Vectors    [][]float32
	Usage      Usage
}

// Embedder is implemented by adapters that can embed text (the
// OpenAI-compatible adapter; Anthropic has no embeddings API).
type Embedder interface {
	Embed(ctx context.Context, p *Provider, ep *Endpoint, req *EmbedRequest) (*EmbedResponse, error)
}

// ErrNoEmbedder says the routed provider's adapter cannot embed.
var ErrNoEmbedder = errors.New("gateway: adapter cannot embed")

// Embed routes an embedding request like a chat request (middlewares,
// policies, budgets), tries candidates in order, and writes a ledger row.
func (g *Gateway) Embed(ctx context.Context, req *EmbedRequest) (*EmbedResponse, error) {
	if len(req.Inputs) == 0 {
		return &EmbedResponse{}, nil
	}
	req.Metadata.TaskClass = TaskEmbed
	chatReq := &Request{Model: req.Model, Metadata: req.Metadata}
	cands, dec, err := g.Prepare(ctx, chatReq)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	var lastErr error
	for _, c := range cands {
		ad, ok := g.adapters[c.Provider.Kind]
		if !ok {
			lastErr = fmt.Errorf("gateway: no adapter for %s", c.Provider.Kind)
			continue
		}
		em, ok := ad.(Embedder)
		if !ok {
			lastErr = fmt.Errorf("%w: %s", ErrNoEmbedder, c.Provider.Kind)
			continue
		}
		dec.Tried = append(dec.Tried, c.Endpoint.ID)
		resp, err := em.Embed(ctx, c.Provider, c.Endpoint, req)
		if err != nil {
			lastErr = err
			g.Log.Warn("gateway: embed failed, trying next", "endpoint", c.Endpoint.ID, "err", err)
			if ctx.Err() != nil {
				break
			}
			continue
		}
		resp.EndpointID = c.Endpoint.ID
		if resp.Model == "" {
			resp.Model = c.Endpoint.ModelName
		}
		g.record(ctx, UsageRecord{Metadata: req.Metadata, EndpointID: c.Endpoint.ID, Model: resp.Model,
			Decision: withChosen(dec, c.Endpoint.ID), Usage: resp.Usage, CostUSD: c.Endpoint.Pricing.Cost(resp.Usage),
			Latency: time.Since(start), FinishReason: FinishStop})
		return resp, nil
	}
	if lastErr == nil {
		lastErr = ErrNoRoute
	}
	g.record(ctx, UsageRecord{Metadata: req.Metadata, Decision: dec, Latency: time.Since(start), FinishReason: FinishError, Err: lastErr.Error()})
	return nil, fmt.Errorf("gateway: embed: all %d candidates failed: %w", len(cands), lastErr)
}
