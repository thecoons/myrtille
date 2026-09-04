// Package initphase executes the declarative HTTP steps configured under
// `init.steps` to bring the target service into a known state, extracting
// values (IDs, etc.) from responses into a state.Dict that k6 scenarios can
// later randomize over. Steps may nest (`children`), in which case a child
// step runs once per iteration of its parent, with that iteration's parsed
// JSON response exposed as `.Parent` — enabling dependent resources (e.g. an
// order created under a given user) rather than only flat, independent pools.
package initphase

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/thecoons/myrtille/internal/config"
	"github.com/thecoons/myrtille/internal/state"
	"github.com/tidwall/gjson"
)

const requestTimeout = 30 * time.Second

// templateData is exposed to url/body/count templates as `.BaseURL`,
// `.Index`, `.Vars` (from the project's top-level `vars:`), `.Parent` (the
// parent iteration's parsed JSON response, nil at the root), and `.Dict`
// (a read-only snapshot of values extracted so far, for picking from an
// existing pool).
type templateData struct {
	BaseURL string
	Index   int
	Vars    map[string]any
	Parent  map[string]any
	Dict    map[string][]any
}

// templateFuncs are available in every url/body/count template.
var templateFuncs = template.FuncMap{
	"random": func(min, max int) (int, error) {
		if max < min {
			return 0, fmt.Errorf("random: max (%d) must be >= min (%d)", max, min)
		}
		return min + rand.Intn(max-min+1), nil
	},
	"pick": func(items []any) (any, error) {
		if len(items) == 0 {
			return nil, fmt.Errorf("pick: called on an empty pool")
		}
		return items[rand.Intn(len(items))], nil
	},
}

// StepResult reports what a single init step produced (aggregated across all
// of its iterations), for use in the report. Children mirrors the
// corresponding config step's Children, one entry per declared child,
// itself aggregated across every parent iteration.
type StepResult struct {
	Name      string
	Requests  int
	Extracted map[string]int
	Children  []StepResult
}

// Summary reports what the whole init phase produced. At most one of Steps
// or Command is meaningful, mirroring init.steps/init.command's mutual
// exclusivity in config: Command is set instead of Steps when init.command
// was used (see RunCommand).
type Summary struct {
	Steps   []StepResult
	Command *CommandResult
}

// Run executes each configured top-level init step, in declaration order. It
// aborts on the first HTTP error (status >= 400), extraction error, or
// unresolvable count: a partially-initialized service would make the
// subsequent load test unreliable, so we fail fast rather than run k6
// against unknown state.
func Run(ctx context.Context, cfg *config.Config, dict *state.Dict) (*Summary, error) {
	return runAll(ctx, cfg.Init.Steps, cfg.Service.BaseURL, cfg.Vars, dict, true)
}

// RunTeardown executes cfg.Teardown.Steps the same way Run executes
// cfg.Init.Steps, except it never aborts early: teardown is best-effort
// cleanup, so one failed request (e.g. deleting something already gone)
// must not prevent the rest of the cleanup from running. The returned error,
// if any, joins every failure encountered via errors.Join.
func RunTeardown(ctx context.Context, cfg *config.Config, dict *state.Dict) (*Summary, error) {
	return runAll(ctx, cfg.Teardown.Steps, cfg.Service.BaseURL, cfg.Vars, dict, false)
}

func runAll(ctx context.Context, steps []config.Step, baseURL string, vars map[string]any, dict *state.Dict, abortOnError bool) (*Summary, error) {
	client := &http.Client{Timeout: requestTimeout}
	summary := &Summary{}
	var errs []error

	for _, step := range steps {
		result, err := runStep(ctx, client, baseURL, step, vars, nil, dict, abortOnError)
		summary.Steps = append(summary.Steps, result)
		if err != nil {
			if abortOnError {
				return summary, err
			}
			errs = append(errs, err)
		}
	}

	return summary, errors.Join(errs...)
}

// runStep executes one step `step.Count` times (Count is itself a template,
// resolved once up front against vars/parent/dict), and, once per iteration,
// runs step.Children with that iteration's parsed response as their parent.
// Every child's result is aggregated across all parent iterations rather
// than reported once per iteration, mirroring the flat step's own
// aggregation across its Count iterations. When abortOnError is false, any
// per-iteration failure is recorded rather than raised, and execution moves
// on to the next iteration/child — see RunTeardown.
func runStep(ctx context.Context, client *http.Client, baseURL string, step config.Step, vars map[string]any, parent map[string]any, dict *state.Dict, abortOnError bool) (StepResult, error) {
	result := StepResult{Name: step.Name, Extracted: map[string]int{}}
	var errs []error

	childAccs := make([]StepResult, len(step.Children))
	for i, child := range step.Children {
		childAccs[i] = StepResult{Name: child.Name, Extracted: map[string]int{}}
	}

	countData := templateData{BaseURL: baseURL, Vars: vars, Parent: parent, Dict: dict.Snapshot()}
	count, err := resolveCount(step.Count, countData)
	if err != nil {
		result.Children = childAccs
		return result, fmt.Errorf("init step %q: %w", stepLabel(step), err)
	}

	for i := range count {
		data := templateData{BaseURL: baseURL, Index: i, Vars: vars, Parent: parent, Dict: dict.Snapshot()}

		body, err := executeStep(ctx, client, step, data)
		if err != nil {
			stepErr := fmt.Errorf("init step %q (iteration %d): %w", stepLabel(step), i, err)
			if abortOnError {
				result.Children = childAccs
				return result, stepErr
			}
			errs = append(errs, stepErr)
			continue
		}
		result.Requests++

		extractFailed := false
		for _, ex := range step.Extract {
			n, err := extractInto(dict, body, ex)
			if err != nil {
				stepErr := fmt.Errorf("init step %q (iteration %d): extracting %q: %w", stepLabel(step), i, ex.Path, err)
				if abortOnError {
					result.Children = childAccs
					return result, stepErr
				}
				errs = append(errs, stepErr)
				extractFailed = true
				continue
			}
			result.Extracted[ex.As] += n
		}
		if extractFailed || len(step.Children) == 0 {
			continue
		}

		var parentObj map[string]any
		if err := json.Unmarshal(body, &parentObj); err != nil {
			stepErr := fmt.Errorf("init step %q (iteration %d): response must be a JSON object for children to reference via .Parent: %w", stepLabel(step), i, err)
			if abortOnError {
				result.Children = childAccs
				return result, stepErr
			}
			errs = append(errs, stepErr)
			continue
		}

		for ci, child := range step.Children {
			childResult, err := runStep(ctx, client, baseURL, child, vars, parentObj, dict, abortOnError)
			mergeStepResult(&childAccs[ci], childResult)
			if err != nil {
				if abortOnError {
					result.Children = childAccs
					return result, err
				}
				errs = append(errs, err)
			}
		}
	}

	result.Children = childAccs
	return result, errors.Join(errs...)
}

// mergeStepResult folds next into acc, summing request/extraction counts and
// recursively merging children (matched by position, since both come from
// the same, static config.Step.Children list).
func mergeStepResult(acc *StepResult, next StepResult) {
	acc.Requests += next.Requests
	for k, v := range next.Extracted {
		acc.Extracted[k] += v
	}
	if len(acc.Children) == 0 {
		acc.Children = next.Children
		return
	}
	for i := range acc.Children {
		mergeStepResult(&acc.Children[i], next.Children[i])
	}
}

// resolveCount renders step.Count as a template (e.g. a literal "20", or
// "{{.Vars.x}}" / "{{random 1 5}}") and parses the result as a non-negative
// integer.
func resolveCount(countExpr string, data templateData) (int, error) {
	rendered, err := renderTemplate(countExpr, data)
	if err != nil {
		return 0, fmt.Errorf("rendering count template %q: %w", countExpr, err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(rendered))
	if err != nil {
		return 0, fmt.Errorf("count %q must resolve to an integer, got %q", countExpr, rendered)
	}
	if n < 0 {
		return 0, fmt.Errorf("count %q resolved to a negative value %d", countExpr, n)
	}
	return n, nil
}

func stepLabel(step config.Step) string {
	if step.Name != "" {
		return step.Name
	}
	return step.URL
}

func executeStep(ctx context.Context, client *http.Client, step config.Step, data templateData) ([]byte, error) {
	url, err := renderTemplate(step.URL, data)
	if err != nil {
		return nil, fmt.Errorf("rendering url template: %w", err)
	}

	var bodyReader io.Reader
	if step.Body != "" {
		renderedBody, err := renderTemplate(step.Body, data)
		if err != nil {
			return nil, fmt.Errorf("rendering body template: %w", err)
		}
		bodyReader = strings.NewReader(renderedBody)
	}

	req, err := http.NewRequestWithContext(ctx, step.Method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	for k, v := range step.Headers {
		req.Header.Set(k, v)
	}
	if step.Body != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, truncate(respBody, 200))
	}

	return respBody, nil
}

func renderTemplate(text string, data templateData) (string, error) {
	tmpl, err := template.New("init-step").Funcs(templateFuncs).Parse(text)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// extractInto applies a single extract rule to a response body and appends
// the result(s) into dict, returning how many values were appended.
func extractInto(dict *state.Dict, body []byte, ex config.Extract) (int, error) {
	result := gjson.GetBytes(body, ex.Path)
	if !result.Exists() {
		return 0, fmt.Errorf("path not found in response")
	}

	if result.IsArray() {
		items := result.Array()
		values := make([]any, 0, len(items))
		for _, item := range items {
			values = append(values, item.Value())
		}
		dict.AppendMany(ex.As, values)
		return len(values), nil
	}

	dict.Append(ex.As, result.Value())
	return 1, nil
}

func truncate(b []byte, n int) string {
	s := string(b)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// FlatStep pairs a StepResult with its nesting depth (0 = a top-level init
// step), for renderers that display the init phase as an indented list
// rather than a tree.
type FlatStep struct {
	Depth int
	Step  StepResult
}

// Flatten walks steps depth-first (a step immediately followed by its own
// children), returning each one paired with its nesting depth.
func Flatten(steps []StepResult) []FlatStep {
	var out []FlatStep
	var walk func(depth int, steps []StepResult)
	walk = func(depth int, steps []StepResult) {
		for _, s := range steps {
			out = append(out, FlatStep{Depth: depth, Step: s})
			walk(depth+1, s.Children)
		}
	}
	walk(0, steps)
	return out
}
