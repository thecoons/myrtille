// Package initphase executes the declarative HTTP steps configured under
// `init.steps` to bring the target service into a known state, extracting
// values (IDs, etc.) from responses into a state.Dict that k6 scenarios can
// later randomize over.
package initphase

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"text/template"
	"time"

	"github.com/antobarth/myrtille/internal/config"
	"github.com/antobarth/myrtille/internal/state"
	"github.com/tidwall/gjson"
)

const requestTimeout = 30 * time.Second

// templateData is exposed to url/body templates as `.BaseURL` and `.Index`.
type templateData struct {
	BaseURL string
	Index   int
}

// StepResult reports what a single init step produced, for use in the report.
type StepResult struct {
	Name      string
	Requests  int
	Extracted map[string]int
}

// Summary reports what the whole init phase produced.
type Summary struct {
	Steps []StepResult
}

// Run executes each configured init step, in declaration order, `step.Count`
// times each, extracting values into dict. It aborts on the first HTTP
// error (status >= 400) or transport error: a partially-initialized service
// would make the subsequent load test unreliable, so we fail fast rather
// than run k6 against unknown state.
func Run(ctx context.Context, cfg *config.Config, dict *state.Dict) (*Summary, error) {
	client := &http.Client{Timeout: requestTimeout}
	summary := &Summary{}

	for _, step := range cfg.Init.Steps {
		result := StepResult{Name: step.Name, Extracted: map[string]int{}}

		for i := 0; i < step.Count; i++ {
			body, err := executeStep(ctx, client, cfg.Service.BaseURL, step, i)
			if err != nil {
				return summary, fmt.Errorf("init step %q (iteration %d): %w", stepLabel(step), i, err)
			}
			result.Requests++

			for _, ex := range step.Extract {
				n, err := extractInto(dict, body, ex)
				if err != nil {
					return summary, fmt.Errorf("init step %q (iteration %d): extracting %q: %w", stepLabel(step), i, ex.Path, err)
				}
				result.Extracted[ex.As] += n
			}
		}

		summary.Steps = append(summary.Steps, result)
	}

	return summary, nil
}

func stepLabel(step config.Step) string {
	if step.Name != "" {
		return step.Name
	}
	return step.URL
}

func executeStep(ctx context.Context, client *http.Client, baseURL string, step config.Step, index int) ([]byte, error) {
	data := templateData{BaseURL: baseURL, Index: index}

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
	tmpl, err := template.New("init-step").Parse(text)
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
