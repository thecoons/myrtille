package oteltrace

import (
	"encoding/json"
	"os"
	"sort"
	"sync"
	"time"

	"go.k6.io/k6/v2/metrics"
)

// spanStatsFileEnv, when set, is the path this extension periodically
// writes its per-span-name aggregation to, as JSON — read back by
// internal/k6run after the k6 process exits (see
// docs/plans/otel-span-metrics.md's "Extension" section). Absent (e.g.
// someone using k6/x/oteltrace outside myrtille): the aggregation still
// happens, it's just never written anywhere. Named MYRTILLE_SPAN_STATS_FILE
// on the myrtille side too — not shared via import, these are separate Go
// modules (same reasoning as pkg/promscrape's metricPrefix/
// dashboardconfig.MetricPrefix duplication).
const spanStatsFileEnv = "MYRTILLE_SPAN_STATS_FILE"

// spanStatsWriteInterval is how often the stats file is rewritten. Not a
// clean end-of-run flush — there isn't one available (see the package
// doc's note on promscrape's own equivalent constraint) — periodic
// overwriting sidesteps needing one entirely. A run shorter than this
// never gets a single write; accepted trade-off for a post-run diagnostic
// table, documented in the plan.
const spanStatsWriteInterval = time.Second

// SpanStat is one row of the per-span-name breakdown written to
// spanStatsFileEnv — the JSON wire shape internal/k6run decodes back into
// a report table. Durations are milliseconds, matching svc_span_duration.
type SpanStat struct {
	Name      string  `json:"name"`
	Count     int     `json:"count"`
	AvgMs     float64 `json:"avg_ms"`
	MinMs     float64 `json:"min_ms"`
	MaxMs     float64 `json:"max_ms"`
	P90Ms     float64 `json:"p90_ms"`
	P95Ms     float64 `json:"p95_ms"`
	ErrorRate float64 `json:"error_rate"`
}

// spanAgg accumulates one span name's samples. duration/errors reuse k6's
// own Trend/Rate sinks (go.k6.io/k6/v2/metrics) rather than a hand-rolled
// percentile algorithm — Format(0) already returns exactly
// min/max/avg/med/p(90)/p(95) for a Trend and rate for a Rate, the same
// computation k6 itself uses for svc_span_duration's own aggregate, so
// the per-span numbers stay consistent with what the CLI summary/report
// already shows for the whole metric. count is tracked separately rather
// than read off RateSink.Total (which happens to equal it today, since
// exactly one error-sample is added per span) — decouples this from an
// internal-sink detail that isn't part of the Sink interface's contract.
type spanAgg struct {
	duration metrics.Sink
	errors   metrics.Sink
	count    int
}

// spanStats is the receiver's per-span-name aggregation, protected by mu
// since HTTP requests are handled concurrently (unlike promscrape's single
// polling goroutine — see the package doc).
type spanStats struct {
	mu   sync.Mutex
	aggs map[string]*spanAgg
}

func newSpanStats() *spanStats {
	return &spanStats{aggs: make(map[string]*spanAgg)}
}

func (s *spanStats) record(span spanSample) {
	s.mu.Lock()
	defer s.mu.Unlock()

	agg, ok := s.aggs[span.name]
	if !ok {
		agg = &spanAgg{duration: metrics.NewSink(metrics.Trend), errors: metrics.NewSink(metrics.Rate)}
		s.aggs[span.name] = agg
	}

	agg.duration.Add(metrics.Sample{Value: span.durationMs})

	errValue := 0.0
	if span.isError {
		errValue = 1.0
	}
	agg.errors.Add(metrics.Sample{Value: errValue})

	agg.count++
}

// snapshot returns the current aggregation as SpanStats, sorted by name
// for a deterministic file (the avg-descending sort a report actually
// wants is internal/k6run's job, once it's decoding this back — see the
// plan doc).
func (s *spanStats) snapshot() []SpanStat {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]SpanStat, 0, len(s.aggs))
	for name, agg := range s.aggs {
		durFmt := agg.duration.Format(0)
		errFmt := agg.errors.Format(0)
		out = append(out, SpanStat{
			Name:      name,
			Count:     agg.count,
			AvgMs:     durFmt["avg"],
			MinMs:     durFmt["min"],
			MaxMs:     durFmt["max"],
			P90Ms:     durFmt["p(90)"],
			P95Ms:     durFmt["p(95)"],
			ErrorRate: errFmt["rate"],
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// writeFile marshals the current snapshot and writes it to path,
// best-effort — a marshal or write failure here has nothing sensible to
// do about it (this runs from a background ticker, nothing is watching
// for a returned error), so it's silently skipped rather than surfaced.
func (s *spanStats) writeFile(path string) {
	data, err := json.Marshal(s.snapshot())
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o644)
}

// startWriteLoop periodically overwrites path with the current snapshot,
// forever — like promscrape's own polling goroutine, this never stops on
// its own (no clean end-of-run hook available to an extension, see the
// package doc); it dies with the k6 process itself.
func (s *spanStats) startWriteLoop(path string) {
	ticker := time.NewTicker(spanStatsWriteInterval)
	defer ticker.Stop()

	for range ticker.C {
		s.writeFile(path)
	}
}
