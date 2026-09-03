// Package oteltrace is an xk6 extension: `import oteltrace from
// "k6/x/oteltrace"` runs a local OTLP/HTTP trace receiver for the duration
// of a k6 run, so a service under test can export its spans there instead
// of a real collector — see docs/plans/otel-span-metrics.md.
//
// `new oteltrace.Receiver()` must run in the init context: metric
// registration requires it (the same constraint k6/x/promscrape has, and
// the same constraint k6's own metrics.Trend/Rate constructors have).
// `.start()` then opens an HTTP server on defaultListenAddr handling
// `POST /v1/traces` (OTLP/HTTP, protobuf-encoded) and returns immediately;
// call it from setup() (it needs a VU state, which init doesn't have).
//
// Every span received is reduced to two samples — svc_span_duration (a
// Trend, in ms) and svc_span_errors (a Rate) — tagged by span_name (and
// otel_service, when the span's resource carries a service.name
// attribute), rather than one metric per span name: k6 only allows metric
// registration during init, and spans arrive continuously throughout the
// run rather than in one discovery pass the way promscrape's Prometheus
// scrape does, so there's no way to know span names ahead of time. Tagging
// a fixed pair of metrics sidesteps that entirely and matches how k6 tags
// its own http_req_duration by url — see the plan doc's "Décisions
// actées".
//
// Unlike k6/x/promscrape's single background polling goroutine, requests
// here arrive concurrently from Go's own http.Server (one goroutine per
// in-flight request), so multiple goroutines can call push() at the same
// time — channel sends are safe for concurrent callers on their own, no
// extra locking needed, but each goroutine still needs to run push()'s
// panic recovery independently, not share a single shared recover.
package oteltrace

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/grafana/sobek"
	"go.k6.io/k6/v2/js/common"
	"go.k6.io/k6/v2/js/modules"
	"go.k6.io/k6/v2/metrics"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func init() {
	modules.Register("k6/x/oteltrace", New())
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
			"Receiver": mi.XReceiver,
		},
	}
}

// defaultListenAddr is the standard OTLP/HTTP port — a service's own OTel
// SDK, configured to export to a local collector as it normally would,
// reaches this receiver without myrtille needing to communicate the
// address to it (see the plan doc's "Décisions actées" for why: the
// managed service starts before k6/this extension does, so there's no way
// to inject a dynamically-chosen port into its environment in time).
const defaultListenAddr = ":4318"

// receiver is the object returned to JS by `new oteltrace.Receiver()`.
type receiver struct {
	vu           modules.VU
	spanDuration *metrics.Metric // Trend, ms
	spanErrors   *metrics.Metric // Rate
	stats        *spanStats
}

// XReceiver is the Receiver constructor. It must be called at init scope
// (top level of the script) since metric registration requires
// vu.InitEnv(), which is only non-nil during init.
func (mi *ModuleInstance) XReceiver(call sobek.ConstructorCall, rt *sobek.Runtime) *sobek.Object {
	initEnv := mi.vu.InitEnv()
	if initEnv == nil {
		common.Throw(rt, fmt.Errorf("oteltrace.Receiver must be constructed in the init context"))
	}

	duration, err := initEnv.Registry.NewMetric("svc_span_duration", metrics.Trend, metrics.Time)
	if err != nil {
		common.Throw(rt, fmt.Errorf("oteltrace: registering svc_span_duration: %w", err))
	}
	errors, err := initEnv.Registry.NewMetric("svc_span_errors", metrics.Rate, metrics.Default)
	if err != nil {
		common.Throw(rt, fmt.Errorf("oteltrace: registering svc_span_errors: %w", err))
	}

	r := &receiver{vu: mi.vu, spanDuration: duration, spanErrors: errors, stats: newSpanStats()}

	obj := rt.NewObject()
	if err := obj.Set("start", rt.ToValue(r.start)); err != nil {
		common.Throw(rt, err)
	}

	return obj
}

// spanSample is one OTel span reduced to what push() needs — kept separate
// from the protobuf types so reduceSpans can be unit tested without a k6 VU
// state or metrics registry.
type spanSample struct {
	name       string
	otelSvc    string // "" if the span's resource has no service.name attribute
	durationMs float64
	isError    bool
}

// reduceSpans flattens every span across every resource/scope in an export
// request into spanSamples. A span whose end time precedes its start time
// (malformed data — never expected from a well-behaved OTel SDK) is
// skipped entirely rather than producing a negative duration.
func reduceSpans(req *coltracepb.ExportTraceServiceRequest) []spanSample {
	var out []spanSample

	for _, rs := range req.GetResourceSpans() {
		svcName := resourceServiceName(rs.GetResource())

		for _, ss := range rs.GetScopeSpans() {
			for _, span := range ss.GetSpans() {
				if span.GetEndTimeUnixNano() < span.GetStartTimeUnixNano() {
					continue
				}

				durationMs := float64(span.GetEndTimeUnixNano()-span.GetStartTimeUnixNano()) / float64(time.Millisecond)

				out = append(out, spanSample{
					name:       span.GetName(),
					otelSvc:    svcName,
					durationMs: durationMs,
					isError:    span.GetStatus().GetCode() == tracepb.Status_STATUS_CODE_ERROR,
				})
			}
		}
	}

	return out
}

// resourceServiceName reads the service.name attribute off an OTel
// Resource, or "" if absent (no resource at all, no such attribute, or a
// non-string value — OTel's semantic conventions specify service.name as a
// string, so anything else is treated the same as absent).
func resourceServiceName(res *resourcepb.Resource) string {
	for _, attr := range res.GetAttributes() {
		if attr.GetKey() == "service.name" {
			return attr.GetValue().GetStringValue()
		}
	}
	return ""
}

// start begins listening on defaultListenAddr for OTLP/HTTP trace export
// requests. Must be called with a non-nil vu.State() (e.g. from setup());
// calling it more than once is not supported (two servers would race on
// the same address).
func (r *receiver) start() error {
	state := r.vu.State()
	if state == nil {
		return fmt.Errorf("oteltrace: start() must be called with a VU state (e.g. from setup())")
	}

	samples := state.Samples
	logger := state.Logger

	var stopped atomic.Bool

	// push is called from concurrent request-handling goroutines (unlike
	// promscrape's single ticker goroutine), so each call needs its own
	// recover — the underlying channel send is already safe for
	// concurrent callers on its own.
	push := func(sample metrics.Sample) {
		if stopped.Load() {
			return
		}
		defer func() {
			if recover() != nil {
				stopped.Store(true)
			}
		}()
		samples <- sample
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/traces", func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		var exportReq coltracepb.ExportTraceServiceRequest
		if err := proto.Unmarshal(body, &exportReq); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		now := time.Now()
		baseTags := state.Tags.GetCurrentValues().Tags

		for _, span := range reduceSpans(&exportReq) {
			r.stats.record(span)

			tags := baseTags.With("span_name", span.name)
			if span.otelSvc != "" {
				tags = tags.With("otel_service", span.otelSvc)
			}

			push(metrics.Sample{
				TimeSeries: metrics.TimeSeries{Metric: r.spanDuration, Tags: tags},
				Time:       now,
				Value:      span.durationMs,
			})

			errValue := 0.0
			if span.isError {
				errValue = 1.0
			}
			push(metrics.Sample{
				TimeSeries: metrics.TimeSeries{Metric: r.spanErrors, Tags: tags},
				Time:       now,
				Value:      errValue,
			})
		}

		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{Addr: defaultListenAddr, Handler: mux}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.WithError(err).Warn("oteltrace: server error")
		}
	}()

	// No explicit shutdown hook: same constraint k6/x/promscrape ran into
	// (nothing in the public extension API signals a run ending) — the
	// server dies with the k6 process itself, and push()'s panic recovery
	// handles the Samples channel closing while a request is still being
	// handled.

	if path := os.Getenv(spanStatsFileEnv); path != "" {
		go r.stats.startWriteLoop(path)
	}

	return nil
}
