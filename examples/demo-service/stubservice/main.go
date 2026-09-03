// Command stubservice is a tiny fixture service used by the myrtille demo:
// it exposes /healthz (for service.managed.readiness), /users (create,
// delete), /products (list), /orders (write endpoint scenarios hit under
// load), and /metrics (Prometheus format — a counter, a gauge, and a
// histogram, all three real metric types service.metrics.url scrapes), so
// examples/demo-service/myrtille.yaml has something real to talk to.
//
// It also emits OTel spans (see tracing.go) for /users and /orders — one
// span per request, plus a simulated "check_inventory" downstream child
// span under /orders — so service.traces.enabled has something real to
// receive too.
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

	"go.opentelemetry.io/otel/codes"
)

var (
	userCount     int64
	usersDeleted  int64
	requestsTotal int64
	ordersTotal   int64

	// Order amount, exposed as a real Prometheus histogram (below) rather
	// than another counter/gauge — internal/metrics.Parse reduces it to
	// _sum/_count series, but it still exercises the histogram branch of
	// the real Prometheus text-exposition parser (bucket lines included),
	// unlike stub_requests_total/stub_users_created above which only ever
	// exercise the counter/gauge branches.
	orderAmountSum int64
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

		atomic.AddInt64(&requestsTotal, 1)
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)

		id := atomic.AddInt64(&userCount, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":   fmt.Sprintf("user-%d", id),
			"name": body.Name,
		})
	})

	mux.HandleFunc("DELETE /users/{id}", func(w http.ResponseWriter, r *http.Request) {
		_, span := tracer.Start(r.Context(), "delete_user")
		defer span.End()

		atomic.AddInt64(&requestsTotal, 1)
		atomic.AddInt64(&usersDeleted, 1)
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("/products", func(w http.ResponseWriter, r *http.Request) {
		_, span := tracer.Start(r.Context(), "list_products")
		defer span.End()

		atomic.AddInt64(&requestsTotal, 1)
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

		atomic.AddInt64(&requestsTotal, 1)
		n := atomic.AddInt64(&ordersTotal, 1)

		// Simulated downstream call, not a real one — stubservice has
		// nothing to actually call — just to give service.traces.enabled a
		// realistic-looking child span to receive (see the package doc and
		// docs/plans/otel-span-metrics.md tranche 5). Occasionally "fails"
		// (synthetic only: never affects the real HTTP response below) so
		// svc_span_errors has non-zero data to show in a demo run too.
		checkInventory(ctx, n)

		amount := orderAmountTiers[n%int64(len(orderAmountTiers))]
		atomic.AddInt64(&orderAmountSum, amount)
		atomic.AddInt64(&orderAmountTierCounts[n%int64(len(orderAmountTiers))], 1)

		w.WriteHeader(http.StatusCreated)
	})

	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "# TYPE stub_requests_total counter\nstub_requests_total %d\n", atomic.LoadInt64(&requestsTotal))
		fmt.Fprintf(w, "# TYPE stub_orders_total counter\nstub_orders_total %d\n", atomic.LoadInt64(&ordersTotal))
		fmt.Fprintf(w, "# TYPE stub_users_created gauge\nstub_users_created %d\n", atomic.LoadInt64(&userCount))
		fmt.Fprintf(w, "# TYPE stub_users_deleted counter\nstub_users_deleted %d\n", atomic.LoadInt64(&usersDeleted))

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
		fmt.Fprintf(w, "stub_order_amount_sum %d\n", atomic.LoadInt64(&orderAmountSum))
		fmt.Fprintf(w, "stub_order_amount_count %d\n", le100)
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	shutdownTracing, err := initTracing(ctx)
	if err != nil {
		log.Fatalf("initializing tracing: %v", err)
	}

	srv := &http.Server{Addr: ":8080", Handler: mux}
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

// checkInventory is a simulated downstream call — stubservice has nothing
// real to call — that exists purely to give the "place_order" span a child
// span, the way a real order-placement endpoint would have one for its
// actual inventory-service call. Every 7th order simulates a failure
// (arbitrary, just non-zero) so a demo run's svc_span_errors has real
// non-zero data to show, not just zeros.
func checkInventory(ctx context.Context, orderN int64) {
	_, span := tracer.Start(ctx, "check_inventory")
	defer span.End()

	time.Sleep(time.Duration(5+rand.Intn(15)) * time.Millisecond) //nolint:gosec // demo-only jitter, not security-sensitive

	if orderN%7 == 0 {
		span.SetStatus(codes.Error, "simulated: out of stock")
		return
	}
	span.SetAttributes(spanAttr("inventory.result", "in_stock"))
}
