package initphase

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/antobarth/myrtille/internal/config"
	"github.com/antobarth/myrtille/internal/state"
)

func newTestServer(t *testing.T, mux *http.ServeMux) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func TestRunCreatesUsersAndExtractsIDs(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id": "%s-id"}`, body.Name)
	})
	ts := newTestServer(t, mux)

	cfg := &config.Config{
		Service: config.ServiceConfig{BaseURL: ts.URL},
		Init: config.InitConfig{Steps: []config.Step{
			{
				Name:   "create_user",
				Method: "POST",
				URL:    "{{.BaseURL}}/users",
				Body:   `{"name":"user-{{.Index}}"}`,
				Count:  "3",
				Extract: []config.Extract{
					{Path: "id", As: "user_ids"},
				},
			},
		}},
	}

	dict := state.New()
	summary, err := Run(context.Background(), cfg, dict)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if got := dict.Count("user_ids"); got != 3 {
		t.Fatalf("dict.Count(user_ids) = %d, want 3", got)
	}
	if len(summary.Steps) != 1 || summary.Steps[0].Requests != 3 {
		t.Fatalf("unexpected summary: %+v", summary.Steps)
	}
	if summary.Steps[0].Extracted["user_ids"] != 3 {
		t.Fatalf("unexpected extracted count: %+v", summary.Steps[0].Extracted)
	}
}

func TestRunExtractsArrayWithGjsonModifier(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/products", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"id":"p1"},{"id":"p2"},{"id":"p3"}]`)
	})
	ts := newTestServer(t, mux)

	cfg := &config.Config{
		Service: config.ServiceConfig{BaseURL: ts.URL},
		Init: config.InitConfig{Steps: []config.Step{
			{
				Name:   "list_products",
				Method: "GET",
				URL:    "{{.BaseURL}}/products",
				Count:  "1",
				Extract: []config.Extract{
					{Path: "#.id", As: "product_ids"},
				},
			},
		}},
	}

	dict := state.New()
	if _, err := Run(context.Background(), cfg, dict); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if got := dict.Count("product_ids"); got != 3 {
		t.Fatalf("dict.Count(product_ids) = %d, want 3", got)
	}
}

func TestRunAbortsOnHTTPErrorAndSkipsLaterSteps(t *testing.T) {
	var laterStepCalls int32

	mux := http.NewServeMux()
	mux.HandleFunc("/fails", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"boom"}`))
	})
	mux.HandleFunc("/later", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&laterStepCalls, 1)
		w.Write([]byte(`{}`))
	})
	ts := newTestServer(t, mux)

	cfg := &config.Config{
		Service: config.ServiceConfig{BaseURL: ts.URL},
		Init: config.InitConfig{Steps: []config.Step{
			{Name: "fails", Method: "GET", URL: "{{.BaseURL}}/fails", Count: "1"},
			{Name: "later", Method: "GET", URL: "{{.BaseURL}}/later", Count: "1"},
		}},
	}

	dict := state.New()
	_, err := Run(context.Background(), cfg, dict)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if calls := atomic.LoadInt32(&laterStepCalls); calls != 0 {
		t.Fatalf("expected later step to be skipped, but it was called %d times", calls)
	}
}

func TestRunTeardownContinuesPastFailingStepAndAggregatesErrors(t *testing.T) {
	var laterStepCalls int32

	mux := http.NewServeMux()
	mux.HandleFunc("/fails", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/later", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&laterStepCalls, 1)
		w.Write([]byte(`{}`))
	})
	ts := newTestServer(t, mux)

	cfg := &config.Config{
		Service: config.ServiceConfig{BaseURL: ts.URL},
		Teardown: config.TeardownConfig{Steps: []config.Step{
			{Name: "delete_missing", Method: "DELETE", URL: "{{.BaseURL}}/fails", Count: "1"},
			{Name: "later", Method: "GET", URL: "{{.BaseURL}}/later", Count: "1"},
		}},
	}

	dict := state.New()
	summary, err := RunTeardown(context.Background(), cfg, dict)
	if err == nil {
		t.Fatal("expected a non-nil aggregated error, got nil")
	}
	if !strings.Contains(err.Error(), "delete_missing") {
		t.Errorf("expected error to mention the failing step, got: %v", err)
	}
	if calls := atomic.LoadInt32(&laterStepCalls); calls != 1 {
		t.Fatalf("expected the later step to still run despite the earlier failure, got %d calls", calls)
	}
	if len(summary.Steps) != 2 || summary.Steps[1].Requests != 1 {
		t.Fatalf("expected both steps reported, with the later one having run: %+v", summary.Steps)
	}
}

func TestRunErrorsOnMissingExtractPath(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/thing", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"unrelated": "value"}`)
	})
	ts := newTestServer(t, mux)

	cfg := &config.Config{
		Service: config.ServiceConfig{BaseURL: ts.URL},
		Init: config.InitConfig{Steps: []config.Step{
			{
				Name:   "thing",
				Method: "GET",
				URL:    "{{.BaseURL}}/thing",
				Count:  "1",
				Extract: []config.Extract{
					{Path: "missing", As: "ids"},
				},
			},
		}},
	}

	dict := state.New()
	if _, err := Run(context.Background(), cfg, dict); err == nil {
		t.Fatal("expected error for missing extract path, got nil")
	}
}

func TestRunSendsCustomHeaders(t *testing.T) {
	var receivedHeader string
	mux := http.NewServeMux()
	mux.HandleFunc("/secure", func(w http.ResponseWriter, r *http.Request) {
		receivedHeader = r.Header.Get("Authorization")
		fmt.Fprint(w, `{}`)
	})
	ts := newTestServer(t, mux)

	cfg := &config.Config{
		Service: config.ServiceConfig{BaseURL: ts.URL},
		Init: config.InitConfig{Steps: []config.Step{
			{
				Name:    "secure",
				Method:  "GET",
				URL:     "{{.BaseURL}}/secure",
				Count:   "1",
				Headers: map[string]string{"Authorization": "Bearer token"},
			},
		}},
	}

	dict := state.New()
	if _, err := Run(context.Background(), cfg, dict); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if receivedHeader != "Bearer token" {
		t.Fatalf("Authorization header = %q, want %q", receivedHeader, "Bearer token")
	}
}

func TestRunContextCancellation(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	ts := newTestServer(t, mux)

	cfg := &config.Config{
		Service: config.ServiceConfig{BaseURL: ts.URL},
		Init: config.InitConfig{Steps: []config.Step{
			{Name: "slow", Method: "GET", URL: "{{.BaseURL}}/slow", Count: "1"},
		}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	dict := state.New()
	if _, err := Run(ctx, cfg, dict); err == nil {
		t.Fatal("expected error for cancelled context, got nil")
	}
}

func TestRunNestedChildrenReceiveParentAndAggregateResults(t *testing.T) {
	var mu sync.Mutex
	productsByUser := map[string][]string{}

	mux := http.NewServeMux()
	mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id": "%s-id"}`, body.Name)
	})
	mux.HandleFunc("/orders", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			UserID string `json:"user_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		productsByUser[body.UserID] = append(productsByUser[body.UserID], body.UserID)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id": "order-for-%s"}`, body.UserID)
	})
	ts := newTestServer(t, mux)

	cfg := &config.Config{
		Service: config.ServiceConfig{BaseURL: ts.URL},
		Init: config.InitConfig{Steps: []config.Step{
			{
				Name:   "create_users",
				Method: "POST",
				URL:    "{{.BaseURL}}/users",
				Body:   `{"name":"user-{{.Index}}"}`,
				Count:  "2",
				Extract: []config.Extract{
					{Path: "id", As: "user_ids"},
				},
				Children: []config.Step{
					{
						Name:   "create_orders",
						Method: "POST",
						URL:    "{{.BaseURL}}/orders",
						Body:   `{"user_id":"{{.Parent.id}}"}`,
						Count:  "2",
						Extract: []config.Extract{
							{Path: "id", As: "order_ids"},
						},
					},
				},
			},
		}},
	}

	dict := state.New()
	summary, err := Run(context.Background(), cfg, dict)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if got := dict.Count("user_ids"); got != 2 {
		t.Fatalf("dict.Count(user_ids) = %d, want 2", got)
	}
	// 2 users * 2 orders each = 4 orders total, each referencing a real parent user id.
	if got := dict.Count("order_ids"); got != 4 {
		t.Fatalf("dict.Count(order_ids) = %d, want 4", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(productsByUser) != 2 {
		t.Fatalf("expected orders for 2 distinct users, got %d: %+v", len(productsByUser), productsByUser)
	}
	for user, orders := range productsByUser {
		if len(orders) != 2 {
			t.Fatalf("expected 2 orders for user %q, got %d", user, len(orders))
		}
	}

	if len(summary.Steps) != 1 {
		t.Fatalf("expected 1 top-level step, got %d", len(summary.Steps))
	}
	parent := summary.Steps[0]
	if parent.Requests != 2 {
		t.Fatalf("parent.Requests = %d, want 2", parent.Requests)
	}
	if len(parent.Children) != 1 {
		t.Fatalf("expected 1 aggregated child result, got %d", len(parent.Children))
	}
	child := parent.Children[0]
	if child.Requests != 4 {
		t.Fatalf("child.Requests = %d, want 4 (aggregated across both parent iterations)", child.Requests)
	}
	if child.Extracted["order_ids"] != 4 {
		t.Fatalf("child.Extracted[order_ids] = %d, want 4", child.Extracted["order_ids"])
	}
}

func TestRunPicksFromExistingPool(t *testing.T) {
	var receivedProductIDs []string
	var mu sync.Mutex

	mux := http.NewServeMux()
	mux.HandleFunc("/products", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"id":"p1"},{"id":"p2"},{"id":"p3"}]`)
	})
	mux.HandleFunc("/orders", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ProductID string `json:"product_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		receivedProductIDs = append(receivedProductIDs, body.ProductID)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"order-id"}`)
	})
	ts := newTestServer(t, mux)

	cfg := &config.Config{
		Service: config.ServiceConfig{BaseURL: ts.URL},
		Init: config.InitConfig{Steps: []config.Step{
			{
				Name:  "list_products",
				URL:   "{{.BaseURL}}/products",
				Count: "1",
				Extract: []config.Extract{
					{Path: "#.id", As: "product_ids"},
				},
			},
			{
				Name:   "create_orders",
				Method: "POST",
				URL:    "{{.BaseURL}}/orders",
				Body:   `{"product_id":"{{pick .Dict.product_ids}}"}`,
				Count:  "5",
			},
		}},
	}

	dict := state.New()
	if _, err := Run(context.Background(), cfg, dict); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(receivedProductIDs) != 5 {
		t.Fatalf("expected 5 orders, got %d", len(receivedProductIDs))
	}
	valid := map[string]bool{"p1": true, "p2": true, "p3": true}
	for _, id := range receivedProductIDs {
		if !valid[id] {
			t.Fatalf("order referenced unexpected product_id %q", id)
		}
	}
}

func TestRunCountFromVars(t *testing.T) {
	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		fmt.Fprint(w, `{}`)
	})
	ts := newTestServer(t, mux)

	cfg := &config.Config{
		Vars:    map[string]any{"users_count": 4},
		Service: config.ServiceConfig{BaseURL: ts.URL},
		Init: config.InitConfig{Steps: []config.Step{
			{Name: "create_users", Method: "GET", URL: "{{.BaseURL}}/users", Count: "{{.Vars.users_count}}"},
		}},
	}

	dict := state.New()
	if _, err := Run(context.Background(), cfg, dict); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 4 {
		t.Fatalf("expected 4 calls, got %d", got)
	}
}

func TestRunCountFromRandomRespectsBounds(t *testing.T) {
	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		fmt.Fprint(w, `{}`)
	})
	ts := newTestServer(t, mux)

	cfg := &config.Config{
		Service: config.ServiceConfig{BaseURL: ts.URL},
		Init: config.InitConfig{Steps: []config.Step{
			{Name: "create_users", Method: "GET", URL: "{{.BaseURL}}/users", Count: "{{random 2 4}}"},
		}},
	}

	dict := state.New()
	if _, err := Run(context.Background(), cfg, dict); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	got := atomic.LoadInt32(&calls)
	if got < 2 || got > 4 {
		t.Fatalf("expected between 2 and 4 calls, got %d", got)
	}
}

func TestRunInvalidCountTemplateFails(t *testing.T) {
	cfg := &config.Config{
		Service: config.ServiceConfig{BaseURL: "http://example.invalid"},
		Init: config.InitConfig{Steps: []config.Step{
			{Name: "bad", URL: "{{.BaseURL}}/x", Count: "not-a-number"},
		}},
	}

	dict := state.New()
	if _, err := Run(context.Background(), cfg, dict); err == nil {
		t.Fatal("expected error for unresolvable count, got nil")
	}
}

func TestFlattenOrdersDepthFirstWithDepth(t *testing.T) {
	steps := []StepResult{
		{
			Name: "parent",
			Children: []StepResult{
				{Name: "child"},
			},
		},
		{Name: "sibling"},
	}

	flat := Flatten(steps)
	if len(flat) != 3 {
		t.Fatalf("expected 3 flattened entries, got %d", len(flat))
	}
	want := []struct {
		name  string
		depth int
	}{
		{"parent", 0},
		{"child", 1},
		{"sibling", 0},
	}
	for i, w := range want {
		if flat[i].Step.Name != w.name || flat[i].Depth != w.depth {
			t.Fatalf("flat[%d] = {%q, depth %d}, want {%q, depth %d}", i, flat[i].Step.Name, flat[i].Depth, w.name, w.depth)
		}
	}
}
