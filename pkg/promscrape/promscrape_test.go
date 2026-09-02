package promscrape

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	promsample "github.com/thecoons/myrtille/internal/metrics"
)

func TestSeriesResolveGaugeAlwaysReportsRawValue(t *testing.T) {
	ser := &series{kind: promsample.KindGauge, last: map[string]float64{}}

	value, ok := ser.resolve(promsample.Sample{Value: 42})
	if !ok || value != 42 {
		t.Fatalf("expected (42, true), got (%v, %v)", value, ok)
	}

	// A second, different value still reports as-is — no delta tracking
	// for gauges.
	value, ok = ser.resolve(promsample.Sample{Value: 10})
	if !ok || value != 10 {
		t.Fatalf("expected (10, true), got (%v, %v)", value, ok)
	}
}

func TestSeriesResolveCounterFirstObservationReportsNothing(t *testing.T) {
	ser := &series{kind: promsample.KindCounter, last: map[string]float64{}}

	_, ok := ser.resolve(promsample.Sample{Value: 100})
	if ok {
		t.Fatal("expected no value on a counter's first observation")
	}

	if got := ser.last[""]; got != 100 {
		t.Fatalf("expected baseline 100 to be stored, got %v", got)
	}
}

func TestSeriesResolveCounterReportsDeltaSinceLastScrape(t *testing.T) {
	ser := &series{kind: promsample.KindCounter, last: map[string]float64{}}

	ser.resolve(promsample.Sample{Value: 100})

	delta, ok := ser.resolve(promsample.Sample{Value: 130})
	if !ok || delta != 30 {
		t.Fatalf("expected (30, true), got (%v, %v)", delta, ok)
	}

	delta, ok = ser.resolve(promsample.Sample{Value: 145})
	if !ok || delta != 15 {
		t.Fatalf("expected (15, true), got (%v, %v)", delta, ok)
	}
}

func TestSeriesResolveCounterResetTreatedAsNewBaseline(t *testing.T) {
	ser := &series{kind: promsample.KindCounter, last: map[string]float64{}}

	ser.resolve(promsample.Sample{Value: 500})

	// The target process restarted: its counter is back near zero.
	_, ok := ser.resolve(promsample.Sample{Value: 3})
	if ok {
		t.Fatal("expected no value when a counter decreases (process restart)")
	}
	if got := ser.last[""]; got != 3 {
		t.Fatalf("expected new baseline 3 to be stored, got %v", got)
	}

	// Subsequent scrapes resume reporting deltas against the new baseline.
	delta, ok := ser.resolve(promsample.Sample{Value: 8})
	if !ok || delta != 5 {
		t.Fatalf("expected (5, true), got (%v, %v)", delta, ok)
	}
}

func TestSeriesResolveCounterTracksEachLabelSetIndependently(t *testing.T) {
	ser := &series{kind: promsample.KindCounter, last: map[string]float64{}}

	ser.resolve(promsample.Sample{Labels: map[string]string{"status": "200"}, Value: 100})
	ser.resolve(promsample.Sample{Labels: map[string]string{"status": "500"}, Value: 5})

	delta200, ok := ser.resolve(promsample.Sample{Labels: map[string]string{"status": "200"}, Value: 110})
	if !ok || delta200 != 10 {
		t.Fatalf("expected status=200 delta (10, true), got (%v, %v)", delta200, ok)
	}

	// status=500 must not have been perturbed by status=200's scrapes.
	delta500, ok := ser.resolve(promsample.Sample{Labels: map[string]string{"status": "500"}, Value: 6})
	if !ok || delta500 != 1 {
		t.Fatalf("expected status=500 delta (1, true), got (%v, %v)", delta500, ok)
	}
}

func TestLabelKeyIsOrderIndependent(t *testing.T) {
	a := labelKey(map[string]string{"method": "GET", "status": "200"})
	b := labelKey(map[string]string{"status": "200", "method": "GET"})
	if a != b {
		t.Fatalf("expected identical keys regardless of map iteration order, got %q and %q", a, b)
	}

	c := labelKey(map[string]string{"method": "POST", "status": "200"})
	if a == c {
		t.Fatalf("expected different label sets to produce different keys, both were %q", a)
	}

	if got := labelKey(nil); got != "" {
		t.Fatalf("expected empty key for no labels, got %q", got)
	}
}

func TestFetchParsesPrometheusPayloadIntoSamples(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "# TYPE svc_requests_total counter\nsvc_requests_total 42\n"+
			"# TYPE svc_queue_depth gauge\nsvc_queue_depth 7\n")
	}))
	defer ts.Close()

	samples, err := fetch(ts.Client(), ts.URL)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	byName := map[string]promsample.Sample{}
	for _, s := range samples {
		byName[s.Name] = s
	}

	if s := byName["svc_requests_total"]; s.Value != 42 || s.Kind != promsample.KindCounter {
		t.Fatalf("unexpected svc_requests_total sample: %+v", s)
	}
	if s := byName["svc_queue_depth"]; s.Value != 7 || s.Kind != promsample.KindGauge {
		t.Fatalf("unexpected svc_queue_depth sample: %+v", s)
	}
}
