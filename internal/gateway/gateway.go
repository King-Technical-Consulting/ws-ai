package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// UsageRecord is one ledger row. The store package persists it; the gateway
// only produces it.
type UsageRecord struct {
	Metadata     Metadata
	EndpointID   string
	Model        string
	Decision     Decision
	Usage        Usage
	CostUSD      float64
	Latency      time.Duration
	TTFT         time.Duration
	FinishReason FinishReason
	Err          string
}

// UsageRecorder persists ledger rows. Implementations must not block the
// stream; the gateway calls Record from a goroutine.
type UsageRecorder interface {
	Record(ctx context.Context, rec UsageRecord)
}

// Middleware wraps a request before routing. Compaction and budget checks
// are middleware. They may mutate the request or RouteInput.
type Middleware func(ctx context.Context, req *Request, in *RouteInput) error

// Gateway routes canonical requests to adapters with failover and logging.
type Gateway struct {
	Registry *Registry
	Router   *Router
	adapters map[ProviderKind]Adapter
	recorder UsageRecorder
	mws      []Middleware
	Log      *slog.Logger
}

// New builds a gateway.
func New(reg *Registry, router *Router, rec UsageRecorder, log *slog.Logger) *Gateway {
	if log == nil {
		log = slog.Default()
	}
	return &Gateway{Registry: reg, Router: router, adapters: map[ProviderKind]Adapter{}, recorder: rec, Log: log}
}

// RegisterAdapter installs an adapter for a provider kind.
func (g *Gateway) RegisterAdapter(kind ProviderKind, a Adapter) { g.adapters[kind] = a }

// Use appends middleware, run in order before routing.
func (g *Gateway) Use(m ...Middleware) { g.mws = append(g.mws, m...) }

// ErrNoRoute means no endpoint could serve the request.
var ErrNoRoute = errors.New("gateway: no route")

// Stream routes and streams. Failover happens only before the first token:
// if a candidate returns a retryable error with nothing emitted, the next
// candidate is tried. The returned channel always ends with EventFinish or
// EventError.
func (g *Gateway) Stream(ctx context.Context, req *Request) (<-chan StreamEvent, error) {
	cands, dec, err := g.Prepare(ctx, req)
	if err != nil {
		return nil, err
	}
	return g.StreamCandidates(ctx, req, cands, dec), nil
}

// Prepare runs the middlewares (budgets, compaction) and routes, without
// starting a stream. Stream uses it; the external API uses it on its own
// to decide between the translate path and a raw passthrough to a native
// Anthropic endpoint, then calls StreamCandidates or proxies itself.
func (g *Gateway) Prepare(ctx context.Context, req *Request) ([]Candidate, Decision, error) {
	in := RouteInput{
		Selector:  req.Model,
		TaskClass: req.Metadata.TaskClass,
		UserID:    req.Metadata.UserID,
		AgentID:   req.Metadata.AgentID,
		External:  req.Metadata.External,
		Required:  RequiredCapabilities(req),
	}
	if in.Selector == "" {
		in.Selector = "auto"
	}
	if in.TaskClass == "" {
		in.TaskClass = TaskChat
	}
	for _, mw := range g.mws {
		if err := mw(ctx, req, &in); err != nil {
			return nil, Decision{}, err
		}
	}
	cands, dec, err := g.Router.Route(in)
	if err != nil {
		g.record(ctx, UsageRecord{Metadata: req.Metadata, Decision: dec, Err: err.Error()})
		return nil, dec, fmt.Errorf("%w: %v", ErrNoRoute, err)
	}
	return cands, dec, nil
}

// StreamCandidates streams over an already routed candidate list, with
// the same failover and ledger behaviour as Stream.
func (g *Gateway) StreamCandidates(ctx context.Context, req *Request, cands []Candidate, dec Decision) <-chan StreamEvent {
	out := make(chan StreamEvent, 64)
	go g.run(ctx, req, cands, dec, out)
	return out
}

// Record writes a ledger row for a call the gateway did not run itself,
// such as the external API's passthrough, so budgets and the ledger see
// every call whichever path served it.
func (g *Gateway) Record(ctx context.Context, rec UsageRecord) { g.record(ctx, rec) }

func (g *Gateway) run(ctx context.Context, req *Request, cands []Candidate, dec Decision, out chan<- StreamEvent) {
	defer close(out)
	start := time.Now()
	var lastErr error

	for i, c := range cands {
		ad, ok := g.adapters[c.Provider.Kind]
		if !ok {
			lastErr = fmt.Errorf("gateway: no adapter for %s", c.Provider.Kind)
			continue
		}
		dec.Tried = append(dec.Tried, c.Endpoint.ID)
		g.Log.Debug("gateway: trying endpoint", "endpoint", c.Endpoint.ID, "attempt", i+1, "task", req.Metadata.TaskClass)

		ch, err := ad.Stream(ctx, c.Provider, c.Endpoint, req)
		if err != nil {
			lastErr = err
			continue // adapter-side build errors are not provider faults; try next
		}

		var usage Usage
		var finish FinishReason
		var ttft time.Duration
		emitted := false
		failedRetryable := false

		for ev := range ch {
			switch ev.Type {
			case EventError:
				if ev.Retryable && !emitted {
					failedRetryable = true
					lastErr = ev.Err
					g.Log.Warn("gateway: retryable failure, failing over", "endpoint", c.Endpoint.ID, "err", ev.ErrText)
					break
				}
				// Non-retryable or mid-stream: surface and stop.
				lastErr = ev.Err
				out <- ev
				g.record(ctx, UsageRecord{Metadata: req.Metadata, EndpointID: c.Endpoint.ID, Model: c.Endpoint.ModelName,
					Decision: withChosen(dec, c.Endpoint.ID), Usage: usage, CostUSD: c.Endpoint.Pricing.Cost(usage),
					Latency: time.Since(start), TTFT: ttft, FinishReason: FinishError, Err: ev.ErrText})
				return
			case EventTextDelta, EventToolCallStart, EventReasoningDelta:
				if !emitted {
					emitted = true
					ttft = time.Since(start)
				}
				out <- ev
			case EventUsage:
				if ev.Usage != nil {
					usage.Add(*ev.Usage)
				}
				out <- ev
			case EventFinish:
				finish = ev.FinishReason
				out <- ev
			default:
				out <- ev
			}
			if failedRetryable {
				break
			}
		}
		if failedRetryable {
			// drain remaining events so the adapter goroutine can exit
			for range ch {
			}
			continue
		}
		g.record(ctx, UsageRecord{Metadata: req.Metadata, EndpointID: c.Endpoint.ID, Model: c.Endpoint.ModelName,
			Decision: withChosen(dec, c.Endpoint.ID), Usage: usage, CostUSD: c.Endpoint.Pricing.Cost(usage),
			Latency: time.Since(start), TTFT: ttft, FinishReason: finish})
		return
	}

	if lastErr == nil {
		lastErr = ErrNoRoute
	}
	out <- ErrorEvent(fmt.Errorf("gateway: all %d candidates failed: %w", len(cands), lastErr), false)
	g.record(ctx, UsageRecord{Metadata: req.Metadata, Decision: dec, Latency: time.Since(start), FinishReason: FinishError, Err: lastErr.Error()})
}

// Complete is a convenience that streams and accumulates.
func (g *Gateway) Complete(ctx context.Context, req *Request) (*Response, error) {
	ch, err := g.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	return Accumulate(ch)
}

func (g *Gateway) record(ctx context.Context, rec UsageRecord) {
	if g.recorder == nil {
		return
	}
	// Detach from the request context so a cancelled stream still logs.
	bg := context.WithoutCancel(ctx)
	go g.recorder.Record(bg, rec)
}

func withChosen(d Decision, id string) Decision {
	d.Chosen = id
	return d
}

// DecisionJSON serializes a decision for the ledger.
func DecisionJSON(d Decision) json.RawMessage {
	b, _ := json.Marshal(d)
	return b
}
