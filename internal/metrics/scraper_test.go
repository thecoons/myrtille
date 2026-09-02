package metrics

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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

func TestSummarizeGroupsBySeriesAndComputesStats(t *testing.T) {
	base := time.Now()
	samples := []Sample{
		{Timestamp: base, Name: "memory_usage_bytes", Value: 100},
		{Timestamp: base.Add(time.Second), Name: "memory_usage_bytes", Value: 200},
		{Timestamp: base.Add(2 * time.Second), Name: "memory_usage_bytes", Value: 300},
		{Timestamp: base, Name: "http_requests_total", Labels: map[string]string{"method": "GET"}, Value: 10},
		{Timestamp: base.Add(time.Second), Name: "http_requests_total", Labels: map[string]string{"method": "GET"}, Value: 20},
		{Timestamp: base, Name: "http_requests_total", Labels: map[string]string{"method": "POST"}, Value: 5},
	}

	summaries := Summarize(samples)
	if len(summaries) != 3 {
		t.Fatalf("expected 3 series summaries, got %d: %+v", len(summaries), summaries)
	}

	var memSummary *SeriesSummary
	for i := range summaries {
		if summaries[i].Name == "memory_usage_bytes" {
			memSummary = &summaries[i]
		}
	}
	if memSummary == nil {
		t.Fatal("expected memory_usage_bytes summary")
	}
	if memSummary.Count != 3 || memSummary.Min != 100 || memSummary.Max != 300 || memSummary.Avg != 200 {
		t.Fatalf("unexpected memory_usage_bytes summary: %+v", memSummary)
	}
}

func TestGroupSeriesGroupsByNameAndLabels(t *testing.T) {
	base := time.Now()
	samples := []Sample{
		{Timestamp: base, Name: "memory_usage_bytes", Value: 100},
		{Timestamp: base.Add(time.Second), Name: "memory_usage_bytes", Value: 200},
		{Timestamp: base, Name: "http_requests_total", Labels: map[string]string{"method": "GET"}, Value: 10},
		{Timestamp: base.Add(time.Second), Name: "http_requests_total", Labels: map[string]string{"method": "GET"}, Value: 20},
		{Timestamp: base, Name: "http_requests_total", Labels: map[string]string{"method": "POST"}, Value: 5},
	}

	series := GroupSeries(samples)
	if len(series) != 3 {
		t.Fatalf("expected 3 series, got %d: %+v", len(series), series)
	}

	var mem *Series
	for i := range series {
		if series[i].Name == "memory_usage_bytes" {
			mem = &series[i]
		}
	}
	if mem == nil {
		t.Fatal("expected memory_usage_bytes series")
	}
	if len(mem.Samples) != 2 || mem.Samples[0].Value != 100 || mem.Samples[1].Value != 200 {
		t.Fatalf("unexpected memory_usage_bytes samples: %+v", mem.Samples)
	}
}

func TestScraperRunCollectsMultipleSamplesUntilCancelled(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, samplePayload)
	}))
	defer ts.Close()

	scraper := NewScraper(ts.URL, 20*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Millisecond)
	defer cancel()

	scraper.Run(ctx)

	samples := scraper.Samples()
	if len(samples) == 0 {
		t.Fatal("expected at least one sample to be collected")
	}
	if errs := scraper.Errors(); len(errs) != 0 {
		t.Fatalf("expected no scrape errors, got %v", errs)
	}
}

func TestScraperRunRecordsErrorsOnFailedScrape(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	scraper := NewScraper(ts.URL, 10*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	scraper.Run(ctx)

	if errs := scraper.Errors(); len(errs) == 0 {
		t.Fatal("expected scrape errors to be recorded")
	}
	if samples := scraper.Samples(); len(samples) != 0 {
		t.Fatalf("expected no samples on failed scrapes, got %d", len(samples))
	}
}
