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

// installFakeK6 writes a shell-script stand-in for the k6 binary onto PATH
// for the duration of the test, so Run() can be exercised without a real
// k6 install. The fake writes a fixed summary JSON to whatever path follows
// --summary-export and exits with $FAKE_K6_EXIT_CODE (default 0).
func installFakeK6(t *testing.T, summaryJSON string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake k6 shim is a POSIX shell script")
	}

	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"exit_code=${FAKE_K6_EXIT_CODE:-0}\n" +
		"summary_path=\"\"\n" +
		"prev=\"\"\n" +
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
