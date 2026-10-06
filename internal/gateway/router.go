package gateway

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

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
	Aliases  map[string][]string `yaml:"aliases" json:"aliases"`
}

// Rule matches requests and yields an ordered preference list.
type Rule struct {
	Match   Match        `yaml:"match" json:"match"`
	Prefer  []string     `yaml:"prefer" json:"prefer"`
	Deny    []string     `yaml:"deny" json:"deny"`
	Require Capabilities `yaml:"require" json:"require"`
	// MaxCostPerCallUSD drops candidates whose estimated cost exceeds this.
	MaxCostPerCallUSD float64 `yaml:"max_cost_per_call_usd" json:"max_cost_per_call_usd"`
	// LocalOnly restricts candidates to Local endpoints (used by budget downgrade).
	LocalOnly bool `yaml:"local_only" json:"local_only"`
}

// Match conditions. Empty slices match everything.
type Match struct {
	TaskClass []TaskClass `yaml:"task_class" json:"task_class"`
	Users     []string    `yaml:"users" json:"users"`
	Agents    []string    `yaml:"agents" json:"agents"`
	Selector  []string    `yaml:"selector" json:"selector"` // match the request's Model selector
	External  *bool       `yaml:"external" json:"external"`
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
	// Downgrade is set by the budget middleware when a budget is exceeded
	// with on_exceed=downgrade: only Local endpoints may be used.
	Downgrade bool
}

// Candidate is one endpoint the router chose, in order.
type Candidate struct {
	Endpoint *Endpoint
	Provider *Provider
	Reason   string
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
	policies []Policy // sorted by priority asc
}

// NewRouter creates a router.
func NewRouter(reg *Registry, policies []Policy) *Router {
	r := &Router{reg: reg}
	r.SetPolicies(policies)
	return r
}

// SetPolicies replaces the policy set.
func (r *Router) SetPolicies(ps []Policy) {
	sorted := append([]Policy(nil), ps...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Priority < sorted[j].Priority })
	r.policies = sorted
}

// Policies returns the current policies.
func (r *Router) Policies() []Policy { return r.policies }

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

	// 2. Alias or auto: walk policies in priority order; first matching rule
	//    (or alias) wins.
	var prefs []string
	var rule Rule
	for _, p := range r.policies {
		if in.Selector != "" && in.Selector != "auto" {
			if list, ok := p.Aliases[in.Selector]; ok {
				prefs = list
				dec.Policy = p.Name
				dec.Reason = "alias"
				break
			}
		}
		for i, rl := range p.Rules {
			if rl.Match.matches(in) {
				prefs, rule = rl.Prefer, rl
				dec.Policy, dec.Rule = p.Name, i
				dec.Reason = "rule"
				break
			}
		}
		if prefs != nil {
			break
		}
	}
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
	if len(out) == 0 {
		return nil, dec, fmt.Errorf("router: no endpoint satisfies selector %q task %q (caps %+v, downgrade=%v)", in.Selector, in.TaskClass, in.Required, in.Downgrade)
	}
	return out, dec, nil
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
