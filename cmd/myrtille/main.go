// Command myrtille orchestrates a load-test run against a service: bring
// it into a known state (init), run a k6 scenario against it while
// scraping its /metrics endpoint, and write a report of what happened.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/thecoons/myrtille/internal/config"
	"github.com/thecoons/myrtille/internal/initphase"
	"github.com/thecoons/myrtille/internal/orchestrator"
	"github.com/thecoons/myrtille/internal/servicelifecycle"
	"github.com/thecoons/myrtille/internal/state"
	"github.com/thecoons/myrtille/internal/style"
	"github.com/thecoons/myrtille/internal/suite"
)

// version is set at build time via -ldflags "-X main.version=..." (used by
// the release workflow). Left at its default, it falls back to the module
// version Go embeds automatically for `go install pkg@version`.
var version = "dev"

func init() {
	if version != "dev" {
		return
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		version = info.Main.Version
	}
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	err := newRootCmd().ExecuteContext(ctx)
	if err == nil {
		return
	}

	if ece, ok := errors.AsType[*exitCodeError](err); ok {
		style.New(os.Stderr).Fail("Error: %s", ece.Error())
		os.Exit(ece.code)
	}
	style.New(os.Stderr).Fail("Error: %s", err)
	os.Exit(1)
}

// exitCodeError lets the run command propagate k6's own exit code as
// myrtille's process exit code, rather than always exiting 1.
type exitCodeError struct {
	code int
	err  error
}

func (e *exitCodeError) Error() string { return e.err.Error() }
func (e *exitCodeError) Unwrap() error { return e.err }

func newRootCmd() *cobra.Command {
	var configPath string
	var envFilePath string

	root := &cobra.Command{
		Use:           "myrtille",
		Short:         "Orchestrate k6 load tests: init service state, run scenarios, and report results.",
		Version:       version,
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	root.PersistentFlags().StringVarP(&configPath, "config", "c", "myrtille.yaml", "path to the myrtille config file")
	root.PersistentFlags().StringVar(&envFilePath, "env-file", "",
		"path to a .env file of defaults to merge into the process environment (overrides the config's own env_file field; resolved relative to the current directory)")

	root.AddCommand(newRunCmd(&configPath, &envFilePath))
	root.AddCommand(newInitCmd(&configPath, &envFilePath))
	root.AddCommand(newTeardownCmd(&configPath, &envFilePath))

	return root
}

// loadConfig loads the myrtille config at configPath, overriding its
// env_file field with envFilePath when the --env-file flag was given.
func loadConfig(configPath, envFilePath string) (*config.Config, error) {
	var opts []config.LoadOption
	if envFilePath != "" {
		opts = append(opts, config.WithEnvFileOverride(envFilePath))
	}
	return config.Load(configPath, opts...)
}

func newRunCmd(configPath, envFilePath *string) *cobra.Command {
	var preloadedStateFile string
	var suitePath string
	var skipServiceLifecycle bool

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the init phase, then k6 (with metrics scraping), then write a report",
		RunE: func(cmd *cobra.Command, args []string) error {
			if suitePath != "" {
				if cmd.Flags().Changed("config") {
					return fmt.Errorf("--suite is mutually exclusive with --config")
				}
				return runSuite(cmd, suitePath)
			}

			cfg, err := loadConfig(*configPath, *envFilePath)
			if err != nil {
				return err
			}

			rpt, runErr := orchestrator.Run(cmd.Context(), cfg, preloadedStateFile, skipServiceLifecycle, cmd.OutOrStdout(), cmd.ErrOrStderr())

			if outDir, writeErr := rpt.WriteFiles(cfg.ReportOutputDir(), cfg.Report.Formats); writeErr != nil {
				style.New(cmd.ErrOrStderr()).Warn("failed to write report: %v", writeErr)
			} else {
				// Left as a plain, unstyled line deliberately: --suite
				// (internal/suite/runner.go's reportPathTap) scans stdout
				// for this exact "report written to " prefix at the very
				// start of the line to recover each scenario's report
				// directory — a symbol prefix here would break that match.
				fmt.Fprintf(cmd.OutOrStdout(), "report written to %s\n", outDir)
			}

			if runErr != nil && rpt.K6 != nil {
				return &exitCodeError{code: rpt.K6.ExitCode, err: runErr}
			}
			return runErr
		},
	}
	cmd.Flags().StringVar(&suitePath, "suite", "",
		"path to a suite.yaml file listing multiple scenario configs to run in order (mutually exclusive with --config)")
	cmd.Flags().StringVar(&preloadedStateFile, "state-file", "",
		"path to a pre-existing state JSON file to load instead of running init.steps (mutually exclusive with init.steps)")
	cmd.Flags().BoolVar(&skipServiceLifecycle, "skip-service-lifecycle", false,
		"never start/stop service.managed even if configured (for myrtille run --suite's own use with restart_between_runs: false; a service.base_url is still required to already be reachable)")
	_ = cmd.Flags().MarkHidden("skip-service-lifecycle")

	return cmd
}

// runSuite loads the suite file at path and runs each listed scenario in
// order, each as its own `myrtille run` subprocess (see
// docs/plans/suite-mode.md tranche 0's finding on why: an in-process loop
// would leak process-global env state — os.Setenv from config.Load —
// between scenarios). One scenario failing (or even failing to launch at
// all) does not stop the rest — matches "get every scenario's report from
// one CI step" over "stop at the first red". The overall exit code is
// non-zero if any scenario failed.
//
// When s.RestartsBetweenRuns() is false, runSuite itself starts one
// shared service instance (using the first scenario's service config —
// validated by suite.Load to require service.managed and a consistent
// service.base_url across every scenario) before the loop,
// stops it once after, and every scenario subprocess runs with
// --skip-service-lifecycle so it never tries to manage its own instance.
// The default (true) needs none of this: each scenario already
// starts/stops its own service independently, which restarts it between
// scenarios for free.
func runSuite(cmd *cobra.Command, path string) error {
	s, err := suite.Load(path)
	if err != nil {
		return err
	}

	myrtilleBin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving myrtille's own executable path to re-run it per scenario: %w", err)
	}

	scenarioPaths := s.ScenarioPaths()
	skipServiceLifecycle := !s.RestartsBetweenRuns()

	if skipServiceLifecycle {
		firstCfg, err := config.Load(scenarioPaths[0])
		if err != nil {
			return fmt.Errorf("loading %s to start the shared service: %w", scenarioPaths[0], err)
		}
		handle, err := servicelifecycle.Start(firstCfg, cmd.ErrOrStderr())
		if err != nil {
			return fmt.Errorf("starting shared service: %w", err)
		}
		style.New(cmd.OutOrStdout()).Done("shared service started (ready after %s)", handle.ReadyAfter())
		defer func() {
			result := handle.Stop(firstCfg)
			if result.Err != nil {
				style.New(cmd.ErrOrStderr()).Fail("stopping shared service failed: %v", result.Err)
			} else {
				style.New(cmd.OutOrStdout()).Done("shared service stopped (signal=%s, clean=%v)", result.Signal, result.Clean)
			}
		}()
	}

	results := make([]*suite.ScenarioResult, 0, len(scenarioPaths))
	for _, p := range scenarioPaths {
		if cmd.Context().Err() != nil {
			// The suite itself was interrupted (e.g. Ctrl+C) while an
			// earlier scenario was running: don't attempt further
			// scenarios, which would fail instantly against an
			// already-cancelled context anyway. The shared-service Stop
			// deferred above still runs regardless.
			break
		}
		fmt.Fprintf(cmd.OutOrStdout(), "\n=== scenario: %s ===\n", p)
		result, err := suite.RunScenario(cmd.Context(), myrtilleBin, p, skipServiceLifecycle, cmd.OutOrStdout(), cmd.ErrOrStderr())
		if err != nil {
			style.New(cmd.ErrOrStderr()).Fail("scenario %q failed to run: %v", p, err)
			result = &suite.ScenarioResult{ConfigPath: p, ExitCode: -1, Passed: false}
		}
		results = append(results, result)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "\n=== suite summary ===\n")
	anyFailed := false
	for _, r := range results {
		status := "PASS"
		if !r.Passed {
			status = "FAIL"
			anyFailed = true
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%-4s  %-40s  %s\n", status, r.ConfigPath, nonEmpty(r.ReportDir, "(no report)"))
	}

	if anyFailed {
		return &exitCodeError{code: 1, err: fmt.Errorf("one or more scenarios in the suite failed")}
	}
	return nil
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func newInitCmd(configPath, envFilePath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Run only the init phase and print the resulting state dictionary (for debugging a config)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(*configPath, *envFilePath)
			if err != nil {
				return err
			}

			dict := state.New()
			summary, err := initphase.Run(cmd.Context(), cfg, dict)
			if err != nil {
				return err
			}

			for _, step := range summary.Steps {
				fmt.Fprintf(cmd.OutOrStdout(), "%s: %d request(s), extracted %v\n", step.Name, step.Requests, step.Extracted)
			}

			if err := initphase.Derive(cfg, dict); err != nil {
				return err
			}

			data, err := json.MarshalIndent(dict, "", "  ")
			if err != nil {
				return fmt.Errorf("marshaling state dict: %w", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(data))

			return nil
		},
	}
}

func newTeardownCmd(configPath, envFilePath *string) *cobra.Command {
	var stateFilePath string

	cmd := &cobra.Command{
		Use:   "teardown",
		Short: "Run only the teardown phase against an existing state file (e.g. to clean up after a run that was killed before its own cleanup could run)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(*configPath, *envFilePath)
			if err != nil {
				return err
			}

			dict, err := state.LoadFile(stateFilePath)
			if err != nil {
				return err
			}

			summary, err := initphase.RunTeardown(cmd.Context(), cfg, dict)
			for _, step := range summary.Steps {
				fmt.Fprintf(cmd.OutOrStdout(), "%s: %d request(s), extracted %v\n", step.Name, step.Requests, step.Extracted)
			}
			return err
		},
	}
	cmd.Flags().StringVar(&stateFilePath, "state-file", "", "path to a state JSON file previously written by `run` (required)")
	_ = cmd.MarkFlagRequired("state-file")

	return cmd
}
