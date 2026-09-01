package report

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestLineChartConfigEmpty(t *testing.T) {
	if _, ok := lineChartConfig("empty series", "", nil); ok {
		t.Fatal("expected ok=false for no points")
	}
}

func TestLineChartConfigSinglePoint(t *testing.T) {
	cfg, ok := lineChartConfig("single point", "", []chartPoint{{X: 0, Y: 42}})
	if !ok {
		t.Fatal("expected ok=true")
	}
	m, isMap := cfg.(map[string]any)
	if !isMap || m["type"] != "line" {
		t.Errorf("expected a line chart config, got %#v", cfg)
	}
}

func TestLineChartConfigConstantValue(t *testing.T) {
	cfg, ok := lineChartConfig("flat series", "", []chartPoint{{X: 0, Y: 5}, {X: 1, Y: 5}, {X: 2, Y: 5}})
	if !ok {
		t.Fatal("expected ok=true")
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshaling config: %v", err)
	}
	if strings.Count(string(data), `"y":5`) != 3 {
		t.Errorf("expected 3 points at y=5, got:\n%s", data)
	}
}

func TestLineChartConfigFiltersNonFinitePoints(t *testing.T) {
	points := []chartPoint{
		{X: 0, Y: 1},
		{X: 1, Y: math.NaN()},
		{X: 2, Y: math.Inf(1)},
		{X: 3, Y: math.Inf(-1)},
		{X: 4, Y: 2},
	}
	cfg, ok := lineChartConfig("mixed", "", points)
	if !ok {
		t.Fatal("expected ok=true, some points are finite")
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshaling config with non-finite points present: %v", err)
	}
	for _, bad := range []string{"NaN", "+Inf", "-Inf", "Inf"} {
		if strings.Contains(string(data), bad) {
			t.Errorf("expected non-finite values filtered out, found %q in:\n%s", bad, data)
		}
	}
}

func TestLineChartConfigAllNonFinite(t *testing.T) {
	points := []chartPoint{{X: 0, Y: math.NaN()}, {X: 1, Y: math.Inf(1)}}
	if _, ok := lineChartConfig("all bad", "", points); ok {
		t.Fatal("expected ok=false when no point is finite")
	}
}

func TestWriteChartScriptsEscapesForScriptContext(t *testing.T) {
	cfg, ok := lineChartConfig(`</script><script>alert(1)</script>`, "", []chartPoint{{X: 0, Y: 1}})
	if !ok {
		t.Fatal("expected ok=true")
	}

	var b strings.Builder
	writeChartScripts(&b, []chartEntry{{ID: "chart-0", Config: cfg}})
	out := b.String()

	if strings.Contains(out, "</script><script>alert(1)</script>") {
		t.Errorf("malicious title broke out of its script context unescaped:\n%s", out)
	}
	if !strings.Contains(out, "Chart.js v4.5.1") {
		t.Errorf("expected vendored Chart.js source to be inlined")
	}
	if !strings.Contains(out, "__MYRTILLE_CHARTS__") {
		t.Errorf("expected chart data to be assigned to __MYRTILLE_CHARTS__, got:\n%s", out)
	}
}

func TestMetricUnitInfersFromSuffix(t *testing.T) {
	cases := map[string]string{
		"inventory_request_duration_seconds_sum":   "seconds",
		"inventory_request_duration_seconds_count": "count",
		"inventory_errors_total":                   "count",
		"response_size_bytes":                      "bytes",
		"cache_hit_ratio":                          "ratio",
		"cpu_usage_percent":                        "percent",
		"inventory_queue_depth":                    "",
	}
	for name, want := range cases {
		if got := metricUnit(name); got != want {
			t.Errorf("metricUnit(%q) = %q, want %q", name, got, want)
		}
	}
}
