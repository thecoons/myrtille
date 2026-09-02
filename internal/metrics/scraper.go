// Package metrics periodically scrapes a Prometheus-format /metrics
// endpoint on the target service while k6 is running, so the report phase
// can show how internal service metrics behaved under load.
package metrics

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

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

// Scraper periodically fetches a Prometheus-format /metrics endpoint and
// retains every sample observed.
type Scraper struct {
	url      string
	interval time.Duration
	client   *http.Client

	mu         sync.Mutex
	samples    []Sample
	scrapeErrs []error
}

// NewScraper returns a Scraper polling url every interval.
func NewScraper(url string, interval time.Duration) *Scraper {
	return &Scraper{
		url:      url,
		interval: interval,
		client:   &http.Client{Timeout: 10 * time.Second},
	}
}

// Run scrapes immediately, then every interval, until ctx is cancelled. It
// is meant to run in its own goroutine alongside the k6 subprocess. A
// failed scrape is recorded but does not stop the loop: a transient blip on
// /metrics shouldn't abort an otherwise healthy load test run.
func (s *Scraper) Run(ctx context.Context) {
	s.scrapeOnce(ctx)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.scrapeOnce(ctx)
		}
	}
}

func (s *Scraper) scrapeOnce(ctx context.Context) {
	samples, err := scrape(ctx, s.client, s.url)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.scrapeErrs = append(s.scrapeErrs, err)
		return
	}
	s.samples = append(s.samples, samples...)
}

// Samples returns every sample collected so far.
func (s *Scraper) Samples() []Sample {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Sample, len(s.samples))
	copy(out, s.samples)
	return out
}

// Errors returns every scrape error encountered so far.
func (s *Scraper) Errors() []error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]error, len(s.scrapeErrs))
	copy(out, s.scrapeErrs)
	return out
}

func scrape(ctx context.Context, client *http.Client, url string) ([]Sample, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("building metrics request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("scraping metrics: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("metrics endpoint returned status %d", resp.StatusCode)
	}

	return Parse(resp.Body, time.Now())
}

// Parse decodes a Prometheus text-exposition payload into samples, all
// stamped with ts. Histograms and summaries are reduced to their `_sum` and
// `_count` series rather than exposing individual buckets/quantiles, which
// is enough to see whether a metric moved during the run without the
// report needing to understand bucket layouts.
func Parse(r io.Reader, ts time.Time) ([]Sample, error) {
	parser := expfmt.NewTextParser(model.LegacyValidation)
	families, err := parser.TextToMetricFamilies(r)
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

// SeriesSummary aggregates every sample of one metric series (name + label
// set) observed during a run.
type SeriesSummary struct {
	Name   string
	Labels map[string]string
	Count  int
	Min    float64
	Max    float64
	Avg    float64

	sum float64
}

// Summarize groups samples by series (name + labels) and reduces each group
// to count/min/max/avg, in a stable order suitable for direct rendering in
// a report.
func Summarize(samples []Sample) []SeriesSummary {
	groups := make(map[string]*SeriesSummary)
	var order []string

	for _, s := range samples {
		key := seriesKey(s.Name, s.Labels)
		g, ok := groups[key]
		if !ok {
			g = &SeriesSummary{Name: s.Name, Labels: s.Labels, Min: s.Value, Max: s.Value}
			groups[key] = g
			order = append(order, key)
		}
		g.Count++
		g.sum += s.Value
		if s.Value < g.Min {
			g.Min = s.Value
		}
		if s.Value > g.Max {
			g.Max = s.Value
		}
	}

	sort.Strings(order)

	out := make([]SeriesSummary, 0, len(order))
	for _, key := range order {
		g := groups[key]
		g.Avg = g.sum / float64(g.Count)
		out = append(out, *g)
	}
	return out
}

// Series is one metric series (name + label set) with every raw sample
// observed for it, in scrape order, for callers that need the time
// evolution rather than a single aggregate (e.g. rendering a chart).
type Series struct {
	Name    string
	Labels  map[string]string
	Samples []Sample
}

// GroupSeries groups samples by series (name + labels), preserving every
// raw sample in scrape order, in the same stable series order as Summarize.
func GroupSeries(samples []Sample) []Series {
	groups := make(map[string]*Series)
	var order []string

	for _, s := range samples {
		key := seriesKey(s.Name, s.Labels)
		g, ok := groups[key]
		if !ok {
			g = &Series{Name: s.Name, Labels: s.Labels}
			groups[key] = g
			order = append(order, key)
		}
		g.Samples = append(g.Samples, s)
	}

	sort.Strings(order)

	out := make([]Series, 0, len(order))
	for _, key := range order {
		out = append(out, *groups[key])
	}
	return out
}

// String renders the series as "name{label="value",...}", matching how the
// series was grouped by Summarize.
func (s SeriesSummary) String() string {
	return seriesKey(s.Name, s.Labels)
}

func seriesKey(name string, labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString(name)
	if len(keys) > 0 {
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "%s=%q", k, labels[k])
		}
		b.WriteByte('}')
	}
	return b.String()
}
