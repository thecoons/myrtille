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
// and matches the viewer's OS theme. The --series-1/--text-secondary/
// --gridline variables are also read at runtime by chartInitScript (see
// chartjs.go), so Chart.js charts pick up the same palette.
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
  --row-alt: rgba(11, 11, 11, 0.025);
  --row-hover: rgba(11, 11, 11, 0.05);
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
    --row-alt: rgba(255, 255, 255, 0.03);
    --row-hover: rgba(255, 255, 255, 0.06);
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
.table-wrap { background: var(--surface-1); border-radius: 6px; padding: 4px 16px; margin: 8px 0 20px; overflow-x: auto; }
table { border-collapse: collapse; width: 100%; font-variant-numeric: tabular-nums; }
th, td { text-align: left; padding: 8px 12px; white-space: nowrap; }
th { color: var(--text-secondary); font-weight: 600; font-size: 12px; text-transform: uppercase; letter-spacing: 0.03em; border-bottom: 1px solid var(--axis); }
td { border-bottom: 1px solid var(--gridline); }
th.num, td.num { text-align: right; }
tbody tr:last-child td { border-bottom: none; }
tbody tr:nth-child(even) td { background: var(--row-alt); }
tbody tr:hover td { background: var(--row-hover); }
.chart-grid { display: flex; flex-wrap: wrap; gap: 16px; margin-bottom: 20px; }
.chart-wrap { background: var(--surface-1); border-radius: 6px; padding: 8px; flex: 1 1 420px; height: 260px; position: relative; }
.chart-empty { color: var(--text-muted); font-size: 12px; }
</style>`

// HTML renders a self-contained report: the same sections as Markdown, plus
// a Chart.js line chart per scraped metric series and a Chart.js bar chart
// per k6 metric summary, so a benchmark's behavior over time can be read
// (and hovered for exact values) visually rather than from a table of
// aggregates alone.
func (r *Report) HTML() string {
	var b strings.Builder
	var charts []chartEntry

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
	writeHTMLK6Section(&b, r.K6, &charts)
	writeHTMLMetricsSection(&b, r.MetricSamples, r.MetricSeries, r.ScrapeErrors, r.StartedAt, &charts)
	writeChartScripts(&b, charts)

	b.WriteString("</body></html>\n")
	return b.String()
}

func writeHTMLInitSection(b *strings.Builder, init *initphase.Summary) {
	b.WriteString("<h2>Init Phase</h2>\n")
	if init == nil || len(init.Steps) == 0 {
		b.WriteString("<p><em>No init steps configured.</em></p>\n")
		return
	}

	b.WriteString("<div class=\"table-wrap\"><table>\n")
	b.WriteString("<thead><tr><th>Step</th><th class=\"num\">Requests</th><th>Extracted</th></tr></thead>\n<tbody>\n")
	for _, fs := range initphase.Flatten(init.Steps) {
		name := strings.Repeat("↳ ", fs.Depth) + nonEmpty(fs.Step.Name, "-")
		fmt.Fprintf(b, "<tr><td>%s</td><td class=\"num\">%d</td><td>%s</td></tr>\n",
			html.EscapeString(name), fs.Step.Requests, html.EscapeString(formatIntMap(fs.Step.Extracted)))
	}
	b.WriteString("</tbody></table></div>\n")
}

func writeHTMLK6Section(b *strings.Builder, result *k6run.Result, charts *[]chartEntry) {
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
		cfg, ok := barChartConfig(name, k6MetricUnit(name), result.Summary.Metrics[name].Values)
		writeChartCanvas(b, charts, cfg, ok)
	}
	b.WriteString("</div>\n")
}

func writeHTMLMetricsSection(b *strings.Builder, samples []metrics.Sample, series []metrics.SeriesSummary, scrapeErrors []string, startedAt time.Time, charts *[]chartEntry) {
	b.WriteString("<h2>Metrics During Load</h2>\n")
	if len(series) == 0 {
		b.WriteString("<p><em>No metrics collected.</em></p>\n")
	} else {
		b.WriteString("<div class=\"table-wrap\"><table>\n")
		b.WriteString("<thead><tr><th>Series</th><th class=\"num\">Samples</th><th class=\"num\">Min</th><th class=\"num\">Max</th><th class=\"num\">Avg</th></tr></thead>\n<tbody>\n")
		for _, s := range series {
			fmt.Fprintf(b, "<tr><td>%s</td><td class=\"num\">%d</td><td class=\"num\">%g</td><td class=\"num\">%g</td><td class=\"num\">%g</td></tr>\n",
				html.EscapeString(s.String()), s.Count, s.Min, s.Max, s.Avg)
		}
		b.WriteString("</tbody></table></div>\n")

		b.WriteString("<div class=\"chart-grid\">\n")
		for _, sr := range metrics.GroupSeries(samples) {
			title := (metrics.SeriesSummary{Name: sr.Name, Labels: sr.Labels}).String()
			points := make([]chartPoint, len(sr.Samples))
			for i, s := range sr.Samples {
				points[i] = chartPoint{X: s.Timestamp.Sub(startedAt).Seconds(), Y: s.Value}
			}
			cfg, ok := lineChartConfig(title, metricUnit(sr.Name), points)
			writeChartCanvas(b, charts, cfg, ok)
		}
		b.WriteString("</div>\n")
	}

	if len(scrapeErrors) > 0 {
		fmt.Fprintf(b, "<p><em>%d metrics scrape error(s) occurred during the run.</em></p>\n", len(scrapeErrors))
	}
}

// writeChartCanvas appends a <canvas> (and its matching chartEntry) when cfg
// is usable, or a "no data" placeholder otherwise, matching the Markdown
// report's placeholder convention for empty sections.
func writeChartCanvas(b *strings.Builder, charts *[]chartEntry, cfg any, ok bool) {
	if !ok {
		b.WriteString("<div class=\"chart-wrap\"><p class=\"chart-empty\">no data</p></div>\n")
		return
	}
	id := fmt.Sprintf("chart-%d", len(*charts))
	*charts = append(*charts, chartEntry{ID: id, Config: cfg})
	fmt.Fprintf(b, "<div class=\"chart-wrap\"><canvas id=%q></canvas></div>\n", id)
}
