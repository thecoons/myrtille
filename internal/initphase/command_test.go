package initphase

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/thecoons/myrtille/internal/config"
)

func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("init.command runs via `sh -c`, a POSIX shell")
	}
}

func TestRunCommandLoadsWrittenState(t *testing.T) {
	skipOnWindows(t)

	cfg := &config.Config{
		Init: config.InitConfig{
			Command:        `echo "{\"user_ids\":[\"user-1\",\"user-2\"]}" > "$MYRTILLE_STATE_OUTPUT"`,
			CommandTimeout: config.Duration(5 * time.Second),
		},
	}

	var stdout, stderr bytes.Buffer
	summary, dict, err := RunCommand(context.Background(), cfg, &stdout, &stderr)
	if err != nil {
		t.Fatalf("RunCommand returned error: %v", err)
	}

	if summary.Command == nil || summary.Command.ExitCode != 0 || summary.Command.TimedOut {
		t.Fatalf("unexpected command summary: %+v", summary.Command)
	}
	if summary.Command.Command != cfg.Init.Command {
		t.Errorf("expected summary to record the command line, got %q", summary.Command.Command)
	}

	if dict == nil {
		t.Fatal("expected a non-nil dict on success")
	}
	if dict.Count("user_ids") != 2 {
		t.Fatalf("dict.Count(user_ids) = %d, want 2", dict.Count("user_ids"))
	}
}

func TestRunCommandInheritsEnvironment(t *testing.T) {
	skipOnWindows(t)
	t.Setenv("MYRTILLE_TEST_MARKER", "inherited-value")

	cfg := &config.Config{
		Init: config.InitConfig{
			Command:        `printf '{"marker":["%s"]}' "$MYRTILLE_TEST_MARKER" > "$MYRTILLE_STATE_OUTPUT"`,
			CommandTimeout: config.Duration(5 * time.Second),
		},
	}

	var stdout, stderr bytes.Buffer
	_, dict, err := RunCommand(context.Background(), cfg, &stdout, &stderr)
	if err != nil {
		t.Fatalf("RunCommand returned error: %v", err)
	}
	snap := dict.Snapshot()
	if len(snap["marker"]) != 1 || snap["marker"][0] != "inherited-value" {
		t.Fatalf("expected the parent environment to be inherited, got dict %+v", snap)
	}
}

func TestRunCommandStreamsStdoutStderr(t *testing.T) {
	skipOnWindows(t)

	cfg := &config.Config{
		Init: config.InitConfig{
			Command:        `echo "hello stdout"; echo "hello stderr" >&2; echo '{}' > "$MYRTILLE_STATE_OUTPUT"`,
			CommandTimeout: config.Duration(5 * time.Second),
		},
	}

	var stdout, stderr bytes.Buffer
	if _, _, err := RunCommand(context.Background(), cfg, &stdout, &stderr); err != nil {
		t.Fatalf("RunCommand returned error: %v", err)
	}
	if !strings.Contains(stdout.String(), "hello stdout") {
		t.Errorf("expected stdout to be streamed through, got %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "hello stderr") {
		t.Errorf("expected stderr to be streamed through, got %q", stderr.String())
	}
}

func TestRunCommandNonZeroExitFails(t *testing.T) {
	skipOnWindows(t)

	cfg := &config.Config{
		Init: config.InitConfig{
			Command:        `exit 7`,
			CommandTimeout: config.Duration(5 * time.Second),
		},
	}

	var stdout, stderr bytes.Buffer
	summary, dict, err := RunCommand(context.Background(), cfg, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for a non-zero exit code")
	}
	if !strings.Contains(err.Error(), "exited with code 7") {
		t.Errorf("expected the exit code in the error, got %v", err)
	}
	if dict != nil {
		t.Fatalf("expected a nil dict on failure, got %+v", dict)
	}
	if summary.Command == nil || summary.Command.ExitCode != 7 {
		t.Fatalf("unexpected command summary: %+v", summary.Command)
	}
}

func TestRunCommandTimeoutFails(t *testing.T) {
	skipOnWindows(t)

	cfg := &config.Config{
		Init: config.InitConfig{
			Command:        `sleep 5; echo '{}' > "$MYRTILLE_STATE_OUTPUT"`,
			CommandTimeout: config.Duration(50 * time.Millisecond),
		},
	}

	var stdout, stderr bytes.Buffer
	summary, dict, err := RunCommand(context.Background(), cfg, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error when init.command exceeds its timeout")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("expected a timeout error, got %v", err)
	}
	if dict != nil {
		t.Fatalf("expected a nil dict on timeout, got %+v", dict)
	}
	if summary.Command == nil || !summary.Command.TimedOut {
		t.Fatalf("unexpected command summary: %+v", summary.Command)
	}
}

func TestRunCommandMissingOutputFails(t *testing.T) {
	skipOnWindows(t)

	cfg := &config.Config{
		Init: config.InitConfig{
			// Exits 0 without ever writing MYRTILLE_STATE_OUTPUT.
			Command:        `true`,
			CommandTimeout: config.Duration(5 * time.Second),
		},
	}

	var stdout, stderr bytes.Buffer
	summary, dict, err := RunCommand(context.Background(), cfg, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error when the command never writes MYRTILLE_STATE_OUTPUT")
	}
	if dict != nil {
		t.Fatalf("expected a nil dict, got %+v", dict)
	}
	if summary.Command == nil || summary.Command.ExitCode != 0 {
		t.Fatalf("unexpected command summary: %+v", summary.Command)
	}
}

func TestRunCommandCleansUpOutputFile(t *testing.T) {
	skipOnWindows(t)

	before, err := filepath.Glob(filepath.Join(os.TempDir(), "myrtille-init-output-*.json"))
	if err != nil {
		t.Fatalf("globbing temp dir before run: %v", err)
	}

	cfg := &config.Config{
		Init: config.InitConfig{
			Command:        `echo '{}' > "$MYRTILLE_STATE_OUTPUT"`,
			CommandTimeout: config.Duration(5 * time.Second),
		},
	}

	var stdout, stderr bytes.Buffer
	if _, _, err := RunCommand(context.Background(), cfg, &stdout, &stderr); err != nil {
		t.Fatalf("RunCommand returned error: %v", err)
	}

	after, err := filepath.Glob(filepath.Join(os.TempDir(), "myrtille-init-output-*.json"))
	if err != nil {
		t.Fatalf("globbing temp dir after run: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("expected the output file to be cleaned up, before=%v after=%v", before, after)
	}
}
