// Command stubservice is a tiny fixture service used by the myrtille demo:
// it exposes /users (create, delete), /products (list), /orders (write
// endpoint scenarios hit under load), and /metrics (Prometheus format), so
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
)

func main() {
	mux := http.NewServeMux()

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
		atomic.AddInt64(&ordersTotal, 1)
		w.WriteHeader(http.StatusCreated)
	})

	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "# TYPE stub_requests_total counter\nstub_requests_total %d\n", atomic.LoadInt64(&requestsTotal))
		fmt.Fprintf(w, "# TYPE stub_orders_total counter\nstub_orders_total %d\n", atomic.LoadInt64(&ordersTotal))
		fmt.Fprintf(w, "# TYPE stub_users_created gauge\nstub_users_created %d\n", atomic.LoadInt64(&userCount))
		fmt.Fprintf(w, "# TYPE stub_users_deleted counter\nstub_users_deleted %d\n", atomic.LoadInt64(&usersDeleted))
	})

	log.Println("stub service listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", mux))
}
