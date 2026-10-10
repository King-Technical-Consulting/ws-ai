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
	// OwnKey: the call was made with the user's own provider key, so the
	// spend is theirs and does not count against any budget.
	OwnKey bool
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
	// Priority ranks a request when an endpoint is full and requests wait
	// for a slot: higher goes first (the owner's before a member's). nil
	// means every request is equal and the queue is first come, first served.
	Priority func(ctx context.Context, md Metadata) int
	// Keys says which hosted providers a user may use and with whose key
	// (keys.go). nil leaves every provider open to every request.
	Keys KeySource
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
		Keys:      g.accessFor(ctx, req.Metadata.UserID),
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
		return nil, dec, fmt.Errorf("%w: %w", ErrNoRoute, err)
	}
	cands = in.Keys.credit(cands)
	if len(cands) == 0 {
		err := fmt.Errorf("%w: %v", ErrNoRoute, ErrNeedsOwnKey)
		g.record(ctx, UsageRecord{Metadata: req.Metadata, Decision: dec, Err: err.Error()})
		return nil, dec, err
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

		release := func() {}
		if g.Router != nil {
			prio := PriorityDefault
			if g.Priority != nil {
				prio = g.Priority(ctx, req.Metadata)
			}
			r, err := g.Router.Throughput().Acquire(ctx, c.Endpoint.ID, c.Endpoint.Capabilities.MaxConcurrency, prio)
			if err != nil {
				// The caller went away while waiting for a slot.
				out <- ErrorEvent(err, false)
				return
			}
			release = r
		}
		ch, err := ad.Stream(ctx, c.Provider, c.Endpoint, req)
		if err != nil {
			release()
			lastErr = err
			continue // adapter-side build errors are not provider faults; try next
		}

		var usage Usage
		var finish FinishReason
		var ttft time.Duration
		attemptStart := time.Now() // per attempt, so a failed first candidate does not inflate this one's TTFT
		streamedChars := 0
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
				release()
				g.record(ctx, UsageRecord{Metadata: req.Metadata, EndpointID: c.Endpoint.ID, Model: c.Endpoint.ModelName,
					Decision: withChosen(dec, c.Endpoint.ID), Usage: usage, CostUSD: c.Endpoint.Pricing.Cost(usage),
					Latency: time.Since(start), TTFT: ttft, FinishReason: FinishError, Err: ev.ErrText, OwnKey: c.OwnKey})
				return
			case EventTextDelta, EventToolCallStart, EventReasoningDelta:
				if !emitted {
					emitted = true
					ttft = time.Since(attemptStart)
				}
				streamedChars += len(ev.Text) + len(ev.ArgsDelta)
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
		release()
		if failedRetryable {
			// drain remaining events so the adapter goroutine can exit
			for range ch {
			}
			continue
		}
		total := time.Since(start)
		// total here is for this attempt; the ledger row below keeps the
		// whole request's latency.
		if finish != FinishError && g.Router != nil && !hiddenOutput(usage.OutputTokens, streamedChars) {
			g.Router.Throughput().Observe(c.Endpoint.ID, usage.OutputTokens, ttft, time.Since(attemptStart))
		}
		g.record(ctx, UsageRecord{Metadata: req.Metadata, EndpointID: c.Endpoint.ID, Model: c.Endpoint.ModelName,
			Decision: withChosen(dec, c.Endpoint.ID), Usage: usage, CostUSD: c.Endpoint.Pricing.Cost(usage),
			Latency: total, TTFT: ttft, FinishReason: finish, OwnKey: c.OwnKey})
		return
	}

	if lastErr == nil {
		lastErr = ErrNoRoute
	}
	out <- ErrorEvent(fmt.Errorf("gateway: all %d candidates failed: %w", len(cands), lastErr), false)
	g.record(ctx, UsageRecord{Metadata: req.Metadata, Decision: dec, Latency: time.Since(start), FinishReason: FinishError, Err: lastErr.Error()})
}

// hiddenOutput reports whether the provider billed far more output tokens
// than the stream carried: a reasoning model that thinks silently and then
// streams a short answer. Dividing its billed tokens by the short decode
// window would rate it far too fast, so the sample is dropped. About four
// characters make a token; the margin is wide so ordinary variation passes.
// It is a rough guard: dense text (CJK, digits, base64) has fewer bytes per
// token and can be dropped, and hidden reasoning under about twice the
// visible output still counts and overstates the speed.
func hiddenOutput(outputTokens, streamedChars int) bool {
	return outputTokens > 2*(streamedChars/4)+32
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
