// Command stubservice is a tiny fixture service used by the myrtille demo:
// it exposes /healthz (for service.managed.readiness), /users (create,
// delete), /products (list), /orders and /checkout (write endpoints
// scenarios hit under load), and /metrics (Prometheus format — a counter, a
// gauge, and a histogram, all three real metric types service.metrics.url
// scrapes), so examples/demo-service/myrtille.yaml has something real to
// talk to.
//
// It also emits OTel spans (see tracing.go) for /users, /orders and
// /checkout — one span per request, plus a simulated "check_inventory"
// downstream child span under /orders and /checkout — so
// service.traces.enabled has something real to receive too. Unlike
// /orders, /checkout actually fails the HTTP response (409) when the
// simulated inventory check fails, so its k6 check can genuinely fail —
// demonstrating myrtille's trace/check-failure correlation (traceparent
// propagation + report.md's "Failed Traces" section) with a real cause to
// point at, not just a synthetic span with no visible effect.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
)

var (
	userCount     atomic.Int64
	usersDeleted  atomic.Int64
	requestsTotal atomic.Int64
	ordersTotal   atomic.Int64

	// Order amount, exposed as a real Prometheus histogram (below) rather
	// than another counter/gauge — internal/metrics.Parse reduces it to
	// _sum/_count series, but it still exercises the histogram branch of
	// the real Prometheus text-exposition parser (bucket lines included),
	// unlike stub_requests_total/stub_users_created above which only ever
	// exercise the counter/gauge branches.
	orderAmountSum atomic.Int64
	// Each order's amount is one of orderAmountTiers, cycled
	// deterministically (not random) so a demo run's bucket counts are
	// reproducible — orderAmountTierCounts[i] counts orders that landed
	// in orderAmountTiers[i] exactly, not cumulatively; cumulative bucket
	// counts are computed at scrape time in the /metrics handler.
	orderAmountTierCounts [3]int64
)

var orderAmountTiers = [3]int64{5, 30, 75} // dollars — spans the 10/50/100 bucket boundaries below

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("POST /users", func(w http.ResponseWriter, r *http.Request) {
		_, span := tracer.Start(r.Context(), "create_user")
		defer span.End()

		requestsTotal.Add(1)
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)

		id := userCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":   fmt.Sprintf("user-%d", id),
			"name": body.Name,
		})
	})

	mux.HandleFunc("DELETE /users/{id}", func(w http.ResponseWriter, r *http.Request) {
		_, span := tracer.Start(r.Context(), "delete_user")
		defer span.End()

		requestsTotal.Add(1)
		usersDeleted.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("/products", func(w http.ResponseWriter, r *http.Request) {
		_, span := tracer.Start(r.Context(), "list_products")
		defer span.End()

		requestsTotal.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": "product-1"},
			{"id": "product-2"},
			{"id": "product-3"},
		})
	})

	mux.HandleFunc("/orders", func(w http.ResponseWriter, r *http.Request) {
		ctx, span := tracer.Start(r.Context(), "place_order")
		defer span.End()

		requestsTotal.Add(1)
		n := ordersTotal.Add(1)

		// Simulated downstream call, not a real one — stubservice has
		// nothing to actually call — just to give service.traces.enabled a
		// realistic-looking child span to receive (see the package doc and
		// docs/plans/otel-span-metrics.md tranche 5). Occasionally "fails"
		// (synthetic only: never affects the real HTTP response below, unlike
		// /checkout's use of the same helper) so svc_span_errors has non-zero
		// data to show in a demo run too. This endpoint's response must stay
		// unconditionally 201: myrtille's own init phase calls it too (see
		// myrtille.yaml's init.steps.create_users.children), and a non-2xx
		// there aborts the whole run (internal/initphase treats it as fatal)
		// — /checkout exists precisely so the "a failing check can fail" demo
		// doesn't touch this endpoint at all.
		checkInventory(ctx, n)

		amount := orderAmountTiers[n%int64(len(orderAmountTiers))]
		orderAmountSum.Add(amount)
		atomic.AddInt64(&orderAmountTierCounts[n%int64(len(orderAmountTiers))], 1)

		w.WriteHeader(http.StatusCreated)
	})

	// /checkout demonstrates myrtille's trace/check-failure correlation
	// (docs/plans/otel-span-metrics.md's extension, service.traces.enabled +
	// k6-side traceparent propagation): unlike /orders above, a simulated
	// out-of-stock result here actually fails the HTTP response (409), so the
	// k6 "checkout succeeded" check genuinely fails sometimes — myrtille
	// links that failure to this request's trace-id, and report.md's "Failed
	// Traces" section then shows the check_inventory span (with its ERROR
	// status) that caused it, right next to the check that failed. Only used
	// by k6.steps' "checkout" step (myrtille.yaml), never by the init phase.
	mux.HandleFunc("/checkout", func(w http.ResponseWriter, r *http.Request) {
		ctx, span := tracer.Start(r.Context(), "checkout")
		defer span.End()

		requestsTotal.Add(1)
		n := ordersTotal.Add(1)

		if !checkInventory(ctx, n) {
			span.SetStatus(codes.Error, "checkout failed: out of stock")
			w.WriteHeader(http.StatusConflict)
			return
		}

		amount := orderAmountTiers[n%int64(len(orderAmountTiers))]
		orderAmountSum.Add(amount)
		atomic.AddInt64(&orderAmountTierCounts[n%int64(len(orderAmountTiers))], 1)

		w.WriteHeader(http.StatusCreated)
	})

	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "# TYPE stub_requests_total counter\nstub_requests_total %d\n", requestsTotal.Load())
		fmt.Fprintf(w, "# TYPE stub_orders_total counter\nstub_orders_total %d\n", ordersTotal.Load())
		fmt.Fprintf(w, "# TYPE stub_users_created gauge\nstub_users_created %d\n", userCount.Load())
		fmt.Fprintf(w, "# TYPE stub_users_deleted counter\nstub_users_deleted %d\n", usersDeleted.Load())

		// Cumulative bucket counts (each includes every lower bucket, per
		// the Prometheus histogram spec) computed from the three exact-tier
		// counters above — orderAmountTiers is [5, 30, 75], so le="10" only
		// ever contains the 5-tier, le="50" adds the 30-tier, le="100" (and
		// +Inf, since no tier exceeds 100) adds the 75-tier.
		tier0 := atomic.LoadInt64(&orderAmountTierCounts[0])
		tier1 := atomic.LoadInt64(&orderAmountTierCounts[1])
		tier2 := atomic.LoadInt64(&orderAmountTierCounts[2])
		le10 := tier0
		le50 := tier0 + tier1
		le100 := tier0 + tier1 + tier2
		fmt.Fprintf(w, "# TYPE stub_order_amount histogram\n")
		fmt.Fprintf(w, "stub_order_amount_bucket{le=\"10\"} %d\n", le10)
		fmt.Fprintf(w, "stub_order_amount_bucket{le=\"50\"} %d\n", le50)
		fmt.Fprintf(w, "stub_order_amount_bucket{le=\"100\"} %d\n", le100)
		fmt.Fprintf(w, "stub_order_amount_bucket{le=\"+Inf\"} %d\n", le100)
		fmt.Fprintf(w, "stub_order_amount_sum %d\n", orderAmountSum.Load())
		fmt.Fprintf(w, "stub_order_amount_count %d\n", le100)
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	shutdownTracing, err := initTracing(ctx)
	if err != nil {
		log.Fatalf("initializing tracing: %v", err)
	}

	srv := &http.Server{Addr: ":8080", Handler: tracingMiddleware(mux)}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()
	log.Println("stub service listening on :8080")

	<-ctx.Done()

	// service.managed sends stop_signal (TERM by default) to the process
	// group and, if the process hasn't exited within stop_timeout, kills
	// it outright — so this has to be quick. A few seconds is enough to
	// flush whatever spans the batch exporter is currently holding
	// (myrtille's own default stop_timeout is 30s) without risking
	// dragging past it.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_ = srv.Shutdown(shutdownCtx)
	_ = shutdownTracing(shutdownCtx)
}

// tracingMiddleware extracts an incoming W3C traceparent (and baggage)
// header, if any, into the request's context before calling the handler —
// without this, every handler's tracer.Start(r.Context(), ...) call mints a
// brand new trace-id, ignoring whatever the caller sent, since a plain
// net/http request's Context() carries no OTel span context of its own. Only
// meaningful once otel.SetTextMapPropagator is configured with a real
// propagator (see tracing.go's initTracing) — the default global propagator
// is a no-op composite that would make this call a no-op too.
func tracingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// checkInventory is a simulated downstream call — stubservice has nothing
// real to call — that exists purely to give the "place_order"/"checkout"
// span a child span, the way a real order-placement endpoint would have one
// for its actual inventory-service call. Every 7th order simulates a
// failure (arbitrary, just non-zero) so a demo run's svc_span_errors has
// real non-zero data to show, not just zeros. Returns whether the item was
// in stock: /orders ignores it (that endpoint's response must always stay
// 201, see its own comment), /checkout uses it to actually fail the
// request — the one place in this demo where a failed check has a real
// cause to correlate against, via service.traces.enabled.
func checkInventory(ctx context.Context, orderN int64) bool {
	_, span := tracer.Start(ctx, "check_inventory")
	defer span.End()

	time.Sleep(time.Duration(5+rand.Intn(15)) * time.Millisecond) //nolint:gosec // demo-only jitter, not security-sensitive

	if orderN%7 == 0 {
		span.SetStatus(codes.Error, "simulated: out of stock")
		return false
	}
	span.SetAttributes(spanAttr("inventory.result", "in_stock"))
	return true
}
