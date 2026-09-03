// Package suite parses `suite.yaml` — an ordered list of scenario config
// paths that `myrtille run --suite` runs one after another, each as its
// own separate `myrtille run` subprocess (not an in-process loop — see
// docs/plans/suite-mode.md for why: config.Load mutates the process
// environment via os.Setenv, which would silently leak between scenarios
// if they shared one process).
package suite

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/thecoons/myrtille/internal/config"
)

// Config is the parsed shape of a suite.yaml file.
type Config struct {
	Scenarios []string `yaml:"scenarios"`
	// RestartBetweenRuns is nil (unset) by default, meaning true — every
	// scenario in Scenarios manages its own service.managed independently,
	// which already restarts the service between scenarios for free (see
	// docs/plans/suite-mode.md Décisions). Explicit false shares one
	// service instance across the whole suite, started/stopped once by
	// the `myrtille run --suite` process itself (see RunScenario's
	// skipServiceLifecycle) using the first scenario's service config —
	// every scenario's service.base_url must match.
	RestartBetweenRuns *bool `yaml:"restart_between_runs"`

	// dir is the directory containing the suite file; Scenarios entries
	// are resolved against it, like config.Config's K6.Script.
	dir string
}

// RestartsBetweenRuns reports whether each scenario restarts the service
// independently (the default, true) or shares one instance across the
// whole suite (explicit false).
func (c *Config) RestartsBetweenRuns() bool {
	return c.RestartBetweenRuns == nil || *c.RestartBetweenRuns
}

// Load reads, parses, and validates the suite file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading suite file: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing suite file: %w", err)
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolving suite file path: %w", err)
	}
	cfg.dir = filepath.Dir(absPath)

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if !cfg.RestartsBetweenRuns() {
		if err := cfg.validateSharedService(); err != nil {
			return nil, err
		}
	}

	return &cfg, nil
}

func (c *Config) validate() error {
	if len(c.Scenarios) == 0 {
		return fmt.Errorf("invalid suite file: scenarios must list at least one config path")
	}
	for i, s := range c.Scenarios {
		if s == "" {
			return fmt.Errorf("invalid suite file: scenarios[%d] is empty", i)
		}
	}
	return nil
}

// validateSharedService checks that restart_between_runs: false is
// actually usable: the first scenario must configure service.managed
// (there's nothing to share otherwise), and every scenario must target the
// same service.base_url — a suite sharing one instance across scenarios
// pointed at different base URLs would be a silent config mistake, not
// something to guess past.
func (c *Config) validateSharedService() error {
	paths := c.ScenarioPaths()

	first, err := config.Load(paths[0])
	if err != nil {
		return fmt.Errorf("loading %s to resolve the shared service config: %w", paths[0], err)
	}
	if first.Service.Managed == nil {
		return fmt.Errorf("restart_between_runs: false requires the first scenario (%s) to configure service.managed — there is nothing to share otherwise", paths[0])
	}

	for i, p := range paths[1:] {
		cfg, err := config.Load(p)
		if err != nil {
			return fmt.Errorf("loading %s to check service.base_url: %w", p, err)
		}
		if cfg.Service.BaseURL != first.Service.BaseURL {
			return fmt.Errorf("restart_between_runs: false requires every scenario to share the same service.base_url; scenarios[%d] (%s) has %q, expected %q (from %s)",
				i+1, p, cfg.Service.BaseURL, first.Service.BaseURL, paths[0])
		}
	}

	return nil
}

// ScenarioPaths returns each configured scenario path, resolved against
// the suite file's own directory — relative like config.Config's
// K6.Script, absolute paths pass through unchanged.
func (c *Config) ScenarioPaths() []string {
	paths := make([]string, len(c.Scenarios))
	for i, s := range c.Scenarios {
		paths[i] = c.resolvePath(s)
	}
	return paths
}

func (c *Config) resolvePath(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.dir, p)
}
