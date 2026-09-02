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
	"github.com/thecoons/myrtille/internal/servicelifecycle"
)

// Report is the combined result of one myrtille run.
type Report struct {
	Name       string
	Ref        string
	StartedAt  time.Time
	FinishedAt time.Time
	// Error, when non-empty, describes why the run did not complete
	// successfully (e.g. an init step failed, or k6 exited non-zero).
	Error string
	Init  *initphase.Summary
	// Service reports service.start_command's readiness wait and stop
	// outcome, if configured — nil when the service was assumed already
	// running (today's default behavior, unchanged).
	Service *servicelifecycle.Summary
	K6      *k6run.Result
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

	writeServiceSection(&b, r.Service)
	writeStepsSection(&b, "Init Phase", "No init steps configured.", r.Init)
	writeStepsSection(&b, "Teardown Phase", "No teardown steps configured.", r.Teardown)
	if len(r.TeardownErrors) > 0 {
		fmt.Fprintf(&b, "_%d teardown error(s) occurred (cleanup is best-effort)._\n\n", len(r.TeardownErrors))
	}
	writeK6Section(&b, r.K6)

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

// writeServiceSection renders nothing at all when summary is nil (no
// service.start_command configured) — output stays byte-identical to
// before this feature existed for the common case, same as the
// promscrape/dashboard sections elsewhere in this package.
func writeServiceSection(b *strings.Builder, summary *servicelifecycle.Summary) {
	if summary == nil {
		return
	}

	b.WriteString("## Service\n\n")
	fmt.Fprintf(b, "- Command: `%s`\n", summary.Command)
	fmt.Fprintf(b, "- Ready after: %s\n", summary.ReadyAfter.Round(time.Millisecond))

	if summary.Stop == nil {
		fmt.Fprintf(b, "- Stop: _not attempted_\n\n")
		return
	}
	status := "CLEAN"
	if summary.Stop.Err != nil {
		status = fmt.Sprintf("FAILED (%v)", summary.Stop.Err)
	} else if !summary.Stop.Clean {
		status = "TIMED OUT"
	}
	fmt.Fprintf(b, "- Stop: signal **%s**, status **%s**\n\n", summary.Stop.Signal, status)
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

// jsonReport is the on-disk shape of the JSON report; it mirrors Report but
// adds a precomputed duration since time.Duration alone isn't self-describing in JSON.
type jsonReport struct {
	Name           string                    `json:"name"`
	Ref            string                    `json:"ref,omitempty"`
	StartedAt      time.Time                 `json:"started_at"`
	FinishedAt     time.Time                 `json:"finished_at"`
	DurationSec    float64                   `json:"duration_seconds"`
	Error          string                    `json:"error,omitempty"`
	Service        *servicelifecycle.Summary `json:"service,omitempty"`
	Init           *initphase.Summary        `json:"init,omitempty"`
	K6             *k6run.Result             `json:"k6,omitempty"`
	Teardown       *initphase.Summary        `json:"teardown,omitempty"`
	TeardownErrors []string                  `json:"teardown_errors,omitempty"`
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
		Service:        r.Service,
		Init:           r.Init,
		K6:             r.K6,
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
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return "", fmt.Errorf("creating report directory: %w", err)
	}
	dir, err := uniqueReportDir(baseDir, r.StartedAt.Format("20060102-150405"))
	if err != nil {
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
		case "dashboard-html":
			if err := r.writeDashboardHTML(dir); err != nil {
				return "", err
			}
		default:
			return "", fmt.Errorf("unsupported report format %q", format)
		}
	}

	return dir, nil
}

// uniqueReportDir creates and returns a directory named base under
// parent, or base-2, base-3, ... if base is already taken. Two reports
// started within the same second — e.g. back-to-back scenarios in a
// `myrtille run --suite` (see docs/plans/suite-mode.md), which can easily
// finish inside one wall-clock second — must never silently share one
// directory: WriteFiles previously used os.MkdirAll, which is a no-op on
// an already-existing directory, so the second report's files silently
// overwrote the first's.
func uniqueReportDir(parent, base string) (string, error) {
	dir := filepath.Join(parent, base)
	if err := os.Mkdir(dir, 0o755); err == nil {
		return dir, nil
	} else if !os.IsExist(err) {
		return "", err
	}
	for i := 2; ; i++ {
		dir = filepath.Join(parent, fmt.Sprintf("%s-%d", base, i))
		err := os.Mkdir(dir, 0o755)
		if err == nil {
			return dir, nil
		}
		if !os.IsExist(err) {
			return "", err
		}
	}
}

// writeDashboardHTML copies xk6-dashboard's own standalone HTML export
// (produced by k6run.Run when "dashboard-html" is in report.formats — see
// docs/plans/xk6-dashboard-html-export.md) from its temporary location into
// dir/report.html, then removes the temporary file: Run deliberately leaves
// it in place (unlike the summary/dashboard-config temp files it fully
// consumes itself) precisely so this is where it gets cleaned up, once
// actually consumed. Errors loudly rather than silently skipping the file —
// consistent with Run's own refusal to silently drop the export when it
// can't be produced (e.g. no custom k6 binary): a report explicitly asking
// for "dashboard-html" that ends up without report.html should say why.
func (r *Report) writeDashboardHTML(dir string) error {
	if r.K6 == nil || r.K6.DashboardHTMLPath == "" {
		return fmt.Errorf(`"dashboard-html" report format requested, but no dashboard export was produced (requires the custom k6 binary, and a k6 run that reached completion)`)
	}

	data, err := os.ReadFile(r.K6.DashboardHTMLPath)
	if err != nil {
		return fmt.Errorf("reading dashboard html export: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "report.html"), data, 0o644); err != nil {
		return fmt.Errorf("writing dashboard html report: %w", err)
	}

	_ = os.Remove(r.K6.DashboardHTMLPath)
	return nil
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
