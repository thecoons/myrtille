package dashboardconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	dassets "github.com/grafana/xk6-dashboard-assets"
)

func TestBuildAppendsServiceTabToDefaultConfig(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "# TYPE svc_requests_total counter\nsvc_requests_total 42\n"+
			"# TYPE svc_queue_depth gauge\nsvc_queue_depth 7\n")
	}))
	defer ts.Close()

	out, err := Build(context.Background(), ts.URL)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("Build produced invalid JSON: %v", err)
	}

	var want map[string]any
	if err := json.Unmarshal(dassets.Config(), &want); err != nil {
		t.Fatalf("parsing default config: %v", err)
	}
	wantTabs, _ := want["tabs"].([]any)

	gotTabs, _ := got["tabs"].([]any)
	if len(gotTabs) != len(wantTabs)+1 {
		t.Fatalf("expected %d tabs (default %d + 1 Service), got %d", len(wantTabs)+1, len(wantTabs), len(gotTabs))
	}

	// The default tabs must be untouched, in the same order.
	for i, wantTab := range wantTabs {
		wantJSON, _ := json.Marshal(wantTab)
		gotJSON, _ := json.Marshal(gotTabs[i])
		if string(wantJSON) != string(gotJSON) {
			t.Fatalf("default tab %d was modified:\nwant %s\ngot  %s", i, wantJSON, gotJSON)
		}
	}

	serviceTab, ok := gotTabs[len(gotTabs)-1].(map[string]any)
	if !ok || serviceTab["title"] != "Service" {
		t.Fatalf("expected the last tab to be titled Service, got %+v", serviceTab)
	}

	// Both metrics share the "svc" prefix (first segment before "_"), so
	// they land in a single group/section here.
	sections, _ := serviceTab["sections"].([]any)
	if len(sections) != 1 {
		t.Fatalf("expected exactly one section (both metrics share the same name prefix), got %d", len(sections))
	}
	section, _ := sections[0].(map[string]any)
	if section["title"] != "svc" {
		t.Fatalf("expected the section to be titled by the shared name prefix %q, got %+v", "svc", section["title"])
	}
	panels, _ := section["panels"].([]any)
	if len(panels) != 2 {
		t.Fatalf("expected 2 panels (one per distinct metric), got %d: %+v", len(panels), panels)
	}

	queries := make(map[string]string, len(panels))
	for _, p := range panels {
		panel, _ := p.(map[string]any)
		series, _ := panel["series"].([]any)
		if len(series) != 1 {
			t.Fatalf("expected exactly one series per panel, got %+v", panel)
		}
		s, _ := series[0].(map[string]any)
		queries[panel["title"].(string)] = s["query"].(string)
	}

	if q := queries["svc_requests_total"]; q != "svc_svc_requests_total[?!tags && rate]" {
		t.Errorf("unexpected counter query: %q", q)
	}
	if q := queries["svc_queue_depth"]; q != "svc_svc_queue_depth[?!tags && value]" {
		t.Errorf("unexpected gauge query: %q", q)
	}
}

func TestBuildDedupesRepeatedLabelSetsIntoOnePanel(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "# TYPE http_status_total counter\n"+
			`http_status_total{code="200"} 10`+"\n"+
			`http_status_total{code="500"} 1`+"\n")
	}))
	defer ts.Close()

	out, err := Build(context.Background(), ts.URL)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	var got map[string]any
	json.Unmarshal(out, &got) //nolint:errcheck
	tabs, _ := got["tabs"].([]any)
	serviceTab, _ := tabs[len(tabs)-1].(map[string]any)
	sections, _ := serviceTab["sections"].([]any)
	section, _ := sections[0].(map[string]any)
	panels, _ := section["panels"].([]any)

	if len(panels) != 1 {
		t.Fatalf("expected the two label-sets of http_status_total to collapse into 1 panel, got %d: %+v", len(panels), panels)
	}
}

func TestBuildGroupsPanelsByNamePrefix(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "# TYPE stub_requests_total counter\nstub_requests_total 1\n"+
			"# TYPE jvm_gc_pause_seconds gauge\njvm_gc_pause_seconds 2\n"+
			"# TYPE jvm_memory_used_bytes gauge\njvm_memory_used_bytes 3\n"+
			"# TYPE uptime gauge\nuptime 4\n")
	}))
	defer ts.Close()

	out, err := Build(context.Background(), ts.URL)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("Build produced invalid JSON: %v", err)
	}
	tabs, _ := got["tabs"].([]any)
	serviceTab, _ := tabs[len(tabs)-1].(map[string]any)
	sections, _ := serviceTab["sections"].([]any)

	if len(sections) != 3 {
		t.Fatalf("expected 3 sections (jvm, stub, uptime), got %d: %+v", len(sections), sections)
	}

	wantOrder := []string{"jvm", "stub", "uptime"}
	wantPanelTitles := map[string][]string{
		"jvm":    {"jvm_gc_pause_seconds", "jvm_memory_used_bytes"},
		"stub":   {"stub_requests_total"},
		"uptime": {"uptime"},
	}

	for i, section := range sections {
		sec, _ := section.(map[string]any)
		title, _ := sec["title"].(string)
		if title != wantOrder[i] {
			t.Fatalf("section %d: expected title %q (alphabetical order), got %q", i, wantOrder[i], title)
		}

		panels, _ := sec["panels"].([]any)
		var gotTitles []string
		for _, p := range panels {
			panel, _ := p.(map[string]any)
			gotTitles = append(gotTitles, panel["title"].(string))
		}
		want := wantPanelTitles[title]
		if len(gotTitles) != len(want) {
			t.Fatalf("section %q: expected panels %v, got %v", title, want, gotTitles)
		}
		for j, wt := range want {
			if gotTitles[j] != wt {
				t.Fatalf("section %q: expected panels %v, got %v", title, want, gotTitles)
			}
		}
	}
}

func TestBuildReturnsErrorWhenServiceUnreachable(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := ts.URL
	ts.Close() // now guaranteed unreachable

	if _, err := Build(context.Background(), url); err == nil {
		t.Fatal("expected an error when the service isn't reachable")
	}
}
