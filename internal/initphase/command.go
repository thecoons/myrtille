package initphase

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/thecoons/myrtille/internal/config"
	"github.com/thecoons/myrtille/internal/state"
)

// stateOutputEnvVar is the env var through which RunCommand tells
// init.command where to write its resulting state dict.
const stateOutputEnvVar = "MYRTILLE_STATE_OUTPUT"

// CommandResult reports how an init.command execution went, for a minimal
// report summary when init seeding is delegated to an external process
// rather than declarative init.steps.
type CommandResult struct {
	Command  string
	ExitCode int
	Duration time.Duration
	TimedOut bool
}

// RunCommand runs cfg.Init.Command (a shell command line, via `sh -c`),
// inheriting the current process's environment exactly like k6.script — no
// templating, so ordinary env vars just work. The command must write a
// state.Dict-shaped JSON object (the same flat {"key": [...]} shape
// --state-file loads, see state.LoadFile) to the path given via the
// MYRTILLE_STATE_OUTPUT env var before exiting 0; stdout/stderr are
// streamed through unmodified, for visibility during a long seed. A
// non-zero exit or exceeding cfg.Init.CommandTimeout fails the run,
// matching init.steps' fail-fast behavior on an HTTP error — the returned
// *state.Dict is nil whenever err is non-nil.
func RunCommand(ctx context.Context, cfg *config.Config, stdout, stderr io.Writer) (*Summary, *state.Dict, error) {
	outputFile, err := os.CreateTemp("", "myrtille-init-output-*.json")
	if err != nil {
		return nil, nil, fmt.Errorf("creating init.command output file: %w", err)
	}
	outputPath := outputFile.Name()
	outputFile.Close()
	defer os.Remove(outputPath)

	timeout := cfg.Init.CommandTimeout.Duration()
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, "sh", "-c", cfg.Init.Command)
	cmd.Env = append(os.Environ(), stateOutputEnvVar+"="+outputPath)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// Without WaitDelay, Run only returns once every I/O pipe reaches EOF —
	// if the command spawns a child of its own (e.g. backgrounding a
	// process), killing the direct "sh" process on timeout isn't enough:
	// an orphaned descendant can keep the pipe open indefinitely. WaitDelay
	// forces the pipes closed shortly after cancellation regardless.
	cmd.WaitDelay = 2 * time.Second

	start := time.Now()
	runErr := cmd.Run()
	duration := time.Since(start)

	result := &CommandResult{Command: cfg.Init.Command, Duration: duration}
	summary := &Summary{Command: result}

	if runErr != nil && cctx.Err() != nil && errors.Is(cctx.Err(), context.DeadlineExceeded) {
		result.TimedOut = true
		return summary, nil, fmt.Errorf("init.command timed out after %s", timeout)
	}

	if runErr != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](runErr); ok {
			result.ExitCode = exitErr.ExitCode()
			return summary, nil, fmt.Errorf("init.command exited with code %d", result.ExitCode)
		}
		return summary, nil, fmt.Errorf("running init.command: %w", runErr)
	}

	dict, err := state.LoadFile(outputPath)
	if err != nil {
		return summary, nil, fmt.Errorf("loading init.command output: %w", err)
	}

	return summary, dict, nil
}
