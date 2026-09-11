package oteltrace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestFailedTracesLinkThenMatchRattachesSpan(t *testing.T) {
	ft := newFailedTraces()
	ft.link("abc123", "place_order")

	ft.match(spanSample{name: "place_order", otelSvc: "orders-api", durationMs: 12.5, isError: false, traceID: "abc123"})
	ft.match(spanSample{name: "check_inventory", durationMs: 3, isError: true, traceID: "abc123"})

	snap := ft.snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 failed trace, got %d", len(snap))
	}
	got := snap[0]
	if got.TraceID != "abc123" || got.Label != "place_order" {
		t.Errorf("unexpected entry: %+v", got)
	}
	if len(got.Spans) != 2 {
		t.Fatalf("expected 2 rattached spans, got %d: %+v", len(got.Spans), got.Spans)
	}
	if got.Spans[0].Name != "place_order" || got.Spans[0].OtelSvc != "orders-api" || got.Spans[0].DurationMs != 12.5 || got.Spans[0].IsError {
		t.Errorf("unexpected first span: %+v", got.Spans[0])
	}
	if got.Spans[1].Name != "check_inventory" || !got.Spans[1].IsError {
		t.Errorf("unexpected second span: %+v", got.Spans[1])
	}
}

func TestFailedTracesMatchWithoutLinkIsNoop(t *testing.T) {
	ft := newFailedTraces()
	ft.match(spanSample{name: "orphan", durationMs: 1, traceID: "never-linked"})

	if snap := ft.snapshot(); len(snap) != 0 {
		t.Errorf("expected no entries for an unlinked trace-id, got %+v", snap)
	}
}

func TestFailedTracesEmptyTraceIDIsNeverAValidKey(t *testing.T) {
	ft := newFailedTraces()
	ft.link("", "should-never-register") // must be a no-op: "" is never a valid key
	ft.match(spanSample{name: "no_trace_id", durationMs: 1, traceID: ""})

	if snap := ft.snapshot(); len(snap) != 0 {
		t.Errorf("expected an empty trace-id to never register or match, got %+v", snap)
	}
}

func TestFailedTracesLinkIsIdempotentPerTraceID(t *testing.T) {
	ft := newFailedTraces()
	ft.link("abc123", "first_label")
	ft.match(spanSample{name: "s1", traceID: "abc123"})
	ft.link("abc123", "second_label") // must not reset the entry or its spans

	snap := ft.snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(snap))
	}
	if snap[0].Label != "first_label" {
		t.Errorf("expected the first link's label to win, got %q", snap[0].Label)
	}
	if len(snap[0].Spans) != 1 {
		t.Errorf("expected the already-matched span to survive the second link, got %+v", snap[0].Spans)
	}
}

func TestFailedTracesEvictsOldestBeyondMax(t *testing.T) {
	ft := newFailedTraces()
	for i := 0; i < maxFailedTraces+10; i++ {
		ft.link(traceIDForTest(i), "label")
	}

	snap := ft.snapshot()
	if len(snap) != maxFailedTraces {
		t.Fatalf("expected the map to stay bounded at %d entries, got %d", maxFailedTraces, len(snap))
	}

	// The first 10 links (oldest) must have been evicted; the most recent
	// maxFailedTraces must all still be present.
	present := make(map[string]bool, len(snap))
	for _, e := range snap {
		present[e.TraceID] = true
	}
	if present[traceIDForTest(0)] {
		t.Errorf("expected the oldest entry to have been evicted, but it's still present")
	}
	if !present[traceIDForTest(maxFailedTraces+9)] {
		t.Errorf("expected the most recently linked entry to still be present")
	}
}

// traceIDForTest returns a distinct, deterministic fake trace-id for i —
// doesn't need to be valid hex/length, only unique, since these tests never
// go through parseTraceID.
func traceIDForTest(i int) string {
	return "trace-" + string(rune('a'+i%26)) + string(rune('0'+i/26%10))
}

// TestGetSharedFailedTracesReturnsSameInstanceAcrossCalls covers the
// cross-VU fix found during tranche 11's real-run validation (see
// getSharedFailedTraces's doc comment): every call must return the exact
// same *failedTraces, regardless of how many times it's called — mirroring
// how a fresh "receiver" Go struct is constructed per k6 VU, yet all of
// them must reach the one true failed-traces store.
func TestGetSharedFailedTracesReturnsSameInstanceAcrossCalls(t *testing.T) {
	a := getSharedFailedTraces()
	b := getSharedFailedTraces()
	if a != b {
		t.Error("expected getSharedFailedTraces() to return the same instance across calls")
	}
}

func TestFailedTracesWriteFileProducesValidJSON(t *testing.T) {
	ft := newFailedTraces()
	ft.link("abc123", "place_order")
	ft.match(spanSample{name: "place_order", durationMs: 12, traceID: "abc123"})

	path := filepath.Join(t.TempDir(), "failed-traces.json")
	ft.writeFile(path)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading failed-traces file: %v", err)
	}

	var got []FailedTrace
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("failed-traces file is not valid JSON: %v\n%s", err, data)
	}
	if len(got) != 1 || got[0].TraceID != "abc123" || got[0].Label != "place_order" || len(got[0].Spans) != 1 {
		t.Fatalf("unexpected decoded failed traces: %+v", got)
	}
}

func TestFailedTracesWriteFileOverwritesPreviousContent(t *testing.T) {
	ft := newFailedTraces()
	path := filepath.Join(t.TempDir(), "failed-traces.json")

	ft.link("aaa", "step_a")
	ft.writeFile(path)

	ft.link("bbb", "step_b")
	ft.writeFile(path)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading failed-traces file: %v", err)
	}
	var got []FailedTrace
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("failed-traces file is not valid JSON: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected the second write to reflect both accumulated links, got %+v", got)
	}
}

func TestReceiverLinkFailureRejectsMalformedTraceparent(t *testing.T) {
	r := &receiver{failed: newFailedTraces()}
	if err := r.linkFailure("not-a-traceparent", "some_step"); err == nil {
		t.Error("expected an error for a malformed traceparent, got nil")
	}
	if snap := r.failed.snapshot(); len(snap) != 0 {
		t.Errorf("expected no entry to be registered for a rejected traceparent, got %+v", snap)
	}
}

func TestReceiverLinkFailureRegistersValidTraceparent(t *testing.T) {
	r := &receiver{failed: newFailedTraces()}
	tp := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	if err := r.linkFailure(tp, "place_order"); err != nil {
		t.Fatalf("linkFailure: %v", err)
	}

	snap := r.failed.snapshot()
	if len(snap) != 1 || snap[0].TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" || snap[0].Label != "place_order" {
		t.Errorf("unexpected snapshot: %+v", snap)
	}
}
