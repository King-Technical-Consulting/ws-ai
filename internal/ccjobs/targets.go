// Package ccjobs launches, lists, attaches to and kills Claude Code sessions
// in tmux windows on local or SSH targets. It is the dispatcher for the
// claude-subscription lane in docs/CLAUDE_CODE_JOBS.md and is deliberately
// narrow: it hands a prompt file to the official `claude` binary in a real
// interactive terminal and returns a handle. It never reads Claude Code's
// output or credentials.
package ccjobs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Target is one machine that can run Claude Code.
type Target struct {
	// Type is "local" or "ssh".
	Type string `toml:"type"`
	// Host is the ssh destination (an ssh_config alias is best). ssh only.
	Host string `toml:"host"`
	// Session is the tmux session that holds job windows (default "subscription").
	Session string `toml:"session"`
	// DefaultDir is the cwd when a job gives none ("~" allowed).
	DefaultDir string `toml:"default_dir"`
	// Claude optionally pins the claude binary path on the target. When empty
	// it is resolved with `command -v claude` at launch.
	Claude string `toml:"claude"`
	// TmuxSocket optionally selects a tmux server by socket name (-L). Tests use it.
	TmuxSocket string `toml:"tmux_socket"`
	// Repos lists directories on this target that jobs may run in, so the
	// router can pick the target from a job's cwd (spec §6.4, target
	// selection). DefaultDir counts as one without being listed.
	Repos []string `toml:"repos"`
}

// Config is ~/.config/wsj/targets.toml.
type Config struct {
	// Default names the target used when --on is omitted.
	Default string            `toml:"default"`
	Targets map[string]Target `toml:"targets"`
	// WS is where wsj reports job handles for the web tab (optional).
	WS WSConfig `toml:"ws"`
	// SSHConfig is an ssh_config file passed as `ssh -F` for every ssh
	// target ("~" allowed). It is how a host without ~/.ssh, such as the
	// ws container, names each target's key, user and known_hosts.
	// WSJ_SSH_CONFIG (and WS_CC_SSH_CONFIG in ws) override it.
	SSHConfig string `toml:"ssh_config"`
	// NoMux disables ssh connection multiplexing for every ssh target. It
	// is a CLI debugging switch (--no-mux), not a file setting.
	NoMux bool `toml:"-"`
}

var targetNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// DefaultConfigPath honors WSJ_TARGETS, then XDG, then ~/.config.
func DefaultConfigPath() string {
	if p := os.Getenv("WSJ_TARGETS"); p != "" {
		return p
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "wsj", "targets.toml")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "wsj", "targets.toml")
}

// LoadConfig reads and validates the targets file. A missing file yields a
// config with a single local target so `wsj run` works out of the box.
func LoadConfig(path string) (*Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			c = Config{Default: "local", Targets: map[string]Target{"local": {Type: "local"}}}
		} else {
			return nil, err
		}
	} else if err := toml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, c.validate()
}

func (c *Config) validate() error {
	if len(c.Targets) == 0 {
		return errors.New("targets: no [targets.<name>] sections")
	}
	for name, t := range c.Targets {
		if !targetNameRe.MatchString(name) {
			return fmt.Errorf("targets: bad target name %q", name)
		}
		switch t.Type {
		case "local":
			if t.Host != "" {
				return fmt.Errorf("targets.%s: host is only for type = \"ssh\"", name)
			}
		case "ssh":
			if t.Host == "" {
				return fmt.Errorf("targets.%s: host required for type = \"ssh\"", name)
			}
		default:
			return fmt.Errorf("targets.%s: type must be \"local\" or \"ssh\"", name)
		}
		if t.Session == "" {
			t.Session = "subscription"
		}
		if !sessionRe.MatchString(t.Session) {
			return fmt.Errorf("targets.%s: session must match %s", name, sessionRe)
		}
		if t.TmuxSocket != "" && !sessionRe.MatchString(t.TmuxSocket) {
			return fmt.Errorf("targets.%s: tmux_socket must match %s", name, sessionRe)
		}
		c.Targets[name] = t
	}
	if c.Default == "" {
		if len(c.Targets) == 1 {
			for name := range c.Targets {
				c.Default = name
			}
		} else if _, ok := c.Targets["local"]; ok {
			c.Default = "local"
		} else {
			return errors.New("targets: set default = \"<name>\" when more than one target is configured")
		}
	}
	if _, ok := c.Targets[c.Default]; !ok {
		return fmt.Errorf("targets: default %q is not a configured target", c.Default)
	}
	return nil
}

// sshConfig is the ssh_config path: the environment wins over the file.
func (c *Config) sshConfig() string {
	if p := strings.TrimSpace(os.Getenv("WSJ_SSH_CONFIG")); p != "" {
		return p
	}
	return strings.TrimSpace(c.SSHConfig)
}

// ExpandHome replaces a leading ~ with the home directory.
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// Names returns target names sorted.
func (c *Config) Names() []string {
	out := make([]string, 0, len(c.Targets))
	for n := range c.Targets {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Launcher builds the launcher for a named target ("" = default).
func (c *Config) Launcher(name string) (*Launcher, error) {
	if name == "" {
		name = c.Default
	}
	t, ok := c.Targets[name]
	if !ok {
		return nil, fmt.Errorf("unknown target %q (configured: %v)", name, c.Names())
	}
	var h Host
	switch t.Type {
	case "ssh":
		sh := NewSSHHost(name, t.Host, !c.NoMux)
		sh.ConfigFile = ExpandHome(c.sshConfig())
		h = sh
	default:
		h = LocalHost{name: name}
	}
	return &Launcher{Host: h, Session: t.Session, Claude: t.Claude, TmuxSocket: t.TmuxSocket, DefaultDir: t.DefaultDir}, nil
}
