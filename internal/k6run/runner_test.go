package k6run

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/thecoons/myrtille/internal/config"
)

// installFakeK6 writes a shell-script stand-in for the k6 binary onto PATH
// for the duration of the test, so Run() can be exercised without a real
// k6 install. The fake writes a fixed summary JSON to whatever path follows
// --summary-export, its full argv (one per line, for tests that need to
// assert exactly what Run passed it) to the returned path, and exits with
// $FAKE_K6_EXIT_CODE (default 0).
func installFakeK6(t *testing.T, summaryJSON string) (argvPath string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake k6 shim is a POSIX shell script")
	}

	// So Run() resolves the shim below via PATH regardless of what the
	// invoking shell happens to export — resolveK6Binary prefers
	// MYRTILLE_K6_BIN when set.
	t.Setenv(k6BinEnv, "")

	dir := t.TempDir()
	argvPath = filepath.Join(dir, "argv.txt")
	script := "#!/bin/sh\n" +
		"exit_code=${FAKE_K6_EXIT_CODE:-0}\n" +
		"summary_path=\"\"\n" +
		"prev=\"\"\n" +
		"for arg in \"$@\"; do printf '%s\\n' \"$arg\"; done > " + shQuote(argvPath) + "\n" +
		"for arg in \"$@\"; do\n" +
		"  if [ \"$prev\" = \"--summary-export\" ]; then\n" +
		"    summary_path=\"$arg\"\n" +
		"  fi\n" +
		"  prev=\"$arg\"\n" +
		"done\n" +
		"if [ -n \"$summary_path\" ]; then\n" +
		"  cat > \"$summary_path\" <<'SUMMARY_EOF'\n" +
		summaryJSON + "\n" +
		"SUMMARY_EOF\n" +
		"fi\n" +
		"exit \"$exit_code\"\n"

	path := filepath.Join(dir, "k6")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake k6 script: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argvPath
}

// shQuote wraps s in single quotes for safe interpolation into the shell
// scripts installFakeK6/installFakeK6At generate (paths here are always
// t.TempDir()-derived, so no embedded single quotes in practice, but this
// keeps the generated script correct regardless).
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "scenario.js")
	if err := os.WriteFile(scriptPath, []byte("export default function() {}"), 0o644); err != nil {
		t.Fatalf("writing scenario.js: %v", err)
	}

	yaml := "service:\n  base_url: http://localhost:8080\nk6:\n  script: ./scenario.js\n"
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

// testConfigWithMetricsURL is like testConfig but sets service.metrics.url,
// which drives both k6gen's promscrape wiring and (with a custom binary)
// Run's dashboard config generation.
func testConfigWithMetricsURL(t *testing.T, metricsURL string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "scenario.js")
	if err := os.WriteFile(scriptPath, []byte("export default function() {}"), 0o644); err != nil {
		t.Fatalf("writing scenario.js: %v", err)
	}

	yaml := "service:\n  base_url: http://localhost:8080\n  metrics:\n    url: " + metricsURL + "\nk6:\n  script: ./scenario.js\n"
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

// installFakeK6At is like installFakeK6 but writes the shim to an arbitrary
// path rather than a PATH directory named "k6", for tests exercising
// MYRTILLE_K6_BIN directly instead of PATH resolution. Returns the path its
// argv (one per line) is captured to. Also copies whatever file
// $XK6_DASHBOARD_CONFIG points to (if set) to path+".dashboardconfig"
// before that file gets removed by Run's own defer — the only way a test
// can inspect its content afterward.
func installFakeK6At(t *testing.T, path, summaryJSON string) (argvPath string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake k6 shim is a POSIX shell script")
	}

	argvPath = path + ".argv"
	dashboardConfigCopy := path + ".dashboardconfig"
	script := "#!/bin/sh\n" +
		"summary_path=\"\"\n" +
		"prev=\"\"\n" +
		"for arg in \"$@\"; do printf '%s\\n' \"$arg\"; done > " + shQuote(argvPath) + "\n" +
		"if [ -n \"$XK6_DASHBOARD_CONFIG\" ] && [ -f \"$XK6_DASHBOARD_CONFIG\" ]; then\n" +
		"  cp \"$XK6_DASHBOARD_CONFIG\" " + shQuote(dashboardConfigCopy) + "\n" +
		"fi\n" +
		"for arg in \"$@\"; do\n" +
		"  if [ \"$prev\" = \"--summary-export\" ]; then\n" +
		"    summary_path=\"$arg\"\n" +
		"  fi\n" +
		"  prev=\"$arg\"\n" +
		"done\n" +
		"if [ -n \"$summary_path\" ]; then\n" +
		"  cat > \"$summary_path\" <<'SUMMARY_EOF'\n" +
		summaryJSON + "\n" +
		"SUMMARY_EOF\n" +
		"fi\n"

	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake k6 script at %s: %v", path, err)
	}
	return argvPath
}

func TestResolveK6BinaryDefaultsToPath(t *testing.T) {
	t.Setenv(k6BinEnv, "")
	installFakeK6(t, fakeSummaryJSON)

	got, custom, err := resolveK6Binary()
	if err != nil {
		t.Fatalf("resolveK6Binary: %v", err)
	}
	if filepath.Base(got) != "k6" {
		t.Fatalf("expected the PATH-resolved k6, got %q", got)
	}
	if custom {
		t.Fatal("expected custom=false when resolved from PATH")
	}
}

func TestResolveK6BinaryUsesOverrideWhenSet(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no "k6" on PATH — override must be what's used
	customPath := filepath.Join(t.TempDir(), "k6-custom")
	installFakeK6At(t, customPath, fakeSummaryJSON)
	t.Setenv(k6BinEnv, customPath)

	got, custom, err := resolveK6Binary()
	if err != nil {
		t.Fatalf("resolveK6Binary: %v", err)
	}
	if got != customPath {
		t.Fatalf("expected override path %q, got %q", customPath, got)
	}
	if !custom {
		t.Fatal("expected custom=true when MYRTILLE_K6_BIN is set")
	}
}

func TestResolveK6BinaryOverrideMissingFileReturnsError(t *testing.T) {
	t.Setenv(k6BinEnv, filepath.Join(t.TempDir(), "does-not-exist"))

	if _, _, err := resolveK6Binary(); err == nil {
		t.Fatal("expected error when MYRTILLE_K6_BIN points at a missing file")
	}
}

// TestRunUsesK6BinOverride is the walking-skeleton check: with no "k6" on
// PATH at all, Run must still succeed by shelling out to MYRTILLE_K6_BIN —
// proving the custom-binary wiring, not just resolveK6Binary in isolation.
func TestRunUsesK6BinOverride(t *testing.T) {
	// PATH is left as-is (unlike TestResolveK6BinaryUsesOverrideWhenSet):
	// the shim script below shells out to `cat`, so it needs a real PATH to
	// find it. What this test actually checks — that the override wins over
	// whatever "k6" PATH would resolve to — doesn't require PATH to be
	// empty.
	custom := filepath.Join(t.TempDir(), "k6-custom")
	installFakeK6At(t, custom, fakeSummaryJSON)
	t.Setenv(k6BinEnv, custom)

	cfg := testConfig(t)

	var stdout, stderr bytes.Buffer
	result, err := Run(context.Background(), cfg, cfg.K6ScriptPath(), "/tmp/state.json", &stdout, &stderr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Passed {
		t.Fatalf("expected a passed result, got %+v", result)
	}
}

// TestRunRequestsLiveDashboardWhenUsingCustomBinary is step 4's core check:
// MYRTILLE_K6_BIN alone must be enough to get a live (non-headless)
// dashboard port, and must NOT pass open= — see
// docs/plans/xk6-live-dashboard.md, step 4 (k6 itself prints the dashboard
// URL to stdout; myrtille deliberately doesn't also try to launch a
// browser). There's no headless/record= counterpart to this test anymore
// (step 6 removed both, along with the homemade HTML report that was their
// only consumer) — without a custom binary, Run doesn't request
// --out web-dashboard at all, see TestRunSkipsDashboardWithoutCustomBinary.
func TestRunRequestsLiveDashboardWhenUsingCustomBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	custom := filepath.Join(t.TempDir(), "k6-custom")
	argvPath := installFakeK6At(t, custom, fakeSummaryJSON)
	t.Setenv(k6BinEnv, custom)

	cfg := testConfig(t)

	var stdout, stderr bytes.Buffer
	if _, err := Run(context.Background(), cfg, cfg.K6ScriptPath(), "/tmp/state.json", &stdout, &stderr); err != nil {
		t.Fatalf("Run: %v", err)
	}

	dashboardArg := findWebDashboardArg(t, argvPath)
	if !strings.Contains(dashboardArg, "port=0") {
		t.Errorf("expected a live port (port=0), got %q", dashboardArg)
	}
	if strings.Contains(dashboardArg, "open=") {
		t.Errorf("expected no open= — myrtille doesn't launch a browser itself, got %q", dashboardArg)
	}
}

// TestRunSkipsDashboardWithoutCustomBinary is the regression check paired
// with the above: without MYRTILLE_K6_BIN, Run must not pass --out
// web-dashboard at all — there's no live dashboard to serve it on, and (as
// of step 6) nothing left that consumes a headless record file either.
func TestRunSkipsDashboardWithoutCustomBinary(t *testing.T) {
	argvPath := installFakeK6(t, fakeSummaryJSON)
	cfg := testConfig(t)

	var stdout, stderr bytes.Buffer
	if _, err := Run(context.Background(), cfg, cfg.K6ScriptPath(), "/tmp/state.json", &stdout, &stderr); err != nil {
		t.Fatalf("Run: %v", err)
	}

	data, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatalf("reading captured argv: %v", err)
	}
	if strings.Contains(string(data), "web-dashboard=") {
		t.Errorf("expected no --out web-dashboard argument, got argv:\n%s", data)
	}
}

// TestRunGeneratesDashboardConfigWhenLiveAndMetricsURLSet is step 5's core
// check: with a custom binary AND service.metrics.url both set, Run must
// scrape the service once itself and pass k6 a dashboard config (via
// XK6_DASHBOARD_CONFIG) containing a "Service" tab built from what it found.
func TestRunGeneratesDashboardConfigWhenLiveAndMetricsURLSet(t *testing.T) {
	metricsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "# TYPE svc_widgets_total counter\nsvc_widgets_total 3\n")
	}))
	defer metricsServer.Close()

	custom := filepath.Join(t.TempDir(), "k6-custom")
	installFakeK6At(t, custom, fakeSummaryJSON)
	t.Setenv(k6BinEnv, custom)

	cfg := testConfigWithMetricsURL(t, metricsServer.URL)

	var stdout, stderr bytes.Buffer
	if _, err := Run(context.Background(), cfg, cfg.K6ScriptPath(), "/tmp/state.json", &stdout, &stderr); err != nil {
		t.Fatalf("Run: %v", err)
	}

	data, err := os.ReadFile(custom + ".dashboardconfig")
	if err != nil {
		t.Fatalf("reading captured dashboard config (was XK6_DASHBOARD_CONFIG even set?): %v", err)
	}

	var cfgJSON map[string]any
	if err := json.Unmarshal(data, &cfgJSON); err != nil {
		t.Fatalf("dashboard config is not valid JSON: %v\n%s", err, data)
	}
	tabs, _ := cfgJSON["tabs"].([]any)
	if len(tabs) == 0 {
		t.Fatal("expected at least the Service tab in tabs")
	}
	last, _ := tabs[len(tabs)-1].(map[string]any)
	if last["title"] != "Service" {
		t.Fatalf("expected the last tab to be titled Service, got %+v", last)
	}
	if !strings.Contains(string(data), "svc_svc_widgets_total") {
		t.Errorf("expected a panel querying svc_svc_widgets_total, got:\n%s", data)
	}
}

// TestRunSkipsDashboardConfigWithoutMetricsURL is the paired regression
// check: a custom binary alone, without service.metrics.url, must not
// attempt to build a dashboard config (there'd be nothing to scrape).
func TestRunSkipsDashboardConfigWithoutMetricsURL(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "k6-custom")
	installFakeK6At(t, custom, fakeSummaryJSON)
	t.Setenv(k6BinEnv, custom)

	cfg := testConfig(t) // no service.metrics.url

	var stdout, stderr bytes.Buffer
	if _, err := Run(context.Background(), cfg, cfg.K6ScriptPath(), "/tmp/state.json", &stdout, &stderr); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if _, err := os.Stat(custom + ".dashboardconfig"); !os.IsNotExist(err) {
		t.Fatalf("expected no dashboard config to be generated, stat err: %v", err)
	}
}

// findWebDashboardArg reads argvPath (written by installFakeK6/
// installFakeK6At) and returns the single `web-dashboard=...` argument
// value, failing the test if there isn't exactly one.
func findWebDashboardArg(t *testing.T, argvPath string) string {
	t.Helper()
	data, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatalf("reading captured argv: %v", err)
	}

	var found []string
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if strings.HasPrefix(line, "web-dashboard=") {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly one web-dashboard= argument, got %v (full argv: %q)", found, data)
	}
	return found[0]
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
