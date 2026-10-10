package gateway

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Policy is one routing policy. Policies are data: YAML on disk seeds the
// database, the admin UI edits them, and the router re-reads on NOTIFY.
//
// Example:
//
//	name: default
//	priority: 100
//	rules:
//	  - match: { task_class: [summarize, title, classify, reflect] }
//	    prefer: [local-llama/gpt-oss-20b, anthropic/claude-haiku-4-5]
//	  - match: { task_class: [code] }
//	    prefer: [anthropic/claude-opus-5-5, local-vllm/qwen3-coder-next]
//	    require: { tools: true }
//	  - match: {}
//	    prefer: [anthropic/claude-sonnet-5-5, openrouter/deepseek/deepseek-chat-v3, local-llama/gpt-oss-20b]
//	aliases:
//	  cheap: [local-llama/gpt-oss-20b, anthropic/claude-haiku-4-5]
//	  best: [anthropic/claude-opus-5-5]
type Policy struct {
	Name     string              `yaml:"name" json:"name"`
	Priority int                 `yaml:"priority" json:"priority"`
	Rules    []Rule              `yaml:"rules" json:"rules"`
	Aliases  map[string][]string `yaml:"aliases,omitempty" json:"aliases"`
}

// Selectors is every model name this policy gives meaning to, with the
// endpoints it prefers for it: the aliases, and the selector of any rule
// that matches on one (a rule with `match: {selector: [fast]}` makes
// "fast" a name clients can send, with the rule's rank and filters, which
// a plain alias cannot carry). An alias wins over a rule of the same name.
func (p Policy) Selectors() map[string][]string {
	out := map[string][]string{}
	for _, r := range p.Rules {
		for _, sel := range r.Match.Selector {
			if _, dup := out[sel]; !dup {
				out[sel] = r.Prefer
			}
		}
	}
	for k, v := range p.Aliases {
		out[k] = v
	}
	return out
}

// Rule matches requests and yields an ordered preference list.
type Rule struct {
	Match   Match        `yaml:"match" json:"match"`
	Prefer  []string     `yaml:"prefer" json:"prefer"`
	Deny    []string     `yaml:"deny,omitempty" json:"deny"`
	Require Capabilities `yaml:"require,omitempty" json:"require"`
	// MaxCostPerCallUSD drops candidates whose estimated cost exceeds this.
	MaxCostPerCallUSD float64 `yaml:"max_cost_per_call_usd,omitempty" json:"max_cost_per_call_usd"`
	// LocalOnly restricts candidates to Local endpoints (used by budget downgrade).
	LocalOnly bool `yaml:"local_only,omitempty" json:"local_only"`
	// Rank reorders the candidates the rule produced. "" keeps the order
	// of prefer; "throughput" puts the fastest first, by measured decode
	// speed (or the declared throughput_class until enough requests have
	// been seen). The sort is stable, so equal speeds keep prefer order.
	Rank string `yaml:"rank,omitempty" json:"rank"`
	// MinTokensPerSec drops candidates whose measured decode speed is
	// below it. Endpoints without enough measurements are kept, and the
	// filter never empties the list.
	MinTokensPerSec float64 `yaml:"min_tokens_per_sec,omitempty" json:"min_tokens_per_sec"`
}

// Match conditions. Empty slices match everything.
type Match struct {
	TaskClass []TaskClass `yaml:"task_class,omitempty" json:"task_class"`
	Users     []string    `yaml:"users,omitempty" json:"users"`
	Agents    []string    `yaml:"agents,omitempty" json:"agents"`
	Selector  []string    `yaml:"selector,omitempty" json:"selector"` // match the request's Model selector
	External  *bool       `yaml:"external,omitempty" json:"external"`
}

func (m Match) matches(in RouteInput) bool {
	if len(m.TaskClass) > 0 && !containsTC(m.TaskClass, in.TaskClass) {
		return false
	}
	if len(m.Users) > 0 && !contains(m.Users, in.UserID) {
		return false
	}
	if len(m.Agents) > 0 && !contains(m.Agents, in.AgentID) {
		return false
	}
	if len(m.Selector) > 0 && !contains(m.Selector, in.Selector) {
		return false
	}
	if m.External != nil && *m.External != in.External {
		return false
	}
	return true
}

// RouteInput is what the router needs to pick endpoints.
type RouteInput struct {
	Selector    string // req.Model: endpoint id, alias, or "auto"
	TaskClass   TaskClass
	UserID      string
	AgentID     string
	External    bool
	Required    Capabilities // derived from the request: tools, vision, etc.
	EstTokensIn int
	// Keys is what the user may use of the hosted providers; nil means no
	// enforcement (no user, or no key source configured).
	Keys *KeyAccess
	// Downgrade is set by the budget middleware when a budget is exceeded
	// with on_exceed=downgrade: only Local endpoints may be used.
	Downgrade bool
}

// Candidate is one endpoint the router chose, in order.
type Candidate struct {
	Endpoint *Endpoint
	// Provider carries the credentials this request is to use: after
	// Prepare, a hosted provider is either a copy holding the user's own
	// key or the shared provider (see keys.go).
	Provider *Provider
	// OwnKey is true when Provider holds the user's own key, so the spend
	// is theirs and no budget counts it.
	OwnKey bool
	Reason string
}

// Decision is logged to the ledger.
type Decision struct {
	Selector   string   `json:"selector"`
	Policy     string   `json:"policy,omitempty"`
	Rule       int      `json:"rule"`
	Candidates []string `json:"candidates"`
	Chosen     string   `json:"chosen,omitempty"`
	Tried      []string `json:"tried,omitempty"`
	Reason     string   `json:"reason,omitempty"`
}

// Router evaluates policies against the registry.
type Router struct {
	reg      *Registry
	mu       sync.RWMutex // guards policies
	policies []Policy     // sorted by priority asc
	tp       *Throughput
}

// NewRouter creates a router.
func NewRouter(reg *Registry, policies []Policy) *Router {
	r := &Router{reg: reg, tp: NewThroughput()}
	r.SetPolicies(policies)
	return r
}

// SetPolicies replaces the policy set.
func (r *Router) SetPolicies(ps []Policy) {
	sorted := append([]Policy(nil), ps...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Priority < sorted[j].Priority })
	r.mu.Lock()
	r.policies = sorted
	r.mu.Unlock()
}

// Throughput returns the tracker of measured endpoint speeds.
func (r *Router) Throughput() *Throughput { return r.tp }

// Policies returns the current policies.
func (r *Router) Policies() []Policy {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.policies
}

// Route returns an ordered candidate list and the decision record.
func (r *Router) Route(in RouteInput) ([]Candidate, Decision, error) {
	dec := Decision{Selector: in.Selector}

	// 1. Direct endpoint id: honor it, but still enforce enabled/health and
	//    budget downgrade.
	if ep, ok := r.reg.Endpoint(in.Selector); ok {
		c, why := r.candidate(ep, in, Rule{})
		if c == nil {
			return nil, dec, fmt.Errorf("router: endpoint %s unavailable: %s", in.Selector, why)
		}
		dec.Candidates = []string{ep.ID}
		dec.Reason = "direct"
		return []Candidate{*c}, dec, nil
	}

	prefs, rule := r.prefs(in, &dec)

	var out []Candidate
	for _, id := range prefs {
		ep, ok := r.reg.Endpoint(id)
		if !ok {
			continue
		}
		if contains(rule.Deny, id) {
			continue
		}
		if c, _ := r.candidate(ep, in, rule); c != nil {
			out = append(out, *c)
			dec.Candidates = append(dec.Candidates, id)
		}
	}
	if len(out) == 0 && dec.Reason != "fallback-all" {
		// The policy named endpoints this box doesn't have (a seed written
		// for the GPU fleet, running on a cloud-only host). Fall back to any
		// enabled endpoint that meets the rule's requirements, local first,
		// then cheapest, rather than failing the request.
		var ids []string
		for _, ep := range r.reg.Endpoints() {
			if contains(rule.Deny, ep.ID) {
				continue
			}
			if c, _ := r.candidate(ep, in, rule); c != nil {
				ids = append(ids, ep.ID)
			}
		}
		sort.SliceStable(ids, func(i, j int) bool {
			a, _ := r.reg.Endpoint(ids[i])
			b, _ := r.reg.Endpoint(ids[j])
			if a.Local != b.Local {
				return a.Local
			}
			return a.Pricing.InputPerM+a.Pricing.OutputPerM < b.Pricing.InputPerM+b.Pricing.OutputPerM
		})
		for _, id := range ids {
			ep, _ := r.reg.Endpoint(id)
			c, _ := r.candidate(ep, in, rule)
			out = append(out, *c)
			dec.Candidates = append(dec.Candidates, id)
		}
		if len(out) > 0 {
			dec.Reason += "-fallback"
		}
	}
	if len(out) > 1 && (rule.Rank == "throughput" || rule.MinTokensPerSec > 0) {
		out, dec.Reason = r.applySpeed(out, rule, dec.Reason)
		dec.Candidates = dec.Candidates[:0]
		for _, c := range out {
			dec.Candidates = append(dec.Candidates, c.Endpoint.ID)
		}
	}
	if len(out) == 0 {
		if nm := r.mediaBlame(in, rule); nm != nil {
			// No image or video engine to route to at all: say that, not
			// that a key is missing for a provider that is not set up.
			return nil, dec, fmt.Errorf("router: no endpoint satisfies selector %q task %q (caps %+v, downgrade=%v): %w", in.Selector, in.TaskClass, in.Required, in.Downgrade, nm)
		}
		return nil, dec, fmt.Errorf("router: no endpoint satisfies selector %q task %q (caps %+v, downgrade=%v)%s%s%s", in.Selector, in.TaskClass, in.Required, in.Downgrade, r.sizeBlame(in, rule), r.visionBlame(in, rule), r.keyBlame(in, rule))
	}
	return out, dec, nil
}

// prefs resolves the selector to the endpoints a policy names for it and the
// rule that applies: an alias or the first matching rule, walking policies
// in priority order, else every enabled endpoint, local first, then
// cheapest. dec records which it was.
func (r *Router) prefs(in RouteInput, dec *Decision) ([]string, Rule) {
	// 2. Alias or auto: walk policies in priority order; first matching rule
	//    (or alias) wins.
	prefs, rule := PreferredBy(r.Policies(), in, dec)
	if prefs == nil {
		// 3. No policy matched: every enabled endpoint, local first, then cheapest.
		for _, ep := range r.reg.Endpoints() {
			prefs = append(prefs, ep.ID)
		}
		dec.Reason = "fallback-all"
		sort.SliceStable(prefs, func(i, j int) bool {
			a, _ := r.reg.Endpoint(prefs[i])
			b, _ := r.reg.Endpoint(prefs[j])
			if a.Local != b.Local {
				return a.Local
			}
			return a.Pricing.InputPerM+a.Pricing.OutputPerM < b.Pricing.InputPerM+b.Pricing.OutputPerM
		})
	}
	return prefs, rule
}

// PreferredBy walks policies (sorted by priority) and returns the ordered
// preference list the first alias or matching rule gives in, with that
// rule; nil when no policy speaks for the request. dec, when not nil,
// records which policy and rule it was. It is the policy half of routing,
// usable on any policy set: the training flywheel asks it what the other
// policies prefer for a task class before it writes its own rule.
func PreferredBy(policies []Policy, in RouteInput, dec *Decision) ([]string, Rule) {
	if dec == nil {
		dec = &Decision{}
	}
	for _, p := range policies {
		if in.Selector != "" && in.Selector != "auto" {
			if list, ok := p.Aliases[in.Selector]; ok {
				dec.Policy = p.Name
				dec.Reason = "alias"
				return list, Rule{}
			}
		}
		for i, rl := range p.Rules {
			if rl.Match.matches(in) {
				dec.Policy, dec.Rule = p.Name, i
				dec.Reason = "rule"
				return rl.Prefer, rl
			}
		}
	}
	return nil, Rule{}
}

// ContextCeiling is the largest context window among the endpoints that
// could serve in, were the request small enough: what compaction has to fit
// a conversation into before routing. 0 when no endpoint is eligible or
// none declares a window. A direct pick is its endpoint's window; an alias
// or rule is the best of its list, and the policy fallback (any endpoint
// meeting the rule) counts too, since Route would take it.
func (r *Router) ContextCeiling(in RouteInput) int {
	in.EstTokensIn = 0
	if ep, ok := r.reg.Endpoint(in.Selector); ok {
		if c, _ := r.candidate(ep, in, Rule{}); c != nil {
			return ep.Capabilities.ContextWindow
		}
		return 0
	}
	var dec Decision
	prefs, rule := r.prefs(in, &dec)
	max := 0
	consider := func(id string) {
		ep, ok := r.reg.Endpoint(id)
		if !ok || contains(rule.Deny, id) {
			return
		}
		if c, _ := r.candidate(ep, in, rule); c != nil && ep.Capabilities.ContextWindow > max {
			max = ep.Capabilities.ContextWindow
		}
	}
	for _, id := range prefs {
		consider(id)
	}
	if max == 0 && dec.Reason != "fallback-all" {
		for _, ep := range r.reg.Endpoints() {
			consider(ep.ID)
		}
	}
	return max
}

// sizeBlame is ": context too small" when the request's size is what left
// it without a route: some endpoint would have served it were it smaller.
// A direct pick names this reason itself; an alias or auto drops the
// per-endpoint reasons, and the person who sent a conversation too long
// for every model should be told that, not that no model is available.
func (r *Router) sizeBlame(in RouteInput, rule Rule) string {
	if in.EstTokensIn <= 0 {
		return ""
	}
	relaxed := in
	relaxed.EstTokensIn = 0
	for _, ep := range r.reg.Endpoints() {
		if contains(rule.Deny, ep.ID) {
			continue
		}
		if c, _ := r.candidate(ep, relaxed, rule); c != nil {
			return ": context too small"
		}
	}
	return ""
}

// visionBlame is ": needs a model that can read images" when the attached
// image is what left no route: some endpoint the selector covers would
// have served the same request without it. Named before the key blame,
// since picking a model that reads images is the fix the person can make
// at the composer (an image on "local" with a text-only local model).
func (r *Router) visionBlame(in RouteInput, rule Rule) string {
	if !in.Required.Vision {
		return ""
	}
	relaxed := in
	relaxed.Required.Vision = false
	for _, ep := range r.reg.Endpoints() {
		if contains(rule.Deny, ep.ID) {
			continue
		}
		if c, _ := r.candidate(ep, relaxed, rule); c != nil {
			return ": needs a model that can read images"
		}
	}
	return ""
}

// NoMediaError says a media request has no engine to go to: the registry
// holds no image (or video) endpoint, every one of them is disabled, or
// every enabled one is down. It is the router's reason for the person, so
// internal/media and the agent's user-facing messages pass its wording
// through as it is.
type NoMediaError struct {
	Kind     string   // "image" or "video"
	Disabled bool     // endpoints exist but none is enabled
	Down     []string // enabled endpoints that are health "down" and would serve the request if up
	Why      string   // the health error of the first of Down, when it has one
}

func (e *NoMediaError) Error() string {
	switch {
	case len(e.Down) == 1:
		why := ""
		if e.Why != "" {
			why = ": " + e.Why
		}
		return "the " + e.Kind + " endpoint " + e.Down[0] + " is down" + why + "; the owner checks the server it points at (COMFYUI_URL) and Admin, endpoints"
	case len(e.Down) > 1:
		return "every " + e.Kind + " endpoint is down (" + strings.Join(e.Down, ", ") + "); the owner checks the servers they point at and Admin, endpoints"
	case e.Disabled:
		return "every " + e.Kind + " endpoint is disabled; the owner enables one under Admin, endpoints"
	}
	return "no " + e.Kind + " endpoint is configured; the owner sets COMFYUI_URL or a media provider key, or enables one under Admin, endpoints"
}

// Sentence is the message as a sentence for a page or a chat.
func (e *NoMediaError) Sentence() string {
	s := e.Error()
	return strings.ToUpper(s[:1]) + s[1:] + "."
}

// mediaBlame names the missing engine when the request is for an image or a
// video and the registry has nothing it can use: no endpoint of that family
// is configured (COMFYUI_URL empty, no provider key), all are switched off
// in Admin, or an enabled one is down. The router drops a "down" endpoint
// before it looks at keys, so a down local engine would otherwise be blamed
// on a key the person lacks for a hosted one. A down endpoint is named only
// when it would have served this person were it up (their keys allow it,
// nothing else rules it out); then the fix is its server, not a key. It is
// nil when an enabled endpoint is up, so a missing key is then the
// keyBlame's to name.
func (r *Router) mediaBlame(in RouteInput, rule Rule) *NoMediaError {
	need := in.Required.Media
	if need == nil {
		return nil
	}
	kind := "image"
	family := func(m *MediaCaps) bool { return m.Image || m.ImageEdit || m.Upscale }
	if need.Video || need.ImageToVideo {
		kind = "video"
		family = func(m *MediaCaps) bool { return m.Video || m.ImageToVideo }
	}
	exists, enabled := false, false
	var down []string
	for _, ep := range r.reg.Endpoints() {
		if ep.Capabilities.Media == nil || !family(ep.Capabilities.Media) {
			continue
		}
		exists = true
		if !ep.Enabled {
			continue
		}
		enabled = true
		if ep.Health.Status != "down" || contains(rule.Deny, ep.ID) {
			continue
		}
		up := *ep
		up.Health = Health{}
		if c, _ := r.candidate(&up, in, rule); c != nil {
			down = append(down, ep.ID)
		}
	}
	switch {
	case len(down) > 0:
		sort.Strings(down)
		first, _ := r.reg.Endpoint(down[0])
		return &NoMediaError{Kind: kind, Down: down, Why: first.Health.Error}
	case enabled:
		return nil
	case exists:
		return &NoMediaError{Kind: kind, Disabled: true}
	}
	return &NoMediaError{Kind: kind}
}

// keyBlame is ": needs your own API key" when the person's missing key is
// what left no route: some endpoint would have served them had they a key
// for its provider.
func (r *Router) keyBlame(in RouteInput, rule Rule) string {
	if in.Keys == nil {
		return ""
	}
	relaxed := in
	relaxed.Keys = &KeyAccess{Shared: true, Own: map[string]bool{}}
	for _, p := range r.reg.Providers() {
		relaxed.Keys.Own[p.ID] = true
	}
	for _, ep := range r.reg.Endpoints() {
		if contains(rule.Deny, ep.ID) {
			continue
		}
		if c, _ := r.candidate(ep, relaxed, rule); c != nil {
			return ": needs your own API key"
		}
	}
	return ""
}

// applySpeed applies a rule's min_tokens_per_sec filter and rank. Each
// candidate's estimate is read once, so a measurement that lands during
// the sort cannot make the comparator inconsistent.
func (r *Router) applySpeed(in []Candidate, rule Rule, reason string) ([]Candidate, string) {
	type scored struct {
		c        Candidate
		tps      float64
		measured bool
	}
	all := make([]scored, len(in))
	for i, c := range in {
		tps, m := r.tp.Estimate(c.Endpoint)
		all[i] = scored{c, tps, m}
	}
	if rule.MinTokensPerSec > 0 {
		// The filter judges the measured speed, not the speed under load.
		var kept []scored
		for _, x := range all {
			if !x.measured || x.tps >= rule.MinTokensPerSec {
				kept = append(kept, x)
			}
		}
		if len(kept) > 0 && len(kept) < len(all) {
			all = kept
			reason += "+min-speed"
		}
	}
	// The rank is the speed a new request is expected to get: the endpoint's
	// speed scaled down by how many of its slots are taken.
	for i := range all {
		all[i].tps *= loadFactor(r.tp.InFlight(all[i].c.Endpoint.ID), all[i].c.Endpoint.Capabilities.MaxConcurrency)
	}
	if rule.Rank == "throughput" {
		sort.SliceStable(all, func(i, j int) bool { return all[i].tps > all[j].tps })
		reason += "+throughput"
	}
	out := make([]Candidate, len(all))
	for i, x := range all {
		out[i] = x.c
	}
	return out, reason
}

// candidate applies hard filters. Returns nil and a reason when excluded.
func (r *Router) candidate(ep *Endpoint, in RouteInput, rule Rule) (*Candidate, string) {
	if !ep.Enabled {
		return nil, "disabled"
	}
	if ep.Health.Status == "down" {
		return nil, "down"
	}
	if (in.Downgrade || rule.LocalOnly) && !ep.Local {
		return nil, "budget downgrade: local only"
	}
	req := in.Required
	req.merge(rule.Require)
	if why := ep.Capabilities.satisfies(req); why != "" {
		return nil, why
	}
	if in.EstTokensIn > 0 && ep.Capabilities.ContextWindow > 0 && in.EstTokensIn > ep.Capabilities.ContextWindow {
		return nil, "context too small"
	}
	if rule.MaxCostPerCallUSD > 0 && !ep.Local {
		est := ep.Pricing.Cost(Usage{InputTokens: in.EstTokensIn, OutputTokens: 2048})
		if ep.Capabilities.IsMedia() {
			est = ep.Pricing.MediaCost(1, 0) // one image; video is priced by the media service
		}
		if est > rule.MaxCostPerCallUSD {
			return nil, "over max cost per call"
		}
	}
	p, ok := r.reg.Provider(ep.ProviderID)
	if !ok {
		return nil, "provider missing"
	}
	if !in.Keys.allows(p) {
		return nil, "no API key of yours for " + p.ID
	}
	return &Candidate{Endpoint: ep, Provider: p}, ""
}

func (c *Capabilities) merge(o Capabilities) {
	c.Tools = c.Tools || o.Tools
	c.Vision = c.Vision || o.Vision
	c.JSONMode = c.JSONMode || o.JSONMode
	c.Reasoning = c.Reasoning || o.Reasoning
	c.PromptCache = c.PromptCache || o.PromptCache
	c.Embeddings = c.Embeddings || o.Embeddings
	if o.ContextWindow > c.ContextWindow {
		c.ContextWindow = o.ContextWindow
	}
	if o.Media != nil {
		if c.Media == nil {
			m := *o.Media
			c.Media = &m
		} else {
			c.Media.Image = c.Media.Image || o.Media.Image
			c.Media.ImageEdit = c.Media.ImageEdit || o.Media.ImageEdit
			c.Media.Video = c.Media.Video || o.Media.Video
			c.Media.ImageToVideo = c.Media.ImageToVideo || o.Media.ImageToVideo
			c.Media.Upscale = c.Media.Upscale || o.Media.Upscale
		}
	}
}

// satisfies returns "" when have covers need, else the first missing capability.
func (have Capabilities) satisfies(need Capabilities) string {
	// Media and text endpoints never stand in for each other.
	switch {
	case need.Media == nil && have.Media != nil:
		return "media endpoint"
	case need.Media != nil && have.Media == nil:
		return "not a media endpoint"
	case need.Media != nil:
		switch {
		case need.Media.Image && !have.Media.Image:
			return "no image generation"
		case need.Media.ImageEdit && !have.Media.ImageEdit:
			return "no image editing"
		case need.Media.Video && !have.Media.Video:
			return "no video generation"
		case need.Media.ImageToVideo && !have.Media.ImageToVideo:
			return "no image to video"
		case need.Media.Upscale && !have.Media.Upscale:
			return "no upscaling"
		}
		return ""
	}
	switch {
	case need.Tools && !have.Tools:
		return "no tool calling"
	case need.Vision && !have.Vision:
		return "no vision"
	case need.JSONMode && !have.JSONMode:
		return "no json mode"
	case need.Embeddings && !have.Embeddings:
		return "not an embedding model"
	case need.ContextWindow > 0 && have.ContextWindow > 0 && have.ContextWindow < need.ContextWindow:
		return "context window too small"
	}
	return ""
}

// RequiredCapabilities derives hard requirements from a request.
func RequiredCapabilities(req *Request) Capabilities {
	var c Capabilities
	if len(req.Tools) > 0 {
		c.Tools = true
	}
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			if p.Kind == PartImage {
				c.Vision = true
			}
		}
	}
	switch req.Metadata.TaskClass {
	case TaskEmbed:
		c.Embeddings = true
	case TaskImage:
		c.Media = &MediaCaps{Image: true}
	case TaskVideo:
		c.Media = &MediaCaps{Video: true}
	}
	// A media job can be more precise: an edit, or image to video, needs
	// that capability rather than the class's default.
	if req.Media != nil && c.Media != nil {
		m := MediaCaps{Image: req.Media.Image, ImageEdit: req.Media.ImageEdit, Video: req.Media.Video, ImageToVideo: req.Media.ImageToVideo, Upscale: req.Media.Upscale}
		c.Media = &m
	}
	return c
}

// LoadPolicyDir reads every *.yaml in dir as a Policy.
func LoadPolicyDir(dir string) ([]Policy, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	var out []Policy
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		p, err := ParsePolicy(string(b))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		if p.Name == "" {
			p.Name = strings.TrimSuffix(filepath.Base(f), ".yaml")
		}
		out = append(out, p)
	}
	return out, nil
}

// ParsePolicy parses one policy document.
func ParsePolicy(y string) (Policy, error) {
	var p Policy
	if err := yaml.Unmarshal([]byte(y), &p); err != nil {
		return p, err
	}
	if p.Priority == 0 {
		p.Priority = 100
	}
	for i, rl := range p.Rules {
		if rl.Rank != "" && rl.Rank != "throughput" {
			return p, fmt.Errorf("rule %d: unknown rank %q (only \"throughput\")", i, rl.Rank)
		}
		if rl.MinTokensPerSec < 0 {
			return p, fmt.Errorf("rule %d: min_tokens_per_sec must not be negative", i)
		}
	}
	return p, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func containsTC(list []TaskClass, s TaskClass) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
