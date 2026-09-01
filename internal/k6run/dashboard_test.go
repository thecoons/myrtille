package k6run

import (
	"testing"
	"time"
)

func TestParseDashboardRecordExtractsTrendAndCounter(t *testing.T) {
	record := `{"event":"metric","data":{"time":{"type":"gauge","contains":"time"}}}
{"event":"start","data":[[1000]]}
{"event":"metric","data":{"http_req_duration":{"type":"trend","contains":"time"},"http_reqs":{"type":"counter"}}}
{"event":"snapshot","data":[[329.27,339.44,330.63,314.12,338.5,338.97,339.34],[5,4.9958],[1000000]]}
{"event":"snapshot","data":[[300.0,310.0,305.0,290.0,308.0,309.0,309.5],[6,5.1],[1001000]]}
`

	series, err := parseDashboardRecord([]byte(record))
	if err != nil {
		t.Fatalf("parseDashboardRecord returned error: %v", err)
	}
	if len(series) != 2 {
		t.Fatalf("expected 2 series, got %d: %+v", len(series), series)
	}

	byName := make(map[string]DashboardSeries, len(series))
	for _, s := range series {
		byName[s.Name] = s
	}

	trend, ok := byName["http_req_duration"]
	if !ok {
		t.Fatal("expected http_req_duration series")
	}
	if trend.Aggregate != "avg" || trend.Unit != "milliseconds" {
		t.Errorf("unexpected trend series metadata: %+v", trend)
	}
	if len(trend.Points) != 2 || trend.Points[0].Value != 329.27 || trend.Points[1].Value != 300.0 {
		t.Errorf("unexpected trend points: %+v", trend.Points)
	}
	if !trend.Points[0].Time.Equal(time.UnixMilli(1000000)) {
		t.Errorf("unexpected trend timestamp: %v", trend.Points[0].Time)
	}

	counter, ok := byName["http_reqs"]
	if !ok {
		t.Fatal("expected http_reqs series")
	}
	if counter.Aggregate != "rate" || counter.Unit != "count" {
		t.Errorf("unexpected counter series metadata: %+v", counter)
	}
	if len(counter.Points) != 2 || counter.Points[0].Value != 4.9958 || counter.Points[1].Value != 5.1 {
		t.Errorf("unexpected counter points: %+v", counter.Points)
	}
}

func TestParseDashboardRecordSkipsSubmetricsAndTimePseudoMetric(t *testing.T) {
	record := `{"event":"metric","data":{"time":{"type":"gauge","contains":"time"}}}
{"event":"metric","data":{"http_req_duration":{"type":"trend","contains":"time"},"http_req_duration{group:api}":{"type":"trend","contains":"time"}}}
{"event":"snapshot","data":[[1,1,1,1,1,1,1],[2,2,2,2,2,2,2],[1000000]]}
`

	series, err := parseDashboardRecord([]byte(record))
	if err != nil {
		t.Fatalf("parseDashboardRecord returned error: %v", err)
	}
	if len(series) != 1 || series[0].Name != "http_req_duration" {
		t.Fatalf("expected only the base metric, got %+v", series)
	}
}

func TestParseDashboardRecordToleratesMalformedLines(t *testing.T) {
	record := `not json at all
{"event":"metric","data":{"time":{"type":"gauge","contains":"time"}}}
{"event":"metric","data":{"vus":{"type":"gauge"}}}

{"event":"snapshot","data":[[1000000],[5]]}
{"event":"snapshot","data":"unexpected shape"}
`
	series, err := parseDashboardRecord([]byte(record))
	if err != nil {
		t.Fatalf("parseDashboardRecord returned error: %v", err)
	}
	if len(series) != 1 || series[0].Name != "vus" || len(series[0].Points) != 1 {
		t.Fatalf("expected one tolerant vus series, got %+v", series)
	}
}

func TestParseDashboardRecordEmptyInput(t *testing.T) {
	series, err := parseDashboardRecord(nil)
	if err != nil {
		t.Fatalf("parseDashboardRecord returned error: %v", err)
	}
	if len(series) != 0 {
		t.Fatalf("expected no series, got %+v", series)
	}
}
