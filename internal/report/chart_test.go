package report

import "strings"

import "testing"

func assertValidSVG(t *testing.T, svg string) {
	t.Helper()
	if !strings.Contains(svg, "<svg") || !strings.Contains(svg, "</svg>") {
		t.Fatalf("output is not a well-formed svg fragment:\n%s", svg)
	}
	for _, bad := range []string{"NaN", "+Inf", "-Inf"} {
		if strings.Contains(svg, bad) {
			t.Errorf("output contains %q, want no non-finite values:\n%s", bad, svg)
		}
	}
}

func TestRenderLineChartEmpty(t *testing.T) {
	svg := renderLineChart("empty series", nil)
	assertValidSVG(t, svg)
	if !strings.Contains(svg, "no data") {
		t.Errorf("expected empty-state text, got:\n%s", svg)
	}
}

func TestRenderLineChartSinglePoint(t *testing.T) {
	svg := renderLineChart("single point", []chartPoint{{X: 0, Y: 42}})
	assertValidSVG(t, svg)
	if !strings.Contains(svg, "<circle") {
		t.Errorf("expected a single point rendered as a circle, got:\n%s", svg)
	}
}

func TestRenderLineChartConstantValue(t *testing.T) {
	svg := renderLineChart("flat series", []chartPoint{{X: 0, Y: 5}, {X: 1, Y: 5}, {X: 2, Y: 5}})
	assertValidSVG(t, svg)
	if !strings.Contains(svg, "<polyline") {
		t.Errorf("expected a polyline for multiple points, got:\n%s", svg)
	}
}

func TestRenderLineChartEscapesTitle(t *testing.T) {
	svg := renderLineChart(`<script>alert("x")</script>`, []chartPoint{{X: 0, Y: 1}})
	if strings.Contains(svg, "<script>") {
		t.Errorf("expected title to be escaped, got:\n%s", svg)
	}
}

func TestRenderBarChartEmpty(t *testing.T) {
	svg := renderBarChart("empty metric", nil)
	assertValidSVG(t, svg)
	if !strings.Contains(svg, "no data") {
		t.Errorf("expected empty-state text, got:\n%s", svg)
	}
}

func TestRenderBarChartValues(t *testing.T) {
	svg := renderBarChart("http_req_duration", map[string]float64{"avg": 12.3, "max": 45.6, "min": 0})
	assertValidSVG(t, svg)
	for _, want := range []string{"avg", "max", "min", "<rect"} {
		if !strings.Contains(svg, want) {
			t.Errorf("expected bar chart to contain %q, got:\n%s", want, svg)
		}
	}
}

func TestRenderBarChartAllZero(t *testing.T) {
	svg := renderBarChart("all zero", map[string]float64{"a": 0, "b": 0})
	assertValidSVG(t, svg)
}
