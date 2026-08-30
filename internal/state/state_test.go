package state

import (
	"encoding/json"
	"os"
	"sync"
	"testing"
)

func TestAppendAndCount(t *testing.T) {
	d := New()
	d.Append("user_ids", "u1")
	d.Append("user_ids", "u2")
	d.AppendMany("product_ids", []any{"p1", "p2", "p3"})

	if got := d.Count("user_ids"); got != 2 {
		t.Errorf("Count(user_ids) = %d, want 2", got)
	}
	if got := d.Count("product_ids"); got != 3 {
		t.Errorf("Count(product_ids) = %d, want 3", got)
	}
	if got := d.Count("missing"); got != 0 {
		t.Errorf("Count(missing) = %d, want 0", got)
	}
}

func TestKeysSorted(t *testing.T) {
	d := New()
	d.Append("zeta", 1)
	d.Append("alpha", 2)

	got := d.Keys()
	want := []string{"alpha", "zeta"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Keys() = %v, want %v", got, want)
	}
}

func TestSummary(t *testing.T) {
	d := New()
	d.Append("user_ids", "u1")
	d.Append("user_ids", "u2")

	summary := d.Summary()
	if summary["user_ids"] != 2 {
		t.Errorf("Summary()[user_ids] = %d, want 2", summary["user_ids"])
	}
}

func TestMarshalJSON(t *testing.T) {
	d := New()
	d.Append("user_ids", "u1")

	data, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}

	var decoded map[string][]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal returned error: %v", err)
	}
	if len(decoded["user_ids"]) != 1 || decoded["user_ids"][0] != "u1" {
		t.Errorf("decoded = %v, want user_ids: [u1]", decoded)
	}
}

func TestWriteTempFile(t *testing.T) {
	d := New()
	d.Append("user_ids", "u1")
	d.Append("user_ids", "u2")

	path, err := d.WriteTempFile()
	if err != nil {
		t.Fatalf("WriteTempFile returned error: %v", err)
	}
	defer os.Remove(path)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading temp file: %v", err)
	}

	var decoded map[string][]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal returned error: %v", err)
	}
	if len(decoded["user_ids"]) != 2 {
		t.Errorf("decoded user_ids = %v, want 2 entries", decoded["user_ids"])
	}
}

func TestConcurrentAppend(t *testing.T) {
	d := New()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d.Append("ids", i)
		}(i)
	}
	wg.Wait()

	if got := d.Count("ids"); got != 100 {
		t.Errorf("Count(ids) = %d, want 100", got)
	}
}
