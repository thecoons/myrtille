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
	if !(initIdx < teardownIdx && teardownIdx < k6Idx) {
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

func TestWriteFilesRejectsUnsupportedFormat(t *testing.T) {
	r := sampleReport()
	if _, err := r.WriteFiles(t.TempDir(), []string{"pdf"}); err == nil {
		t.Fatal("expected error for unsupported format")
	}
}
