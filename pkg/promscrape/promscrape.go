// Package promscrape is an xk6 extension: `import promscrape from
// "k6/x/promscrape"` lets a k6 script mirror a service's Prometheus
// /metrics endpoint into k6's own metrics pipeline, so every series shows
// up in k6's live web-dashboard alongside k6's own metrics — see
// docs/plans/xk6-live-dashboard.md.
//
// `new promscrape.Scraper(url)` must run in the init context: it does one
// synchronous discovery scrape of url, and registers one k6 metric per
// distinct series name found (Counter for Prometheus COUNTER/histogram-or-
// summary-derived _sum/_count, Gauge for GAUGE/UNTYPED — see
// internal/metrics.Parse), since k6 only allows metric registration during
// init (the same constraint k6/metrics' own Counter/Gauge/Trend/Rate
// constructors have). `.start(intervalMs)` then begins polling url every
// intervalMs from a background goroutine and pushing samples for whichever
// series were seen at construction time; call it from setup() (it needs a
// VU state, which init doesn't have).
//
// Two constraints discovered by the step-0 spike (see the plan doc) shape
// start()'s implementation: setup()'s vu.Context() is cancelled the moment
// setup() returns, so the background goroutine can't wait on it; and
// nothing in the public extension API signals a run ending, so the
// goroutine instead recovers from the "send on closed channel" panic k6
// produces when it closes the Samples channel at the very end of the run.
package promscrape

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/grafana/sobek"
	promsample "github.com/thecoons/myrtille/internal/metrics"
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

// scraper is the object returned to JS by `new promscrape.Scraper(url)`.
type scraper struct {
	vu     modules.VU
	url    string
	client *http.Client
	// series holds one entry per Prometheus series name seen at
	// construction time. A series absent from a later poll's response
	// (or that only appears later, not at construction) is silently
	// skipped — see the package doc.
	series map[string]*series
}

// series pairs a registered k6 metric with the delta-tracking state needed
// to turn a Prometheus counter's cumulative value into a per-tick delta.
type series struct {
	metric *metrics.Metric
	kind   promsample.Kind
	// last holds the previous raw value per label-set (keyed by
	// labelKey), so a counter series broken down by Prometheus labels
	// (e.g. by status code) gets an independent delta per label-set
	// rather than one shared across all of them. Only ever touched by
	// the single polling goroutine start() spawns, so no locking.
	last map[string]float64
}

// XScraper is the Scraper constructor. It must be called at init scope (top
// level of the script) since metric registration requires vu.InitEnv(),
// which is only non-nil during init.
func (mi *ModuleInstance) XScraper(call sobek.ConstructorCall, rt *sobek.Runtime) *sobek.Object {
	initEnv := mi.vu.InitEnv()
	if initEnv == nil {
		common.Throw(rt, fmt.Errorf("promscrape.Scraper must be constructed in the init context"))
	}

	url := call.Argument(0).String()
	client := &http.Client{Timeout: 5 * time.Second}

	discovered, err := fetch(client, url)
	if err != nil {
		common.Throw(rt, fmt.Errorf("promscrape: discovering series at %s: %w", url, err))
	}

	seriesByName := make(map[string]*series, len(discovered))
	for _, sample := range discovered {
		if _, ok := seriesByName[sample.Name]; ok {
			continue
		}

		k6Type := metrics.Gauge
		if sample.Kind == promsample.KindCounter {
			k6Type = metrics.Counter
		}

		m, err := initEnv.Registry.NewMetric(sample.Name, k6Type, metrics.Default)
		if err != nil {
			common.Throw(rt, fmt.Errorf("promscrape: registering metric %s: %w", sample.Name, err))
		}

		seriesByName[sample.Name] = &series{metric: m, kind: sample.Kind, last: make(map[string]float64)}
	}

	s := &scraper{vu: mi.vu, url: url, client: client, series: seriesByName}

	obj := rt.NewObject()
	if err := obj.Set("start", rt.ToValue(s.start)); err != nil {
		common.Throw(rt, err)
	}

	return obj
}

// start begins polling s.url every intervalMs, pushing one sample per
// series discovered at construction time. Must be called with a non-nil
// vu.State() (e.g. from setup()); calling it more than once is not
// supported (two goroutines would race on each series' delta state).
func (s *scraper) start(intervalMs int) error {
	state := s.vu.State()
	if state == nil {
		return fmt.Errorf("promscrape: start() must be called with a VU state (e.g. from setup())")
	}

	samples := state.Samples
	logger := state.Logger

	interval := time.Duration(intervalMs) * time.Millisecond

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

		observed, err := fetch(s.client, s.url)
		if err != nil {
			logger.WithError(err).Warn("promscrape: scrape failed")
			return
		}

		now := time.Now()
		baseTags := state.Tags.GetCurrentValues().Tags

		for _, sample := range observed {
			ser, ok := s.series[sample.Name]
			if !ok {
				continue
			}

			value, ok := ser.resolve(sample)
			if !ok {
				continue
			}

			tags := baseTags
			for k, v := range sample.Labels {
				tags = tags.With(k, v)
			}

			push(metrics.Sample{
				TimeSeries: metrics.TimeSeries{Metric: ser.metric, Tags: tags},
				Time:       now,
				Value:      value,
			})
		}
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

// resolve turns a freshly scraped sample into the value that should be
// pushed to k6, or false if there's nothing to push yet. A gauge's raw
// value always resolves. A counter resolves to the delta since the last
// scrape of the same label-set; its first observation has nothing to diff
// against, and a value lower than last time is treated as the target
// process having restarted (its counter reset to zero) — both cases store
// the new baseline and report nothing rather than a meaningless or
// negative delta.
func (ser *series) resolve(sample promsample.Sample) (float64, bool) {
	if ser.kind == promsample.KindGauge {
		return sample.Value, true
	}

	key := labelKey(sample.Labels)
	last, seen := ser.last[key]
	ser.last[key] = sample.Value

	if !seen || sample.Value < last {
		return 0, false
	}

	return sample.Value - last, true
}

// labelKey returns a stable string identity for a label set, so two
// scrapes of the same Prometheus label combination map to the same key
// regardless of map iteration order.
func labelKey(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}

	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
		b.WriteByte(';')
	}
	return b.String()
}

func fetch(client *http.Client, url string) ([]promsample.Sample, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return promsample.Parse(resp.Body, time.Now())
}
