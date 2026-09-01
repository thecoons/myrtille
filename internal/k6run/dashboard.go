package k6run

import (
	"bufio"
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// DashboardPoint is one timestamped value of a DashboardSeries.
type DashboardPoint struct {
	Time  time.Time
	Value float64
}

// DashboardSeries is one metric's representative evolution over the run,
// decoded from k6's own web-dashboard record stream (see
// https://github.com/grafana/k6/tree/master/internal/dashboard). Aggregate
// names which statistic Value holds ("avg" for a Trend metric, "value" for
// a Gauge, "rate" for a Counter/Rate) — chosen as the single most
// representative series per metric; the full aggregate breakdown
// (min/max/percentiles) is already available via Summary.Metrics.
type DashboardSeries struct {
	Name      string
	Aggregate string
	Unit      string
	Points    []DashboardPoint
}

// dashboardMetricMeta mirrors one entry of a "metric" event's data, as
// emitted by k6's dashboard registry (metric_type.go/value_type.go marshal
// MetricType/ValueType as these plain strings).
type dashboardMetricMeta struct {
	Type     string `json:"type"`
	Contains string `json:"contains"`
}

// dashboardEnvelope mirrors one JSONL line of a --record stream: {"event":
// "<name>", "data": <event-specific payload>}.
type dashboardEnvelope struct {
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
}

// dashboardAggregates lists, per metric type, the fields k6 packs
// positionally into a snapshot/cumulative event's per-metric array (see
// aggregateNames in k6's internal/dashboard/meter.go), and which index is
// this package's chosen representative series for that type.
var dashboardAggregates = map[string]struct {
	names []string
	pick  int
}{
	"trend":   {names: []string{"avg", "max", "med", "min", "p(90)", "p(95)", "p(99)"}, pick: 0},
	"counter": {names: []string{"count", "rate"}, pick: 1},
	"gauge":   {names: []string{"value"}, pick: 0},
	"rate":    {names: []string{"rate"}, pick: 0},
}

// dashboardTimeMetric is the synthetic gauge metric k6's dashboard always
// registers first, whose value is the snapshot's own wall-clock
// epoch-milliseconds — used here only to timestamp points, never charted.
const dashboardTimeMetric = "time"

// parseDashboardRecord decodes a k6 --out web-dashboard=record=... JSONL
// stream into one representative time series per top-level metric.
// Submetrics (tag-broken-down names, containing '{') and malformed lines
// are silently skipped, tolerating k6 protocol drift the same way
// parseSummary tolerates --summary-export's — this is best-effort
// visualization data, not something a run's success should hinge on.
func parseDashboardRecord(data []byte) ([]DashboardSeries, error) {
	meta := make(map[string]dashboardMetricMeta)
	var names []string // kept sorted, mirrors k6's own registry ordering

	series := make(map[string]*DashboardSeries)

	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}

		var env dashboardEnvelope
		if err := json.Unmarshal(line, &env); err != nil {
			continue
		}

		switch env.Event {
		case "metric":
			var newMeta map[string]dashboardMetricMeta
			if err := json.Unmarshal(env.Data, &newMeta); err != nil {
				continue
			}
			for name, m := range newMeta {
				if _, exists := meta[name]; exists {
					continue
				}
				meta[name] = m
				names = append(names, name)
			}
			sort.Strings(names)

		case "snapshot":
			var values [][]float64
			if err := json.Unmarshal(env.Data, &values); err != nil || len(values) != len(names) {
				continue
			}

			var at time.Time
			for i, name := range names {
				if name == dashboardTimeMetric && len(values[i]) > 0 {
					at = time.UnixMilli(int64(values[i][0]))
					break
				}
			}
			if at.IsZero() {
				continue
			}

			for i, name := range names {
				if name == dashboardTimeMetric || strings.Contains(name, "{") {
					continue
				}
				m, ok := meta[name]
				if !ok {
					continue
				}
				agg, ok := dashboardAggregates[m.Type]
				if !ok || agg.pick >= len(values[i]) {
					continue
				}

				s, exists := series[name]
				if !exists {
					s = &DashboardSeries{Name: name, Aggregate: agg.names[agg.pick], Unit: dashboardUnit(m)}
					series[name] = s
				}
				s.Points = append(s.Points, DashboardPoint{Time: at, Value: values[i][agg.pick]})
			}
		}
	}

	out := make([]DashboardSeries, 0, len(series))
	for _, s := range series {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	return out, nil
}

// dashboardUnit infers a display unit from a metric's Contains field
// (k6's own value-type hint — "time" values are always in milliseconds,
// "data" in bytes) rather than a hardcoded per-metric-name table, so it
// works for custom script metrics too, not just k6's built-ins.
func dashboardUnit(m dashboardMetricMeta) string {
	switch m.Contains {
	case "time":
		return "milliseconds"
	case "data":
		return "bytes"
	}
	switch m.Type {
	case "rate":
		return "ratio"
	case "counter":
		return "count"
	default:
		return ""
	}
}
