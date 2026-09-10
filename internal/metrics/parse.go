// Package metrics decodes a Prometheus text-exposition payload into
// samples. It's the shared parsing primitive behind both
// internal/dashboardconfig (a one-shot discovery scrape, run before k6
// starts, to build the live dashboard's "Service" tab) and
// pkg/xk6ext/promscrape (which mirrors the same series into k6's own metrics
// pipeline during the run) — see docs/plans/xk6-live-dashboard.md.
package metrics

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// exemplarSuffix matches the trailing OpenMetrics-style exemplar annotation
// (`# {labels} value [timestamp]`) that instrumentation libraries such as
// Micrometer append to a sample line when a sampled trace/span is attached
// at increment time — even though the rest of the response is plain
// Prometheus text, not OpenMetrics. The legacy expfmt.TextParser used below
// doesn't understand it and errors out ("expected integer as timestamp, got
// \"#\""), so it's stripped before parsing.
var exemplarSuffix = regexp.MustCompile(`\s+#\s+\{[^{}]*\}\s+[-+0-9.eE]+(?:\s+[0-9]+(?:\.[0-9]+)?)?\s*$`)

// stripExemplars removes trailing exemplar annotations from each
// non-comment line of a Prometheus text-exposition payload, leaving the
// rest of the line (metric, labels, value, timestamp) untouched.
func stripExemplars(r io.Reader) (io.Reader, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	var buf bytes.Buffer
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "#") {
			if loc := exemplarSuffix.FindStringIndex(line); loc != nil {
				line = line[:loc[0]]
			}
		}
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return &buf, nil
}

// K6Prefix is prepended to a Prometheus series name wherever it's turned
// into a k6 metric or dashboard query — pkg/xk6ext/promscrape uses it when
// registering k6 metrics, internal/dashboardconfig uses it when building
// panel queries for those same metrics. Defined once here, the shared
// ground both already import, so the two can never drift apart the way a
// hand-copied constant could.
const K6Prefix = "svc_"

// Kind classifies how a Sample's Value should be interpreted across
// scrapes: KindCounter values are cumulative (monotonically increasing
// since the target process started, so consumers wanting a rate need the
// delta between two scrapes), KindGauge values stand on their own (the
// current level, directly comparable as-is).
type Kind string

const (
	KindCounter Kind = "counter"
	KindGauge   Kind = "gauge"
)

// Sample is a single observed value for a metric series at a point in time.
type Sample struct {
	Timestamp time.Time
	Name      string
	Labels    map[string]string
	Value     float64
	Kind      Kind
}

// Parse decodes a Prometheus text-exposition payload into samples, all
// stamped with ts. Histograms and summaries are reduced to their `_sum` and
// `_count` series rather than exposing individual buckets/quantiles, which
// is enough to see whether a metric moved during the run without callers
// needing to understand bucket layouts.
func Parse(r io.Reader, ts time.Time) ([]Sample, error) {
	stripped, err := stripExemplars(r)
	if err != nil {
		return nil, fmt.Errorf("parsing metrics: %w", err)
	}

	parser := expfmt.NewTextParser(model.LegacyValidation)
	families, err := parser.TextToMetricFamilies(stripped)
	if err != nil {
		return nil, fmt.Errorf("parsing metrics: %w", err)
	}

	var samples []Sample
	for name, family := range families {
		for _, m := range family.Metric {
			labels := make(map[string]string, len(m.Label))
			for _, l := range m.Label {
				labels[l.GetName()] = l.GetValue()
			}

			switch family.GetType() {
			case dto.MetricType_COUNTER:
				samples = append(samples, Sample{Timestamp: ts, Name: name, Labels: labels, Value: m.GetCounter().GetValue(), Kind: KindCounter})
			case dto.MetricType_GAUGE:
				samples = append(samples, Sample{Timestamp: ts, Name: name, Labels: labels, Value: m.GetGauge().GetValue(), Kind: KindGauge})
			case dto.MetricType_UNTYPED:
				samples = append(samples, Sample{Timestamp: ts, Name: name, Labels: labels, Value: m.GetUntyped().GetValue(), Kind: KindGauge})
			case dto.MetricType_SUMMARY:
				// _sum and _count are both cumulative totals since the
				// process started (like a counter), not standalone levels —
				// same reasoning as the histogram case below.
				sm := m.GetSummary()
				samples = append(samples,
					Sample{Timestamp: ts, Name: name + "_sum", Labels: labels, Value: sm.GetSampleSum(), Kind: KindCounter},
					Sample{Timestamp: ts, Name: name + "_count", Labels: labels, Value: float64(sm.GetSampleCount()), Kind: KindCounter},
				)
			case dto.MetricType_HISTOGRAM:
				h := m.GetHistogram()
				samples = append(samples,
					Sample{Timestamp: ts, Name: name + "_sum", Labels: labels, Value: h.GetSampleSum(), Kind: KindCounter},
					Sample{Timestamp: ts, Name: name + "_count", Labels: labels, Value: float64(h.GetSampleCount()), Kind: KindCounter},
				)
			}
		}
	}

	return samples, nil
}
