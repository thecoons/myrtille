package k6gen

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/thecoons/myrtille/internal/config"
)

func TestGenerateRendersStepsAndOptions(t *testing.T) {
	cfg := &config.Config{
		Service: config.ServiceConfig{BaseURL: "http://localhost:8080"},
		Vars:    map[string]any{"label": "hello"},
		K6: config.K6Config{
			Steps: []config.K6Step{
				{
					Name:   "place_order",
					Method: "POST",
					URL:    "{{.BaseURL}}/orders",
					Body:   `{"userId":"{{pick "user_ids"}}","label":"{{.Vars.label}}"}`,
					Checks: map[string]string{"status is 201": "r.status === 201"},
					Sleep:  config.Duration(200 * time.Millisecond),
				},
				{
					Name:   "browse",
					Method: "GET",
					URL:    "{{.BaseURL}}/products/{{random 1 3}}",
				},
			},
			Options: config.K6Options{
				VUs:      10,
				Duration: config.Duration(30 * time.Second),
				Thresholds: map[string][]string{
					"http_req_failed": {"rate<0.01"},
				},
			},
		},
	}

	path, cleanup, err := Generate(cfg)
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	defer cleanup()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading generated script: %v", err)
	}
	js := string(data)

	wantSnippets := []string{
		"export const options = {",
		`"vus": 10`,
		`"duration": "30s"`,
		`"http_req_failed"`,
		"http.request(\"POST\", `http://localhost:8080/orders`",
		`${pick(state["user_ids"])}`,
		`"label":"hello"`,
		`check(res, { "status is 201": (r) => r.status === 201 });`,
		"sleep(0.2);",
		"http.request(\"GET\", `http://localhost:8080/products/${randomInt(1, 3)}`",
		"null,", // browse step has no body
	}
	for _, snippet := range wantSnippets {
		if !strings.Contains(js, snippet) {
			t.Errorf("generated script missing %q\n--- full script ---\n%s", snippet, js)
		}
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected generated file to exist before cleanup: %v", err)
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected cleanup to remove generated file, stat err = %v", err)
	}
}

func TestGenerateRendersPerStepTags(t *testing.T) {
	cfg := &config.Config{
		Service: config.ServiceConfig{BaseURL: "http://localhost:8080"},
		Vars:    map[string]any{"selector": "live"},
		K6: config.K6Config{
			Steps: []config.K6Step{
				{
					Name:   "get_perimeter",
					Method: "GET",
					URL:    "{{.BaseURL}}/perimeter",
					Tags: map[string]string{
						"endpoint": "get",
						"selector": "{{.Vars.selector}}",
					},
				},
				{
					Name:   "list_perimeters",
					Method: "GET",
					URL:    "{{.BaseURL}}/perimeters",
				},
			},
			Options: config.K6Options{
				Thresholds: map[string][]string{
					"http_req_duration{endpoint:get}": {"p(95)<300"},
				},
			},
		},
	}

	path, cleanup, err := Generate(cfg)
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	defer cleanup()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading generated script: %v", err)
	}
	js := string(data)

	for _, snippet := range []string{
		`tags: { "endpoint": ` + "`get`" + `, "selector": ` + "`live`" + ` }`,
		`http_req_duration{endpoint:get}`,
	} {
		if !strings.Contains(js, snippet) {
			t.Errorf("generated script missing %q\n--- full script ---\n%s", snippet, js)
		}
	}

	// The tagless step still gets an (empty) tags object, matching
	// headers' existing always-present convention.
	if !strings.Contains(js, "http.request(\"GET\", `http://localhost:8080/perimeters`, null, { headers: {}, tags: {} });") {
		t.Errorf("expected the tagless step to render an empty tags object, got:\n%s", js)
	}
}

func TestGenerateOmitsOptionsBlockWhenUnset(t *testing.T) {
	cfg := &config.Config{
		Service: config.ServiceConfig{BaseURL: "http://localhost:8080"},
		K6: config.K6Config{
			Steps: []config.K6Step{
				{Method: "GET", URL: "{{.BaseURL}}/health"},
			},
		},
	}

	path, cleanup, err := Generate(cfg)
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	defer cleanup()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading generated script: %v", err)
	}
	if strings.Contains(string(data), "export const options") {
		t.Errorf("expected no options block, got:\n%s", data)
	}
}

func TestGenerateInvalidRandomRangeFails(t *testing.T) {
	cfg := &config.Config{
		Service: config.ServiceConfig{BaseURL: "http://localhost:8080"},
		K6: config.K6Config{
			Steps: []config.K6Step{
				{Method: "GET", URL: `{{.BaseURL}}/x/{{random 5 1}}`},
			},
		},
	}

	if _, _, err := Generate(cfg); err == nil {
		t.Fatal("expected error for random with max < min, got nil")
	}
}
