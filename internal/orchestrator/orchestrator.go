// Package orchestrator wires the init, k6 run, and report phases together
// into the single pipeline the CLI drives. Service metrics scraping is no
// longer a separate Go-side phase here — see internal/k6run and
// pkg/promscrape — this package doesn't need to know about it.
package orchestrator

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/thecoons/myrtille/internal/config"
	"github.com/thecoons/myrtille/internal/initphase"
	"github.com/thecoons/myrtille/internal/k6gen"
	"github.com/thecoons/myrtille/internal/k6run"
	"github.com/thecoons/myrtille/internal/report"
	"github.com/thecoons/myrtille/internal/state"
)

// Run executes init -> k6 run -> report. It always returns a non-nil
// *report.Report, even when a phase fails, so
// the caller can still persist a report describing the failure; err is
// non-nil whenever the pipeline did not complete successfully, which
// callers should map to a non-zero process exit code.
//
// preloadedStateFile, when non-empty, loads the state dict from that JSON
// file (see state.LoadFile) instead of running init.steps — for seeding
// logic too dynamic for init.steps' declarative template/count/extract
// shape (e.g. recursive generators with per-level arithmetic). It is
// mutually exclusive with init.steps; Report.Init stays nil in this mode,
// since no init phase ran. Pass "" for the existing init.steps behavior.
func Run(ctx context.Context, cfg *config.Config, preloadedStateFile string, stdout, stderr io.Writer) (*report.Report, error) {
	startedAt := time.Now()
	rpt := &report.Report{Name: cfg.Name, Ref: cfg.Ref, StartedAt: startedAt}

	if preloadedStateFile != "" && (len(cfg.Init.Steps) > 0 || cfg.Init.Command != "") {
		err := fmt.Errorf("--state-file is mutually exclusive with init.steps and init.command")
		rpt.FinishedAt = time.Now()
		rpt.Error = err.Error()
		return rpt, err
	}

	dict := state.New()

	if len(cfg.Teardown.Steps) > 0 {
		// Registered first so it runs last (defers are LIFO), after state
		// file / generated-script cleanup below. context.Background() is
		// deliberate, not ctx: ctx is cancelled on Ctrl+C/SIGTERM, and a
		// run being interrupted is exactly the case where its teardown
		// still needs to fire. Each request is still bounded by the
		// http.Client timeout inside initphase, so this can't hang forever.
		// dict is captured by reference: whichever branch below assigns it
		// (loaded from --state-file, produced by init.command, or mutated
		// in place by init.steps) is what teardown sees, even though this
		// defer is registered before that assignment happens.
		defer func() {
			teardownSummary, tdErr := initphase.RunTeardown(context.Background(), cfg, dict)
			rpt.Teardown = teardownSummary
			if tdErr != nil {
				rpt.TeardownErrors = append(rpt.TeardownErrors, tdErr.Error())
			}
		}()
	}

	switch {
	case preloadedStateFile != "":
		loaded, err := state.LoadFile(preloadedStateFile)
		if err != nil {
			rpt.FinishedAt = time.Now()
			rpt.Error = fmt.Sprintf("loading state file failed: %v", err)
			return rpt, fmt.Errorf("loading state file failed: %w", err)
		}
		dict = loaded

	case cfg.Init.Command != "":
		summary, loaded, err := initphase.RunCommand(ctx, cfg, stdout, stderr)
		rpt.Init = summary
		if err != nil {
			rpt.FinishedAt = time.Now()
			rpt.Error = fmt.Sprintf("init command failed: %v", err)
			return rpt, fmt.Errorf("init command failed: %w", err)
		}
		dict = loaded

	default:
		initSummary, err := initphase.Run(ctx, cfg, dict)
		rpt.Init = initSummary
		if err != nil {
			rpt.FinishedAt = time.Now()
			rpt.Error = fmt.Sprintf("init phase failed: %v", err)
			return rpt, fmt.Errorf("init phase failed: %w", err)
		}
	}

	if err := initphase.Derive(cfg, dict); err != nil {
		rpt.FinishedAt = time.Now()
		rpt.Error = fmt.Sprintf("init derive failed: %v", err)
		return rpt, fmt.Errorf("init derive failed: %w", err)
	}

	stateFilePath, err := dict.WriteTempFile()
	if err != nil {
		rpt.FinishedAt = time.Now()
		rpt.Error = fmt.Sprintf("writing state file failed: %v", err)
		return rpt, err
	}
	defer os.Remove(stateFilePath)
	if len(cfg.Teardown.Steps) > 0 {
		// Printed so the state file can be recovered for a manual
		// `myrtille teardown --state-file` run if this process is killed
		// hard enough (e.g. kill -9) to skip the deferred cleanup above.
		fmt.Fprintf(stderr, "state file: %s\n", stateFilePath)
	}

	scriptPath := cfg.K6ScriptPath()
	if cfg.K6.Script == "" {
		var genCleanup func()
		scriptPath, genCleanup, err = k6gen.Generate(cfg)
		if err != nil {
			rpt.FinishedAt = time.Now()
			rpt.Error = fmt.Sprintf("generating k6 scenario failed: %v", err)
			return rpt, fmt.Errorf("generating k6 scenario failed: %w", err)
		}
		defer genCleanup()
	}

	k6Result, k6Err := k6run.Run(ctx, cfg, scriptPath, stateFilePath, stdout, stderr)

	rpt.K6 = k6Result
	rpt.FinishedAt = time.Now()

	if k6Err != nil {
		rpt.Error = fmt.Sprintf("k6 run failed: %v", k6Err)
		return rpt, k6Err
	}
	if !k6Result.Passed {
		err := fmt.Errorf("k6 run did not pass (exit code %d)", k6Result.ExitCode)
		rpt.Error = err.Error()
		return rpt, err
	}

	return rpt, nil
}
