package metrics

import (
	"strings"
	"testing"
	"time"
)

const samplePayload = `# HELP http_requests_total Total requests
# TYPE http_requests_total counter
http_requests_total{method="GET",status="200"} 100
http_requests_total{method="POST",status="200"} 50
# HELP memory_usage_bytes Current memory
# TYPE memory_usage_bytes gauge
memory_usage_bytes 1048576
# HELP request_duration_seconds Duration
# TYPE request_duration_seconds histogram
request_duration_seconds_bucket{le="0.1"} 10
request_duration_seconds_bucket{le="0.5"} 20
request_duration_seconds_bucket{le="+Inf"} 25
request_duration_seconds_sum 12.5
request_duration_seconds_count 25
`

func TestParseCounterGaugeAndHistogram(t *testing.T) {
	ts := time.Now()
	samples, err := Parse(strings.NewReader(samplePayload), ts)
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}

	byName := map[string][]Sample{}
	for _, s := range samples {
		byName[s.Name] = append(byName[s.Name], s)
	}

	if len(byName["http_requests_total"]) != 2 || byName["http_requests_total"][0].Kind != KindCounter {
		t.Fatalf("expected 2 counter http_requests_total samples, got %+v", byName["http_requests_total"])
	}
	if len(byName["memory_usage_bytes"]) != 1 || byName["memory_usage_bytes"][0].Value != 1048576 || byName["memory_usage_bytes"][0].Kind != KindGauge {
		t.Fatalf("unexpected memory_usage_bytes samples: %+v", byName["memory_usage_bytes"])
	}
	if len(byName["request_duration_seconds_sum"]) != 1 || byName["request_duration_seconds_sum"][0].Value != 12.5 || byName["request_duration_seconds_sum"][0].Kind != KindCounter {
		t.Fatalf("unexpected request_duration_seconds_sum samples: %+v", byName["request_duration_seconds_sum"])
	}
	if len(byName["request_duration_seconds_count"]) != 1 || byName["request_duration_seconds_count"][0].Value != 25 || byName["request_duration_seconds_count"][0].Kind != KindCounter {
		t.Fatalf("unexpected request_duration_seconds_count samples: %+v", byName["request_duration_seconds_count"])
	}
}
