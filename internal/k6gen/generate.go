// Package k6gen renders the declarative HTTP steps configured under
// `k6.steps` into a k6-compatible JS scenario file, for projects that don't
// need a hand-written scenario.js. It is the run-phase counterpart to
// internal/initphase: same `pick`/`random` template func names, but here
// they expand to JS source fragments (resolved by the generated script at
// k6-runtime, once per iteration) rather than to values resolved once by Go
// at generation time — k6, not Go, drives a scenario's iteration loop.
//
// Generate is only called when config.Config.K6.Script is empty; Validate
// guarantees exactly one of Script/Steps is set.
package k6gen

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"github.com/thecoons/myrtille/internal/config"
)

// templateData is exposed to url/body/header templates as `.BaseURL` and
// `.Vars` — the subset of internal/initphase's templateData that is known
// statically at generation time (no `.Index`/`.Parent`/`.Dict`: those only
// make sense for a Go-driven loop over HTTP calls, not a k6-driven one).
type templateData struct {
	BaseURL string
	Vars    map[string]any
}

// templateFuncs, unlike internal/initphase's function of the same names,
// return JS source text: the actual pick/random happens in the generated
// script at k6-runtime, once per iteration, via the pick/randomInt helpers
// emitted in preamble.
var templateFuncs = template.FuncMap{
	"pick": func(pool string) string {
		return fmt.Sprintf("${pick(state[%s])}", jsString(pool))
	},
	"random": func(min, max int) (string, error) {
		if max < min {
			return "", fmt.Errorf("random: max (%d) must be >= min (%d)", max, min)
		}
		return fmt.Sprintf("${randomInt(%d, %d)}", min, max), nil
	},
	// uniqueId expands to a value guaranteed unique per k6 iteration
	// (__VU/__ITER are k6 runtime globals, no import needed), for resource
	// names that must not collide — e.g. `{{uniqueId}}` in a body template.
	// Guarded with typeof checks because __VU/__ITER are only defined
	// inside the default function's VU context — a k6.setup step using
	// uniqueId runs inside setup(), where they're undefined (confirmed via
	// a real k6 run: referencing them directly throws "ReferenceError:
	// __ITER is not defined").
	"uniqueId": func() string {
		return "${typeof __VU !== 'undefined' ? __VU : 'setup'}-${typeof __ITER !== 'undefined' ? __ITER : 0}-${Date.now()}"
	},
}

const preamble = `import http from 'k6/http';
import { check, sleep } from 'k6';

const state = JSON.parse(open(__ENV.STATE_FILE));

function pick(pool) {
  return pool[Math.floor(Math.random() * pool.length)];
}

function randomInt(min, max) {
  return min + Math.floor(Math.random() * (max - min + 1));
}

`

// extractPathHelper is only emitted when k6.setup is configured. path is a
// plain dot-separated JS property/array-index path (e.g. "name",
// "items.0.id") — a deliberately simpler subset of init.steps' gjson
// syntax (no "#.field" flatten-map), sufficient for pulling one field out
// of a setup call's JSON response.
const extractPathHelper = `function extractPath(obj, path) {
  return path.split('.').reduce((o, k) => (o == null ? undefined : o[k]), obj);
}

`

// Generate renders cfg.K6.Setup/Steps/Options into a k6 script, writing it
// to a temp file. The caller must invoke the returned cleanup once the k6
// run has finished (or failed).
//
// When cfg.K6.Setup is non-empty, a k6 setup() function is emitted, running
// those steps once before any iteration and returning the (possibly
// setup-extended) state pool. Getting this right matters: k6 runs each VU
// as a separate JS isolate, so a module-level variable mutated inside
// setup() would NOT be visible to other VUs — the only sanctioned channel
// is setup()'s return value, delivered to every VU as the `data` parameter
// of the default function. Rather than parameterizing pick/random's
// generated `state[...]` fragments (which would need to read `state` in
// setup() but `data` in the default function), the default function
// instead does `const state = data;` as its first line, shadowing the
// module-level `const state` with this VU's copy of setup()'s result —
// pick/random's fragments stay untouched, always referencing `state`. When
// Setup is empty (the common case), none of this is emitted: output is
// byte-identical to before this feature existed.
func Generate(cfg *config.Config) (string, func(), error) {
	data := templateData{BaseURL: cfg.Service.BaseURL, Vars: cfg.Vars}
	hasSetup := len(cfg.K6.Setup) > 0

	var script strings.Builder
	script.WriteString(preamble)
	if hasSetup {
		script.WriteString(extractPathHelper)

		script.WriteString("export function setup() {\n")
		for _, step := range cfg.K6.Setup {
			stepJS, err := renderSetupStep(step, data)
			if err != nil {
				return "", nil, fmt.Errorf("rendering k6 setup step %q: %w", setupStepLabel(step), err)
			}
			script.WriteString(stepJS)
		}
		script.WriteString("  return state;\n}\n\n")
	}

	optsJSON, ok, err := renderOptions(cfg.K6.Options)
	if err != nil {
		return "", nil, err
	}
	if ok {
		script.WriteString("export const options = ")
		script.WriteString(optsJSON)
		script.WriteString(";\n\n")
	}

	if hasSetup {
		script.WriteString("export default function (data) {\n  const state = data;\n")
	} else {
		script.WriteString("export default function () {\n")
	}
	for _, step := range cfg.K6.Steps {
		stepJS, err := renderStep(step, data)
		if err != nil {
			return "", nil, fmt.Errorf("rendering k6 step %q: %w", stepLabel(step), err)
		}
		script.WriteString(stepJS)
	}
	script.WriteString("}\n")

	f, err := os.CreateTemp("", "myrtille-scenario-*.js")
	if err != nil {
		return "", nil, fmt.Errorf("creating generated scenario file: %w", err)
	}
	if _, err := f.WriteString(script.String()); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", nil, fmt.Errorf("writing generated scenario file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", nil, fmt.Errorf("closing generated scenario file: %w", err)
	}

	path := f.Name()
	return path, func() { os.Remove(path) }, nil
}

// renderOptions builds the `export const options` payload from only the
// non-zero K6Options fields, so an empty/unset K6Options omits the block
// entirely and lets k6's own defaults (1 VU, 1 iteration) apply.
func renderOptions(o config.K6Options) (string, bool, error) {
	m := map[string]any{}
	if o.VUs > 0 {
		m["vus"] = o.VUs
	}
	if o.Duration.Duration() > 0 {
		m["duration"] = o.Duration.Duration().String()
	}
	if o.Iterations > 0 {
		m["iterations"] = o.Iterations
	}
	if len(o.Stages) > 0 {
		stages := make([]map[string]any, len(o.Stages))
		for i, s := range o.Stages {
			stages[i] = map[string]any{
				"duration": s.Duration.Duration().String(),
				"target":   s.Target,
			}
		}
		m["stages"] = stages
	}
	if len(o.Thresholds) > 0 {
		m["thresholds"] = o.Thresholds
	}
	if len(m) == 0 {
		return "", false, nil
	}

	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", false, fmt.Errorf("marshaling k6 options: %w", err)
	}
	return string(data), true, nil
}

func renderStep(step config.K6Step, data templateData) (string, error) {
	url, err := renderJSLiteral(step.URL, data)
	if err != nil {
		return "", fmt.Errorf("rendering url template: %w", err)
	}

	bodyExpr := "null"
	if step.Body != "" {
		rendered, err := renderJSLiteral(step.Body, data)
		if err != nil {
			return "", fmt.Errorf("rendering body template: %w", err)
		}
		bodyExpr = "`" + rendered + "`"
	}

	headersExpr, err := renderJSObjectLiteral("header", step.Headers, data)
	if err != nil {
		return "", err
	}
	tagsExpr, err := renderJSObjectLiteral("tag", step.Tags, data)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "  // %s\n", stepLabel(step))
	if step.Repeat != "" {
		n, err := resolveRepeat(step.Repeat, data)
		if err != nil {
			return "", fmt.Errorf("resolving repeat: %w", err)
		}
		fmt.Fprintf(&b, "  for (let i = 0; i < %d; i++) {\n", n)
	} else {
		b.WriteString("  {\n")
	}
	fmt.Fprintf(&b, "    const res = http.request(%s, `%s`, %s, { headers: %s, tags: %s });\n",
		jsString(step.Method), url, bodyExpr, headersExpr, tagsExpr)

	if len(step.Checks) > 0 {
		checkNames := make([]string, 0, len(step.Checks))
		for name := range step.Checks {
			checkNames = append(checkNames, name)
		}
		sort.Strings(checkNames)

		var checks strings.Builder
		for i, name := range checkNames {
			if i > 0 {
				checks.WriteString(", ")
			}
			fmt.Fprintf(&checks, "%s: (r) => %s", jsString(name), step.Checks[name])
		}
		fmt.Fprintf(&b, "    check(res, { %s });\n", checks.String())
	}
	b.WriteString("  }\n")

	if step.Sleep.Duration() > 0 {
		fmt.Fprintf(&b, "  sleep(%g);\n", step.Sleep.Duration().Seconds())
	}

	return b.String(), nil
}

func stepLabel(step config.K6Step) string {
	if step.Name != "" {
		return step.Name
	}
	return step.URL
}

// renderSetupStep renders one k6.setup step: an http.request call (like
// renderStep, minus tags/checks/sleep/repeat — not meaningful for a
// run-once bootstrap call) followed by, per Extract rule, pushing the
// extracted value into the shared state pool that pick/random read from.
func renderSetupStep(step config.K6SetupStep, data templateData) (string, error) {
	url, err := renderJSLiteral(step.URL, data)
	if err != nil {
		return "", fmt.Errorf("rendering url template: %w", err)
	}

	bodyExpr := "null"
	if step.Body != "" {
		rendered, err := renderJSLiteral(step.Body, data)
		if err != nil {
			return "", fmt.Errorf("rendering body template: %w", err)
		}
		bodyExpr = "`" + rendered + "`"
	}

	headersExpr, err := renderJSObjectLiteral("header", step.Headers, data)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "  // %s\n", setupStepLabel(step))
	b.WriteString("  {\n")
	fmt.Fprintf(&b, "    const res = http.request(%s, `%s`, %s, { headers: %s, tags: {} });\n",
		jsString(step.Method), url, bodyExpr, headersExpr)

	if len(step.Extract) > 0 {
		b.WriteString("    const body = res.json();\n")
		for _, ex := range step.Extract {
			key := jsString(ex.As)
			fmt.Fprintf(&b, "    state[%s] = state[%s] || [];\n", key, key)
			fmt.Fprintf(&b, "    state[%s].push(extractPath(body, %s));\n", key, jsString(ex.Path))
		}
	}
	b.WriteString("  }\n")

	return b.String(), nil
}

func setupStepLabel(step config.K6SetupStep) string {
	if step.Name != "" {
		return step.Name
	}
	return step.URL
}

// resolveRepeat renders step.Repeat as a plain Go template (e.g. a literal
// "3", or "{{.Vars.x}}") against .BaseURL/.Vars — deliberately without
// templateFuncs: unlike url/body, Repeat must resolve to a concrete integer
// at generation time (it becomes a JS for loop's bound), not a JS fragment
// resolved at k6 runtime, so pick/random (which return JS source text)
// wouldn't make sense here.
func resolveRepeat(repeatExpr string, data templateData) (int, error) {
	tmpl, err := template.New("k6-step-repeat").Parse(repeatExpr)
	if err != nil {
		return 0, fmt.Errorf("parsing repeat template %q: %w", repeatExpr, err)
	}
	var buf strings.Builder
	if err := tmpl.Execute(&buf, data); err != nil {
		return 0, fmt.Errorf("rendering repeat template %q: %w", repeatExpr, err)
	}

	rendered := strings.TrimSpace(buf.String())
	n, err := strconv.Atoi(rendered)
	if err != nil {
		return 0, fmt.Errorf("repeat %q must resolve to an integer, got %q", repeatExpr, rendered)
	}
	if n < 0 {
		return 0, fmt.Errorf("repeat %q resolved to a negative value %d", repeatExpr, n)
	}
	return n, nil
}

// literalEscaper protects a raw url/body/header template string against
// accidentally terminating the JS template literal it will be wrapped in,
// or having a stray backslash reinterpreted by JS's own escape handling.
var literalEscaper = strings.NewReplacer(`\`, `\\`, "`", "\\`")

func renderJSLiteral(raw string, data templateData) (string, error) {
	escaped := literalEscaper.Replace(raw)

	tmpl, err := template.New("k6-step").Funcs(templateFuncs).Parse(escaped)
	if err != nil {
		return "", err
	}

	var buf strings.Builder
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// renderJSObjectLiteral renders a string-valued map (headers, tags) as a JS
// object literal, each value templated via renderJSLiteral and keys sorted
// for deterministic script output. Returns "{}" for an empty/nil map.
// label identifies the map in error messages (e.g. "header", "tag").
func renderJSObjectLiteral(label string, m map[string]string, data templateData) (string, error) {
	if len(m) == 0 {
		return "{}", nil
	}

	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for i, k := range keys {
		rendered, err := renderJSLiteral(m[k], data)
		if err != nil {
			return "", fmt.Errorf("rendering %s %q template: %w", label, k, err)
		}
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s: `%s`", jsString(k), rendered)
	}
	return "{ " + b.String() + " }", nil
}
