package oteltrace

import (
	"encoding/json"
	"os"
	"sort"
	"sync"
	"time"
)

// maxFailedTraces bounds how many distinct failed traces are remembered at
// once — a constant, not configurable (see docs/plans/otel-span-metrics.md's
// "pas de config à remplir" ethos, already applied to spanStatsWriteInterval
// and this same extension's other internals). Unlike spanStats (one entry
// per span *name*, naturally bounded by how many distinct names a service
// has), this is one entry per *failure* — a run with many failing checks
// would otherwise grow this map for the run's whole duration. Oldest
// entries are evicted first once the bound is exceeded.
const maxFailedTraces = 200

// FailedTraceSpan is one span rattached to a failed trace — the same fields
// spanSample already carries, minus the trace-id itself (already the
// failedTraces map key, so redundant per-span here).
type FailedTraceSpan struct {
	Name       string  `json:"name"`
	OtelSvc    string  `json:"otel_service,omitempty"`
	DurationMs float64 `json:"duration_ms"`
	IsError    bool    `json:"is_error"`
}

// FailedTrace is one check() failure linked to its W3C trace-id (see
// receiver.linkFailure), plus whatever spans have been rattached to it so
// far by matching span trace-ids as they arrive. Spans may never arrive at
// all (see docs/plans/otel-span-metrics.md tranche 8's finding on the
// receiver having to still be alive when the service's batch exporter
// fires) — best-effort, same as the rest of this extension.
type FailedTrace struct {
	TraceID string            `json:"trace_id"`
	Label   string            `json:"label"`
	Spans   []FailedTraceSpan `json:"spans"`
}

// failedTraces is the receiver's trace_id -> failure record store,
// protected by mu: linkFailure is called from a VU's own goroutine, while
// match is called from the HTTP handler's (possibly several, concurrent)
// request-handling goroutines — same concurrency shape spanStats already
// has to deal with, see its own doc comment. order tracks insertion order
// for FIFO eviction — a plain slice, since maxFailedTraces is small enough
// that a linear scan/removal isn't worth a more complex structure.
type failedTraces struct {
	mu      sync.Mutex
	entries map[string]*FailedTrace
	order   []string
}

func newFailedTraces() *failedTraces {
	return &failedTraces{entries: make(map[string]*FailedTrace)}
}

// sharedFailedTraces and getSharedFailedTraces make failedTraces a
// process-wide singleton, one per k6 process rather than one per receiver
// instance. **Found in real-run validation, not anticipated in the plan**:
// k6 gives every VU its own JS runtime, including the dedicated VU that
// runs setup()/teardown() — so `const __oteltrace = new
// oteltrace.Receiver()` at module scope constructs a *different* Go
// receiver struct in every VU. Only the setup VU's instance ever has
// start() called on it (the one real HTTP server for the whole run); a
// regular VU's own instance is otherwise inert. linkFailure is the first
// method meant to be called from a regular VU's default() — reproduced for
// real: linking from default() left the live server's own failedTraces
// map (visible via a temporary debug route) empty, while linking from
// setup() (the same VU instance that started the server) worked
// immediately. metrics.Metric handles don't have this problem — k6's own
// Registry dedupes by name and hands back the same object to every VU —
// but failedTraces has no such k6-owned registry to lean on, so it needs
// its own: every receiver, regardless of which VU's `new
// oteltrace.Receiver()` constructed it, shares this one instance.
var (
	sharedFailedTracesOnce sync.Once
	sharedFailedTracesVal  *failedTraces
)

func getSharedFailedTraces() *failedTraces {
	sharedFailedTracesOnce.Do(func() {
		sharedFailedTracesVal = newFailedTraces()
	})
	return sharedFailedTracesVal
}

// link registers traceID as a pending failure correlation, labeled label
// (the step that failed). A traceID already tracked is left untouched —
// spans it may have already matched are not discarded — rather than
// overwritten: in practice each request gets its own freshly generated
// traceparent (see newTraceparent), so a genuine duplicate link for the
// same trace-id isn't expected, but silently keeping the first registration
// is a safer default than clobbering any spans already matched to it.
func (f *failedTraces) link(traceID, label string) {
	// "" is never a valid correlation key — same invariant match() enforces
	// on the span side (a span with no trace-id at all). In practice
	// receiver.linkFailure already guarantees a non-empty, well-formed
	// traceID via parseTraceID before calling this, but enforcing it here
	// too keeps the invariant real regardless of caller.
	if traceID == "" {
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if _, exists := f.entries[traceID]; exists {
		return
	}

	f.entries[traceID] = &FailedTrace{TraceID: traceID, Label: label, Spans: []FailedTraceSpan{}}
	f.order = append(f.order, traceID)

	if len(f.order) > maxFailedTraces {
		oldest := f.order[0]
		f.order = f.order[1:]
		delete(f.entries, oldest)
	}
}

// match rattaches span to its failedTraces entry, if span.traceID is
// currently tracked — a no-op otherwise (span.traceID empty, or simply not
// a trace anyone linked a failure to; the overwhelming common case, since
// most spans belong to successful requests).
func (f *failedTraces) match(span spanSample) {
	if span.traceID == "" {
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	entry, ok := f.entries[span.traceID]
	if !ok {
		return
	}

	entry.Spans = append(entry.Spans, FailedTraceSpan{
		Name:       span.name,
		OtelSvc:    span.otelSvc,
		DurationMs: span.durationMs,
		IsError:    span.isError,
	})
}

// snapshot returns a copy of the current failed-trace entries, sorted by
// trace-id for determinism. No file/JS export exists yet for this (that's
// docs/plans/otel-span-metrics.md tranche 13) — today this only backs
// tests and, temporarily, tranche 11's own real-run validation.
func (f *failedTraces) snapshot() []FailedTrace {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]FailedTrace, 0, len(f.entries))
	for _, e := range f.entries {
		spans := make([]FailedTraceSpan, len(e.Spans))
		copy(spans, e.Spans)
		out = append(out, FailedTrace{TraceID: e.TraceID, Label: e.Label, Spans: spans})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].TraceID < out[j].TraceID })
	return out
}

// failedTracesFileEnv, when set, is the path this extension periodically
// writes its current failedTraces snapshot to, as JSON — read back by
// internal/k6run after the k6 process exits, mirroring spanStatsFileEnv
// exactly (see stats.go's own doc comment for why periodic overwriting
// rather than a clean end-of-run flush: no such hook is available to an
// extension). Named MYRTILLE_FAILED_TRACES_FILE on the myrtille side too,
// not shared via import for the same reason spanStatsFileEnv isn't either.
const failedTracesFileEnv = "MYRTILLE_FAILED_TRACES_FILE"

// failedTracesWriteInterval mirrors spanStatsWriteInterval — same
// reasoning, same value, kept as its own constant rather than reusing
// spanStatsWriteInterval directly since the two concepts are otherwise
// unrelated and could reasonably diverge later.
const failedTracesWriteInterval = time.Second

// writeFile marshals the current snapshot and writes it to path,
// best-effort — mirrors spanStats.writeFile exactly (see its own doc
// comment: nothing is watching for a returned error from a background
// ticker, so a marshal/write failure is silently skipped).
func (f *failedTraces) writeFile(path string) {
	data, err := json.Marshal(f.snapshot())
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o644)
}

// startWriteLoop periodically overwrites path with the current snapshot,
// forever — mirrors spanStats.startWriteLoop exactly (dies with the k6
// process itself, no explicit stop).
func (f *failedTraces) startWriteLoop(path string) {
	ticker := time.NewTicker(failedTracesWriteInterval)
	defer ticker.Stop()

	for range ticker.C {
		f.writeFile(path)
	}
}
