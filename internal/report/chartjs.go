package report

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// chartJSSource is Chart.js v4.5.1 (UMD build), vendored under assets/chartjs
// (LICENSE alongside it) so report.html stays self-contained and works
// offline: the library is inlined into the page, not fetched from a CDN.
//
//go:embed assets/chartjs/chart.umd.min.js
var chartJSSource string

// chartPoint is one X/Y data point to plot, in data units (not pixels).
type chartPoint struct {
	X float64
	Y float64
}

// chartEntry pairs a <canvas> element's id with the Chart.js config to
// instantiate on it. Config is a plain map rather than a typed binding of
// Chart.js's config schema: that schema is large and JS-shaped, and a
// map[string]any marshals to exactly the JSON Chart.js expects without
// modeling API surface this project doesn't otherwise touch.
type chartEntry struct {
	ID     string `json:"id"`
	Config any    `json:"config"`
}

// lineChartConfig builds a Chart.js line-chart config for points (assumed
// sorted by X). Points with a non-finite Y (NaN/+Inf/-Inf — encoding/json
// errors on these, unlike the SVG renderer this replaces, which tolerated
// them by construction) are dropped. ok is false when no plottable point
// remains, so the caller can fall back to a text placeholder instead of an
// empty chart. unit, when non-empty (e.g. "seconds", "bytes"), labels the Y
// axis so the reader isn't left guessing the dimension of the plotted value.
func lineChartConfig(title, unit string, points []chartPoint) (cfg any, ok bool) {
	data := make([]map[string]float64, 0, len(points))
	for _, p := range points {
		if math.IsNaN(p.Y) || math.IsInf(p.Y, 0) {
			continue
		}
		data = append(data, map[string]float64{"x": p.X, "y": p.Y})
	}
	if len(data) == 0 {
		return nil, false
	}

	return map[string]any{
		"type": "line",
		"data": map[string]any{
			"datasets": []map[string]any{{
				"label":            title,
				"data":             data,
				"borderWidth":      2,
				"tension":          0,
				"pointRadius":      2,
				"pointHoverRadius": 5,
				"fill":             true,
			}},
		},
		"options": map[string]any{
			"responsive":          true,
			"maintainAspectRatio": false,
			"animation":           false,
			"plugins": map[string]any{
				"legend": map[string]any{"display": false},
				"title":  map[string]any{"display": true, "text": title},
			},
			"scales": map[string]any{
				"x": map[string]any{
					"type":  "linear",
					"title": map[string]any{"display": true, "text": "elapsed seconds"},
				},
				"y": map[string]any{
					"beginAtZero": false,
					"title":       map[string]any{"display": unit != "", "text": unit},
				},
			},
		},
	}, true
}

// metricUnit infers a display unit for a Prometheus metric name from its
// suffix, following Prometheus's own naming convention
// (https://prometheus.io/docs/practices/naming/#base-units) rather than
// requiring the scraper to carry HELP/UNIT metadata through the pipeline.
// Returns "" when no convention matches, in which case the axis is left
// unlabeled rather than showing a guessed unit.
func metricUnit(name string) string {
	switch {
	case strings.HasSuffix(name, "_count"):
		// The _count series synthesized from a histogram/summary (see
		// metrics.Parse) counts observations, regardless of what unit the
		// observations themselves are in.
		return "count"
	case strings.Contains(name, "_seconds"):
		return "seconds"
	case strings.Contains(name, "_bytes"):
		return "bytes"
	case strings.Contains(name, "_ratio"):
		return "ratio"
	case strings.Contains(name, "_percent"):
		return "percent"
	case strings.HasSuffix(name, "_total"):
		return "count"
	default:
		return ""
	}
}

// chartInitScript is hand-written (not vendored): it resolves the palette
// CSS variables already defined in htmlStyle at load time — so charts match
// whichever light/dark theme prefers-color-scheme selected — and
// instantiates one Chart.js chart per entry in window.__MYRTILLE_CHARTS__.
const chartInitScript = `(function () {
  var css = getComputedStyle(document.documentElement);
  var seriesColor = css.getPropertyValue('--series-1').trim();
  var textColor = css.getPropertyValue('--text-secondary').trim();
  var gridColor = css.getPropertyValue('--gridline').trim();

  Chart.defaults.color = textColor;
  Chart.defaults.borderColor = gridColor;
  Chart.defaults.font.family = getComputedStyle(document.body).fontFamily;

  (window.__MYRTILLE_CHARTS__ || []).forEach(function (entry) {
    var canvas = document.getElementById(entry.id);
    if (!canvas) return;
    var datasets = entry.config.data.datasets;
    for (var i = 0; i < datasets.length; i++) {
      if (!datasets[i].borderColor) datasets[i].borderColor = seriesColor;
      if (!datasets[i].backgroundColor) {
        datasets[i].backgroundColor = entry.config.type === 'bar' ? seriesColor : seriesColor + '1a';
      }
    }
    new Chart(canvas.getContext('2d'), entry.config);
  });
})();`

// writeChartScripts inlines the vendored Chart.js library, the collected
// chart configs (as JSON — encoding/json HTML-escapes '<', '>' and '&' by
// default, so an adversarial metric/title name containing "</script>"
// cannot break out of the surrounding <script> tag), and the init script
// that instantiates them.
func writeChartScripts(b *strings.Builder, charts []chartEntry) {
	fmt.Fprintf(b, "<script>%s</script>\n", chartJSSource)

	data, err := json.Marshal(charts)
	if err != nil {
		// Config values are built exclusively by lineChartConfig/
		// barChartConfig above, which only ever produce JSON-safe types
		// (strings, finite float64s, bools, nested maps/slices thereof) —
		// this is unreachable in practice, so there's nothing more useful
		// to do than skip rendering charts for this report.
		return
	}

	fmt.Fprintf(b, "<script>window.__MYRTILLE_CHARTS__ = %s;</script>\n", data)
	fmt.Fprintf(b, "<script>%s</script>\n", chartInitScript)
}
