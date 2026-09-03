package servicelifecycle

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/thecoons/myrtille/internal/config"
)

// testServerSrc is a minimal, real TCP-listening HTTP server, compiled
// once in TestMain and reused by every Stop test that needs a genuine
// process to signal — Stop's job (send a real signal to a real process
// group, then poll until a real port frees up) can't be verified against
// a fake/no-op subprocess.
const testServerSrc = `package main

import (
	"fmt"
	"net/http"
	"os"
)

func main() {
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	fmt.Println("test server listening, pid=", os.Getpid())
	if err := http.ListenAndServe("127.0.0.1:"+os.Args[1], nil); err != nil {
		fmt.Fprintln(os.Stderr, "listen error:", err)
		os.Exit(1)
	}
}
`

var testServerBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "servicelifecycle-testbin")
	if err != nil {
		fmt.Println("setting up test server bin:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	srcPath := filepath.Join(dir, "testserver.go")
	if err := os.WriteFile(srcPath, []byte(testServerSrc), 0o644); err != nil {
		fmt.Println("writing test server source:", err)
		os.Exit(1)
	}
	testServerBin = filepath.Join(dir, "testserver")
	build := exec.Command("go", "build", "-o", testServerBin, srcPath)
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Printf("building test server: %v\n%s\n", err, out)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

// freePort asks the OS for an unused TCP port by briefly binding to :0.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding a free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestResolveReadinessURLRelativePath(t *testing.T) {
	got, err := resolveReadinessURL("http://localhost:8080", "/healthz")
	if err != nil {
		t.Fatalf("resolveReadinessURL returned error: %v", err)
	}
	if got != "http://localhost:8080/healthz" {
		t.Errorf("got %q, want %q", got, "http://localhost:8080/healthz")
	}
}

func TestResolveReadinessURLAbsolute(t *testing.T) {
	got, err := resolveReadinessURL("http://localhost:8080", "http://localhost:9090/other")
	if err != nil {
		t.Fatalf("resolveReadinessURL returned error: %v", err)
	}
	if got != "http://localhost:9090/other" {
		t.Errorf("got %q, want %q", got, "http://localhost:9090/other")
	}
}

func testConfig(t *testing.T, baseURL, startCommand string, timeout, interval time.Duration) *config.Config {
	t.Helper()
	return &config.Config{
		Service: config.ServiceConfig{
			BaseURL: baseURL,
			Managed: &config.ManagedConfig{
				StartCommand: startCommand,
				Readiness: config.ReadinessConfig{
					URL:      "/healthz",
					Timeout:  config.Duration(timeout),
					Interval: config.Duration(interval),
				},
			},
		},
	}
}

func TestStartRefusesWhenSomethingAlreadyAnswersReadinessURL(t *testing.T) {
	var requests int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)

	// A start_command that would be trivially observable if it actually
	// ran (it never should, here) — writing a marker file lets the test
	// assert on that directly, not just infer it from Start's return.
	markerDir := t.TempDir()
	marker := markerDir + "/started"
	cfg := testConfig(t, ts.URL, "touch "+marker, 3*time.Second, 50*time.Millisecond)

	var stderr bytes.Buffer
	_, err := Start(cfg, &stderr)
	if err == nil {
		t.Fatal("expected Start to refuse when the readiness URL already answers, got nil error")
	}
	if !strings.Contains(err.Error(), "already responding") {
		t.Errorf("expected an already-responding error, got %q", err.Error())
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Error("expected start_command to never run, but its marker file was created")
	}
	if atomic.LoadInt32(&requests) == 0 {
		t.Error("expected at least one preflight probe to have reached the already-running service")
	}
}

func TestStartWaitsForReadiness(t *testing.T) {
	var readyAfterN int32 = 3
	var requests int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&requests, 1)
		if n < readyAfterN {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)

	cfg := testConfig(t, ts.URL, "echo starting", 5*time.Second, 50*time.Millisecond)

	var stderr bytes.Buffer
	handle, err := Start(cfg, &stderr)
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if handle.ReadyAfter() <= 0 {
		t.Errorf("expected a positive ReadyAfter, got %v", handle.ReadyAfter())
	}
	if atomic.LoadInt32(&requests) < readyAfterN {
		t.Errorf("expected at least %d readiness probes, got %d", readyAfterN, requests)
	}

	// No Stop yet (tranche 3) — clean up the started process ourselves so
	// the test doesn't leak it.
	killHandleForTest(t, handle)
}

func TestStartTimesOutWhenNeverReady(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(ts.Close)

	marker := "SERVICELIFECYCLE_TEST_MARKER"
	cfg := testConfig(t, ts.URL, fmt.Sprintf("echo %s; sleep 5", marker), 300*time.Millisecond, 50*time.Millisecond)

	var stderr bytes.Buffer
	_, err := Start(cfg, &stderr)
	if err == nil {
		t.Fatal("expected error when the service never becomes ready, got nil")
	}
	if !strings.Contains(err.Error(), "did not become ready") {
		t.Errorf("expected a readiness-timeout error, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), marker) {
		t.Errorf("expected the captured log tail (including %q) in the error, got %q", marker, err.Error())
	}
}

func TestStartFailsFastWhenCommandExitsEarly(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(ts.Close)

	// A generous timeout: if early-exit detection didn't work, Start would
	// block for this whole duration instead of returning almost instantly.
	const timeout = 4 * time.Second
	cfg := testConfig(t, ts.URL, "echo dying now; exit 3", timeout, 50*time.Millisecond)

	var stderr bytes.Buffer
	start := time.Now()
	_, err := Start(cfg, &stderr)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error when the service process exits before becoming ready, got nil")
	}
	if !strings.Contains(err.Error(), "exited before becoming ready") {
		t.Errorf("expected an early-exit error, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "dying now") {
		t.Errorf("expected the captured log tail in the error, got %q", err.Error())
	}
	if elapsed >= timeout/2 {
		t.Errorf("expected Start to return promptly on early exit, took %v (timeout was %v)", elapsed, timeout)
	}
}

// killHandleForTest force-kills the process group a Handle tracks, as a
// test-cleanup safety net for tests that don't exercise Stop themselves
// (or where Stop is expected to fail to actually stop it).
func killHandleForTest(t *testing.T, h *Handle) {
	t.Helper()
	t.Cleanup(func() {
		_ = syscall.Kill(-h.pgid, syscall.SIGKILL)
	})
}

func startTestServer(t *testing.T) (*config.Config, int) {
	t.Helper()
	port := freePort(t)
	cfg := &config.Config{
		Service: config.ServiceConfig{
			BaseURL: fmt.Sprintf("http://127.0.0.1:%d", port),
			Managed: &config.ManagedConfig{
				StartCommand: fmt.Sprintf("%s %d", testServerBin, port),
				StopSignal:   "TERM",
				StopTimeout:  config.Duration(3 * time.Second),
				Readiness: config.ReadinessConfig{
					URL:      "/healthz",
					Timeout:  config.Duration(3 * time.Second),
					Interval: config.Duration(50 * time.Millisecond),
				},
			},
		},
	}
	return cfg, port
}

func TestStartWritesServiceOutputToConfiguredLogFile(t *testing.T) {
	cfg, _ := startTestServer(t)
	logPath := filepath.Join(t.TempDir(), "service.log")
	cfg.Service.Managed.LogFile = logPath

	var stderr bytes.Buffer
	handle, err := Start(cfg, &stderr)
	if err != nil {
		t.Fatalf("Start returned error: %v (log: %s)", err, stderr.String())
	}
	killHandleForTest(t, handle)

	if !strings.Contains(stderr.String(), logPath) {
		t.Errorf("expected stderr to announce the log path %q, got %q", logPath, stderr.String())
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading configured log file: %v", err)
	}
	if !strings.Contains(string(data), "test server listening") {
		t.Errorf("expected the service's own stdout in %s, got: %s", logPath, data)
	}
}

func TestStartLogFileDirectoryCreatedIfMissing(t *testing.T) {
	cfg, _ := startTestServer(t)
	logPath := filepath.Join(t.TempDir(), "nested", "dir", "service.log")
	cfg.Service.Managed.LogFile = logPath

	var stderr bytes.Buffer
	handle, err := Start(cfg, &stderr)
	if err != nil {
		t.Fatalf("Start returned error: %v (log: %s)", err, stderr.String())
	}
	killHandleForTest(t, handle)

	if _, err := os.Stat(logPath); err != nil {
		t.Errorf("expected %s to exist (parent dirs auto-created), got: %v", logPath, err)
	}
}

func TestStopKeepsConfiguredLogFile(t *testing.T) {
	cfg, _ := startTestServer(t)
	logPath := filepath.Join(t.TempDir(), "service.log")
	cfg.Service.Managed.LogFile = logPath

	var stderr bytes.Buffer
	handle, err := Start(cfg, &stderr)
	if err != nil {
		t.Fatalf("Start returned error: %v (log: %s)", err, stderr.String())
	}

	handle.Stop(cfg)

	if _, err := os.Stat(logPath); err != nil {
		t.Errorf("expected the configured log file to survive Stop, got: %v", err)
	}
}

func TestStopRemovesDefaultTempLogFile(t *testing.T) {
	cfg, _ := startTestServer(t) // Service.LogFile left unset

	var stderr bytes.Buffer
	handle, err := Start(cfg, &stderr)
	if err != nil {
		t.Fatalf("Start returned error: %v (log: %s)", err, stderr.String())
	}
	tempLogPath := handle.logPath

	handle.Stop(cfg)

	if _, err := os.Stat(tempLogPath); !os.IsNotExist(err) {
		t.Errorf("expected the default temp log file %s to be removed after Stop, got: %v", tempLogPath, err)
	}
}

func TestStopSendsSignalAndConfirmsPortFree(t *testing.T) {
	cfg, port := startTestServer(t)

	var stderr bytes.Buffer
	handle, err := Start(cfg, &stderr)
	if err != nil {
		t.Fatalf("Start returned error: %v (log: %s)", err, stderr.String())
	}

	result := handle.Stop(cfg)
	if result.Err != nil {
		t.Fatalf("Stop returned an error: %v", result.Err)
	}
	if !result.Clean {
		t.Errorf("expected a clean stop, got %+v", result)
	}
	if result.Signal != "TERM" {
		t.Errorf("expected Signal %q, got %q", "TERM", result.Signal)
	}

	// The default Go process (no signal handler installed) dies
	// immediately on SIGTERM, so the port must be free right away — a
	// real, direct confirmation, not just trusting Stop's own report.
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		conn.Close()
		t.Errorf("expected port %d to be free after Stop, but it still accepted a connection", port)
	}
}

func TestStopIsBestEffortWhenProcessSurvivesSignal(t *testing.T) {
	// SIGHUP's default disposition terminates a process too — but a
	// `trap` in the launched shell can catch it, simulating a service
	// that doesn't die from the configured signal within StopTimeout,
	// without needing a second real test binary.
	port := freePort(t)
	cfg := &config.Config{
		Service: config.ServiceConfig{
			BaseURL: fmt.Sprintf("http://127.0.0.1:%d", port),
			Managed: &config.ManagedConfig{
				StartCommand: fmt.Sprintf("trap '' HUP; %s %d", testServerBin, port),
				StopSignal:   "HUP",
				StopTimeout:  config.Duration(500 * time.Millisecond),
				Readiness: config.ReadinessConfig{
					URL:      "/healthz",
					Timeout:  config.Duration(3 * time.Second),
					Interval: config.Duration(50 * time.Millisecond),
				},
			},
		},
	}

	var stderr bytes.Buffer
	handle, err := Start(cfg, &stderr)
	if err != nil {
		t.Fatalf("Start returned error: %v (log: %s)", err, stderr.String())
	}
	t.Cleanup(func() { _ = syscall.Kill(-handle.pgid, syscall.SIGKILL) })

	result := handle.Stop(cfg)
	if result.Err != nil {
		t.Fatalf("expected Stop to report no error even when the service ignores the signal (best-effort), got %v", result.Err)
	}
	if result.Clean {
		t.Error("expected Clean=false: the process traps HUP and should still be up")
	}
}

func TestStopReportsErrorForUnknownSignal(t *testing.T) {
	cfg, _ := startTestServer(t)
	cfg.Service.Managed.StopSignal = "BOGUS"

	var stderr bytes.Buffer
	handle, err := Start(cfg, &stderr)
	if err != nil {
		t.Fatalf("Start returned error: %v (log: %s)", err, stderr.String())
	}
	t.Cleanup(func() { _ = syscall.Kill(-handle.pgid, syscall.SIGKILL) })

	result := handle.Stop(cfg)
	if result.Err == nil {
		t.Fatal("expected an error for an unknown stop signal, got nil")
	}
}

// TestNewReturnsExternalLifecycleWhenManagedNil proves New(cfg) with
// Service.Managed == nil produces a Lifecycle that does nothing observable
// at all — no `sh -c` ever launched, Start returns immediately with a nil
// Summary and no error, and Stop is safe to call (returns nil) even though
// nothing was ever started. This is the case orchestrator.Run hits for
// every project that doesn't configure service.managed — by far the
// common case — so it must stay a true no-op.
func TestNewReturnsExternalLifecycleWhenManagedNil(t *testing.T) {
	cfg := &config.Config{Service: config.ServiceConfig{BaseURL: "http://127.0.0.1:1"}}

	lifecycle := New(cfg)
	if _, ok := lifecycle.(externalLifecycle); !ok {
		t.Fatalf("expected New to return externalLifecycle, got %T", lifecycle)
	}

	var stderr bytes.Buffer
	start := time.Now()
	summary, err := lifecycle.Start(&stderr)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("expected no error from the external lifecycle's Start, got %v", err)
	}
	if summary != nil {
		t.Errorf("expected a nil Summary from the external lifecycle, got %+v", summary)
	}
	if stderr.Len() != 0 {
		t.Errorf("expected no stderr output from the external lifecycle, got %q", stderr.String())
	}
	if elapsed > 10*time.Millisecond {
		t.Errorf("expected Start to return immediately (no process/network involved), took %v", elapsed)
	}

	if result := lifecycle.Stop(); result != nil {
		t.Errorf("expected a nil StopResult from the external lifecycle, got %+v", result)
	}
}

// TestManagedLifecycleStartsAndStopsRealProcess proves the Lifecycle
// interface's managed implementation genuinely wires through to the same
// Start/Handle.Stop primitives exercised directly by the rest of this
// file — a real process is started and later stopped through the
// interface alone, not by calling Start/Handle.Stop directly.
func TestManagedLifecycleStartsAndStopsRealProcess(t *testing.T) {
	cfg, port := startTestServer(t)

	lifecycle := New(cfg)
	if _, ok := lifecycle.(*managedLifecycle); !ok {
		t.Fatalf("expected New to return *managedLifecycle, got %T", lifecycle)
	}

	var stderr bytes.Buffer
	summary, err := lifecycle.Start(&stderr)
	if err != nil {
		t.Fatalf("Start returned error: %v (log: %s)", err, stderr.String())
	}
	if summary == nil {
		t.Fatal("expected a non-nil Summary from the managed lifecycle")
	}
	if !strings.Contains(stderr.String(), "starting service...") {
		t.Errorf("expected a starting-service message on stderr, got %q", stderr.String())
	}

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	if conn, err := net.DialTimeout("tcp", addr, time.Second); err != nil {
		t.Errorf("expected the service to be reachable after Start, got: %v", err)
	} else {
		conn.Close()
	}

	result := lifecycle.Stop()
	if result == nil {
		t.Fatal("expected a non-nil StopResult from the managed lifecycle")
	}
	if !result.Clean {
		t.Errorf("expected a clean stop, got %+v", result)
	}

	if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		conn.Close()
		t.Errorf("expected port %d to be free after Stop, but it still accepted a connection", port)
	}
}
