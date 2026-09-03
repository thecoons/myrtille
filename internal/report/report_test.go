package report

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thecoons/myrtille/internal/initphase"
	"github.com/thecoons/myrtille/internal/k6run"
	"github.com/thecoons/myrtille/internal/servicelifecycle"
)

func sampleReport() *Report {
	started := time.Date(2026, 8, 30, 22, 0, 0, 0, time.UTC)
	return &Report{
		Name:       "demo-service",
		Ref:        "JIRA-PROJ-45",
		StartedAt:  started,
		FinishedAt: started.Add(90 * time.Second),
		Init: &initphase.Summary{Steps: []initphase.StepResult{
			{Name: "create_users", Requests: 20, Extracted: map[string]int{"user_ids": 20}},
		}},
		K6: &k6run.Result{
			ExitCode: 0,
			Passed:   true,
			Duration: 30 * time.Second,
			Summary: &k6run.Summary{
				Metrics: map[string]k6run.MetricSummary{
					"http_req_duration": {
						Values:     map[string]float64{"avg": 12.3, "p(95)": 45.6},
						Thresholds: map[string]bool{"p(95)<500": true},
					},
				},
				Checks: []k6run.CheckResult{
					{Name: "status is 201", Path: "::status is 201", Passes: 20, Fails: 0},
					{Name: "status is 200", Path: "::user flow::status is 200", Passes: 18, Fails: 2},
				},
			},
		},
		Teardown: &initphase.Summary{Steps: []initphase.StepResult{
			{Name: "delete_users", Requests: 19, Extracted: map[string]int{}},
		}},
		TeardownErrors: []string{"teardown step \"delete_users\" (iteration 3): unexpected status 404"},
	}
}

func reportWithSpanStats() *Report {
	r := sampleReport()
	r.K6.SpanStats = []k6run.SpanStat{
		{Name: "check_inventory", Count: 235, AvgMs: 12.4, MinMs: 5, MaxMs: 20, P90Ms: 18, P95Ms: 19, ErrorRate: 0.0688},
		{Name: "place_order", Count: 235, AvgMs: 0.05, MinMs: 0.01, MaxMs: 0.5, P90Ms: 0.1, P95Ms: 0.2, ErrorRate: 0},
	}
	return r
}

func TestMarkdownShowsSpansSection(t *testing.T) {
	md := reportWithSpanStats().Markdown()

	for _, want := range []string{"### Spans", "check_inventory", "place_order", "235"} {
		if !strings.Contains(md, want) {
			t.Errorf("expected markdown to contain %q, got:\n%s", want, md)
		}
	}
}

func TestMarkdownOmitsSpansSectionWhenEmpty(t *testing.T) {
	md := sampleReport().Markdown()
	if strings.Contains(md, "### Spans") {
		t.Errorf("expected no Spans section when K6.SpanStats is empty, got:\n%s", md)
	}
}

// TestMarkdownShowsSpansSectionEvenWithNilSummary guards against the
// early-return restructure this needed: SpanStats comes from a mechanism
// entirely independent of --summary-export (see
// docs/plans/otel-span-metrics.md's "Extension" section), so it must
// still render even when Summary itself is nil (failed to parse, or k6
// exited before writing it).
func TestMarkdownShowsSpansSectionEvenWithNilSummary(t *testing.T) {
	r := reportWithSpanStats()
	r.K6.Summary = nil

	md := r.Markdown()
	if !strings.Contains(md, "### Spans") || !strings.Contains(md, "check_inventory") {
		t.Errorf("expected Spans section even with a nil Summary, got:\n%s", md)
	}
}

func TestJSONIncludesSpanStats(t *testing.T) {
	data, err := reportWithSpanStats().JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}

	var decoded struct {
		K6 struct {
			SpanStats []struct {
				Name      string  `json:"name"`
				Count     int     `json:"count"`
				AvgMs     float64 `json:"avg_ms"`
				ErrorRate float64 `json:"error_rate"`
			} `json:"SpanStats"`
		} `json:"k6"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshaling report JSON: %v", err)
	}

	if len(decoded.K6.SpanStats) != 2 {
		t.Fatalf("expected 2 span stats in JSON, got %d:\n%s", len(decoded.K6.SpanStats), data)
	}
	if decoded.K6.SpanStats[0].Name != "check_inventory" || decoded.K6.SpanStats[0].Count != 235 {
		t.Errorf("unexpected first span stat: %+v", decoded.K6.SpanStats[0])
	}
}

func TestMarkdownContainsExpectedSections(t *testing.T) {
	md := sampleReport().Markdown()

	for _, want := range []string{
		"demo-service",
		"JIRA-PROJ-45",
		"create_users",
		"user_ids=20",
		"PASSED",
		"http_req_duration",
		"p(95)<500: OK",
		"### Checks",
		"::status is 201: 20 passed, 0 failed [OK]",
		"::user flow::status is 200: 18 passed, 2 failed [FAIL]",
		"delete_users",
		"1 teardown error(s)",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("Markdown() missing expected substring %q\n--- full output ---\n%s", want, md)
		}
	}
}

func TestMarkdownShowsInitCommandSummary(t *testing.T) {
	r := &Report{
		StartedAt:  time.Now(),
		FinishedAt: time.Now(),
		Init: &initphase.Summary{Command: &initphase.CommandResult{
			Command:  "./seed.sh",
			ExitCode: 0,
			Duration: 2500 * time.Millisecond,
		}},
	}
	md := r.Markdown()

	for _, want := range []string{
		"Command: `./seed.sh`",
		"Status: **OK** (exit code 0)",
		"Duration: 2.5s",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("Markdown() missing expected substring %q\n--- full output ---\n%s", want, md)
		}
	}
	if strings.Contains(md, "No init steps configured") {
		t.Error("Markdown() should not show the empty-init placeholder when init.command ran")
	}
}

func TestMarkdownShowsServiceSummary(t *testing.T) {
	r := &Report{
		StartedAt:  time.Now(),
		FinishedAt: time.Now(),
		Service: &servicelifecycle.Summary{
			Command:    "./start.sh",
			ReadyAfter: 2500 * time.Millisecond,
			Stop:       &servicelifecycle.StopResult{Signal: "TERM", Clean: true},
		},
	}
	md := r.Markdown()

	for _, want := range []string{
		"## Service",
		"Command: `./start.sh`",
		"Ready after: 2.5s",
		"signal **TERM**, status **CLEAN**",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("Markdown() missing expected substring %q\n--- full output ---\n%s", want, md)
		}
	}
}

func TestMarkdownShowsServiceStopTimedOut(t *testing.T) {
	r := &Report{
		StartedAt:  time.Now(),
		FinishedAt: time.Now(),
		Service: &servicelifecycle.Summary{
			Command:    "./start.sh",
			ReadyAfter: time.Second,
			Stop:       &servicelifecycle.StopResult{Signal: "TERM", Clean: false},
		},
	}
	md := r.Markdown()

	if !strings.Contains(md, "signal **TERM**, status **TIMED OUT**") {
		t.Errorf("Markdown() missing expected timed-out status\n--- full output ---\n%s", md)
	}
}

func TestMarkdownOmitsServiceSectionWhenNil(t *testing.T) {
	md := sampleReport().Markdown()
	if strings.Contains(md, "## Service") {
		t.Errorf("Markdown() should omit the Service section when Report.Service is nil, got:\n%s", md)
	}
}

func TestMarkdownHandlesEmptyReport(t *testing.T) {
	r := &Report{Name: "", StartedAt: time.Now(), FinishedAt: time.Now()}
	md := r.Markdown()

	for _, want := range []string{
		"(unnamed)",
		"No init steps configured",
		"k6 did not run",
		"No teardown steps configured",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("Markdown() missing expected placeholder %q\n--- full output ---\n%s", want, md)
		}
	}
}

func TestMarkdownShowsError(t *testing.T) {
	r := &Report{StartedAt: time.Now(), FinishedAt: time.Now(), Error: "init phase failed: boom"}
	md := r.Markdown()
	if !strings.Contains(md, "**ERROR:** init phase failed: boom") {
		t.Errorf("Markdown() missing error line:\n%s", md)
	}
}

func TestMarkdownOrdersTeardownAfterInitBeforeK6(t *testing.T) {
	md := sampleReport().Markdown()

	initIdx := strings.Index(md, "## Init Phase")
	teardownIdx := strings.Index(md, "## Teardown Phase")
	k6Idx := strings.Index(md, "## k6 Results")
	if initIdx == -1 || teardownIdx == -1 || k6Idx == -1 {
		t.Fatalf("expected all three sections present, got init=%d teardown=%d k6=%d", initIdx, teardownIdx, k6Idx)
	}
	if initIdx >= teardownIdx || teardownIdx >= k6Idx {
		t.Errorf("expected order Init < Teardown < k6, got init=%d teardown=%d k6=%d", initIdx, teardownIdx, k6Idx)
	}
}

func TestJSONRoundTrip(t *testing.T) {
	data, err := sampleReport().JSON()
	if err != nil {
		t.Fatalf("JSON returned error: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal returned error: %v", err)
	}

	if decoded["name"] != "demo-service" {
		t.Errorf("name = %v, want demo-service", decoded["name"])
	}
	if decoded["ref"] != "JIRA-PROJ-45" {
		t.Errorf("ref = %v, want JIRA-PROJ-45", decoded["ref"])
	}
	if decoded["duration_seconds"] != float64(90) {
		t.Errorf("duration_seconds = %v, want 90", decoded["duration_seconds"])
	}

	k6, ok := decoded["k6"].(map[string]any)
	if !ok {
		t.Fatalf("expected k6 object in JSON, got %v", decoded["k6"])
	}
	summary, ok := k6["Summary"].(map[string]any)
	if !ok {
		t.Fatalf("expected k6.Summary object in JSON, got %v", k6["Summary"])
	}
	checks, ok := summary["Checks"].([]any)
	if !ok || len(checks) != 2 {
		t.Fatalf("expected 2 checks in k6.Summary.Checks, got %v", summary["Checks"])
	}

	if _, present := decoded["service"]; present {
		t.Errorf("expected no \"service\" key when Report.Service is nil, got %v", decoded["service"])
	}
}

func TestJSONIncludesServiceWhenSet(t *testing.T) {
	r := &Report{
		StartedAt:  time.Now(),
		FinishedAt: time.Now(),
		Service: &servicelifecycle.Summary{
			Command:    "./start.sh",
			ReadyAfter: 2 * time.Second,
			Stop:       &servicelifecycle.StopResult{Signal: "TERM", Clean: true},
		},
	}
	data, err := r.JSON()
	if err != nil {
		t.Fatalf("JSON returned error: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal returned error: %v", err)
	}
	svc, ok := decoded["service"].(map[string]any)
	if !ok {
		t.Fatalf("expected a service object in JSON, got %v", decoded["service"])
	}
	if svc["Command"] != "./start.sh" {
		t.Errorf("service.Command = %v, want ./start.sh", svc["Command"])
	}
	stop, ok := svc["Stop"].(map[string]any)
	if !ok {
		t.Fatalf("expected service.Stop object in JSON, got %v", svc["Stop"])
	}
	if stop["Signal"] != "TERM" || stop["Clean"] != true {
		t.Errorf("service.Stop = %v, want Signal=TERM Clean=true", stop)
	}
}

func TestWriteFilesCreatesRequestedFormats(t *testing.T) {
	dir := t.TempDir()
	r := sampleReport()

	outDir, err := r.WriteFiles(dir, []string{"markdown", "json"})
	if err != nil {
		t.Fatalf("WriteFiles returned error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(outDir, "report.md")); err != nil {
		t.Errorf("expected report.md to exist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "report.json")); err != nil {
		t.Errorf("expected report.json to exist: %v", err)
	}
}

func TestWriteFilesOnlyRequestedFormat(t *testing.T) {
	dir := t.TempDir()
	r := sampleReport()

	outDir, err := r.WriteFiles(dir, []string{"json"})
	if err != nil {
		t.Fatalf("WriteFiles returned error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(outDir, "report.json")); err != nil {
		t.Errorf("expected report.json to exist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "report.md")); !os.IsNotExist(err) {
		t.Errorf("expected report.md to not exist, stat err = %v", err)
	}
}

func TestWriteFilesDisambiguatesSameSecondReports(t *testing.T) {
	dir := t.TempDir()

	r1 := sampleReport()
	r1.Name = "first"
	r2 := sampleReport()
	r2.Name = "second"
	r2.StartedAt = r1.StartedAt // same second — must not collide

	outDir1, err := r1.WriteFiles(dir, []string{"markdown"})
	if err != nil {
		t.Fatalf("first WriteFiles returned error: %v", err)
	}
	outDir2, err := r2.WriteFiles(dir, []string{"markdown"})
	if err != nil {
		t.Fatalf("second WriteFiles returned error: %v", err)
	}

	if outDir1 == outDir2 {
		t.Fatalf("expected two distinct report directories for reports started in the same second, got the same one: %s", outDir1)
	}

	md1, err := os.ReadFile(filepath.Join(outDir1, "report.md"))
	if err != nil {
		t.Fatalf("reading first report.md: %v", err)
	}
	md2, err := os.ReadFile(filepath.Join(outDir2, "report.md"))
	if err != nil {
		t.Fatalf("reading second report.md: %v", err)
	}
	if !strings.Contains(string(md1), "first") {
		t.Errorf("expected the first report's own content to survive, got:\n%s", md1)
	}
	if !strings.Contains(string(md2), "second") {
		t.Errorf("expected the second report's own content to survive, got:\n%s", md2)
	}
}

func TestWriteFilesRejectsUnsupportedFormat(t *testing.T) {
	r := sampleReport()
	if _, err := r.WriteFiles(t.TempDir(), []string{"pdf"}); err == nil {
		t.Fatal("expected error for unsupported format")
	}
}

// TestWriteFilesCopiesDashboardHTMLExport is step 2's core check:
// "dashboard-html" copies k6run.Result.DashboardHTMLPath to report.html in
// the report directory, and cleans up the source temp file — see
// writeDashboardHTML's doc comment for why WriteFiles (not Run) owns that
// cleanup.
func TestWriteFilesCopiesDashboardHTMLExport(t *testing.T) {
	dir := t.TempDir()
	exportPath := filepath.Join(t.TempDir(), "export.html")
	if err := os.WriteFile(exportPath, []byte("<html>dashboard</html>"), 0o644); err != nil {
		t.Fatalf("writing fake export: %v", err)
	}

	r := sampleReport()
	r.K6.DashboardHTMLPath = exportPath

	outDir, err := r.WriteFiles(dir, []string{"dashboard-html"})
	if err != nil {
		t.Fatalf("WriteFiles returned error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(outDir, "report.html"))
	if err != nil {
		t.Fatalf("expected report.html to exist: %v", err)
	}
	if string(data) != "<html>dashboard</html>" {
		t.Fatalf("unexpected report.html content: %q", data)
	}

	if _, err := os.Stat(exportPath); !os.IsNotExist(err) {
		t.Errorf("expected the temp export file to be removed after copying, stat err = %v", err)
	}
}

// TestWriteFilesFailsDashboardHTMLWithoutExport is the paired regression
// check: requesting "dashboard-html" when Run never produced an export
// (e.g. no custom k6 binary, or k6 never ran) must fail loudly rather than
// silently omit report.html.
func TestWriteFilesFailsDashboardHTMLWithoutExport(t *testing.T) {
	r := sampleReport()
	r.K6.DashboardHTMLPath = "" // never produced

	if _, err := r.WriteFiles(t.TempDir(), []string{"dashboard-html"}); err == nil {
		t.Fatal("expected an error when no dashboard export was produced")
	}
}

// TestWriteFilesFailsDashboardHTMLWithNilK6 covers the case where k6 never
// ran at all (e.g. init phase failed first) — R.K6 itself is nil, not just
// DashboardHTMLPath empty.
func TestWriteFilesFailsDashboardHTMLWithNilK6(t *testing.T) {
	r := sampleReport()
	r.K6 = nil

	if _, err := r.WriteFiles(t.TempDir(), []string{"dashboard-html"}); err == nil {
		t.Fatal("expected an error when k6 never ran")
	}
}
