package training

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"gopkg.in/yaml.v3"

	"github.com/jking323/ws/internal/gateway"
	"github.com/jking323/ws/internal/store"
)

// The training flywheel's own routing policy. Promoting an adapter into a
// task class, or routing a task class to a frontier model to collect its
// answers (distillation), writes one rule per task class into a policy
// named AdapterPolicy, at a priority that beats the default policy. The
// rules are derived from a `training.routes` list kept in the same
// document, so the policy editor in Admin shows exactly what the page
// did, and the page can take a route out again without guessing which
// endpoints it put in. Each rule prefers the routed endpoints (newest
// first) and then whatever the other policies preferred for that class,
// so the gateway still fails over when the adapter's box is down.

// AdapterPolicy is the policy's name.
const AdapterPolicy = "training-adapters"

// adapterPolicyPriority beats the default policy (100).
const adapterPolicyPriority = 50

const adapterPolicyHeader = `# Written by the Training page (promote into a task class, or route a
# class to a model to distil from). The rules are rebuilt from
# training.routes on every change; edit the routes, not the rules, or
# rename the policy to take it over by hand.
`

// Route sends one task class to one endpoint first.
type Route struct {
	TaskClass string `yaml:"task_class" json:"task_class"`
	Endpoint  string `yaml:"endpoint" json:"endpoint"`
}

// routedPolicy is the document: the gateway reads name, priority and
// rules; training reads its routes back from the training block.
type routedPolicy struct {
	Name     string         `yaml:"name"`
	Priority int            `yaml:"priority"`
	Rules    []gateway.Rule `yaml:"rules"`
	Training struct {
		Routes []Route `yaml:"routes"`
	} `yaml:"training"`
}

var taskClassRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// Routes lists the routes the policy holds.
func (s *Service) Routes(ctx context.Context) ([]Route, error) {
	p, err := s.loadRoutes(ctx)
	if err != nil {
		return nil, err
	}
	return p.Training.Routes, nil
}

// AddRoute puts endpoint first for taskClass (ahead of an earlier route
// for the same class) and rewrites the policy. The endpoint must exist.
func (s *Service) AddRoute(ctx context.Context, taskClass, endpoint string) error {
	endpoint = strings.TrimSpace(endpoint)
	if s.Endpoint != nil {
		if _, ok := s.Endpoint(endpoint); !ok {
			return fmt.Errorf("%w: unknown endpoint %q", ErrInvalid, endpoint)
		}
	}
	return s.addRoute(ctx, taskClass, endpoint)
}

// addRoute is AddRoute for an endpoint known to exist (an adapter's own).
func (s *Service) addRoute(ctx context.Context, taskClass, endpoint string) error {
	taskClass = strings.ToLower(strings.TrimSpace(taskClass))
	if !taskClassRe.MatchString(taskClass) {
		return fmt.Errorf("%w: task class: lowercase letters, digits, dashes and underscores", ErrInvalid)
	}
	if endpoint == "" {
		return fmt.Errorf("%w: endpoint is required", ErrInvalid)
	}
	p, err := s.loadRoutes(ctx)
	if err != nil {
		return err
	}
	kept := p.Training.Routes[:0]
	for _, r := range p.Training.Routes {
		if r.TaskClass != taskClass || r.Endpoint != endpoint {
			kept = append(kept, r)
		}
	}
	p.Training.Routes = append([]Route{{TaskClass: taskClass, Endpoint: endpoint}}, kept...)
	return s.saveRoutes(ctx, p)
}

// RemoveRoute takes one route out; with an empty taskClass, every route
// to the endpoint (an adapter unpromoted or deleted).
func (s *Service) RemoveRoute(ctx context.Context, taskClass, endpoint string) error {
	p, err := s.loadRoutes(ctx)
	if err != nil {
		return err
	}
	kept := p.Training.Routes[:0]
	for _, r := range p.Training.Routes {
		if r.Endpoint == endpoint && (taskClass == "" || r.TaskClass == taskClass) {
			continue
		}
		kept = append(kept, r)
	}
	if len(kept) == len(p.Training.Routes) {
		return nil
	}
	p.Training.Routes = kept
	return s.saveRoutes(ctx, p)
}

func (s *Service) loadRoutes(ctx context.Context) (*routedPolicy, error) {
	p := &routedPolicy{Name: AdapterPolicy, Priority: adapterPolicyPriority}
	row, err := s.DB.GetRoutingPolicyByName(ctx, AdapterPolicy)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, nil
	}
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal([]byte(row.Yaml), p); err != nil {
		return nil, fmt.Errorf("training: the %s policy does not parse: %w", AdapterPolicy, err)
	}
	p.Name, p.Priority = AdapterPolicy, int(row.Priority)
	return p, nil
}

// saveRoutes rebuilds the rules from the routes and stores the policy,
// or deletes it when no route is left.
func (s *Service) saveRoutes(ctx context.Context, p *routedPolicy) error {
	if len(p.Training.Routes) == 0 {
		if err := s.DB.DeleteRoutingPolicyByName(ctx, AdapterPolicy); err != nil {
			return err
		}
		s.reloadPolicies(ctx)
		return nil
	}
	p.Rules = nil
	var classes []string
	byClass := map[string][]string{}
	for _, r := range p.Training.Routes {
		if _, seen := byClass[r.TaskClass]; !seen {
			classes = append(classes, r.TaskClass)
		}
		byClass[r.TaskClass] = append(byClass[r.TaskClass], r.Endpoint)
	}
	sort.Strings(classes) // one rule per class, in a stable order
	for _, tc := range classes {
		prefer := byClass[tc]
		if s.BasePrefer != nil {
			for _, id := range s.BasePrefer(tc) {
				if !containsStr(prefer, id) {
					prefer = append(prefer, id)
				}
			}
		}
		p.Rules = append(p.Rules, gateway.Rule{Match: gateway.Match{TaskClass: []gateway.TaskClass{gateway.TaskClass(tc)}}, Prefer: prefer})
	}
	b, err := yaml.Marshal(p)
	if err != nil {
		return err
	}
	doc := adapterPolicyHeader + string(b)
	if _, err := gateway.ParsePolicy(doc); err != nil {
		return fmt.Errorf("training: the rebuilt %s policy does not parse: %w", AdapterPolicy, err)
	}
	if _, err := s.DB.UpsertRoutingPolicy(ctx, store.UpsertRoutingPolicyParams{Name: AdapterPolicy, Yaml: doc, Priority: int32(p.Priority), Enabled: true}); err != nil {
		return err
	}
	s.reloadPolicies(ctx)
	return nil
}

func (s *Service) reloadPolicies(ctx context.Context) {
	if s.ReloadPolicies != nil {
		if err := s.ReloadPolicies(ctx); err != nil {
			s.log().Warn("training: policies reload", "err", err)
		}
	}
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Preview returns the first n examples of a dataset's train (or eval)
// file, parsed, so the page can show what a trainer would see.
func (s *Service) Preview(ctx context.Context, id uuid.UUID, eval bool, n int) ([]Example, error) {
	if n <= 0 {
		n = 5
	}
	if n > 50 {
		n = 50
	}
	b, err := s.DatasetFile(ctx, id, eval)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	out := []Example{}
	for dec.More() && len(out) < n {
		var e Example
		if err := dec.Decode(&e); err != nil {
			return out, nil
		}
		out = append(out, e)
	}
	return out, nil
}
