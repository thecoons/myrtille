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

// Generate renders cfg.K6.Steps/Options into a k6 script, writing it to a
// temp file. The caller must invoke the returned cleanup once the k6 run
// has finished (or failed).
func Generate(cfg *config.Config) (string, func(), error) {
	data := templateData{BaseURL: cfg.Service.BaseURL, Vars: cfg.Vars}

	var script strings.Builder
	script.WriteString(preamble)

	optsJSON, ok, err := renderOptions(cfg.K6.Options)
	if err != nil {
		return "", nil, err
	}
	if ok {
		script.WriteString("export const options = ")
		script.WriteString(optsJSON)
		script.WriteString(";\n\n")
	}

	script.WriteString("export default function () {\n")
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

	headerKeys := make([]string, 0, len(step.Headers))
	for k := range step.Headers {
		headerKeys = append(headerKeys, k)
	}
	sort.Strings(headerKeys)

	headersExpr := "{}"
	if len(headerKeys) > 0 {
		var headers strings.Builder
		for i, k := range headerKeys {
			rendered, err := renderJSLiteral(step.Headers[k], data)
			if err != nil {
				return "", fmt.Errorf("rendering header %q template: %w", k, err)
			}
			if i > 0 {
				headers.WriteString(", ")
			}
			fmt.Fprintf(&headers, "%s: `%s`", jsString(k), rendered)
		}
		headersExpr = "{ " + headers.String() + " }"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "  // %s\n", stepLabel(step))
	b.WriteString("  {\n")
	fmt.Fprintf(&b, "    const res = http.request(%s, `%s`, %s, { headers: %s });\n", jsString(step.Method), url, bodyExpr, headersExpr)

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
