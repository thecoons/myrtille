// Package config parses and validates the myrtille.yaml file that describes
// a single load-test project: the service under test, the declarative init
// steps, the k6 script to run, and where to write the report.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var validMethods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "PATCH": true,
	"DELETE": true, "HEAD": true, "OPTIONS": true,
}

var validFormats = map[string]bool{"markdown": true, "json": true, "html": true}

// Duration wraps time.Duration so it can be parsed from a YAML string such
// as "5s", since time.Duration's underlying int64 does not unmarshal from a
// human-readable string on its own.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) Duration() time.Duration { return time.Duration(d) }

type Config struct {
	Name    string         `yaml:"name"`
	Ref     string         `yaml:"ref"`
	Vars    map[string]any `yaml:"vars"`
	Service ServiceConfig  `yaml:"service"`
	Init    InitConfig     `yaml:"init"`
	K6      K6Config       `yaml:"k6"`
	Report  ReportConfig   `yaml:"report"`

	// dir is the directory containing the config file; relative paths
	// (k6 script, report output dir) are resolved against it.
	dir string
}

type ServiceConfig struct {
	BaseURL string        `yaml:"base_url"`
	Metrics MetricsConfig `yaml:"metrics"`
}

type MetricsConfig struct {
	URL      string   `yaml:"url"`
	Interval Duration `yaml:"interval"`
}

type InitConfig struct {
	Steps []Step `yaml:"steps"`
}

// Step describes one declarative HTTP call (repeated Count times) run during
// the init phase. Count is a string rather than an int so it can hold either
// a plain literal ("20") or a text/template expression resolved at run time
// against the project's vars and the current template funcs (e.g.
// "{{.Vars.users_count}}", "{{random 1 5}}") — see internal/initphase.
type Step struct {
	Name    string            `yaml:"name"`
	Method  string            `yaml:"method"`
	URL     string            `yaml:"url"`
	Headers map[string]string `yaml:"headers"`
	Body    string            `yaml:"body"`
	Count   string            `yaml:"count"`
	Extract []Extract         `yaml:"extract"`
	// Children are executed once per iteration of this step, with access to
	// the parsed JSON response of that iteration (exposed as `.Parent` in
	// their own url/body/count templates) — see internal/initphase.
	Children []Step `yaml:"children"`
}

type Extract struct {
	Path string `yaml:"path"`
	As   string `yaml:"as"`
}

type K6Config struct {
	Script   string   `yaml:"script"`
	Args     []string `yaml:"args"`
	StateEnv string   `yaml:"state_env"`
}

type ReportConfig struct {
	OutputDir string   `yaml:"output_dir"`
	Formats   []string `yaml:"formats"`
}

// Load reads, parses, defaults and validates the config at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolving config path: %w", err)
	}
	cfg.dir = filepath.Dir(absPath)

	cfg.applyDefaults()

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// Dir returns the absolute directory containing the config file.
func (c *Config) Dir() string { return c.dir }

// K6ScriptPath returns the k6 script path resolved against the config file's directory.
func (c *Config) K6ScriptPath() string {
	return c.resolvePath(c.K6.Script)
}

// ReportOutputDir returns the report output directory resolved against the config file's directory.
func (c *Config) ReportOutputDir() string {
	return c.resolvePath(c.Report.OutputDir)
}

func (c *Config) resolvePath(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.dir, p)
}

func (c *Config) applyDefaults() {
	if c.Service.Metrics.Interval == 0 {
		c.Service.Metrics.Interval = Duration(5 * time.Second)
	}
	if c.K6.StateEnv == "" {
		c.K6.StateEnv = "STATE_FILE"
	}
	if c.Report.OutputDir == "" {
		c.Report.OutputDir = "./reports"
	}
	if len(c.Report.Formats) == 0 {
		c.Report.Formats = []string{"markdown", "json"}
	}
	applyStepDefaults(c.Init.Steps)
}

func applyStepDefaults(steps []Step) {
	for i := range steps {
		step := &steps[i]
		if step.Method == "" {
			step.Method = "GET"
		}
		step.Method = strings.ToUpper(step.Method)
		if step.Count == "" {
			step.Count = "1"
		}
		applyStepDefaults(step.Children)
	}
}

// Validate checks that the config is structurally sound. It does not check
// reachability of the service or existence of the k6 script on disk.
func (c *Config) Validate() error {
	var errs []string

	if c.Service.BaseURL == "" {
		errs = append(errs, "service.base_url is required")
	}
	if c.K6.Script == "" {
		errs = append(errs, "k6.script is required")
	}

	errs = append(errs, validateSteps("init.steps", c.Init.Steps)...)

	for _, f := range c.Report.Formats {
		if !validFormats[f] {
			errs = append(errs, fmt.Sprintf("report.formats: unsupported format %q", f))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("invalid config:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

func validateSteps(prefix string, steps []Step) []string {
	var errs []string

	for i, step := range steps {
		label := fmt.Sprintf("%s[%d]", prefix, i)
		if step.Name != "" {
			label = fmt.Sprintf("%s[%d] (%s)", prefix, i, step.Name)
		}
		if step.URL == "" {
			errs = append(errs, fmt.Sprintf("%s: url is required", label))
		}
		if !validMethods[step.Method] {
			errs = append(errs, fmt.Sprintf("%s: unsupported method %q", label, step.Method))
		}
		// Count may be a template expression (e.g. "{{.Vars.x}}" or
		// "{{random 1 5}}"), only resolved at run time. Only a plain
		// literal can be checked statically here.
		if !strings.Contains(step.Count, "{{") {
			if n, err := strconv.Atoi(step.Count); err != nil || n < 0 {
				errs = append(errs, fmt.Sprintf("%s: count must be a non-negative integer or a template expression, got %q", label, step.Count))
			}
		}
		for j, ex := range step.Extract {
			if ex.Path == "" {
				errs = append(errs, fmt.Sprintf("%s.extract[%d]: path is required", label, j))
			}
			if ex.As == "" {
				errs = append(errs, fmt.Sprintf("%s.extract[%d]: as is required", label, j))
			}
		}

		errs = append(errs, validateSteps(fmt.Sprintf("%s.children", label), step.Children)...)
	}

	return errs
}
