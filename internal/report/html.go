package report

import (
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/antobarth/myrtille/internal/initphase"
	"github.com/antobarth/myrtille/internal/k6run"
	"github.com/antobarth/myrtille/internal/metrics"
)

// htmlStyle is a self-contained stylesheet (light + dark, no external
// assets) shared by every report.html, so the report is readable offline
// and matches the viewer's OS theme.
const htmlStyle = `<style>
:root {
  color-scheme: light;
  --surface-1: #fcfcfb;
  --page-plane: #f9f9f7;
  --text-primary: #0b0b0b;
  --text-secondary: #52514e;
  --text-muted: #898781;
  --gridline: #e1e0d9;
  --axis: #c3c2b7;
  --series-1: #2a78d6;
  --status-critical: #d03b3b;
}
@media (prefers-color-scheme: dark) {
  :root {
    color-scheme: dark;
    --surface-1: #1a1a19;
    --page-plane: #0d0d0d;
    --text-primary: #ffffff;
    --text-secondary: #c3c2b7;
    --text-muted: #898781;
    --gridline: #2c2c2a;
    --axis: #383835;
    --series-1: #3987e5;
    --status-critical: #e66767;
  }
}
body {
  margin: 0;
  padding: 24px;
  background: var(--page-plane);
  color: var(--text-primary);
  font-family: system-ui, -apple-system, "Segoe UI", sans-serif;
}
.error { color: var(--status-critical); }
table { border-collapse: collapse; margin: 8px 0 20px; font-variant-numeric: tabular-nums; }
th, td { text-align: left; padding: 4px 12px 4px 0; border-bottom: 1px solid var(--gridline); }
th { color: var(--text-secondary); font-weight: 600; }
.chart-grid { display: flex; flex-wrap: wrap; gap: 16px; margin-bottom: 20px; }
.chart { background: var(--surface-1); border-radius: 6px; }
.chart-title { fill: var(--text-primary); font-size: 13px; font-weight: 600; }
.chart-empty { fill: var(--text-muted); font-size: 12px; }
.chart-axis { stroke: var(--axis); stroke-width: 1; }
.chart-axis-label { fill: var(--text-muted); font-size: 11px; }
.chart-line { fill: none; stroke: var(--series-1); stroke-width: 2; stroke-linecap: round; stroke-linejoin: round; }
.chart-end-ring { fill: var(--surface-1); }
.chart-end-dot { fill: var(--series-1); }
.chart-end-label { fill: var(--text-secondary); font-size: 11px; }
.chart-bar { fill: var(--series-1); }
.chart-bar-label { fill: var(--text-secondary); font-size: 11px; }
.chart-bar-value { fill: var(--text-secondary); font-size: 11px; font-variant-numeric: tabular-nums; }
</style>`

// HTML renders a self-contained report: the same sections as Markdown,
// plus an SVG evolution chart per scraped metric series and an SVG bar
// chart per k6 metric summary, so a benchmark's behavior over time can be
// read visually rather than from a table of aggregates alone.
func (r *Report) HTML() string {
	var b strings.Builder

	name := html.EscapeString(nonEmpty(r.Name, "(unnamed)"))
	b.WriteString("<!DOCTYPE html>\n<html lang=\"en\"><head><meta charset=\"utf-8\">\n")
	fmt.Fprintf(&b, "<title>Load Test Report: %s</title>\n", name)
	b.WriteString(htmlStyle)
	b.WriteString("\n</head><body>\n")

	fmt.Fprintf(&b, "<h1>Load Test Report: %s</h1>\n", name)
	if r.Ref != "" {
		fmt.Fprintf(&b, "<p><strong>Ref:</strong> %s</p>\n", html.EscapeString(r.Ref))
	}
	if r.Error != "" {
		fmt.Fprintf(&b, "<p class=\"error\"><strong>ERROR:</strong> %s</p>\n", html.EscapeString(r.Error))
	}
	fmt.Fprintf(&b, "<ul><li>Started: %s</li><li>Finished: %s</li><li>Duration: %s</li></ul>\n",
		r.StartedAt.Format(time.RFC3339), r.FinishedAt.Format(time.RFC3339), r.Duration().Round(time.Second))

	writeHTMLInitSection(&b, r.Init)
	writeHTMLK6Section(&b, r.K6)
	writeHTMLMetricsSection(&b, r.MetricSamples, r.MetricSeries, r.ScrapeErrors, r.StartedAt)

	b.WriteString("</body></html>\n")
	return b.String()
}

func writeHTMLInitSection(b *strings.Builder, init *initphase.Summary) {
	b.WriteString("<h2>Init Phase</h2>\n")
	if init == nil || len(init.Steps) == 0 {
		b.WriteString("<p><em>No init steps configured.</em></p>\n")
		return
	}

	b.WriteString("<table><tr><th>Step</th><th>Requests</th><th>Extracted</th></tr>\n")
	for _, s := range init.Steps {
		fmt.Fprintf(b, "<tr><td>%s</td><td>%d</td><td>%s</td></tr>\n",
			html.EscapeString(nonEmpty(s.Name, "-")), s.Requests, html.EscapeString(formatIntMap(s.Extracted)))
	}
	b.WriteString("</table>\n")
}

func writeHTMLK6Section(b *strings.Builder, result *k6run.Result) {
	b.WriteString("<h2>k6 Results</h2>\n")
	if result == nil {
		b.WriteString("<p><em>k6 did not run.</em></p>\n")
		return
	}

	status := "PASSED"
	switch {
	case result.ThresholdsFailed:
		status = "THRESHOLDS FAILED"
	case !result.Passed:
		status = "FAILED"
	}
	fmt.Fprintf(b, "<p>Status: <strong>%s</strong> (exit code %d)<br>k6 run duration: %s</p>\n",
		html.EscapeString(status), result.ExitCode, result.Duration.Round(time.Second))

	if result.Summary == nil || len(result.Summary.Metrics) == 0 {
		return
	}

	b.WriteString("<div class=\"chart-grid\">\n")
	for _, name := range sortedMetricNames(result.Summary.Metrics) {
		b.WriteString(renderBarChart(name, result.Summary.Metrics[name].Values))
		b.WriteString("\n")
	}
	b.WriteString("</div>\n")
}

func writeHTMLMetricsSection(b *strings.Builder, samples []metrics.Sample, series []metrics.SeriesSummary, scrapeErrors []string, startedAt time.Time) {
	b.WriteString("<h2>Metrics During Load</h2>\n")
	if len(series) == 0 {
		b.WriteString("<p><em>No metrics collected.</em></p>\n")
	} else {
		b.WriteString("<table><tr><th>Series</th><th>Samples</th><th>Min</th><th>Max</th><th>Avg</th></tr>\n")
		for _, s := range series {
			fmt.Fprintf(b, "<tr><td>%s</td><td>%d</td><td>%g</td><td>%g</td><td>%g</td></tr>\n",
				html.EscapeString(s.String()), s.Count, s.Min, s.Max, s.Avg)
		}
		b.WriteString("</table>\n")

		b.WriteString("<div class=\"chart-grid\">\n")
		for _, sr := range metrics.GroupSeries(samples) {
			title := (metrics.SeriesSummary{Name: sr.Name, Labels: sr.Labels}).String()
			points := make([]chartPoint, len(sr.Samples))
			for i, s := range sr.Samples {
				points[i] = chartPoint{X: s.Timestamp.Sub(startedAt).Seconds(), Y: s.Value}
			}
			b.WriteString(renderLineChart(title, points))
			b.WriteString("\n")
		}
		b.WriteString("</div>\n")
	}

	if len(scrapeErrors) > 0 {
		fmt.Fprintf(b, "<p><em>%d metrics scrape error(s) occurred during the run.</em></p>\n", len(scrapeErrors))
	}
}
