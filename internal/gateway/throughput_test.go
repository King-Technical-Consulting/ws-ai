package gateway

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestThroughputObserve(t *testing.T) {
	tp := NewThroughput()
	// 100 tokens in 4 s after a 1 s wait: 25 tokens/s.
	tp.Observe("a", 100, time.Second, 5*time.Second)
	s, ok := tp.Stat("a")
	if !ok || s.Samples != 1 || s.TokensPerSec < 24.9 || s.TokensPerSec > 25.1 || s.TTFTMS != 1000 {
		t.Fatalf("first sample: %+v %v", s, ok)
	}
	// With few samples the average is a plain mean, so the second sample
	// counts half, not tpsAlpha.
	tp.Observe("a", 200, time.Second, 3*time.Second) // 100 tok/s
	s, _ = tp.Stat("a")
	if want := 62.5; s.TokensPerSec < want-0.1 || s.TokensPerSec > want+0.1 {
		t.Fatalf("average = %v, want about %v", s.TokensPerSec, want)
	}
	// After enough samples a new one moves it by tpsAlpha.
	for i := 0; i < 10; i++ {
		tp.Observe("m", 100, time.Second, 5*time.Second) // 25 tok/s
	}
	tp.Observe("m", 200, time.Second, 3*time.Second) // 100 tok/s
	if m, _ := tp.Stat("m"); m.TokensPerSec < 25+tpsAlpha*75-0.1 || m.TokensPerSec > 25+tpsAlpha*75+0.1 {
		t.Fatalf("moving average = %v", m.TokensPerSec)
	}
	// Too short, no first token, or a tiny decode phase: ignored.
	tp.Observe("b", minObservedTokens-1, time.Second, 5*time.Second)
	tp.Observe("b", 100, 0, 5*time.Second)
	tp.Observe("b", 100, time.Second, time.Second+minObservedDecode/2)
	if _, ok := tp.Stat("b"); ok {
		t.Fatal("noise was recorded")
	}
}

func TestThroughputEstimate(t *testing.T) {
	tp := NewThroughput()
	ep := &Endpoint{ID: "x", ThroughputClass: "low"}
	if v, m := tp.Estimate(ep); v != classTPS["low"] || m {
		t.Fatalf("prior: %v %v", v, m)
	}
	if v, _ := tp.Estimate(&Endpoint{ID: "y"}); v != classTPS["medium"] {
		t.Fatalf("unknown class should rank as medium, got %v", v)
	}
	for i := 0; i < minSamples-1; i++ {
		tp.Observe("x", 400, time.Second, 5*time.Second) // 100 tok/s
	}
	if _, m := tp.Estimate(ep); m {
		t.Fatal("measured too early")
	}
	tp.Observe("x", 400, time.Second, 5*time.Second)
	if v, m := tp.Estimate(ep); !m || v < 99 || v > 101 {
		t.Fatalf("measured: %v %v", v, m)
	}
}

func TestThroughputStaleFallsBackToClass(t *testing.T) {
	tp := NewThroughput()
	clock := time.Now()
	tp.now = func() time.Time { return clock }
	ep := &Endpoint{ID: "x", ThroughputClass: "high"}
	for i := 0; i < minSamples; i++ {
		tp.Observe("x", 100, time.Second, 11*time.Second) // 10 tok/s, measured slow
	}
	if v, m := tp.Estimate(ep); !m || v > 11 {
		t.Fatalf("fresh: %v %v", v, m)
	}
	// Left unused, the endpoint is trusted at its declared class again, so
	// the router can try it and measure it anew.
	clock = clock.Add(staleAfter + time.Second)
	if v, m := tp.Estimate(ep); m || v != classTPS["high"] {
		t.Fatalf("stale: %v %v", v, m)
	}
}

func TestThroughputRecoversAfterAStaleSpell(t *testing.T) {
	tp := NewThroughput()
	clock := time.Now()
	tp.now = func() time.Time { return clock }
	ep := &Endpoint{ID: "x", ThroughputClass: "high"}
	for i := 0; i < 5; i++ {
		tp.Observe("x", 100, time.Second, 11*time.Second) // 10 tok/s
	}
	clock = clock.Add(staleAfter + time.Second)
	// New, fast samples after the spell: the old average must not drag them down.
	for i := 0; i < minSamples; i++ {
		tp.Observe("x", 1000, time.Second, 11*time.Second) // 100 tok/s
	}
	if v, m := tp.Estimate(ep); !m || v < 99 || v > 101 {
		t.Fatalf("after recovery: %v %v", v, m)
	}
}

func TestHiddenOutput(t *testing.T) {
	if hiddenOutput(60, 240) {
		t.Error("60 tokens in 240 characters is ordinary")
	}
	if hiddenOutput(300, 1000) {
		t.Error("300 tokens against 1000 characters is within the margin")
	}
	// 4000 tokens billed, a 20-token answer streamed: silent reasoning.
	if !hiddenOutput(4000, 80) {
		t.Error("silent reasoning was not detected")
	}
}

func TestPolicyRankValidation(t *testing.T) {
	for _, y := range []string{"rank: throughtput", "min_tokens_per_sec: -1"} {
		if _, err := ParsePolicy("name: p\nrules:\n  - match: {}\n    prefer: [a]\n    " + y + "\n"); err == nil {
			t.Errorf("%q should be refused", y)
		}
	}
	if _, err := ParsePolicy("name: p\nrules:\n  - match: {}\n    prefer: [a]\n    rank: throughput\n    min_tokens_per_sec: 5\n"); err != nil {
		t.Errorf("valid rule refused: %v", err)
	}
}

func TestRouteMinSpeedLeavesATrace(t *testing.T) {
	r := speedRouter(t, "    min_tokens_per_sec: 30\n")
	measure(r, "p/orin", 10)
	_, dec := routeIDs(t, r)
	if dec.Reason != "rule+min-speed" {
		t.Errorf("reason = %q", dec.Reason)
	}
}

func TestRouterPoliciesConcurrent(t *testing.T) {
	r := speedRouter(t, "")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			r.SetPolicies(r.Policies())
		}
	}()
	for i := 0; i < 200; i++ {
		routeIDs(t, r)
	}
	<-done
}

func speedRouter(t *testing.T, rule string) *Router {
	t.Helper()
	reg := NewRegistry()
	reg.Replace(
		[]*Provider{{ID: "p", Kind: ProviderOpenAICompat}},
		[]*Endpoint{
			{ID: "p/orin", ProviderID: "p", ModelName: "orin", Enabled: true, Local: true, ThroughputClass: "low"},
			{ID: "p/spark", ProviderID: "p", ModelName: "spark", Enabled: true, Local: true, ThroughputClass: "high"},
			{ID: "p/cloud", ProviderID: "p", ModelName: "cloud", Enabled: true},
		},
	)
	pol, err := ParsePolicy("name: speed\nrules:\n  - match: {}\n    prefer: [p/orin, p/cloud, p/spark]\n" + rule)
	if err != nil {
		t.Fatal(err)
	}
	return NewRouter(reg, []Policy{pol})
}

func routeIDs(t *testing.T, r *Router) ([]string, Decision) {
	t.Helper()
	cands, dec, err := r.Route(RouteInput{Selector: "auto", TaskClass: TaskChat})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range cands {
		ids = append(ids, c.Endpoint.ID)
	}
	return ids, dec
}

func measure(r *Router, id string, tps int) {
	for i := 0; i < minSamples; i++ {
		r.Throughput().Observe(id, tps*4, time.Second, 5*time.Second)
	}
}

func TestRouteWithoutRankKeepsPreferOrder(t *testing.T) {
	r := speedRouter(t, "")
	ids, _ := routeIDs(t, r)
	if ids[0] != "p/orin" || ids[1] != "p/cloud" || ids[2] != "p/spark" {
		t.Fatalf("order = %v", ids)
	}
}

func TestRouteRankThroughput(t *testing.T) {
	r := speedRouter(t, "    rank: throughput\n")
	// No measurements yet: the declared classes order them.
	ids, dec := routeIDs(t, r)
	if ids[0] != "p/spark" || ids[1] != "p/cloud" || ids[2] != "p/orin" {
		t.Fatalf("by class = %v", ids)
	}
	if dec.Reason != "rule+throughput" {
		t.Errorf("reason = %q", dec.Reason)
	}
	if len(dec.Candidates) != 3 || dec.Candidates[0] != "p/spark" {
		t.Errorf("decision candidates = %v", dec.Candidates)
	}
	// Measurements override the class: the "low" box turns out fastest.
	measure(r, "p/orin", 150)
	ids, _ = routeIDs(t, r)
	if ids[0] != "p/orin" {
		t.Fatalf("by measurement = %v", ids)
	}
}

func TestRouteMinTokensPerSec(t *testing.T) {
	r := speedRouter(t, "    min_tokens_per_sec: 30\n")
	measure(r, "p/orin", 10)  // measured slow: dropped
	measure(r, "p/spark", 90) // measured fast: kept
	ids, _ := routeIDs(t, r)  // p/cloud is unmeasured: kept
	if len(ids) != 2 || ids[0] != "p/cloud" || ids[1] != "p/spark" {
		t.Fatalf("filtered = %v", ids)
	}
	// If every candidate is measured slow, the filter must not empty the list.
	measure(r, "p/spark", 5)
	measure(r, "p/spark", 5)
	measure(r, "p/spark", 5)
	r2 := speedRouter(t, "    min_tokens_per_sec: 30\n")
	for _, id := range []string{"p/orin", "p/spark", "p/cloud"} {
		measure(r2, id, 5)
	}
	if ids, _ := routeIDs(t, r2); len(ids) != 3 {
		t.Fatalf("emptied the list: %v", ids)
	}
}

// slowAdapter streams a reply with a gap before the first token and a gap
// before the finish, so the gateway sees a real ttft and decode phase.
type slowAdapter struct{ tokens int }

func (a slowAdapter) Stream(_ context.Context, _ *Provider, _ *Endpoint, _ *Request) (<-chan StreamEvent, error) {
	ch := make(chan StreamEvent, 8)
	go func() {
		defer close(ch)
		time.Sleep(30 * time.Millisecond)
		ch <- StreamEvent{Type: EventTextDelta, Text: strings.Repeat("word ", a.tokens*4/5)} // about a.tokens tokens
		time.Sleep(300 * time.Millisecond)
		ch <- StreamEvent{Type: EventUsage, Usage: &Usage{OutputTokens: a.tokens}}
		ch <- StreamEvent{Type: EventFinish, FinishReason: FinishStop}
	}()
	return ch, nil
}
func (slowAdapter) CountTokens(context.Context, *Provider, *Endpoint, *Request) (int, bool, error) {
	return 0, false, nil
}

func TestGatewayRecordsThroughput(t *testing.T) {
	r := speedRouter(t, "")
	gw := New(r.reg, r, nil, nil)
	gw.RegisterAdapter(ProviderOpenAICompat, slowAdapter{tokens: 60})
	req := &Request{Model: "p/orin", Metadata: Metadata{TaskClass: TaskChat}}
	if _, err := gw.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	s, ok := r.Throughput().Stat("p/orin")
	if !ok || s.Samples != 1 {
		t.Fatalf("no sample recorded: %+v %v", s, ok)
	}
	// 60 tokens over about 0.3 s of decode is roughly 200 tokens/s; allow slack for a loaded runner.
	if s.TokensPerSec < 60 || s.TokensPerSec > 220 || s.TTFTMS < 20 {
		t.Errorf("stat = %+v", s)
	}
}

func TestLoadFactor(t *testing.T) {
	for _, c := range []struct {
		inFlight, capacity int
		min, max           float64
	}{
		{0, 4, 1, 1},       // idle
		{3, 0, 1, 1},       // unknown capacity: not scaled
		{1, 4, 0.85, 0.9},  // one of four slots taken
		{3, 4, 0.6, 0.65},  // nearly full
		{4, 4, 0.35, 0.45}, // full: a new request queues
		{8, 4, 0.1, 0.25},  // queue building
	} {
		if f := loadFactor(c.inFlight, c.capacity); f < c.min || f > c.max {
			t.Errorf("loadFactor(%d, %d) = %v, want %v to %v", c.inFlight, c.capacity, f, c.min, c.max)
		}
	}
	// More load never raises the factor.
	prev := 2.0
	for n := 0; n <= 12; n++ {
		if f := loadFactor(n, 4); f > prev {
			t.Errorf("factor rose at %d in flight: %v after %v", n, f, prev)
		} else {
			prev = f
		}
	}
}

func TestInFlightBeginDone(t *testing.T) {
	tp := NewThroughput()
	d1 := tp.Begin("a")
	d2 := tp.Begin("a")
	if n := tp.InFlight("a"); n != 2 {
		t.Fatalf("in flight = %d", n)
	}
	if got := tp.All()["a"].InFlight; got != 2 {
		t.Fatalf("All reports %d for a busy endpoint with no measurement", got)
	}
	d1()
	d1() // idempotent
	if n := tp.InFlight("a"); n != 1 {
		t.Fatalf("a second done changed the count: %d", n)
	}
	d2()
	if n := tp.InFlight("a"); n != 0 {
		t.Fatalf("not released: %d", n)
	}
	if _, ok := tp.All()["a"]; ok {
		t.Error("an idle, never-measured endpoint should be absent")
	}
}

func TestRouteRankPrefersFreeSlot(t *testing.T) {
	r := speedRouter(t, "    rank: throughput\n")
	for _, id := range []string{"p/orin", "p/spark"} {
		ep, _ := r.reg.Endpoint(id)
		ep.Capabilities.MaxConcurrency = 2
	}
	measure(r, "p/spark", 100) // the faster one when idle
	measure(r, "p/orin", 60)
	ids, _ := routeIDs(t, r)
	if ids[0] != "p/spark" {
		t.Fatalf("idle: %v", ids)
	}
	// Spark's slots fill up (2 of 2 plus a queue): Orin, with room, now wins.
	d := []func(){r.Throughput().Begin("p/spark"), r.Throughput().Begin("p/spark"), r.Throughput().Begin("p/spark")}
	ids, _ = routeIDs(t, r)
	if ids[0] != "p/orin" {
		t.Fatalf("with spark full: %v", ids)
	}
	for _, f := range d {
		f()
	}
	ids, _ = routeIDs(t, r)
	if ids[0] != "p/spark" {
		t.Fatalf("after the load cleared: %v", ids)
	}
}

func TestGatewayHoldsASlotWhileStreaming(t *testing.T) {
	r := speedRouter(t, "")
	gw := New(r.reg, r, nil, nil)
	gw.RegisterAdapter(ProviderOpenAICompat, slowAdapter{tokens: 60})
	req := &Request{Model: "p/orin", Metadata: Metadata{TaskClass: TaskChat}}
	ch, err := gw.Stream(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // the adapter is still mid-reply
	if n := r.Throughput().InFlight("p/orin"); n != 1 {
		t.Fatalf("in flight during the reply = %d", n)
	}
	for range ch {
	}
	time.Sleep(20 * time.Millisecond)
	if n := r.Throughput().InFlight("p/orin"); n != 0 {
		t.Fatalf("still in flight after the reply: %d", n)
	}
}

// failAdapter fails every call, before or during the stream.
type failAdapter struct{ atBuild bool }

func (a failAdapter) Stream(context.Context, *Provider, *Endpoint, *Request) (<-chan StreamEvent, error) {
	if a.atBuild {
		return nil, context.DeadlineExceeded
	}
	ch := make(chan StreamEvent, 1)
	ch <- ErrorEvent(context.DeadlineExceeded, true) // retryable, before any output
	close(ch)
	return ch, nil
}
func (failAdapter) CountTokens(context.Context, *Provider, *Endpoint, *Request) (int, bool, error) {
	return 0, false, nil
}

func TestFailedAttemptsReleaseTheirSlot(t *testing.T) {
	for _, atBuild := range []bool{true, false} {
		r := speedRouter(t, "")
		gw := New(r.reg, r, nil, nil)
		gw.RegisterAdapter(ProviderOpenAICompat, failAdapter{atBuild: atBuild})
		_, _ = gw.Complete(context.Background(), &Request{Model: "auto", Metadata: Metadata{TaskClass: TaskChat}})
		for id, st := range r.Throughput().All() {
			if st.InFlight != 0 {
				t.Errorf("atBuild=%v: %s left %d in flight", atBuild, id, st.InFlight)
			}
		}
	}
}

// A measurement loaded from an earlier process is the prior until fresh
// replies arrive: Estimate reports it as not measured, and it stands in
// for the class when it is higher (a slower one leaves the class in place,
// so the endpoint is tried again). Each counted reply is handed to Persist.
func TestThroughputLoadedIsRememberedNotTrusted(t *testing.T) {
	tp := NewThroughput()
	clock := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	tp.now = func() time.Time { return clock }
	ep := &Endpoint{ID: "p/orin", ThroughputClass: "low"}
	tp.Load("p/slow", ThroughputStat{TokensPerSec: 5, TTFTMS: 400, Samples: 12}, clock.Add(-6*time.Hour))
	if tps, measured := tp.Estimate(&Endpoint{ID: "p/slow", ThroughputClass: "low"}); tps != classTPS["low"] || measured {
		t.Fatalf("a slow remembered measurement should leave the class in place: %v %v", tps, measured)
	}
	tp.Load("p/orin", ThroughputStat{TokensPerSec: 34, TTFTMS: 400, Samples: 12}, clock.Add(-6*time.Hour))
	if tps, measured := tp.Estimate(ep); tps != 34 || measured {
		t.Fatalf("loaded: tps=%v measured=%v, want 34 remembered", tps, measured)
	}
	// Nothing useful is loaded from an empty row.
	tp.Load("p/empty", ThroughputStat{}, clock)
	if _, ok := tp.Stat("p/empty"); ok {
		t.Error("an empty measurement should not be loaded")
	}
	var persisted []ThroughputStat
	done := make(chan struct{}, 8)
	tp.Persist = func(id string, s ThroughputStat, at time.Time) {
		persisted = append(persisted, s)
		done <- struct{}{}
	}
	// Three fresh replies replace the stale average and are written through.
	for i := 0; i < 3; i++ {
		clock = clock.Add(time.Second)
		tp.Observe("p/orin", 100, 300*time.Millisecond, 5300*time.Millisecond)
		<-done
	}
	if tps, measured := tp.Estimate(ep); !measured || tps < 19 || tps > 21 {
		t.Errorf("after three replies: tps=%v measured=%v, want about 20 measured", tps, measured)
	}
	if len(persisted) != 3 || persisted[2].Samples != 3 {
		t.Errorf("persist calls: %+v", persisted)
	}
}
