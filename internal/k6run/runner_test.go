package k6run

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/thecoons/myrtille/internal/config"
)

// fakeDashboardRecordJSONL is a small but realistic --record stream: one
// gauge ("time", used only for timestamping) and one trend metric across
// two snapshots, mirroring the real event shapes captured from a live k6
// run (see internal/k6run/dashboard.go's doc comment for the protocol).
const fakeDashboardRecordJSONL = `{"event":"metric","data":{"time":{"type":"gauge","contains":"time"}}}
{"event":"metric","data":{"http_req_duration":{"type":"trend","contains":"time"}}}
{"event":"snapshot","data":[[329.27,339.44,330.63,314.12,338.5,338.97,339.34],[1000000]]}
{"event":"snapshot","data":[[300.0,310.0,305.0,290.0,308.0,309.0,309.5],[1001000]]}`

// installFakeK6 writes a shell-script stand-in for the k6 binary onto PATH
// for the duration of the test, so Run() can be exercised without a real
// k6 install. The fake writes a fixed summary JSON to whatever path follows
// --summary-export, a canned dashboard JSONL to whatever path follows
// record= in a --out web-dashboard=... argument (if any), and exits with
// $FAKE_K6_EXIT_CODE (default 0).
func installFakeK6(t *testing.T, summaryJSON string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake k6 shim is a POSIX shell script")
	}

	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"exit_code=${FAKE_K6_EXIT_CODE:-0}\n" +
		"summary_path=\"\"\n" +
		"dashboard_arg=\"\"\n" +
		"prev=\"\"\n" +
		"for arg in \"$@\"; do\n" +
		"  if [ \"$prev\" = \"--summary-export\" ]; then\n" +
		"    summary_path=\"$arg\"\n" +
		"  fi\n" +
		"  case \"$arg\" in\n" +
		"    web-dashboard=*) dashboard_arg=\"$arg\" ;;\n" +
		"  esac\n" +
		"  prev=\"$arg\"\n" +
		"done\n" +
		"if [ -n \"$summary_path\" ]; then\n" +
		"  cat > \"$summary_path\" <<'SUMMARY_EOF'\n" +
		summaryJSON + "\n" +
		"SUMMARY_EOF\n" +
		"fi\n" +
		"if [ -n \"$dashboard_arg\" ]; then\n" +
		"  dashboard_path=$(printf '%s' \"$dashboard_arg\" | sed -n 's/.*record=\\([^&]*\\).*/\\1/p')\n" +
		"  if [ -n \"$dashboard_path\" ]; then\n" +
		"    cat > \"$dashboard_path\" <<'DASHBOARD_EOF'\n" +
		fakeDashboardRecordJSONL + "\n" +
		"DASHBOARD_EOF\n" +
		"  fi\n" +
		"fi\n" +
		"exit \"$exit_code\"\n"

	path := filepath.Join(dir, "k6")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake k6 script: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	return testConfigWithReportFormats(t, "")
}

// testConfigWithReportFormats is like testConfig but lets the caller set
// report.formats (e.g. "html"), which drives whether Run requests a k6
// web-dashboard export. An empty formats string omits the report block
// entirely, so config.Config's own defaults (markdown+json, no html) apply.
func testConfigWithReportFormats(t *testing.T, formats string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "scenario.js")
	if err := os.WriteFile(scriptPath, []byte("export default function() {}"), 0o644); err != nil {
		t.Fatalf("writing scenario.js: %v", err)
	}

	yaml := "service:\n  base_url: http://localhost:8080\nk6:\n  script: ./scenario.js\n"
	if formats != "" {
		yaml += "report:\n  formats: [" + formats + "]\n"
	}
	cfgPath := filepath.Join(dir, "myrtille.yaml")
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("loading config: %v", err)
	}
	return cfg
}

const fakeSummaryJSON = `{
  "metrics": {
    "http_req_duration": {
      "avg": 12.3,
      "p(95)": 45.6,
      "thresholds": {"p(95)<500": {"ok": true}}
    },
    "http_reqs": {"count": 100, "rate": 3.3}
  }
}`

func TestRunSuccessParsesSummary(t *testing.T) {
	installFakeK6(t, fakeSummaryJSON)
	cfg := testConfig(t)

	var stdout, stderr bytes.Buffer
	result, err := Run(context.Background(), cfg, cfg.K6ScriptPath(), "/tmp/state.json", &stdout, &stderr)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if !result.Passed || result.ExitCode != 0 {
		t.Fatalf("expected passed run, got %+v", result)
	}
	if result.Summary == nil {
		t.Fatal("expected summary to be parsed")
	}
	if result.Summary.Metrics["http_req_duration"].Values["avg"] != 12.3 {
		t.Fatalf("unexpected avg: %+v", result.Summary.Metrics["http_req_duration"])
	}
	if !result.Summary.Metrics["http_req_duration"].Thresholds["p(95)<500"] {
		t.Fatalf("expected threshold p(95)<500 to be ok")
	}
	if result.Summary.Metrics["http_reqs"].Values["count"] != 100 {
		t.Fatalf("unexpected http_reqs count: %+v", result.Summary.Metrics["http_reqs"])
	}
	if result.DashboardSeries != nil {
		t.Fatalf("expected no dashboard series without the html report format, got %+v", result.DashboardSeries)
	}
}

func TestRunPopulatesDashboardSeriesWhenHTMLFormatRequested(t *testing.T) {
	installFakeK6(t, fakeSummaryJSON)
	cfg := testConfigWithReportFormats(t, `"html"`)

	var stdout, stderr bytes.Buffer
	result, err := Run(context.Background(), cfg, cfg.K6ScriptPath(), "/tmp/state.json", &stdout, &stderr)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if len(result.DashboardSeries) != 1 || result.DashboardSeries[0].Name != "http_req_duration" {
		t.Fatalf("unexpected dashboard series: %+v", result.DashboardSeries)
	}
	if len(result.DashboardSeries[0].Points) != 2 || result.DashboardSeries[0].Points[0].Value != 329.27 {
		t.Fatalf("unexpected dashboard series points: %+v", result.DashboardSeries[0].Points)
	}
}

func TestRunSkipsDashboardWhenHTMLNotRequested(t *testing.T) {
	installFakeK6(t, fakeSummaryJSON)
	cfg := testConfigWithReportFormats(t, `"markdown","json"`)

	var stdout, stderr bytes.Buffer
	result, err := Run(context.Background(), cfg, cfg.K6ScriptPath(), "/tmp/state.json", &stdout, &stderr)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if result.DashboardSeries != nil {
		t.Fatalf("expected no dashboard series, got %+v", result.DashboardSeries)
	}
}

func TestRunThresholdsFailedExitCode(t *testing.T) {
	installFakeK6(t, fakeSummaryJSON)
	t.Setenv("FAKE_K6_EXIT_CODE", "99")
	cfg := testConfig(t)

	var stdout, stderr bytes.Buffer
	result, err := Run(context.Background(), cfg, cfg.K6ScriptPath(), "/tmp/state.json", &stdout, &stderr)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if result.Passed {
		t.Fatal("expected Passed=false for exit code 99")
	}
	if !result.ThresholdsFailed {
		t.Fatal("expected ThresholdsFailed=true for exit code 99")
	}
	if result.ExitCode != 99 {
		t.Fatalf("ExitCode = %d, want 99", result.ExitCode)
	}
}

func TestRunGenericFailureExitCode(t *testing.T) {
	installFakeK6(t, fakeSummaryJSON)
	t.Setenv("FAKE_K6_EXIT_CODE", "1")
	cfg := testConfig(t)

	var stdout, stderr bytes.Buffer
	result, err := Run(context.Background(), cfg, cfg.K6ScriptPath(), "/tmp/state.json", &stdout, &stderr)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Passed || result.ThresholdsFailed {
		t.Fatalf("expected a plain failure, got %+v", result)
	}
	if result.ExitCode != 1 {
		t.Fatalf("ExitCode = %d, want 1", result.ExitCode)
	}
}

func TestRunMissingK6BinaryReturnsError(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	cfg := testConfig(t)

	var stdout, stderr bytes.Buffer
	if _, err := Run(context.Background(), cfg, cfg.K6ScriptPath(), "/tmp/state.json", &stdout, &stderr); err == nil {
		t.Fatal("expected error when k6 binary is missing from PATH")
	}
}

func TestBuildEnvOverridesDuplicateKeys(t *testing.T) {
	t.Setenv("MYRTILLE_TEST_VAR", "original")
	env := buildEnv(map[string]string{"MYRTILLE_TEST_VAR": "overridden"})

	found := false
	for _, kv := range env {
		if kv == "MYRTILLE_TEST_VAR=overridden" {
			found = true
		}
		if kv == "MYRTILLE_TEST_VAR=original" {
			t.Fatalf("original value should have been overridden, got env entry %q", kv)
		}
	}
	if !found {
		t.Fatal("expected overridden value to be present in env")
	}
}

func TestParseSummaryFlattensChecksFromRootGroup(t *testing.T) {
	summary, err := parseSummary([]byte(`{
		"metrics": {},
		"root_group": {
			"checks": {
				"status is 201": {"name": "status is 201", "path": "::status is 201", "passes": 100, "fails": 2}
			},
			"groups": {
				"user flow": {
					"checks": {
						"status is 200": {"name": "status is 200", "path": "::user flow::status is 200", "passes": 50, "fails": 0}
					},
					"groups": {}
				}
			}
		}
	}`))
	if err != nil {
		t.Fatalf("parseSummary returned error: %v", err)
	}

	want := []CheckResult{
		{Name: "status is 201", Path: "::status is 201", Passes: 100, Fails: 2},
		{Name: "status is 200", Path: "::user flow::status is 200", Passes: 50, Fails: 0},
	}
	if len(summary.Checks) != len(want) {
		t.Fatalf("Checks = %+v, want %+v", summary.Checks, want)
	}
	for i, c := range want {
		if summary.Checks[i] != c {
			t.Errorf("Checks[%d] = %+v, want %+v", i, summary.Checks[i], c)
		}
	}
}

func TestParseSummaryHandlesMissingRootGroup(t *testing.T) {
	summary, err := parseSummary([]byte(`{"metrics":{}}`))
	if err != nil {
		t.Fatalf("parseSummary returned error: %v", err)
	}
	if len(summary.Checks) != 0 {
		t.Fatalf("expected no checks, got %+v", summary.Checks)
	}
}

func TestParseSummaryHandlesUnknownFields(t *testing.T) {
	summary, err := parseSummary([]byte(`{"metrics":{"vus":{"value":5},"iterations":{"count":42,"rate":1.4}}}`))
	if err != nil {
		t.Fatalf("parseSummary returned error: %v", err)
	}
	if summary.Metrics["vus"].Values["value"] != 5 {
		t.Fatalf("unexpected vus summary: %+v", summary.Metrics["vus"])
	}
	if summary.Metrics["iterations"].Values["count"] != 42 {
		t.Fatalf("unexpected iterations summary: %+v", summary.Metrics["iterations"])
	}
}
