package gateway

import (
	"sync"
	"time"
)

// Observed speed of each endpoint, measured from real completions. Health
// probes only say an endpoint answers; they say nothing about how fast it
// decodes, which is what separates a Jetson from a rented H100. The router
// uses this to rank candidates when a rule asks for rank: throughput.
const (
	// A reply this short, or a decode phase this brief, says more about
	// network and prompt processing than about generation speed.
	minObservedTokens = 16
	minObservedDecode = 200 * time.Millisecond
	// tpsAlpha weights the newest sample in the moving average.
	tpsAlpha = 0.2
	// minSamples is how many observations an endpoint needs before its
	// measured speed overrides the declared throughput_class.
	minSamples = 3
	// staleAfter is how long a measurement is trusted without a newer one.
	// A router that ranks an endpoint last sends it no traffic, so without
	// this a few slow samples (a warm-up, a busy hour) would keep it last
	// until a restart. Past this age the declared class stands in again and
	// the endpoint can be tried and measured anew.
	staleAfter = 15 * time.Minute
)

// classTPS is the assumed decode speed (tokens per second) for an endpoint
// with too few observations, from its declared throughput_class. Unknown or
// empty classes rank as medium.
var classTPS = map[string]float64{"low": 15, "medium": 40, "high": 100}

// ThroughputStat is the measured speed of one endpoint.
type ThroughputStat struct {
	TokensPerSec float64 `json:"tokens_per_sec"` // moving average of decode speed
	TTFTMS       float64 `json:"ttft_ms"`        // moving average of time to first token
	Samples      int     `json:"samples"`
	InFlight     int     `json:"in_flight"` // requests being served now
	Queued       int     `json:"queued"`    // requests waiting for a free slot

	updated time.Time
}

// Throughput tracks ThroughputStat per endpoint id. It lives outside the
// registry because the registry is rebuilt on reload and health updates
// replace an endpoint's health wholesale. The numbers are kept in memory
// and, when Persist is set, written through after every counted reply and
// loaded again at boot (Load), so a restart ranks by what was measured last
// instead of the declared class; a loaded measurement is remembered, not
// trusted: it is the prior until fresh replies replace it.
type Throughput struct {
	mu    sync.RWMutex
	stats map[string]ThroughputStat
	busy  map[string]int // requests in flight per endpoint
	queue map[string]*waitQueue
	seq   uint64 // arrival order, to keep equal priorities first come, first served
	now   func() time.Time
	// Persist, when set, is called after each counted reply with the
	// endpoint's updated stat, outside the lock and on its own goroutine.
	Persist func(id string, s ThroughputStat, updated time.Time)
}

// Load seeds the tracker with measurements from an earlier process. Each
// is kept with its own time, so one older than staleAfter is a prior only
// (Estimate reports it as not measured) and the next reply starts a fresh
// average, as it would after a quiet spell.
func (t *Throughput) Load(id string, s ThroughputStat, updated time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s.Samples <= 0 || s.TokensPerSec <= 0 {
		return
	}
	s.InFlight, s.Queued = 0, 0
	s.updated = updated
	t.stats[id] = s
}

// Remembered reports whether an endpoint has a measurement, fresh or not.
func (t *Throughput) Remembered(id string) (float64, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	s, ok := t.stats[id]
	return s.TokensPerSec, ok && s.Samples > 0
}

// NewThroughput creates an empty tracker.
func NewThroughput() *Throughput {
	return &Throughput{stats: map[string]ThroughputStat{}, busy: map[string]int{}, queue: map[string]*waitQueue{}, now: time.Now}
}

// Observe records one finished completion. total is the wall time of the
// request and ttft the time to the first generated token; the decode phase
// is their difference. Replies too short to say anything are ignored.
func (t *Throughput) Observe(id string, outputTokens int, ttft, total time.Duration) {
	decode := total - ttft
	if outputTokens < minObservedTokens || ttft <= 0 || decode < minObservedDecode {
		return
	}
	tps := float64(outputTokens) / decode.Seconds()
	ms := float64(ttft) / float64(time.Millisecond)
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.stats[id]
	// Past staleAfter the old average is no longer trusted (Estimate has
	// already stopped using it), so start over rather than blend a fresh
	// sample into it: otherwise one good reply after a slow spell would
	// still read as slow.
	if s.Samples > 0 && t.now().Sub(s.updated) >= staleAfter {
		s = ThroughputStat{}
	}
	// A plain running mean while there are few samples, so the first one
	// does not carry most of the weight; the moving average takes over once
	// a new sample's share would drop below tpsAlpha.
	a := tpsAlpha
	if n := float64(s.Samples + 1); 1/n > a {
		a = 1 / n
	}
	if s.Samples == 0 {
		s.TokensPerSec, s.TTFTMS = tps, ms
	} else {
		s.TokensPerSec += a * (tps - s.TokensPerSec)
		s.TTFTMS += a * (ms - s.TTFTMS)
	}
	s.Samples++
	s.updated = t.now()
	t.stats[id] = s
	if t.Persist != nil {
		go t.Persist(id, s, s.updated)
	}
}

// Stat returns the measured speed of an endpoint, if any was recorded.
func (t *Throughput) Stat(id string) (ThroughputStat, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	s, ok := t.stats[id]
	return s, ok
}

// All returns a copy of every recorded stat, with the requests in flight.
// An endpoint that is busy but has no measurement yet is included.
func (t *Throughput) All() map[string]ThroughputStat {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make(map[string]ThroughputStat, len(t.stats))
	for k, v := range t.stats {
		v.InFlight = t.busy[k]
		v.Queued = t.queuedLocked(k)
		out[k] = v
	}
	for k, n := range t.busy {
		if _, ok := out[k]; !ok && n > 0 {
			out[k] = ThroughputStat{InFlight: n, Queued: t.queuedLocked(k)}
		}
	}
	for k := range t.queue {
		if _, ok := out[k]; !ok && t.queuedLocked(k) > 0 {
			out[k] = ThroughputStat{Queued: t.queuedLocked(k)}
		}
	}
	return out
}

// Begin records a request starting on an endpoint and returns the function
// that records it finishing. The function is safe to call more than once.
func (t *Throughput) Begin(id string) (done func()) {
	t.mu.Lock()
	t.busy[id]++
	t.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			if t.busy[id] > 0 {
				t.busy[id]--
			}
			if t.busy[id] == 0 {
				delete(t.busy, id)
			}
			t.mu.Unlock()
		})
	}
}

// InFlight is the number of requests being served by an endpoint now.
func (t *Throughput) InFlight(id string) int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.busy[id]
}

// loadPenalty is how much of its speed an endpoint is assumed to lose with
// every slot taken, up to half when they are all taken. A first guess: one
// Orin served about 11 tokens/s per request alone and 6 each at four at
// once (board 7c98). The sweep asked of agx-orin should replace it.
const loadPenalty = 0.5

// loadFactor scales an endpoint's speed by how busy it is. An endpoint of
// unknown capacity is not scaled. Below capacity the penalty grows with the
// slots taken; at or past it a new request would queue, so the factor
// shrinks further with every request already waiting.
func loadFactor(inFlight, capacity int) float64 {
	if capacity <= 0 || inFlight <= 0 {
		return 1
	}
	if inFlight < capacity {
		return 1 - loadPenalty*float64(inFlight)/float64(capacity)
	}
	return (1 - loadPenalty) * float64(capacity) / float64(inFlight+1)
}

// Estimate is the speed the router assumes for an endpoint: the measured
// average once there are enough recent samples; otherwise the declared
// class, raised to a remembered average (stale, or loaded from an earlier
// process) when that is higher. A stale measurement never ranks an
// endpoint below its class: one measured slow is tried again at its class
// after a quiet spell, as before, while one measured faster than its class
// keeps that standing across a restart. measured reports the first case.
func (t *Throughput) Estimate(ep *Endpoint) (tps float64, measured bool) {
	s, ok := t.Stat(ep.ID)
	if ok && s.Samples >= minSamples && t.now().Sub(s.updated) < staleAfter {
		return s.TokensPerSec, true
	}
	prior, known := classTPS[ep.ThroughputClass]
	if !known {
		prior = classTPS["medium"]
	}
	if ok && s.Samples > 0 && s.TokensPerSec > prior {
		return s.TokensPerSec, false
	}
	return prior, false
}
