package suite

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/thecoons/myrtille/internal/config"
)

// waitDelayBuffer pads a scenario's own service.stop_timeout when sizing
// how long RunScenario waits after sending SIGTERM before giving up and
// force-killing the child — the child's graceful shutdown (Stop, then
// teardown, then writing its report) takes a bit longer than
// stop_timeout alone bounds.
const waitDelayBuffer = 10 * time.Second

// defaultWaitDelay is used when the scenario has no service.start_command
// at all (nothing to gracefully stop) — still generous enough for
// teardown.steps and report writing to finish.
const defaultWaitDelay = 30 * time.Second

// reportWrittenPrefix is the exact line newRunCmd prints on success (see
// cmd/myrtille/main.go) — an internal contract of this same binary, not a
// third-party format, safe to match directly.
const reportWrittenPrefix = "report written to "

// ScenarioResult reports how one scenario's subprocess run went.
type ScenarioResult struct {
	ConfigPath string
	ReportDir  string
	ExitCode   int
	Passed     bool
}

// RunScenario runs configPath as a separate `<myrtilleBin> run --config
// configPath` subprocess — not an in-process orchestrator.Run call, see
// the package doc comment for why. stdout/stderr are streamed live
// through to out/errOut (a human watching `myrtille run --suite` sees the
// same progress as a plain `myrtille run`), while stdout is also scanned
// for the "report written to %s" line to recover the child's report
// directory.
//
// ctx cancellation sends SIGTERM to the child (not the default SIGKILL —
// see docs/plans/suite-mode.md tranche 0) so its own graceful shutdown
// (service Stop, teardown, report) still runs; WaitDelay is sized off the
// scenario's own service.stop_timeout when it configures
// service.start_command, so a generous per-scenario timeout doesn't get
// cut short by an unrelated, smaller default here.
//
// skipServiceLifecycle adds --skip-service-lifecycle to the child's
// arguments — for restart_between_runs: false, where the suite process
// itself manages one shared service instance around the whole suite (see
// docs/plans/suite-mode.md) rather than each scenario managing its own.
func RunScenario(ctx context.Context, myrtilleBin, configPath string, skipServiceLifecycle bool, out, errOut io.Writer) (*ScenarioResult, error) {
	args := []string{"run", "--config", configPath}
	if skipServiceLifecycle {
		args = append(args, "--skip-service-lifecycle")
	}
	cmd := exec.CommandContext(ctx, myrtilleBin, args...)
	cmd.Cancel = func() error {
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = waitDelayForScenario(configPath, skipServiceLifecycle)

	var reportDir string
	cmd.Stdout = &reportPathTap{Underlying: out, Prefix: reportWrittenPrefix, Dest: &reportDir}
	cmd.Stderr = errOut

	runErr := cmd.Run()

	exitCode := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			return nil, fmt.Errorf("running scenario %q: %w", configPath, runErr)
		}
	}

	return &ScenarioResult{
		ConfigPath: configPath,
		ReportDir:  reportDir,
		ExitCode:   exitCode,
		Passed:     exitCode == 0,
	}, nil
}

// waitDelayForScenario loads configPath just far enough to check
// service.stop_timeout — best-effort: if the config can't even be loaded,
// RunScenario's own subprocess will fail identically and report the real
// error, so a generic fallback here is fine. When skipServiceLifecycle is
// true, the child never stops a service itself (the suite process does,
// separately), so stop_timeout doesn't apply here.
func waitDelayForScenario(configPath string, skipServiceLifecycle bool) time.Duration {
	if skipServiceLifecycle {
		return defaultWaitDelay
	}
	cfg, err := config.Load(configPath)
	if err != nil || cfg.Service.StartCommand == "" {
		return defaultWaitDelay
	}
	return cfg.Service.StopTimeout.Duration() + waitDelayBuffer
}

// reportPathTap forwards every write unmodified to Underlying (live
// streaming), while also scanning line by line for the first line
// starting with Prefix, capturing the remainder into *Dest.
type reportPathTap struct {
	Underlying io.Writer
	Prefix     string
	Dest       *string
	buf        []byte
}

func (t *reportPathTap) Write(p []byte) (int, error) {
	n, err := t.Underlying.Write(p)
	if err != nil {
		return n, err
	}

	t.buf = append(t.buf, p...)
	for {
		i := bytes.IndexByte(t.buf, '\n')
		if i < 0 {
			break
		}
		line := string(t.buf[:i])
		t.buf = t.buf[i+1:]
		if *t.Dest == "" && strings.HasPrefix(line, t.Prefix) {
			*t.Dest = strings.TrimPrefix(line, t.Prefix)
		}
	}
	return n, nil
}
