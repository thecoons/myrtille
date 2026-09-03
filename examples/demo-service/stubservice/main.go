// Command stubservice is a tiny fixture service used by the myrtille demo:
// it exposes /healthz (for service.managed.readiness), /users (create,
// delete), /products (list), /orders (write endpoint scenarios hit under
// load), and /metrics (Prometheus format — a counter, a gauge, and a
// histogram, all three real metric types service.metrics.url scrapes), so
// examples/demo-service/myrtille.yaml has something real to talk to.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync/atomic"
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
		atomic.AddInt64(&requestsTotal, 1)
		atomic.AddInt64(&usersDeleted, 1)
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("/products", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&requestsTotal, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": "product-1"},
			{"id": "product-2"},
			{"id": "product-3"},
		})
	})

	mux.HandleFunc("/orders", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&requestsTotal, 1)
		n := atomic.AddInt64(&ordersTotal, 1)

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

	log.Println("stub service listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", mux))
}
