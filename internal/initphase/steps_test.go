package initphase

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
				Count:  3,
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
				Count:  1,
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
			{Name: "fails", Method: "GET", URL: "{{.BaseURL}}/fails", Count: 1},
			{Name: "later", Method: "GET", URL: "{{.BaseURL}}/later", Count: 1},
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
				Count:  1,
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
				Count:   1,
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
			{Name: "slow", Method: "GET", URL: "{{.BaseURL}}/slow", Count: 1},
		}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	dict := state.New()
	if _, err := Run(ctx, cfg, dict); err == nil {
		t.Fatal("expected error for cancelled context, got nil")
	}
}
