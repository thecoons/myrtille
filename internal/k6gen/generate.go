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
	"github.com/thecoons/myrtille/internal/k6run"
)

// templateData is exposed to url/body/header templates as `.BaseURL` and
// `.Vars` — the subset of internal/initphase's templateData that is known
// statically at generation time (no `.Index`/`.Parent`/`.Dict`: those only
// make sense for a Go-driven loop over HTTP calls, not a k6-driven one).
type templateData struct {
	BaseURL string
	Vars    map[string]any
}

// pickState tracks, within the rendering of a single step (url + body +
// every header/tag value), which pools have already been drawn from via a
// field-selecting {{pick "pool" "field"}} call — so multiple such calls for
// the SAME pool within that one step reference the same randomly-selected
// element (e.g. a correlated (domain, name) pair from one array of
// objects), rather than each drawing its own independent random index.
// Scope is deliberately per-step, not per-template-string: url and body
// (and headers/tags) must all see the same draw. A fresh pickState per
// step also means the JS variables it allocates are re-declared on every
// execution of that step's block — including every iteration of a `repeat`
// loop, so each repetition still gets its own fresh correlated pick.
type pickState struct {
	vars  map[string]string
	order []string
}

func newPickState() *pickState {
	return &pickState{vars: make(map[string]string)}
}

// varFor returns the hoisted JS variable name for pool, allocating one (and
// recording it for declaration via declarations()) the first time pool is
// seen.
func (s *pickState) varFor(pool string) string {
	if v, ok := s.vars[pool]; ok {
		return v
	}
	v := fmt.Sprintf("__pick%d", len(s.order))
	s.vars[pool] = v
	s.order = append(s.order, pool)
	return v
}

// declarations returns one `const __pickN = pick(state["pool"]);` line per
// pool seen via varFor, in first-use order.
func (s *pickState) declarations() []string {
	decls := make([]string, len(s.order))
	for i, pool := range s.order {
		decls[i] = fmt.Sprintf("    const %s = pick(state[%s]);\n", s.vars[pool], jsString(pool))
	}
	return decls
}

// stepTemplateFuncs, unlike internal/initphase's function of the same
// names, return JS source text: the actual pick/random happens in the
// generated script at k6-runtime, once per iteration, via the
// pick/randomInt helpers emitted in preamble. ps is fresh per step (see
// pickState) so a field-selecting pick call can hoist a shared draw.
func stepTemplateFuncs(ps *pickState) template.FuncMap {
	return template.FuncMap{
		// pick, called with just a pool name, keeps today's behavior: a
		// fresh independent random element every time it's evaluated.
		// Called with an optional second (field) argument, it instead
		// reads that field off a single random element of pool, shared
		// with every other field-selecting pick call for the same pool
		// within this step — see pickState. field may be a dot-separated
		// path ("metadata.domain") to reach into a nested pooled object,
		// not just a single top-level property — resolved into a chained
		// bracket access at generation time, since field is a Go string
		// known statically from the YAML, not a k6-runtime value (unlike
		// extractPathHelper's path, which walks a runtime JSON response
		// and so needs a JS-side helper instead).
		"pick": func(pool string, field ...string) (string, error) {
			if len(field) > 1 {
				return "", fmt.Errorf("pick: at most one field argument allowed, got %d", len(field))
			}
			if len(field) == 0 {
				return fmt.Sprintf("${pick(state[%s])}", jsString(pool)), nil
			}
			return fmt.Sprintf("${%s}", jsBracketChain(ps.varFor(pool), field[0])), nil
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

// promscrapeImportLine is spliced into preamble's fixed import block when
// service.metrics.url is set — see renderPreamble. k6/x/promscrape only
// exists in the custom k6 binary built by scripts/build-k6.sh (see
// docs/plans/xk6-live-dashboard.md); a generated script that doesn't need
// it must stay byte-identical to before this feature existed, so this line
// is never emitted when metrics scraping isn't configured.
const promscrapeImportLine = "import promscrape from 'k6/x/promscrape';\n"

// renderPreamble returns preamble, with promscrapeImportLine spliced in
// right after preamble's own last import line when hasMetrics is true.
// Splicing into the untouched constant (rather than building the preamble
// from scratch) is what keeps output byte-identical to before this feature
// existed when hasMetrics is false — the common case.
func renderPreamble(hasMetrics bool) string {
	if !hasMetrics {
		return preamble
	}
	const anchor = "import { check, sleep } from 'k6';\n"
	return strings.Replace(preamble, anchor, anchor+promscrapeImportLine, 1)
}

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
// When cfg.K6.Setup is non-empty, or cfg.Service.Metrics.URL is set, a k6
// setup() function is emitted, returning the (possibly setup-extended)
// state pool. Getting this right matters: k6 runs each VU as a separate JS
// isolate, so a module-level variable mutated inside setup() would NOT be
// visible to other VUs — the only sanctioned channel is setup()'s return
// value, delivered to every VU as the `data` parameter of the default
// function. Rather than parameterizing pick/random's generated `state[...]`
// fragments (which would need to read `state` in setup() but `data` in the
// default function), the default function instead does `const state =
// data;` as its first line, shadowing the module-level `const state` with
// this VU's copy of setup()'s result — pick/random's fragments stay
// untouched, always referencing `state`. When neither Setup nor
// Service.Metrics.URL is set (the common case), none of this is emitted:
// output is byte-identical to before this feature existed.
//
// Service.Metrics.URL additionally emits a `promscrape.Scraper` at module
// scope (metric registration needs k6's init context, which setup() no
// longer has by the time it runs) and a `.start(...)` call as the first
// line of setup() — mirroring how k6.setup steps are declarative
// configuration for a hand-written script's setup(), this is the k6.steps
// equivalent of the two lines documented in the README for k6.script users
// to add themselves (see docs/plans/xk6-live-dashboard.md, step 3).
func Generate(cfg *config.Config) (string, func(), error) {
	data := templateData{BaseURL: cfg.Service.BaseURL, Vars: cfg.Vars}
	hasSetup := len(cfg.K6.Setup) > 0
	// k6run.HasCustomBinary(), not just cfg.Service.Metrics.URL: stock k6
	// doesn't have k6/x/promscrape, so wiring it in unconditionally breaks
	// any run that hasn't opted into MYRTILLE_K6_BIN — see HasCustomBinary's
	// doc comment.
	hasMetrics := cfg.Service.Metrics.URL != "" && k6run.HasCustomBinary()
	emitSetup := hasSetup || hasMetrics

	var script strings.Builder
	script.WriteString(renderPreamble(hasMetrics))
	if hasMetrics {
		fmt.Fprintf(&script, "const __promscrape = new promscrape.Scraper(%s);\n\n", jsString(cfg.Service.Metrics.URL))
	}
	if hasSetup {
		script.WriteString(extractPathHelper)
	}

	if emitSetup {
		script.WriteString("export function setup() {\n")
		if hasMetrics {
			fmt.Fprintf(&script, "  __promscrape.start(%d);\n", cfg.Service.Metrics.Interval.Duration().Milliseconds())
		}
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

	if emitSetup {
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
	ps := newPickState()

	url, err := renderJSLiteral(step.URL, data, ps)
	if err != nil {
		return "", fmt.Errorf("rendering url template: %w", err)
	}

	bodyExpr := "null"
	var bodyLines []string
	switch {
	case step.BodyFrom != "":
		// Deep-clone the picked pool element (never mutate the shared pool
		// itself — see docs/plans/k6-steps-object-pool-patch.md tranche 0)
		// then apply each body_patch entry as a chained bracket assignment.
		// A patch path whose intermediate segment doesn't exist on the
		// picked object throws a real JS TypeError here, at k6 runtime —
		// deliberately not auto-vivified (see tranche 0's decision).
		bodyLines = append(bodyLines, fmt.Sprintf("    const body = JSON.parse(JSON.stringify(%s));\n", ps.varFor(step.BodyFrom)))
		paths := make([]string, 0, len(step.BodyPatch))
		for path := range step.BodyPatch {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		for _, path := range paths {
			rendered, err := renderJSLiteral(step.BodyPatch[path], data, ps)
			if err != nil {
				return "", fmt.Errorf("rendering body_patch %q template: %w", path, err)
			}
			bodyLines = append(bodyLines, fmt.Sprintf("    %s = `%s`;\n", jsBracketChain("body", path), rendered))
		}
		bodyExpr = "JSON.stringify(body)"
	case step.Body != "":
		rendered, err := renderJSLiteral(step.Body, data, ps)
		if err != nil {
			return "", fmt.Errorf("rendering body template: %w", err)
		}
		bodyExpr = "`" + rendered + "`"
	}

	headersExpr, err := renderJSObjectLiteral("header", step.Headers, data, ps)
	if err != nil {
		return "", err
	}
	tagsExpr, err := renderJSObjectLiteral("tag", step.Tags, data, ps)
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
	for _, decl := range ps.declarations() {
		b.WriteString(decl)
	}
	for _, line := range bodyLines {
		b.WriteString(line)
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
	ps := newPickState()

	url, err := renderJSLiteral(step.URL, data, ps)
	if err != nil {
		return "", fmt.Errorf("rendering url template: %w", err)
	}

	bodyExpr := "null"
	if step.Body != "" {
		rendered, err := renderJSLiteral(step.Body, data, ps)
		if err != nil {
			return "", fmt.Errorf("rendering body template: %w", err)
		}
		bodyExpr = "`" + rendered + "`"
	}

	headersExpr, err := renderJSObjectLiteral("header", step.Headers, data, ps)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "  // %s\n", setupStepLabel(step))
	b.WriteString("  {\n")
	for _, decl := range ps.declarations() {
		b.WriteString(decl)
	}
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

func renderJSLiteral(raw string, data templateData, ps *pickState) (string, error) {
	escaped := literalEscaper.Replace(raw)

	tmpl, err := template.New("k6-step").Funcs(stepTemplateFuncs(ps)).Parse(escaped)
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

// jsBracketChain renders `varName["seg1"]["seg2"]...` for a dot-separated
// path, one bracket-indexed segment per path component — a single-segment
// path (the common case: a flat field name) renders exactly as before this
// helper existed (`varName["field"]`).
func jsBracketChain(varName, path string) string {
	var b strings.Builder
	b.WriteString(varName)
	for _, seg := range strings.Split(path, ".") {
		b.WriteString("[")
		b.WriteString(jsString(seg))
		b.WriteString("]")
	}
	return b.String()
}

// renderJSObjectLiteral renders a string-valued map (headers, tags) as a JS
// object literal, each value templated via renderJSLiteral and keys sorted
// for deterministic script output. Returns "{}" for an empty/nil map.
// label identifies the map in error messages (e.g. "header", "tag").
func renderJSObjectLiteral(label string, m map[string]string, data templateData, ps *pickState) (string, error) {
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
		rendered, err := renderJSLiteral(m[k], data, ps)
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
