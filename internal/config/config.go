// Package config parses and validates the myrtille.yaml file that describes
// a single load-test project: the service under test, the declarative init
// steps, the k6 script to run, and where to write the report.
package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var validMethods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "PATCH": true,
	"DELETE": true, "HEAD": true, "OPTIONS": true,
}

var validFormats = map[string]bool{"markdown": true, "json": true, "dashboard-html": true}

// validStopSignals are the signal names service.stop_signal accepts —
// deliberately just the common shutdown-relevant subset, not every signal
// syscall.Signal knows about.
var validStopSignals = map[string]bool{"TERM": true, "INT": true, "HUP": true, "QUIT": true, "KILL": true}

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
	Name     string         `yaml:"name"`
	Ref      string         `yaml:"ref"`
	Vars     map[string]any `yaml:"vars"`
	Service  ServiceConfig  `yaml:"service"`
	Init     InitConfig     `yaml:"init"`
	Teardown TeardownConfig `yaml:"teardown"`
	K6       K6Config       `yaml:"k6"`
	Report   ReportConfig   `yaml:"report"`

	// EnvFile is a path to a .env file whose KEY=value pairs are merged
	// into the process environment at load time, for values not already
	// present in the process env before myrtille started. Resolved
	// relative to the config file's directory, like K6.Script.
	EnvFile string `yaml:"env_file"`

	// dir is the directory containing the config file; relative paths
	// (k6 script, report output dir) are resolved against it.
	dir string
}

type ServiceConfig struct {
	BaseURL string        `yaml:"base_url"`
	Metrics MetricsConfig `yaml:"metrics"`
	// StartCommand, when set, makes `myrtille run` launch the service
	// itself (via `sh -c`, inheriting the process env like init.command)
	// before the init phase, wait for Readiness, then stop it (best-effort)
	// after teardown. Unset means today's behavior: the service is assumed
	// to already be running, and myrtille never touches it. See
	// internal/servicelifecycle.
	StartCommand string `yaml:"start_command"`
	// StopSignal names the signal sent to the whole process group started
	// by StartCommand (not just its direct PID) when the run finishes.
	// Defaults to "TERM" when StartCommand is set; meaningless (and
	// rejected by Validate) otherwise.
	StopSignal string `yaml:"stop_signal"`
	// Readiness configures how `myrtille run` waits for the
	// StartCommand-launched service to come up before the init phase
	// starts. Required when StartCommand is set; rejected otherwise.
	Readiness ReadinessConfig `yaml:"readiness"`
	// StopTimeout bounds how long to wait for the service to actually stop
	// (its readiness URL to stop responding) after StopSignal is sent,
	// before giving up — best-effort, never fails the run. Defaults to 30s
	// when StartCommand is set; rejected otherwise.
	StopTimeout Duration `yaml:"stop_timeout"`
}

type MetricsConfig struct {
	URL      string   `yaml:"url"`
	Interval Duration `yaml:"interval"`
}

// ReadinessConfig configures the readiness poll used to wait for a
// StartCommand-launched service to come up. URL is resolved against
// Service.BaseURL; any response status below 400 counts as ready.
type ReadinessConfig struct {
	URL      string   `yaml:"url"`
	Timeout  Duration `yaml:"timeout"`
	Interval Duration `yaml:"interval"`
}

// InitConfig configures the init phase. At most one of Steps or Command may
// be set: Steps is the declarative template/count/extract mini-language;
// Command instead runs an external setup script for seeding logic too
// dynamic to express declaratively (recursive generators, per-level
// arithmetic, etc.) — see internal/initphase.RunCommand. Neither is
// required — a project with no seeding needs configures neither.
type InitConfig struct {
	Steps []Step `yaml:"steps"`
	// Command is a shell command line (run via `sh -c`), inheriting the
	// current process's environment like k6.script. It must write a
	// state.Dict-shaped JSON object to the path given via the
	// MYRTILLE_STATE_OUTPUT env var before exiting 0.
	Command string `yaml:"command"`
	// CommandTimeout bounds how long Command may run; defaults to 5m.
	CommandTimeout Duration `yaml:"command_timeout"`
	// Derive computes additional state.Dict keys, once, after Steps/Command
	// (or --state-file) have produced the dict — for aggregations that need
	// the whole collection at once (e.g. a set difference) rather than a
	// per-response gjson path. See internal/initphase.Derive.
	Derive []DeriveRule `yaml:"derive"`
}

// DeriveRule computes one state.Dict key by running Expr, a jq expression,
// once against dict[Input] (or the whole dict, as JSON, if Input is empty),
// and writing the result into dict[As] — replacing any existing value for
// that key, unlike Extract's per-iteration append, since Expr runs once
// against the already-complete collection. Rules run in declaration order,
// each able to read a key an earlier rule wrote.
type DeriveRule struct {
	As    string `yaml:"as"`
	Expr  string `yaml:"expr"`
	Input string `yaml:"input"`
}

// TeardownConfig declares HTTP steps run after k6, best-effort, to remove
// whatever init.steps created (e.g. `{{index .Dict.user_ids .Index}}`
// against `count: "{{len .Dict.user_ids}}"`) — see internal/initphase.RunTeardown.
type TeardownConfig struct {
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

// K6Config configures the run phase. Exactly one of Script or Steps must be
// set: Script points at a hand-written k6 script (full control, e.g. custom
// checks, non-HTTP protocols); Steps declares a simple HTTP scenario that
// myrtille generates into a k6 script itself — see internal/k6gen.
type K6Config struct {
	Script string   `yaml:"script"`
	Steps  []K6Step `yaml:"steps"`
	// Setup declares HTTP calls that run once, before any k6.steps
	// iteration (via a generated k6 setup() function), for bootstrapping a
	// shared resource once rather than on every iteration. Only used with
	// Steps — see internal/k6gen.
	Setup    []K6SetupStep `yaml:"setup"`
	Options  K6Options     `yaml:"options"`
	Args     []string      `yaml:"args"`
	StateEnv string        `yaml:"state_env"`
}

// K6SetupStep describes one HTTP call made exactly once, in declaration
// order, before any k6.steps iteration. Same declarative shape as
// init.steps' Step, minus Count/Children (a run-once bootstrap call has no
// repetition axis) and Tags/Checks/Sleep (those are about a load-test's own
// per-iteration metrics, not a one-off setup call). Extracted values are
// merged into the same state pool k6.steps' pick/random read from — see
// internal/k6gen. Unlike init.steps' gjson-based Extract, path here is a
// plain dot-separated JS property/array-index path (e.g. "name",
// "items.0.id") — no gjson's `#.field` flatten-map.
type K6SetupStep struct {
	Name    string            `yaml:"name"`
	Method  string            `yaml:"method"`
	URL     string            `yaml:"url"`
	Headers map[string]string `yaml:"headers"`
	Body    string            `yaml:"body"`
	Extract []Extract         `yaml:"extract"`
}

// K6Step describes one HTTP call made once per k6 iteration, in declaration
// order. Unlike init's Step, there is no Count/Extract/Children: the
// repetition axis for a scenario is k6's own VUs/duration/iterations
// (configured via K6Options), not a per-step count. url/body/header values
// are Go templates with access to `.BaseURL`/`.Vars`, plus `pick`/`random`
// funcs that — unlike their init-phase namesakes — expand to JS source
// fragments resolved by the generated script at k6-runtime, once per
// iteration, rather than once by Go at generation time. See internal/k6gen.
type K6Step struct {
	Name    string            `yaml:"name"`
	Method  string            `yaml:"method"`
	URL     string            `yaml:"url"`
	Headers map[string]string `yaml:"headers"`
	Body    string            `yaml:"body"`
	// BodyFrom names a state dict pool to pick one full object from and
	// deep-clone as this step's body, instead of a plain Body template —
	// for a full-replace PUT that must resend an existing object with only
	// a few fields changed (see BodyPatch). Mutually exclusive with Body.
	// The pick is correlated with any {{pick "pool" "field"}} on the same
	// pool elsewhere in this step (same pickState draw) — see
	// internal/k6gen.
	BodyFrom string `yaml:"body_from"`
	// BodyPatch maps a dot-separated path (object nesting only, no array
	// indices — e.g. "metadata.labels.touched") to a template string
	// (same funcs as Body/URL: pick/random/uniqueId all usable) applied on
	// top of the BodyFrom-picked object's clone before it's sent. A path
	// whose intermediate segment doesn't exist on the picked object fails
	// at k6 runtime (a real JS TypeError) rather than silently creating
	// it. Requires BodyFrom.
	BodyPatch map[string]string `yaml:"body_patch"`
	// Tags maps a k6 metric tag name to its value template, passed through
	// to the generated http.request(..., { tags: {...} }) call — so
	// http_req_duration (and other request metrics) for this step can be
	// segmented per logical variant of a scenario (e.g.
	// `http_req_duration{endpoint:get}` vs `{endpoint:list}` in
	// k6.options.thresholds), the same way a hand-written k6.script would
	// pass `{tags: {...}}` itself.
	Tags map[string]string `yaml:"tags"`
	// Checks maps a check name to a JS boolean expression with the k6
	// response bound to `r` (e.g. "r.status === 201"), spliced verbatim
	// into a generated `check(res, {...})` call.
	Checks map[string]string `yaml:"checks"`
	Sleep  Duration          `yaml:"sleep"`
	// Repeat is a Go template (like init.steps' Count) resolved once at
	// generation time — not at k6 runtime, unlike pick/random — against
	// .BaseURL/.Vars, wrapping this step's request in a JS for loop that
	// runs it that many times per k6 iteration. Empty means run once, with
	// no loop generated (today's behavior, unchanged).
	Repeat string `yaml:"repeat"`
}

// K6Options configures the generated scenario's `export const options`
// block. Only meaningful (and only allowed) when K6Config.Steps is used —
// with a custom Script, options belong in the script itself.
type K6Options struct {
	VUs        int                 `yaml:"vus"`
	Duration   Duration            `yaml:"duration"`
	Iterations int                 `yaml:"iterations"`
	Stages     []K6Stage           `yaml:"stages"`
	Thresholds map[string][]string `yaml:"thresholds"`
}

// IsZero reports whether no K6Options field was set.
func (o K6Options) IsZero() bool {
	return o.VUs == 0 && o.Duration == 0 && o.Iterations == 0 && len(o.Stages) == 0 && len(o.Thresholds) == 0
}

type K6Stage struct {
	Duration Duration `yaml:"duration"`
	Target   int      `yaml:"target"`
}

type ReportConfig struct {
	OutputDir string   `yaml:"output_dir"`
	Formats   []string `yaml:"formats"`
}

// Load reads, parses, defaults and validates the config at path.
// LoadOption customizes Load's behavior.
type LoadOption func(*loadOptions)

type loadOptions struct {
	envFileOverride string
}

// WithEnvFileOverride overrides the config's env_file field with an
// explicit path, resolved relative to the current working directory (like
// the --config flag/path itself) rather than the config file's directory.
// Used by the --env-file CLI flag so it can point elsewhere without
// editing the config; if both are set, the override wins entirely (the
// two files are not merged).
func WithEnvFileOverride(path string) LoadOption {
	return func(o *loadOptions) { o.envFileOverride = path }
}

func Load(path string, opts ...LoadOption) (*Config, error) {
	var lo loadOptions
	for _, opt := range opts {
		opt(&lo)
	}

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

	if err := cfg.loadEnvFile(lo.envFileOverride); err != nil {
		return nil, err
	}

	expandVars(cfg.Vars)

	cfg.applyDefaults()

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	if err := cfg.resolveServiceURLs(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// resolveServiceURLs makes service.metrics.url absolute when it's a
// base_url-relative path (e.g. "/metrics"), the same treatment
// service.readiness.url already gets (see
// internal/servicelifecycle.resolveReadinessURL) — resolved here, once at
// load time, so every consumer (internal/dashboardconfig.Build, the
// generated k6 script's promscrape.Scraper call) already sees an absolute
// URL and none of them need to duplicate the resolution. Runs after
// Validate so service.base_url is known to be non-empty.
func (c *Config) resolveServiceURLs() error {
	if c.Service.Metrics.URL == "" {
		return nil
	}

	base, err := url.Parse(c.Service.BaseURL)
	if err != nil {
		return fmt.Errorf("parsing service.base_url: %w", err)
	}
	ref, err := url.Parse(c.Service.Metrics.URL)
	if err != nil {
		return fmt.Errorf("parsing service.metrics.url: %w", err)
	}
	c.Service.Metrics.URL = base.ResolveReference(ref).String()

	return nil
}

// loadEnvFile merges the KEY=value pairs of a .env file into the process
// environment, skipping any key already present in the process env (a
// value the caller already exported always wins over the file). override,
// when non-empty, takes precedence over c.EnvFile and is resolved against
// the current working directory instead of the config file's directory.
// Neither set is a silent no-op; an explicitly configured path that
// doesn't exist is a load error.
func (c *Config) loadEnvFile(override string) error {
	var path string
	switch {
	case override != "":
		abs, err := filepath.Abs(override)
		if err != nil {
			return fmt.Errorf("resolving env-file path: %w", err)
		}
		path = abs
	case c.EnvFile != "":
		path = c.resolvePath(c.EnvFile)
	default:
		return nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading env file %q: %w", path, err)
	}
	vars, err := ParseEnvFile(data)
	if err != nil {
		return fmt.Errorf("env file %q: %w", path, err)
	}
	for key, value := range vars {
		if _, ok := os.LookupEnv(key); ok {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("setting env var %q from env file %q: %w", key, path, err)
		}
	}
	return nil
}

// expandVars expands `${VAR}` and `${VAR:-default}` references against the
// process environment in every string value of vars, in place. It runs
// after loadEnvFile, so a `vars:` entry can reference a value that came
// from the project's .env file, not just one already exported in the
// caller's shell. Non-string values (numbers, bools, nested
// maps/lists) are left untouched — only a plain string value can hold an
// env reference. `${VAR}` resolves to VAR's value, or "" if unset;
// `${VAR:-default}` resolves to default if VAR is unset or empty,
// matching shell `:-` (not bash's unset-only `-`).
func expandVars(vars map[string]any) {
	for k, v := range vars {
		s, ok := v.(string)
		if !ok {
			continue
		}
		vars[k] = os.Expand(s, expandVarRef)
	}
}

func expandVarRef(ref string) string {
	name, def, hasDefault := strings.Cut(ref, ":-")
	if value, ok := os.LookupEnv(name); ok && value != "" {
		return value
	}
	if hasDefault {
		return def
	}
	return os.Getenv(name)
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
	if c.Service.StartCommand != "" {
		if c.Service.StopSignal == "" {
			c.Service.StopSignal = "TERM"
		}
		if c.Service.Readiness.Timeout == 0 {
			c.Service.Readiness.Timeout = Duration(5 * time.Minute)
		}
		if c.Service.Readiness.Interval == 0 {
			c.Service.Readiness.Interval = Duration(1 * time.Second)
		}
		if c.Service.StopTimeout == 0 {
			c.Service.StopTimeout = Duration(30 * time.Second)
		}
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
	if c.Init.CommandTimeout == 0 {
		c.Init.CommandTimeout = Duration(5 * time.Minute)
	}
	applyStepDefaults(c.Init.Steps)
	applyStepDefaults(c.Teardown.Steps)

	for i := range c.K6.Steps {
		step := &c.K6.Steps[i]
		if step.Method == "" {
			step.Method = "GET"
		}
		step.Method = strings.ToUpper(step.Method)
	}

	for i := range c.K6.Setup {
		step := &c.K6.Setup[i]
		if step.Method == "" {
			step.Method = "GET"
		}
		step.Method = strings.ToUpper(step.Method)
	}
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
	errs = append(errs, validateServiceConfig(c.Service)...)

	hasScript := c.K6.Script != ""
	hasSteps := len(c.K6.Steps) > 0
	switch {
	case hasScript && hasSteps:
		errs = append(errs, "k6: exactly one of script or steps must be set, not both")
	case !hasScript && !hasSteps:
		errs = append(errs, "k6: exactly one of script or steps must be set")
	}
	if hasScript && !c.K6.Options.IsZero() {
		errs = append(errs, "k6.options is only used with k6.steps; with k6.script, configure options in the script itself")
	}
	if hasScript && len(c.K6.Setup) > 0 {
		errs = append(errs, "k6.setup is only used with k6.steps; with k6.script, write your own setup() function directly in the script")
	}

	if c.Init.Command != "" && len(c.Init.Steps) > 0 {
		errs = append(errs, "init: exactly one of command or steps must be set, not both")
	}
	if c.Init.CommandTimeout < 0 {
		errs = append(errs, "init.command_timeout must be >= 0")
	}

	errs = append(errs, validateSteps("init.steps", c.Init.Steps)...)
	errs = append(errs, validateDeriveRules(c.Init.Derive)...)
	errs = append(errs, validateSteps("teardown.steps", c.Teardown.Steps)...)
	errs = append(errs, validateK6Steps(c.K6.Steps)...)
	errs = append(errs, validateK6SetupSteps(c.K6.Setup)...)
	errs = append(errs, validateK6Options(c.K6.Options)...)

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

// validateServiceConfig checks service.start_command/stop_signal/readiness/
// stop_timeout: the lifecycle fields are meaningless (and rejected) without
// start_command, and start_command requires enough of the rest (a
// readiness URL, a recognized stop signal) to actually be usable.
func validateServiceConfig(svc ServiceConfig) []string {
	var errs []string

	hasReadiness := svc.Readiness.URL != "" || svc.Readiness.Timeout != 0 || svc.Readiness.Interval != 0

	if svc.StartCommand == "" {
		if svc.StopSignal != "" {
			errs = append(errs, "service.stop_signal is only used with service.start_command")
		}
		if svc.StopTimeout != 0 {
			errs = append(errs, "service.stop_timeout is only used with service.start_command")
		}
		if hasReadiness {
			errs = append(errs, "service.readiness is only used with service.start_command")
		}
		return errs
	}

	if svc.Readiness.URL == "" {
		errs = append(errs, "service.readiness.url is required when service.start_command is set")
	}
	if svc.Readiness.Timeout < 0 {
		errs = append(errs, "service.readiness.timeout must be >= 0")
	}
	if svc.Readiness.Interval < 0 {
		errs = append(errs, "service.readiness.interval must be >= 0")
	}
	if svc.StopTimeout < 0 {
		errs = append(errs, "service.stop_timeout must be >= 0")
	}
	if !validStopSignals[svc.StopSignal] {
		errs = append(errs, fmt.Sprintf("service.stop_signal: unsupported signal %q", svc.StopSignal))
	}

	return errs
}

func validateDeriveRules(rules []DeriveRule) []string {
	var errs []string

	for i, rule := range rules {
		label := fmt.Sprintf("init.derive[%d]", i)
		if rule.As != "" {
			label = fmt.Sprintf("init.derive[%d] (%s)", i, rule.As)
		}
		if rule.As == "" {
			errs = append(errs, fmt.Sprintf("%s: as is required", label))
		}
		if rule.Expr == "" {
			errs = append(errs, fmt.Sprintf("%s: expr is required", label))
		}
	}

	return errs
}

func validateK6Steps(steps []K6Step) []string {
	var errs []string

	for i, step := range steps {
		label := fmt.Sprintf("k6.steps[%d]", i)
		if step.Name != "" {
			label = fmt.Sprintf("k6.steps[%d] (%s)", i, step.Name)
		}
		if step.URL == "" {
			errs = append(errs, fmt.Sprintf("%s: url is required", label))
		}
		if !validMethods[step.Method] {
			errs = append(errs, fmt.Sprintf("%s: unsupported method %q", label, step.Method))
		}
		if step.Sleep < 0 {
			errs = append(errs, fmt.Sprintf("%s: sleep must be >= 0", label))
		}
		// Repeat may be a template expression (e.g. "{{.Vars.x}}"), only
		// resolved at generation time; only a plain literal can be checked
		// statically here, mirroring init.steps' Count validation.
		if step.Repeat != "" && !strings.Contains(step.Repeat, "{{") {
			if n, err := strconv.Atoi(step.Repeat); err != nil || n < 0 {
				errs = append(errs, fmt.Sprintf("%s: repeat must be a non-negative integer or a template expression, got %q", label, step.Repeat))
			}
		}

		if step.Body != "" && step.BodyFrom != "" {
			errs = append(errs, fmt.Sprintf("%s: exactly one of body or body_from must be set, not both", label))
		}
		if len(step.BodyPatch) > 0 && step.BodyFrom == "" {
			errs = append(errs, fmt.Sprintf("%s: body_patch requires body_from", label))
		}
		patchPaths := make([]string, 0, len(step.BodyPatch))
		for path := range step.BodyPatch {
			patchPaths = append(patchPaths, path)
		}
		sort.Strings(patchPaths)
		for _, path := range patchPaths {
			if path == "" {
				errs = append(errs, fmt.Sprintf("%s.body_patch: path is required", label))
			}
			if step.BodyPatch[path] == "" {
				errs = append(errs, fmt.Sprintf("%s.body_patch[%q]: value is required", label, path))
			}
		}

		names := make([]string, 0, len(step.Checks))
		for name := range step.Checks {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if name == "" {
				errs = append(errs, fmt.Sprintf("%s.checks: name is required", label))
			}
			if step.Checks[name] == "" {
				errs = append(errs, fmt.Sprintf("%s.checks[%q]: expression is required", label, name))
			}
		}

		tagNames := make([]string, 0, len(step.Tags))
		for name := range step.Tags {
			tagNames = append(tagNames, name)
		}
		sort.Strings(tagNames)
		for _, name := range tagNames {
			if name == "" {
				errs = append(errs, fmt.Sprintf("%s.tags: name is required", label))
			}
		}
	}

	return errs
}

func validateK6SetupSteps(steps []K6SetupStep) []string {
	var errs []string

	for i, step := range steps {
		label := fmt.Sprintf("k6.setup[%d]", i)
		if step.Name != "" {
			label = fmt.Sprintf("k6.setup[%d] (%s)", i, step.Name)
		}
		if step.URL == "" {
			errs = append(errs, fmt.Sprintf("%s: url is required", label))
		}
		if !validMethods[step.Method] {
			errs = append(errs, fmt.Sprintf("%s: unsupported method %q", label, step.Method))
		}
		for j, ex := range step.Extract {
			if ex.Path == "" {
				errs = append(errs, fmt.Sprintf("%s.extract[%d]: path is required", label, j))
			}
			if ex.As == "" {
				errs = append(errs, fmt.Sprintf("%s.extract[%d]: as is required", label, j))
			}
		}
	}

	return errs
}

func validateK6Options(o K6Options) []string {
	var errs []string

	if o.VUs < 0 {
		errs = append(errs, "k6.options.vus must be >= 0")
	}
	if o.Iterations < 0 {
		errs = append(errs, "k6.options.iterations must be >= 0")
	}
	if o.Duration < 0 {
		errs = append(errs, "k6.options.duration must be >= 0")
	}
	for i, stage := range o.Stages {
		if stage.Duration <= 0 {
			errs = append(errs, fmt.Sprintf("k6.options.stages[%d]: duration must be > 0", i))
		}
		if stage.Target < 0 {
			errs = append(errs, fmt.Sprintf("k6.options.stages[%d]: target must be >= 0", i))
		}
	}

	exprs := make([]string, 0, len(o.Thresholds))
	for expr := range o.Thresholds {
		exprs = append(exprs, expr)
	}
	sort.Strings(exprs)
	for _, expr := range exprs {
		if expr == "" {
			errs = append(errs, "k6.options.thresholds: metric name is required")
		}
		if len(o.Thresholds[expr]) == 0 {
			errs = append(errs, fmt.Sprintf("k6.options.thresholds[%q]: at least one expression is required", expr))
		}
	}

	return errs
}
