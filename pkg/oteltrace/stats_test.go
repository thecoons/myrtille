package oteltrace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSpanStatsRecordAggregatesByName(t *testing.T) {
	s := newSpanStats()

	s.record(spanSample{name: "check_inventory", durationMs: 60, isError: false})
	s.record(spanSample{name: "check_inventory", durationMs: 20, isError: false})
	s.record(spanSample{name: "insert_order", durationMs: 5, isError: false})

	got := s.snapshot()
	if len(got) != 2 {
		t.Fatalf("expected 2 distinct span names, got %d: %+v", len(got), got)
	}

	byName := make(map[string]SpanStat, len(got))
	for _, stat := range got {
		byName[stat.Name] = stat
	}

	ci := byName["check_inventory"]
	if ci.Count != 2 {
		t.Errorf("expected check_inventory count=2, got %d", ci.Count)
	}
	if ci.AvgMs != 40 {
		t.Errorf("expected check_inventory avg=40, got %v", ci.AvgMs)
	}
	if ci.MinMs != 20 || ci.MaxMs != 60 {
		t.Errorf("expected min=20 max=60, got min=%v max=%v", ci.MinMs, ci.MaxMs)
	}

	io := byName["insert_order"]
	if io.Count != 1 || io.AvgMs != 5 {
		t.Errorf("expected insert_order count=1 avg=5, got %+v", io)
	}
}

func TestSpanStatsErrorRate(t *testing.T) {
	s := newSpanStats()

	for i := 0; i < 7; i++ {
		s.record(spanSample{name: "place_order", durationMs: 1, isError: i == 0})
	}

	got := s.snapshot()
	if len(got) != 1 {
		t.Fatalf("expected 1 span name, got %d", len(got))
	}
	if got[0].Count != 7 {
		t.Errorf("expected count=7, got %d", got[0].Count)
	}
	want := 1.0 / 7.0
	if diff := got[0].ErrorRate - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("expected error rate ~%v, got %v", want, got[0].ErrorRate)
	}
}

func TestSpanStatsSnapshotSortedByName(t *testing.T) {
	s := newSpanStats()
	s.record(spanSample{name: "zeta", durationMs: 1})
	s.record(spanSample{name: "alpha", durationMs: 1})
	s.record(spanSample{name: "mid", durationMs: 1})

	got := s.snapshot()
	want := []string{"alpha", "mid", "zeta"}
	for i, w := range want {
		if got[i].Name != w {
			t.Fatalf("expected sorted order %v, got %v", want, got)
		}
	}
}

func TestSpanStatsWriteFileProducesValidJSON(t *testing.T) {
	s := newSpanStats()
	s.record(spanSample{name: "place_order", durationMs: 10, isError: true})

	path := filepath.Join(t.TempDir(), "stats.json")
	s.writeFile(path)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading stats file: %v", err)
	}

	var got []SpanStat
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("stats file is not valid JSON: %v\n%s", err, data)
	}
	if len(got) != 1 || got[0].Name != "place_order" || got[0].Count != 1 {
		t.Fatalf("unexpected decoded stats: %+v", got)
	}
}

func TestSpanStatsWriteFileOverwritesPreviousContent(t *testing.T) {
	s := newSpanStats()
	path := filepath.Join(t.TempDir(), "stats.json")

	s.record(spanSample{name: "a", durationMs: 1})
	s.writeFile(path)

	s.record(spanSample{name: "b", durationMs: 1})
	s.writeFile(path)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading stats file: %v", err)
	}
	var got []SpanStat
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("stats file is not valid JSON: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected the second write to reflect both accumulated spans, got %+v", got)
	}
}
