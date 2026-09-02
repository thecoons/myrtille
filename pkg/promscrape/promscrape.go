// Package promscrape is an xk6 extension spike: it proves that Go code
// running outside the normal per-iteration JS call flow (a background
// goroutine started once from setup()) can register a k6 metric and push
// samples into k6's own metrics pipeline, so they show up in the live
// web-dashboard (see docs/plans/xk6-live-dashboard.md, step 0).
//
// This is deliberately minimal: one hardcoded metric, a naive single-value
// scrape (no real Prometheus parsing yet — that's step 2). Nothing here is
// meant to survive past the spike.
package promscrape

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/grafana/sobek"
	"go.k6.io/k6/v2/js/common"
	"go.k6.io/k6/v2/js/modules"
	"go.k6.io/k6/v2/metrics"
)

func init() {
	modules.Register("k6/x/promscrape", New())
}

type RootModule struct{}

type ModuleInstance struct {
	vu modules.VU
}

var (
	_ modules.Module   = &RootModule{}
	_ modules.Instance = &ModuleInstance{}
)

func New() *RootModule {
	return &RootModule{}
}

func (*RootModule) NewModuleInstance(vu modules.VU) modules.Instance {
	return &ModuleInstance{vu: vu}
}

func (mi *ModuleInstance) Exports() modules.Exports {
	return modules.Exports{
		Named: map[string]any{
			"Scraper": mi.XScraper,
		},
	}
}

// scraper is the object returned to JS by `new promscrape.Scraper(name)`.
type scraper struct {
	metric *metrics.Metric
	vu     modules.VU
}

// XScraper is the Scraper constructor. It must be called at init scope (top
// level of the script) since metric registration requires vu.InitEnv(),
// which is only non-nil during init — the same constraint k6/metrics'
// Counter/Gauge/Trend/Rate constructors have.
func (mi *ModuleInstance) XScraper(call sobek.ConstructorCall, rt *sobek.Runtime) *sobek.Object {
	initEnv := mi.vu.InitEnv()
	if initEnv == nil {
		common.Throw(rt, fmt.Errorf("promscrape.Scraper must be constructed in the init context"))
	}

	name := call.Argument(0).String()

	m, err := initEnv.Registry.NewMetric(name, metrics.Gauge, metrics.Default)
	if err != nil {
		common.Throw(rt, err)
	}

	s := &scraper{metric: m, vu: mi.vu}

	obj := rt.NewObject()
	if err := obj.Set("start", rt.ToValue(s.start)); err != nil {
		common.Throw(rt, err)
	}

	return obj
}

// requestsTotalRe extracts the value of the stub service's
// stub_requests_total counter from a Prometheus text-exposition payload.
// Spike-only: step 2 replaces this with a real parser (reusing
// internal/metrics.Parse) that discovers every series, not just one
// hardcoded name.
var requestsTotalRe = regexp.MustCompile(`(?m)^stub_requests_total\s+(\S+)$`)

// start begins polling url every intervalMs, pushing the extracted value as
// a sample on s.metric. Must be called with a non-nil vu.State() (e.g. from
// setup()), since that's where the Samples channel and Context come from.
// The goroutine it starts keeps running past the JS call that started it —
// that's the second half of what this spike is testing.
func (s *scraper) start(url string, intervalMs int) error {
	state := s.vu.State()
	if state == nil {
		return fmt.Errorf("promscrape: start() must be called with a VU state (e.g. from setup())")
	}

	samples := state.Samples
	logger := state.Logger
	metric := s.metric

	// Deliberately not s.vu.Context(): when start() is called from setup(),
	// that context is cancelled the moment setup() returns (see k6's
	// internal/js/runner.go Setup(), which wraps it in a
	// context.WithTimeout + defer cancel() scoped to the setup() call
	// itself) — found by this spike after the metric silently never
	// appeared with vu.Context(). Using it here would make every push
	// after setup() returns a silent no-op via PushIfNotDone.
	ctx := context.Background()

	interval := time.Duration(intervalMs) * time.Millisecond
	client := &http.Client{Timeout: 5 * time.Second}

	// The engine closes the Samples channel once at the very end of the
	// whole run (not just setup()) — found by this spike too: k6 gives
	// extensions no run-scoped context or exit hook (vu.Events() exists,
	// but the event.Type it takes lives under k6's internal/event package
	// and can't be imported from outside go.k6.io/k6), so there is no
	// context this goroutine can wait on that lines up with that closure.
	// push recovers from the resulting "send on closed channel" panic and
	// flips stopped so later ticks skip sending instead of panicking again.
	var stopped atomic.Bool

	push := func(sample metrics.Sample) {
		defer func() {
			if recover() != nil {
				stopped.Store(true)
			}
		}()
		samples <- sample
	}

	poll := func() {
		if stopped.Load() {
			return
		}

		value, err := scrapeOne(ctx, client, url)
		if err != nil {
			logger.WithError(err).Warn("promscrape: scrape failed")
			return
		}

		push(metrics.Sample{
			TimeSeries: metrics.TimeSeries{Metric: metric, Tags: state.Tags.GetCurrentValues().Tags},
			Time:       time.Now(),
			Value:      value,
		})
	}

	go func() {
		poll()

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for range ticker.C {
			if stopped.Load() {
				return
			}
			poll()
		}
	}()

	return nil
}

func scrapeOne(ctx context.Context, client *http.Client, url string) (float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}

	match := requestsTotalRe.FindSubmatch(body)
	if match == nil {
		return 0, fmt.Errorf("stub_requests_total not found in %s", url)
	}

	return strconv.ParseFloat(string(match[1]), 64)
}
