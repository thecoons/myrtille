package suite

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// fakeMyrtilleSrc stands in for the real myrtille binary in RunScenario
// tests — it only needs to accept `run --config <path>` (ignored),
// optionally print the report line and/or react to SIGTERM, and exit with
// a controllable code, all driven by env vars so each test can shape its
// behavior without a different binary. It also echoes its own argv so a
// test can confirm exactly which flags RunScenario passed it.
const fakeMyrtilleSrc = `package main

import (
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	fmt.Println("ARGV=" + strings.Join(os.Args[1:], " "))
	if ms := os.Getenv("FAKE_SLEEP_MS"); ms != "" {
		n, _ := strconv.Atoi(ms)
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM)
		select {
		case <-sigCh:
			fmt.Println("fake: received SIGTERM, exiting gracefully")
			os.Exit(7)
		case <-time.After(time.Duration(n) * time.Millisecond):
		}
	}
	if dir := os.Getenv("FAKE_REPORT_DIR"); dir != "" {
		fmt.Println("report written to " + dir)
	}
	code, _ := strconv.Atoi(os.Getenv("FAKE_EXIT_CODE"))
	os.Exit(code)
}
`

var fakeMyrtilleBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "suite-testbin")
	if err != nil {
		fmt.Println("setting up fake myrtille bin:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	srcPath := filepath.Join(dir, "fakemyrtille.go")
	if err := os.WriteFile(srcPath, []byte(fakeMyrtilleSrc), 0o644); err != nil {
		fmt.Println("writing fake myrtille source:", err)
		os.Exit(1)
	}
	fakeMyrtilleBin = filepath.Join(dir, "fakemyrtille")
	build := exec.Command("go", "build", "-o", fakeMyrtilleBin, srcPath)
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Printf("building fake myrtille bin: %v\n%s\n", err, out)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

func TestRunScenarioCapturesReportDirAndExitCode(t *testing.T) {
	t.Setenv("FAKE_REPORT_DIR", "/fake/report/20260101-000000")
	t.Setenv("FAKE_EXIT_CODE", "0")
	t.Setenv("FAKE_SLEEP_MS", "")

	var out, errOut bytes.Buffer
	result, err := RunScenario(context.Background(), fakeMyrtilleBin, "/does/not/exist.yaml", false, &out, &errOut)
	if err != nil {
		t.Fatalf("RunScenario returned error: %v", err)
	}
	if result.ReportDir != "/fake/report/20260101-000000" {
		t.Errorf("ReportDir = %q, want /fake/report/20260101-000000", result.ReportDir)
	}
	if result.ExitCode != 0 || !result.Passed {
		t.Errorf("ExitCode/Passed = %d/%v, want 0/true", result.ExitCode, result.Passed)
	}
	if !bytes.Contains(out.Bytes(), []byte("report written to /fake/report/20260101-000000")) {
		t.Errorf("expected the report line to be streamed live to out, got %q", out.String())
	}
}

func TestRunScenarioAppendsSkipServiceLifecycleFlagWhenTrue(t *testing.T) {
	t.Setenv("FAKE_REPORT_DIR", "")
	t.Setenv("FAKE_EXIT_CODE", "0")
	t.Setenv("FAKE_SLEEP_MS", "")

	var out, errOut bytes.Buffer
	_, err := RunScenario(context.Background(), fakeMyrtilleBin, "/does/not/exist.yaml", true, &out, &errOut)
	if err != nil {
		t.Fatalf("RunScenario returned error: %v", err)
	}
	want := "ARGV=run --config /does/not/exist.yaml --skip-service-lifecycle"
	if !bytes.Contains(out.Bytes(), []byte(want)) {
		t.Errorf("expected the child's argv to include --skip-service-lifecycle, got %q", out.String())
	}
}

func TestRunScenarioOmitsSkipServiceLifecycleFlagWhenFalse(t *testing.T) {
	t.Setenv("FAKE_REPORT_DIR", "")
	t.Setenv("FAKE_EXIT_CODE", "0")
	t.Setenv("FAKE_SLEEP_MS", "")

	var out, errOut bytes.Buffer
	_, err := RunScenario(context.Background(), fakeMyrtilleBin, "/does/not/exist.yaml", false, &out, &errOut)
	if err != nil {
		t.Fatalf("RunScenario returned error: %v", err)
	}
	if bytes.Contains(out.Bytes(), []byte("--skip-service-lifecycle")) {
		t.Errorf("expected no --skip-service-lifecycle in the child's argv, got %q", out.String())
	}
}

func TestRunScenarioNonZeroExitCode(t *testing.T) {
	t.Setenv("FAKE_REPORT_DIR", "")
	t.Setenv("FAKE_EXIT_CODE", "3")
	t.Setenv("FAKE_SLEEP_MS", "")

	var out, errOut bytes.Buffer
	result, err := RunScenario(context.Background(), fakeMyrtilleBin, "/does/not/exist.yaml", false, &out, &errOut)
	if err != nil {
		t.Fatalf("RunScenario returned error for a plain non-zero exit: %v", err)
	}
	if result.ExitCode != 3 || result.Passed {
		t.Errorf("ExitCode/Passed = %d/%v, want 3/false", result.ExitCode, result.Passed)
	}
}

func TestRunScenarioSendsSIGTERMOnCancel(t *testing.T) {
	t.Setenv("FAKE_REPORT_DIR", "")
	t.Setenv("FAKE_EXIT_CODE", "0")
	t.Setenv("FAKE_SLEEP_MS", "5000")

	ctx, cancel := context.WithCancel(context.Background())
	var out, errOut bytes.Buffer

	done := make(chan struct{})
	var result *ScenarioResult
	var runErr error
	go func() {
		result, runErr = RunScenario(ctx, fakeMyrtilleBin, "/does/not/exist.yaml", false, &out, &errOut)
		close(done)
	}()

	time.Sleep(300 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunScenario did not return within 5s of cancellation — SIGTERM handling likely broken")
	}

	if runErr != nil {
		t.Fatalf("RunScenario returned error: %v", runErr)
	}
	if result.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7 (the fake's own graceful-SIGTERM exit code) — got a different code, suggesting it was killed instead of signaled", result.ExitCode)
	}
	if !bytes.Contains(out.Bytes(), []byte("received SIGTERM")) {
		t.Errorf("expected the fake's own graceful-shutdown message in the streamed output, got %q", out.String())
	}
}

func TestReportPathTapCapturesLineSplitAcrossWrites(t *testing.T) {
	var underlying bytes.Buffer
	var dest string
	tap := &reportPathTap{Underlying: &underlying, Prefix: reportWrittenPrefix, Dest: &dest}

	chunks := []string{"some log line\nreport wri", "tten to /a/b/c\nmore output\n"}
	for _, c := range chunks {
		if _, err := tap.Write([]byte(c)); err != nil {
			t.Fatalf("Write returned error: %v", err)
		}
	}

	if dest != "/a/b/c" {
		t.Errorf("Dest = %q, want /a/b/c", dest)
	}
	want := chunks[0] + chunks[1]
	if underlying.String() != want {
		t.Errorf("underlying forwarded output = %q, want %q (every byte forwarded unmodified)", underlying.String(), want)
	}
}

func TestReportPathTapIgnoresNonMatchingLines(t *testing.T) {
	var underlying bytes.Buffer
	var dest string
	tap := &reportPathTap{Underlying: &underlying, Prefix: reportWrittenPrefix, Dest: &dest}

	if _, err := tap.Write([]byte("just some output\nnothing to see here\n")); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if dest != "" {
		t.Errorf("Dest = %q, want empty (no matching line)", dest)
	}
}
