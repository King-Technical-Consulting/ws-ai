package agents

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/jking323/ws/internal/agent"
	"github.com/jking323/ws/internal/gateway"
)

// Preset is a starting point for a new agent (config/presets/*.yaml): the
// fields of an agent row, copied into the form when picked. Nothing links
// the agent back to the preset afterwards, so later edits to either never
// touch the other. Presets live in files, not the database: reviewable in
// git, no migration, readable by an operator.
type Preset struct {
	// Name is the id (lowercase, digits, dashes); the file name without
	// .yaml when omitted.
	Name        string `yaml:"name" json:"name"`
	Title       string `yaml:"title" json:"title"`
	Description string `yaml:"description" json:"description"`
	// Goal and Prompt prefill the agent's goal and system prompt.
	Goal   string `yaml:"goal" json:"goal"`
	Prompt string `yaml:"prompt" json:"prompt"`
	// Model is the model policy (selector, task class, reasoning).
	Model ModelPolicy `yaml:"model" json:"model"`
	// Tools is the allowlist. Required: an empty list means no tools
	// (written as "none" on the agent), since on an agent an empty
	// allowlist means every tool and a preset must never widen by
	// omission. ["*"] means every tool.
	Tools    []string `yaml:"tools" json:"tools"`
	MaxSteps int      `yaml:"max_steps" json:"max_steps"`
	// Note is shown under the preset when picked (what it is for, what to
	// watch out for).
	Note string `yaml:"note" json:"note"`
}

// AllTools in a preset's tools list means every tool the worker has.
const AllTools = "*"

var presetNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// LoadPresets reads every *.yaml in dir, sorted by name. known, when set,
// reports whether a tool name exists; unknown names are returned in warn
// (the preset still loads: some tools exist on the worker only).
func LoadPresets(dir string, known func(string) bool) (presets []Preset, warn []string, err error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, nil, err
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, nil, err
		}
		var p Preset
		if err := yaml.Unmarshal(b, &p); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", f, err)
		}
		if p.Name == "" {
			p.Name = strings.TrimSuffix(filepath.Base(f), ".yaml")
		}
		if err := p.validate(); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", f, err)
		}
		if known != nil {
			for _, t := range p.Tools {
				if t != agent.NoTools && t != AllTools && !known(t) {
					warn = append(warn, p.Name+": unknown tool "+t)
				}
			}
		}
		presets = append(presets, p)
	}
	sort.SliceStable(presets, func(i, j int) bool { return presets[i].Name < presets[j].Name })
	return presets, warn, nil
}

func (p *Preset) validate() error {
	if !presetNameRe.MatchString(p.Name) {
		return fmt.Errorf("name %q: lowercase letters, digits and dashes", p.Name)
	}
	if strings.TrimSpace(p.Title) == "" {
		return errors.New("title is required")
	}
	switch p.Model.TaskClass {
	case "":
		p.Model.TaskClass = gateway.TaskChat
	case gateway.TaskChat, gateway.TaskCode, gateway.TaskSummarize, gateway.TaskVision:
	default:
		return fmt.Errorf("model.task_class %q: chat, code, summarize or vision", p.Model.TaskClass)
	}
	if p.Model.Selector == "" {
		p.Model.Selector = "auto"
	}
	if p.Tools == nil {
		return errors.New("tools is required: [] for none, [\"*\"] for every tool, or a list of names")
	}
	// Normalize: an empty list becomes the agent's "none" word, "*" alone
	// becomes the agent's empty list (every tool), and names are trimmed
	// and unique.
	var tools []string
	seen := map[string]bool{}
	all := false
	for _, t := range p.Tools {
		t = strings.TrimSpace(t)
		switch {
		case t == "":
			continue
		case t == AllTools:
			all = true
		case t == agent.NoTools:
			continue
		case !seen[t]:
			seen[t] = true
			tools = append(tools, t)
		}
	}
	switch {
	case all && len(tools) > 0:
		return errors.New("tools: \"*\" cannot be combined with names")
	case all:
		p.Tools = []string{}
	case len(tools) == 0:
		p.Tools = []string{agent.NoTools}
	default:
		p.Tools = tools
	}
	if p.MaxSteps <= 0 {
		p.MaxSteps = 50
	}
	if p.MaxSteps > 500 {
		return errors.New("max_steps is at most 500")
	}
	return nil
}
