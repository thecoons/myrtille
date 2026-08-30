// Package orchestrator wires the init, k6 run, metrics scrape, and report
// phases together into the single pipeline the CLI drives.
package orchestrator

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/antobarth/myrtille/internal/config"
	"github.com/antobarth/myrtille/internal/initphase"
	"github.com/antobarth/myrtille/internal/k6run"
	"github.com/antobarth/myrtille/internal/metrics"
	"github.com/antobarth/myrtille/internal/report"
	"github.com/antobarth/myrtille/internal/state"
)

// Run executes init -> (k6 run + metrics scrape, concurrently) -> report.
// It always returns a non-nil *report.Report, even when a phase fails, so
// the caller can still persist a report describing the failure; err is
// non-nil whenever the pipeline did not complete successfully, which
// callers should map to a non-zero process exit code.
func Run(ctx context.Context, cfg *config.Config, stdout, stderr io.Writer) (*report.Report, error) {
	startedAt := time.Now()
	rpt := &report.Report{Name: cfg.Name, Ref: cfg.Ref, StartedAt: startedAt}

	dict := state.New()
	initSummary, err := initphase.Run(ctx, cfg, dict)
	rpt.Init = initSummary
	if err != nil {
		rpt.FinishedAt = time.Now()
		rpt.Error = fmt.Sprintf("init phase failed: %v", err)
		return rpt, fmt.Errorf("init phase failed: %w", err)
	}

	stateFilePath, err := dict.WriteTempFile()
	if err != nil {
		rpt.FinishedAt = time.Now()
		rpt.Error = fmt.Sprintf("writing state file failed: %v", err)
		return rpt, err
	}
	defer os.Remove(stateFilePath)

	var scraper *metrics.Scraper
	scrapeCtx, cancelScrape := context.WithCancel(ctx)
	defer cancelScrape()
	scrapeDone := make(chan struct{})
	if cfg.Service.Metrics.URL != "" {
		scraper = metrics.NewScraper(cfg.Service.Metrics.URL, cfg.Service.Metrics.Interval.Duration())
		go func() {
			defer close(scrapeDone)
			scraper.Run(scrapeCtx)
		}()
	} else {
		close(scrapeDone)
	}

	k6Result, k6Err := k6run.Run(ctx, cfg, stateFilePath, stdout, stderr)

	cancelScrape()
	<-scrapeDone

	rpt.K6 = k6Result
	if scraper != nil {
		samples := scraper.Samples()
		rpt.MetricSeries = metrics.Summarize(samples)
		rpt.MetricSamples = samples
		for _, e := range scraper.Errors() {
			rpt.ScrapeErrors = append(rpt.ScrapeErrors, e.Error())
		}
	}
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
