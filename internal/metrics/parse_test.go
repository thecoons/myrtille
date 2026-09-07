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

// exemplarPayload reproduces a real line emitted by Micrometer
// (quarkus-micrometer-registry-prometheus) when a sampled OpenTelemetry
// trace/span is attached to a counter at scrape time: a trailing `# {...}`
// exemplar annotation on an otherwise-plain Prometheus text line.
const exemplarPayload = `# HELP http_server_requests_seconds_count Duration
# TYPE http_server_requests_seconds_count counter
http_server_requests_seconds_count{method="GET",outcome="SUCCESS",status="200",uri="/api/v1/versions/{version}/graph/init/status"} 1.0 # {span_id="faed75a89c2f02d6",trace_id="cbbe1372fd2fc006491fa546e09f5320"} 1.0 1788789290.632
http_server_requests_seconds_count{method="GET",outcome="SUCCESS",status="200",uri="/api/v1/other"} 3.0
`

func TestParseStripsExemplars(t *testing.T) {
	ts := time.Now()
	samples, err := Parse(strings.NewReader(exemplarPayload), ts)
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}

	byName := map[string][]Sample{}
	for _, s := range samples {
		byName[s.Name] = append(byName[s.Name], s)
	}

	got := byName["http_server_requests_seconds_count"]
	if len(got) != 2 {
		t.Fatalf("expected 2 http_server_requests_seconds_count samples, got %+v", got)
	}
	var sawExemplarLine, sawPlainLine bool
	for _, s := range got {
		switch s.Value {
		case 1.0:
			sawExemplarLine = true
		case 3.0:
			sawPlainLine = true
		}
		if s.Kind != KindCounter {
			t.Fatalf("expected counter kind, got %+v", s)
		}
	}
	if !sawExemplarLine || !sawPlainLine {
		t.Fatalf("missing expected samples: %+v", got)
	}
}
