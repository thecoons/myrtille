// Package config parses and validates the myrtille.yaml file that describes
// a single load-test project: the service under test, the declarative init
// steps, the k6 script to run, and where to write the report.
package config

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration wraps time.Duration so it can be parsed from a YAML string such
// as "5s", since time.Duration's underlying int64 does not unmarshal from a
// human-readable string on its own.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(unmarshal func(any) error) error {
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
	Traces  TracesConfig  `yaml:"traces"`
	// Managed, when set, makes `myrtille run` launch the service itself
	// (via ManagedConfig.StartCommand) before the init phase, wait for
	// readiness, then stop it (best-effort) after teardown — nil (the
	// `managed:` block absent) means today's default: the service is
	// assumed to already be running externally, and myrtille never
	// touches it. See internal/servicelifecycle and
	// docs/plans/service-lifecycle.md's "Extension" section for why this
	// is a pointer to a nested block rather than flat fields gated on
	// StartCommand != "" (the pre-service.managed shape).
	Managed *ManagedConfig `yaml:"managed"`

	// migratedFields records any pre-service.managed flat field
	// (start_command/stop_signal/readiness/stop_timeout/log_file) found
	// directly under `service:` by UnmarshalYAML — surfaced as a load
	// error by validateServiceConfig rather than silently dropped by
	// yaml.v3's default unknown-key handling. See
	// docs/plans/service-lifecycle.md's "Extension" Décisions.
	migratedFields []string
}

// migratedServiceFields are the service.* keys that moved under
// service.managed — see ServiceConfig.migratedFields.
var migratedServiceFields = map[string]bool{
	"start_command": true,
	"stop_signal":   true,
	"readiness":     true,
	"stop_timeout":  true,
	"log_file":      true,
}

// UnmarshalYAML decodes ServiceConfig normally, then inspects the raw
// mapping node for two things a plain struct decode can't tell on its own:
//  1. any pre-service.managed flat field, recorded into migratedFields for
//     validateServiceConfig to reject explicitly;
//  2. whether the `managed` key is present at all, even when its value
//     decodes to nil (a bare `managed:` with nothing indented under it —
//     see docs/plans/service-lifecycle.md tranche 7's finding) — treated
//     the same as `managed: {}`, not the same as the key being absent,
//     so an explicit-but-empty managed block can't silently fall back to
//     external mode.
func (s *ServiceConfig) UnmarshalYAML(node *yaml.Node) error {
	type alias ServiceConfig
	var a alias
	if err := node.Decode(&a); err != nil {
		return err
	}
	*s = ServiceConfig(a)

	if node.Kind != yaml.MappingNode {
		return nil
	}
	managedPresent := false
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		if key == "managed" {
			managedPresent = true
			continue
		}
		if migratedServiceFields[key] {
			s.migratedFields = append(s.migratedFields, key)
		}
	}
	sort.Strings(s.migratedFields)
	if s.Managed == nil && managedPresent {
		s.Managed = &ManagedConfig{}
	}
	return nil
}

// ManagedConfig configures myrtille-managed start/stop of the service
// under test (service.managed) — for projects that want a fresh instance
// per `myrtille run` rather than assuming one is already up externally.
type ManagedConfig struct {
	// StartCommand launches the service (via `sh -c`, inheriting the
	// process env like init.command).
	StartCommand string `yaml:"start_command"`
	// StopSignal names the signal sent to the whole process group started
	// by StartCommand (not just its direct PID) when the run finishes.
	// Defaults to "TERM".
	StopSignal string `yaml:"stop_signal"`
	// Readiness configures how `myrtille run` waits for the
	// StartCommand-launched service to come up before the init phase
	// starts. readiness.url is required.
	Readiness ReadinessConfig `yaml:"readiness"`
	// StopTimeout bounds how long to wait for the service to actually stop
	// (its readiness URL to stop responding) after StopSignal is sent,
	// before giving up — best-effort, never fails the run. Defaults to 30s.
	StopTimeout Duration `yaml:"stop_timeout"`
	// LogFile, when set, persists the StartCommand-launched service's
	// stdout/stderr to this path (resolved relative to the config file's
	// directory, like K6.Script) instead of the throwaway temp file used
	// by default — overwritten at the start of every run, not appended
	// across runs.
	LogFile string `yaml:"log_file"`
}

type MetricsConfig struct {
	URL      string   `yaml:"url"`
	Interval Duration `yaml:"interval"`
}

// TracesConfig controls the OTLP/HTTP span receiver (k6/x/oteltrace) — see
// docs/plans/otel-span-metrics.md. Unlike MetricsConfig, there's no URL to
// configure: the receiver listens on a fixed, standard OTLP/HTTP port
// (myrtille doesn't invent an address the service under test would need to
// be told about — see the plan's "Décisions actées" for why), so Enabled
// is the only knob.
type TracesConfig struct {
	Enabled bool `yaml:"enabled"`
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
	// Timeout overrides k6's fixed 60s per-request default (e.g. "90s") —
	// a Go template with the same url/body resolution (`.BaseURL`/`.Vars`,
	// plus pick/random), see internal/k6gen. Empty means k6's own default,
	// unchanged.
	Timeout string    `yaml:"timeout"`
	Extract []Extract `yaml:"extract"`
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
	// Timeout overrides k6's fixed 60s per-request default (e.g. "90s") —
	// a Go template with the same url/body resolution (`.BaseURL`/`.Vars`,
	// plus pick/random), see internal/k6gen. Empty means k6's own default,
	// unchanged — a hand-written k6.script wanting a longer wait for an
	// unbounded/unpaginated endpoint had no declarative equivalent before
	// this field existed.
	Timeout string `yaml:"timeout"`
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
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
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

// ServiceLogFilePath returns service.managed.log_file resolved against the
// config file's directory, or "" when unset (including when service.managed
// itself is unset).
func (c *Config) ServiceLogFilePath() string {
	if c.Service.Managed == nil {
		return ""
	}
	return c.resolvePath(c.Service.Managed.LogFile)
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
