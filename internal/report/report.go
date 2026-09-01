// Package report aggregates the results of the init, k6 run, and metrics
// scraping phases into a single Report, rendered to Markdown and/or JSON
// files for a human (or an issue/ticket comment) to read.
package report

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/thecoons/myrtille/internal/initphase"
	"github.com/thecoons/myrtille/internal/k6run"
	"github.com/thecoons/myrtille/internal/metrics"
)

// Report is the combined result of one myrtille run.
type Report struct {
	Name       string
	Ref        string
	StartedAt  time.Time
	FinishedAt time.Time
	// Error, when non-empty, describes why the run did not complete
	// successfully (e.g. an init step failed, or k6 exited non-zero).
	Error        string
	Init         *initphase.Summary
	K6           *k6run.Result
	MetricSeries []metrics.SeriesSummary
	// MetricSamples holds every raw scraped sample (not just the
	// aggregated MetricSeries), used to render the per-series evolution
	// charts in HTML(). It is intentionally not part of the JSON report:
	// the JSON stays a compact summary, not a full time-series dump.
	MetricSamples []metrics.Sample
	ScrapeErrors  []string
	// Teardown and TeardownErrors report the best-effort cleanup phase, if
	// teardown.steps is configured. Teardown failures never set Error /
	// affect Duration()'s notion of the run's success — cleanup is
	// inherently best-effort, so it's surfaced separately.
	Teardown       *initphase.Summary
	TeardownErrors []string
}

// Duration is how long the whole run took, from init start to k6 finishing.
func (r *Report) Duration() time.Duration {
	return r.FinishedAt.Sub(r.StartedAt)
}

// Markdown renders a human-readable report.
func (r *Report) Markdown() string {
	var b strings.Builder

	fmt.Fprintf(&b, "# Load Test Report: %s\n\n", nonEmpty(r.Name, "(unnamed)"))
	if r.Ref != "" {
		fmt.Fprintf(&b, "**Ref:** %s\n\n", r.Ref)
	}
	if r.Error != "" {
		fmt.Fprintf(&b, "**ERROR:** %s\n\n", r.Error)
	}
	fmt.Fprintf(&b, "- Started: %s\n", r.StartedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "- Finished: %s\n", r.FinishedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "- Duration: %s\n\n", r.Duration().Round(time.Second))

	writeStepsSection(&b, "Init Phase", "No init steps configured.", r.Init)
	writeStepsSection(&b, "Teardown Phase", "No teardown steps configured.", r.Teardown)
	if len(r.TeardownErrors) > 0 {
		fmt.Fprintf(&b, "_%d teardown error(s) occurred (cleanup is best-effort)._\n\n", len(r.TeardownErrors))
	}
	writeK6Section(&b, r.K6)
	writeMetricsSection(&b, r.MetricSeries, r.ScrapeErrors)

	return b.String()
}

func writeStepsSection(b *strings.Builder, heading, emptyMsg string, summary *initphase.Summary) {
	fmt.Fprintf(b, "## %s\n\n", heading)
	if summary == nil {
		fmt.Fprintf(b, "_%s_\n\n", emptyMsg)
		return
	}
	if summary.Command != nil {
		writeCommandSummary(b, summary.Command)
		return
	}
	if len(summary.Steps) == 0 {
		fmt.Fprintf(b, "_%s_\n\n", emptyMsg)
		return
	}

	b.WriteString("| Step | Requests | Extracted |\n|---|---|---|\n")
	for _, fs := range initphase.Flatten(summary.Steps) {
		name := strings.Repeat("↳ ", fs.Depth) + nonEmpty(fs.Step.Name, "-")
		fmt.Fprintf(b, "| %s | %d | %s |\n", name, fs.Step.Requests, formatIntMap(fs.Step.Extracted))
	}
	b.WriteString("\n")
}

func writeCommandSummary(b *strings.Builder, cmd *initphase.CommandResult) {
	status := "OK"
	switch {
	case cmd.TimedOut:
		status = "TIMED OUT"
	case cmd.ExitCode != 0:
		status = "FAILED"
	}
	fmt.Fprintf(b, "- Command: `%s`\n", cmd.Command)
	fmt.Fprintf(b, "- Status: **%s** (exit code %d)\n", status, cmd.ExitCode)
	fmt.Fprintf(b, "- Duration: %s\n\n", cmd.Duration.Round(time.Millisecond))
}

func writeK6Section(b *strings.Builder, result *k6run.Result) {
	b.WriteString("## k6 Results\n\n")
	if result == nil {
		b.WriteString("_k6 did not run._\n\n")
		return
	}

	status := "PASSED"
	switch {
	case result.ThresholdsFailed:
		status = "THRESHOLDS FAILED"
	case !result.Passed:
		status = "FAILED"
	}
	fmt.Fprintf(b, "- Status: **%s** (exit code %d)\n", status, result.ExitCode)
	fmt.Fprintf(b, "- k6 run duration: %s\n\n", result.Duration.Round(time.Second))

	if result.Summary == nil {
		return
	}

	if len(result.Summary.Metrics) > 0 {
		b.WriteString("| Metric | Values | Thresholds |\n|---|---|---|\n")
		for _, name := range sortedMetricNames(result.Summary.Metrics) {
			m := result.Summary.Metrics[name]
			fmt.Fprintf(b, "| %s | %s | %s |\n", name, formatFloatMap(m.Values), formatThresholds(m.Thresholds))
		}
		b.WriteString("\n")
	}

	if len(result.Summary.Checks) > 0 {
		b.WriteString("### Checks\n\n")
		for _, c := range result.Summary.Checks {
			fmt.Fprintf(b, "- %s: %d passed, %d failed [%s]\n", c.Path, c.Passes, c.Fails, checkStatus(c))
		}
		b.WriteString("\n")
	}
}

func checkStatus(c k6run.CheckResult) string {
	if c.Fails > 0 {
		return "FAIL"
	}
	return "OK"
}

func writeMetricsSection(b *strings.Builder, series []metrics.SeriesSummary, scrapeErrors []string) {
	b.WriteString("## Metrics During Load\n\n")
	if len(series) == 0 {
		b.WriteString("_No metrics collected._\n\n")
	} else {
		b.WriteString("| Series | Samples | Min | Max | Avg |\n|---|---|---|---|---|\n")
		for _, s := range series {
			fmt.Fprintf(b, "| %s | %d | %g | %g | %g |\n", s.String(), s.Count, s.Min, s.Max, s.Avg)
		}
		b.WriteString("\n")
	}

	if len(scrapeErrors) > 0 {
		fmt.Fprintf(b, "_%d metrics scrape error(s) occurred during the run._\n\n", len(scrapeErrors))
	}
}

// jsonReport is the on-disk shape of the JSON report; it mirrors Report but
// adds a precomputed duration since time.Duration alone isn't self-describing in JSON.
type jsonReport struct {
	Name           string                  `json:"name"`
	Ref            string                  `json:"ref,omitempty"`
	StartedAt      time.Time               `json:"started_at"`
	FinishedAt     time.Time               `json:"finished_at"`
	DurationSec    float64                 `json:"duration_seconds"`
	Error          string                  `json:"error,omitempty"`
	Init           *initphase.Summary      `json:"init,omitempty"`
	K6             *k6run.Result           `json:"k6,omitempty"`
	MetricSeries   []metrics.SeriesSummary `json:"metric_series,omitempty"`
	ScrapeErrors   []string                `json:"scrape_errors,omitempty"`
	Teardown       *initphase.Summary      `json:"teardown,omitempty"`
	TeardownErrors []string                `json:"teardown_errors,omitempty"`
}

// JSON renders the report as indented JSON.
func (r *Report) JSON() ([]byte, error) {
	payload := jsonReport{
		Name:           r.Name,
		Ref:            r.Ref,
		StartedAt:      r.StartedAt,
		FinishedAt:     r.FinishedAt,
		DurationSec:    r.Duration().Seconds(),
		Error:          r.Error,
		Init:           r.Init,
		K6:             r.K6,
		MetricSeries:   r.MetricSeries,
		ScrapeErrors:   r.ScrapeErrors,
		Teardown:       r.Teardown,
		TeardownErrors: r.TeardownErrors,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshaling report: %w", err)
	}
	return data, nil
}

// WriteFiles renders the report in each requested format ("markdown",
// "json") into a timestamped subdirectory of baseDir, returning that
// subdirectory's path.
func (r *Report) WriteFiles(baseDir string, formats []string) (string, error) {
	dir := filepath.Join(baseDir, r.StartedAt.Format("20060102-150405"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating report directory: %w", err)
	}

	for _, format := range formats {
		switch format {
		case "markdown":
			if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte(r.Markdown()), 0o644); err != nil {
				return "", fmt.Errorf("writing markdown report: %w", err)
			}
		case "json":
			data, err := r.JSON()
			if err != nil {
				return "", err
			}
			if err := os.WriteFile(filepath.Join(dir, "report.json"), data, 0o644); err != nil {
				return "", fmt.Errorf("writing json report: %w", err)
			}
		case "html":
			if err := os.WriteFile(filepath.Join(dir, "report.html"), []byte(r.HTML()), 0o644); err != nil {
				return "", fmt.Errorf("writing html report: %w", err)
			}
		default:
			return "", fmt.Errorf("unsupported report format %q", format)
		}
	}

	return dir, nil
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func formatIntMap(m map[string]int) string {
	if len(m) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

func formatFloatMap(m map[string]float64) string {
	if len(m) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%g", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

func formatThresholds(m map[string]bool) string {
	if len(m) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		status := "OK"
		if !m[k] {
			status = "FAIL"
		}
		parts = append(parts, fmt.Sprintf("%s: %s", k, status))
	}
	return strings.Join(parts, ", ")
}

func sortedMetricNames(m map[string]k6run.MetricSummary) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
