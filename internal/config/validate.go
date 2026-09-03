package config

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
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

// validateServiceConfig checks service.managed: any pre-service.managed
// flat field found by ServiceConfig.UnmarshalYAML is a hard migration
// error (see migratedServiceFields); when service.managed is set, it needs
// enough of its fields (a readiness URL, a recognized stop signal) to
// actually be usable. service.managed absent is always valid — myrtille's
// external-service default.
func validateServiceConfig(svc ServiceConfig) []string {
	var errs []string

	for _, f := range svc.migratedFields {
		errs = append(errs, fmt.Sprintf("service.%s has moved under service.managed — see the README's \"Starting and stopping the service\" section", f))
	}

	if svc.Managed == nil {
		return errs
	}
	m := svc.Managed

	if m.Readiness.URL == "" {
		errs = append(errs, "service.managed.readiness.url is required")
	}
	if m.Readiness.Timeout < 0 {
		errs = append(errs, "service.managed.readiness.timeout must be >= 0")
	}
	if m.Readiness.Interval < 0 {
		errs = append(errs, "service.managed.readiness.interval must be >= 0")
	}
	if m.StopTimeout < 0 {
		errs = append(errs, "service.managed.stop_timeout must be >= 0")
	}
	if !validStopSignals[m.StopSignal] {
		errs = append(errs, fmt.Sprintf("service.managed.stop_signal: unsupported signal %q", m.StopSignal))
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
