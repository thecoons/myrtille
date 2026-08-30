// Command stubservice is a fixture service for the myrtille "inventory"
// example: unlike examples/demo-service (flat, always-increasing counters),
// its metrics are deliberately load-sensitive so a report.html generated
// against it shows genuine evolution — a queue depth gauge that rises and
// falls with concurrency, per-SKU stock levels that drain under load and
// recover via a background restock ticker, a latency histogram that widens
// as the queue backs up, and an error counter driven by simulated overload.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

var (
	requestsTotal int64
	ordersTotal   int64
	errorsTotal   int64
	restocksTotal int64
	queueDepth    int64

	stockMu  sync.Mutex
	stock    = map[string]int64{"sku-1": 200, "sku-2": 150, "sku-3": 80}
	stockCap = map[string]int64{"sku-1": 200, "sku-2": 150, "sku-3": 80}

	latency = newHistogram([]float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1})
)

// histogram is a hand-rolled Prometheus histogram (cumulative bucket
// counts): the project has no client library dependency, and the /metrics
// endpoints across examples write raw text-exposition format directly.
type histogram struct {
	mu     sync.Mutex
	bounds []float64
	counts []int64
	sum    float64
	count  int64
}

func newHistogram(bounds []float64) *histogram {
	return &histogram{bounds: bounds, counts: make([]int64, len(bounds))}
}

func (h *histogram) observe(seconds float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, b := range h.bounds {
		if seconds <= b {
			h.counts[i]++
		}
	}
	h.sum += seconds
	h.count++
}

func (h *histogram) writeTo(w http.ResponseWriter, name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fmt.Fprintf(w, "# TYPE %s histogram\n", name)
	for i, b := range h.bounds {
		fmt.Fprintf(w, "%s_bucket{le=\"%g\"} %d\n", name, b, h.counts[i])
	}
	fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"} %d\n", name, h.count)
	fmt.Fprintf(w, "%s_sum %g\n", name, h.sum)
	fmt.Fprintf(w, "%s_count %d\n", name, h.count)
}

// errorProbability rises sharply once the queue backs up past a handful of
// in-flight requests, simulating a service that starts shedding load under
// pressure — mostly quiet at low concurrency, visibly noisy at the peak of
// the k6 ramp, and quiet again once load drains.
func errorProbability(depth int64) int {
	if depth <= 5 {
		return 1
	}
	p := int(depth-5) * 3
	if p > 25 {
		p = 25
	}
	return p
}

// simulatedLatency grows with queue depth (backpressure) plus jitter, so
// the request-duration histogram widens under load and narrows once the
// queue drains, instead of staying flat for the whole run.
func simulatedLatency(depth int64) time.Duration {
	base := 15*time.Millisecond + time.Duration(depth)*8*time.Millisecond
	if base > 400*time.Millisecond {
		base = 400 * time.Millisecond
	}
	jitter := time.Duration(rand.Intn(12)) * time.Millisecond
	return base + jitter
}

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("/products", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&requestsTotal, 1)
		stockMu.Lock()
		products := make([]map[string]any, 0, len(stock))
		for sku := range stock {
			products = append(products, map[string]any{"id": sku})
		}
		stockMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(products)
	})

	mux.HandleFunc("/orders", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&requestsTotal, 1)
		atomic.AddInt64(&ordersTotal, 1)

		var body struct {
			Sku string `json:"sku"`
			Qty int64  `json:"qty"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Qty <= 0 {
			body.Qty = 1
		}

		depth := atomic.AddInt64(&queueDepth, 1)
		defer atomic.AddInt64(&queueDepth, -1)

		start := time.Now()
		time.Sleep(simulatedLatency(depth))
		latency.observe(time.Since(start).Seconds())

		if rand.Intn(100) < errorProbability(depth) {
			atomic.AddInt64(&errorsTotal, 1)
			http.Error(w, "service overloaded", http.StatusServiceUnavailable)
			return
		}

		stockMu.Lock()
		remaining := stock[body.Sku] - body.Qty
		if remaining < 0 {
			remaining = 0
		}
		stock[body.Sku] = remaining
		stockMu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"sku": body.Sku, "remaining": remaining})
	})

	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "# TYPE inventory_requests_total counter\ninventory_requests_total %d\n", atomic.LoadInt64(&requestsTotal))
		fmt.Fprintf(w, "# TYPE inventory_orders_total counter\ninventory_orders_total %d\n", atomic.LoadInt64(&ordersTotal))
		fmt.Fprintf(w, "# TYPE inventory_errors_total counter\ninventory_errors_total %d\n", atomic.LoadInt64(&errorsTotal))
		fmt.Fprintf(w, "# TYPE inventory_restocks_total counter\ninventory_restocks_total %d\n", atomic.LoadInt64(&restocksTotal))
		fmt.Fprintf(w, "# TYPE inventory_queue_depth gauge\ninventory_queue_depth %d\n", atomic.LoadInt64(&queueDepth))

		fmt.Fprintf(w, "# TYPE inventory_stock_level gauge\n")
		stockMu.Lock()
		for sku, level := range stock {
			fmt.Fprintf(w, "inventory_stock_level{sku=%q} %d\n", sku, level)
		}
		stockMu.Unlock()

		latency.writeTo(w, "inventory_request_duration_seconds")
	})

	// Background replenishment: trickles stock back up so sustained order
	// traffic drains it while idle periods let it recover — a sawtooth
	// instead of a monotonic decline.
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			stockMu.Lock()
			for sku, level := range stock {
				cap := stockCap[sku]
				restocked := level + 15
				if restocked > cap {
					restocked = cap
				}
				if restocked != level {
					stock[sku] = restocked
					atomic.AddInt64(&restocksTotal, 1)
				}
			}
			stockMu.Unlock()
		}
	}()

	log.Println("inventory stub service listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", mux))
}
