// Package dashboardconfig builds the JSON config k6's web-dashboard reads
// from XK6_DASHBOARD_CONFIG (see
// https://github.com/grafana/k6/tree/master/internal/dashboard's
// customize.go): the bundled default config — the same one k6 falls back to
// on its own, fetched at runtime from xk6-dashboard-assets rather than
// vendored, so it can't drift out of sync with whatever version is actually
// bundled — plus one extra "Service" tab with a chart panel per metric
// scraped from the service's own /metrics endpoint. Loading a custom config
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

// Build fetches url once and returns the k6 web-dashboard config (the
// bundled default plus a "Service" tab, one chart panel per distinct
// metric family found) as ready-to-write JSON.
//
// A failed fetch is returned as an error rather than degrading to the
// plain default config: the exact same scrape happens again for real
// inside the k6 script itself (see pkg/promscrape.Scraper), which throws
// and fails the run either way if url isn't reachable — surfacing that
// here first just gives a clearer message before k6 even starts, rather
// than silently producing a dashboard that then immediately fails to
// start k6 for an unrelated-looking reason.
func Build(ctx context.Context, url string) (json.RawMessage, error) {
	samples, err := discover(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("discovering metrics at %s: %w", url, err)
	}

	var cfg map[string]any
	if err := json.Unmarshal(dassets.Config(), &cfg); err != nil {
		return nil, fmt.Errorf("parsing default dashboard config: %w", err)
	}

	tabs, _ := cfg["tabs"].([]any)
	cfg["tabs"] = append(tabs, serviceTab(len(tabs), samples))

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
func serviceTab(tabIndex int, samples []metrics.Sample) map[string]any {
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

	return map[string]any{
		"id":       tabID,
		"title":    "Service",
		"summary":  "Metrics scraped from the service's own /metrics endpoint during the run.",
		"sections": sections,
	}
}
