// Package dashboardconfig builds the JSON config k6's web-dashboard reads
// from XK6_DASHBOARD_CONFIG (see
// https://github.com/grafana/k6/tree/master/internal/dashboard's
// customize.go): the bundled default config — the same one k6 falls back to
// on its own, fetched at runtime from xk6-dashboard-assets rather than
// vendored, so it can't drift out of sync with whatever version is actually
// bundled — plus one extra "Service" tab with a chart panel per metric
// scraped from the service's own /metrics endpoint, and (when
// service.traces.enabled) an aggregate panel each for the OTel spans it
// receives — see docs/plans/otel-span-metrics.md. Loading a custom config
// entirely REPLACES k6's default rather than merging with it (see that
// customize.go), which is why the default has to be reproduced here instead
// of just appending a tab of our own — see
// docs/plans/xk6-live-dashboard.md, step 5.
package dashboardconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	dassets "github.com/grafana/xk6-dashboard-assets"

	"github.com/thecoons/myrtille/internal/metrics"
)

// MetricPrefix is prepended to every Prometheus series name in a panel
// query, so it matches the k6 metric name the samples actually land under —
// kept in sync by hand with pkg/promscrape's own copy of this constant; see
// that package's doc comment on metricPrefix for why it's not shared via
// import.
const MetricPrefix = "svc_"

// Build returns the k6 web-dashboard config (the bundled default plus a
// "Service" tab) as ready-to-write JSON. metricsURL, when non-empty, is
// scraped once to add one chart panel per distinct metric family found
// there (see serviceTab). tracesEnabled, when true, adds a fixed "Spans"
// section with two panels — svc_span_duration/svc_span_errors, aggregated
// across every span regardless of name — see serviceTab's doc comment for
// why this can't be broken down by span name the way the metrics panels
// are broken down by family.
//
// A failed metricsURL fetch is returned as an error rather than degrading
// to the plain default config: the exact same scrape happens again for
// real inside the k6 script itself (see pkg/promscrape.Scraper), which
// throws and fails the run either way if url isn't reachable — surfacing
// that here first just gives a clearer message before k6 even starts,
// rather than silently producing a dashboard that then immediately fails
// to start k6 for an unrelated-looking reason.
func Build(ctx context.Context, metricsURL string, tracesEnabled bool) (json.RawMessage, error) {
	var samples []metrics.Sample
	if metricsURL != "" {
		var err error
		samples, err = discover(ctx, metricsURL)
		if err != nil {
			return nil, fmt.Errorf("discovering metrics at %s: %w", metricsURL, err)
		}
	}

	var cfg map[string]any
	if err := json.Unmarshal(dassets.Config(), &cfg); err != nil {
		return nil, fmt.Errorf("parsing default dashboard config: %w", err)
	}

	tabs, _ := cfg["tabs"].([]any)
	cfg["tabs"] = append(tabs, serviceTab(len(tabs), samples, tracesEnabled))

	out, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshaling dashboard config: %w", err)
	}
	return out, nil
}

func discover(ctx context.Context, url string) ([]metrics.Sample, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return metrics.Parse(resp.Body, time.Now())
}

// serviceTab builds one dashboard tab with a chart panel per distinct
// metric family in samples, deduplicated in first-seen order — the same
// one-k6-metric-per-family rule pkg/promscrape itself applies when
// registering metrics, so every panel here has exactly one series to plot.
// Panels are grouped into one section per name prefix (the part of the
// metric name before its first "_", e.g. "jvm" for "jvm_gc_pause_seconds"),
// sections sorted alphabetically by that key for a stable dashboard layout
// across runs (Prometheus scrape order isn't guaranteed) — mirrors how the
// default config's own "Timings" tab splits HTTP/Browser/WebSocket/gRPC into
// separate titled sections. tabIndex is this tab's position among the
// default config's own tabs (computed from their count rather than
// hardcoded, so this doesn't assume a specific number of built-in tabs),
// used only to build id strings matching the shape the default config's own
// tabs use.
//
// tracesEnabled adds one more, fixed "Spans" section — unlike the
// samples-derived sections above, this one can't be broken down by span
// name: span names aren't known ahead of the run (nothing to discover the
// way metricsURL can be pre-scraped), and even a tag known ahead of time
// wouldn't help — confirmed against a real run (a panel querying
// `svc_span_duration{span_name:x}` renders no data at all: k6's live
// SSE metric-registration stream only ever tracks the base metric and its
// automatic `{group:...}` breakdown, never an arbitrary custom-tag
// submetric, even one referenced by a threshold — that engine only
// resolves against the final accumulated samples at the very end of the
// run, a completely different pipeline from what feeds the live
// dashboard). So both panels here are deliberately aggregate-only,
// exactly the same `[?!tags && ...]` shape every other panel in the
// default config already uses.
func serviceTab(tabIndex int, samples []metrics.Sample, tracesEnabled bool) map[string]any {
	tabID := fmt.Sprintf("tab-%d", tabIndex)

	seen := make(map[string]bool, len(samples))
	groups := make(map[string][]metrics.Sample)
	var groupOrder []string

	for _, s := range samples {
		if seen[s.Name] {
			continue
		}
		seen[s.Name] = true

		key := s.Name
		if i := strings.IndexByte(key, '_'); i >= 0 {
			key = key[:i]
		}
		if _, ok := groups[key]; !ok {
			groupOrder = append(groupOrder, key)
		}
		groups[key] = append(groups[key], s)
	}

	sort.Strings(groupOrder)

	sections := make([]any, 0, len(groupOrder))
	for i, key := range groupOrder {
		sectionID := fmt.Sprintf("%s.section-%d", tabID, i)
		group := groups[key]
		// metrics.Parse ranges over a map internally, so sample order (and
		// thus "first-seen" order here) isn't stable across scrapes of the
		// same target — sort by name for a dashboard layout that doesn't
		// reshuffle panels between runs.
		sort.Slice(group, func(a, b int) bool { return group[a].Name < group[b].Name })
		panels := make([]any, 0, len(group))

		for _, s := range group {
			aggregate := "value"
			if s.Kind == metrics.KindCounter {
				aggregate = "rate"
			}

			panels = append(panels, map[string]any{
				"id":    fmt.Sprintf("%s.panel-%d", sectionID, len(panels)),
				"title": s.Name,
				"kind":  "chart",
				"series": []any{
					map[string]any{
						"query": fmt.Sprintf("%s%s[?!tags && %s]", MetricPrefix, s.Name, aggregate),
					},
				},
			})
		}

		sections = append(sections, map[string]any{
			"id":     sectionID,
			"title":  key,
			"panels": panels,
		})
	}

	if tracesEnabled {
		sectionID := fmt.Sprintf("%s.section-%d", tabID, len(sections))
		sections = append(sections, map[string]any{
			"id":    sectionID,
			"title": "Spans",
			"panels": []any{
				map[string]any{
					"id":    sectionID + ".panel-0",
					"title": "svc_span_duration",
					"kind":  "chart",
					"series": []any{
						map[string]any{"query": "svc_span_duration[?!tags && avg]"},
					},
				},
				map[string]any{
					"id":    sectionID + ".panel-1",
					"title": "svc_span_errors",
					"kind":  "chart",
					"series": []any{
						map[string]any{"query": "svc_span_errors[?!tags && rate]"},
					},
				},
			},
		})
	}

	return map[string]any{
		"id":       tabID,
		"title":    "Service",
		"summary":  serviceTabSummary(len(samples) > 0, tracesEnabled),
		"sections": sections,
	}
}

// serviceTabSummary describes whichever of the two triggers actually
// produced sections — a summary hardcoded to only mention scraped metrics
// would be wrong for a traces-only tab, and vice versa.
func serviceTabSummary(hasMetrics, hasTraces bool) string {
	var parts []string
	if hasMetrics {
		parts = append(parts, "metrics scraped from the service's own /metrics endpoint")
	}
	if hasTraces {
		parts = append(parts, "OTel spans received during the run")
	}
	if len(parts) == 0 {
		return "Service telemetry during the run."
	}
	return "Shows " + strings.Join(parts, " and ") + "."
}
