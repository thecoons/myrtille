package k6gen

import (
	"os"
	"path/filepath"
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

func TestGenerateRendersUniqueId(t *testing.T) {
	cfg := &config.Config{
		Service: config.ServiceConfig{BaseURL: "http://localhost:8080"},
		K6: config.K6Config{
			Steps: []config.K6Step{
				{
					Name:   "create_resource",
					Method: "POST",
					URL:    "{{.BaseURL}}/resources",
					Body:   `{"name":"write-{{uniqueId}}"}`,
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

	want := "write-${typeof __VU !== 'undefined' ? __VU : 'setup'}-${typeof __ITER !== 'undefined' ? __ITER : 0}-${Date.now()}"
	if !strings.Contains(js, want) {
		t.Errorf("generated script missing %q\n--- full script ---\n%s", want, js)
	}
}

func TestGenerateRendersRepeatLoop(t *testing.T) {
	cfg := &config.Config{
		Service: config.ServiceConfig{BaseURL: "http://localhost:8080"},
		Vars:    map[string]any{"perimeters_per_version": 3},
		K6: config.K6Config{
			Steps: []config.K6Step{
				{
					Name:   "touch_perimeter",
					Method: "PATCH",
					URL:    "{{.BaseURL}}/perimeters",
					Repeat: "{{.Vars.perimeters_per_version}}",
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

	if !strings.Contains(js, "for (let i = 0; i < 3; i++) {") {
		t.Errorf("expected a repeat loop for i < 3, got:\n%s", js)
	}
}

func TestGenerateOmitsRepeatLoopByDefault(t *testing.T) {
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
	if strings.Contains(string(data), "for (") {
		t.Errorf("expected no for loop when repeat is unset, got:\n%s", data)
	}
}

func TestGenerateRepeatInvalidValueFails(t *testing.T) {
	cfg := &config.Config{
		Service: config.ServiceConfig{BaseURL: "http://localhost:8080"},
		K6: config.K6Config{
			Steps: []config.K6Step{
				{Method: "GET", URL: "{{.BaseURL}}/x", Repeat: "{{.Vars.missing}}"},
			},
		},
	}

	if _, _, err := Generate(cfg); err == nil {
		t.Fatal("expected error when repeat does not resolve to an integer")
	}
}

func TestGenerateRendersStepTimeout(t *testing.T) {
	cfg := &config.Config{
		Service: config.ServiceConfig{BaseURL: "http://localhost:8080"},
		K6: config.K6Config{
			Steps: []config.K6Step{
				{Method: "GET", URL: "{{.BaseURL}}/big-list", Timeout: "90s"},
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
	if !strings.Contains(string(data), "timeout: `90s`") {
		t.Errorf("expected timeout: `90s` in generated request params, got:\n%s", data)
	}
}

func TestGenerateOmitsTimeoutByDefault(t *testing.T) {
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
	if strings.Contains(string(data), "timeout:") {
		t.Errorf("expected no timeout: in generated request params when unset, got:\n%s", data)
	}
}

func TestGenerateStepTimeoutResolvesVarsTemplate(t *testing.T) {
	cfg := &config.Config{
		Service: config.ServiceConfig{BaseURL: "http://localhost:8080"},
		Vars:    map[string]any{"slow_endpoint_timeout": "2m"},
		K6: config.K6Config{
			Steps: []config.K6Step{
				{Method: "GET", URL: "{{.BaseURL}}/big-list", Timeout: "{{.Vars.slow_endpoint_timeout}}"},
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
	if !strings.Contains(string(data), "timeout: `2m`") {
		t.Errorf("expected timeout: `2m` (resolved from .Vars), got:\n%s", data)
	}
}

func TestGenerateCorrelatesPickWithFieldSelector(t *testing.T) {
	cfg := &config.Config{
		Service: config.ServiceConfig{BaseURL: "http://localhost:8080"},
		K6: config.K6Config{
			Steps: []config.K6Step{
				{
					Name:   "list_perimeter",
					Method: "GET",
					URL:    `{{.BaseURL}}/api/v1/domains/{{pick "perimeter_keys" "domain"}}/perimeters/{{pick "perimeter_keys" "name"}}`,
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

	// A single hoisted draw...
	if !strings.Contains(js, `const __pick0 = pick(state["perimeter_keys"]);`) {
		t.Errorf("expected a single hoisted pick declaration, got:\n%s", js)
	}
	// ...referenced by both field accesses, not two independent pick() calls.
	if !strings.Contains(js, `${__pick0["domain"]}`) || !strings.Contains(js, `${__pick0["name"]}`) {
		t.Errorf("expected both field accesses to reference the same hoisted variable, got:\n%s", js)
	}
	if strings.Count(js, "pick(state[\"perimeter_keys\"])") != 1 {
		t.Errorf("expected exactly one pick() call for the correlated pool, got:\n%s", js)
	}
}

func TestGenerateCorrelatesPickWithDotPathFieldSelector(t *testing.T) {
	cfg := &config.Config{
		Service: config.ServiceConfig{BaseURL: "http://localhost:8080"},
		K6: config.K6Config{
			Steps: []config.K6Step{
				{
					Name:   "list_perimeter",
					Method: "GET",
					URL:    `{{.BaseURL}}/api/v1/domains/{{pick "root_perimeters" "metadata.domain"}}/perimeters/{{pick "root_perimeters" "metadata.name"}}`,
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

	if !strings.Contains(js, `const __pick0 = pick(state["root_perimeters"]);`) {
		t.Errorf("expected a single hoisted pick declaration, got:\n%s", js)
	}
	if !strings.Contains(js, `${__pick0["metadata"]["domain"]}`) || !strings.Contains(js, `${__pick0["metadata"]["name"]}`) {
		t.Errorf("expected chained bracket access for both dot-path field selectors, got:\n%s", js)
	}
	if strings.Count(js, "pick(state[\"root_perimeters\"])") != 1 {
		t.Errorf("expected exactly one pick() call for the correlated pool, got:\n%s", js)
	}
}

func TestGenerateRendersBodyFromAndPatchCorrelatedWithURLPick(t *testing.T) {
	cfg := &config.Config{
		Service: config.ServiceConfig{BaseURL: "http://localhost:8080"},
		K6: config.K6Config{
			Steps: []config.K6Step{
				{
					Name:     "touch_root",
					Method:   "PUT",
					URL:      `{{.BaseURL}}/domains/{{pick "root_perimeters" "metadata.domain"}}/perimeters/{{pick "root_perimeters" "metadata.name"}}`,
					BodyFrom: "root_perimeters",
					BodyPatch: map[string]string{
						"metadata.labels.touched": "{{uniqueId}}",
					},
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

	// Exactly one hoisted draw, shared by the URL's field-picks and body_from.
	if strings.Count(js, "pick(state[\"root_perimeters\"])") != 1 {
		t.Errorf("expected exactly one pick() call for the correlated pool, got:\n%s", js)
	}
	if !strings.Contains(js, `const body = JSON.parse(JSON.stringify(__pick0));`) {
		t.Errorf("expected a deep clone of the hoisted pick variable, got:\n%s", js)
	}
	if !strings.Contains(js, "body[\"metadata\"][\"labels\"][\"touched\"] = `") {
		t.Errorf("expected a chained bracket assignment for the patch path, got:\n%s", js)
	}
	if !strings.Contains(js, "http.request(\"PUT\", `") || !strings.Contains(js, "JSON.stringify(body)") {
		t.Errorf("expected the request body to be JSON.stringify(body), got:\n%s", js)
	}
	// The clone/patch lines must come after the hoisted declaration and
	// before the request call, in that order.
	declIdx := strings.Index(js, "const __pick0 = pick(")
	cloneIdx := strings.Index(js, "const body = JSON.parse(")
	patchIdx := strings.Index(js, "body[\"metadata\"][\"labels\"][\"touched\"]")
	reqIdx := strings.Index(js, "http.request(")
	if !(declIdx < cloneIdx && cloneIdx < patchIdx && patchIdx < reqIdx) {
		t.Errorf("expected declaration < clone < patch < request ordering, got indices %d, %d, %d, %d in:\n%s", declIdx, cloneIdx, patchIdx, reqIdx, js)
	}
}

func TestGenerateBodyFromWithoutPatchClonesUnmodified(t *testing.T) {
	cfg := &config.Config{
		Service: config.ServiceConfig{BaseURL: "http://localhost:8080"},
		K6: config.K6Config{
			Steps: []config.K6Step{
				{
					Method:   "PUT",
					URL:      "{{.BaseURL}}/x",
					BodyFrom: "pool",
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

	if !strings.Contains(js, `const __pick0 = pick(state["pool"]);`) {
		t.Errorf("expected body_from alone to still hoist a pick declaration, got:\n%s", js)
	}
	if !strings.Contains(js, `const body = JSON.parse(JSON.stringify(__pick0));`) {
		t.Errorf("expected a deep clone with no patch lines, got:\n%s", js)
	}
	if strings.Contains(js, "body[") {
		t.Errorf("expected no patch assignment lines when body_patch is unset, got:\n%s", js)
	}
}

func TestGenerateBodyPatchAppliesInSortedKeyOrder(t *testing.T) {
	cfg := &config.Config{
		Service: config.ServiceConfig{BaseURL: "http://localhost:8080"},
		K6: config.K6Config{
			Steps: []config.K6Step{
				{
					Method:   "PUT",
					URL:      "{{.BaseURL}}/x",
					BodyFrom: "pool",
					BodyPatch: map[string]string{
						"zebra": "z",
						"alpha": "a",
					},
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

	alphaIdx := strings.Index(js, `body["alpha"]`)
	zebraIdx := strings.Index(js, `body["zebra"]`)
	if alphaIdx == -1 || zebraIdx == -1 || alphaIdx >= zebraIdx {
		t.Errorf("expected body_patch entries in sorted key order (alpha before zebra), got:\n%s", js)
	}
}

func TestGenerateFieldlessPickStaysIndependent(t *testing.T) {
	cfg := &config.Config{
		Service: config.ServiceConfig{BaseURL: "http://localhost:8080"},
		K6: config.K6Config{
			Steps: []config.K6Step{
				{
					Name:   "mixed",
					Method: "GET",
					URL:    `{{.BaseURL}}/x/{{pick "user_ids"}}/y/{{pick "user_ids"}}`,
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

	if strings.Contains(js, "__pick0") {
		t.Errorf("expected no hoisted variable for fieldless pick calls, got:\n%s", js)
	}
	if strings.Count(js, `${pick(state["user_ids"])}`) != 2 {
		t.Errorf("expected two independent fieldless pick() calls, got:\n%s", js)
	}
}

func TestGenerateCorrelatedPickScopedPerStep(t *testing.T) {
	cfg := &config.Config{
		Service: config.ServiceConfig{BaseURL: "http://localhost:8080"},
		K6: config.K6Config{
			Steps: []config.K6Step{
				{Method: "GET", URL: `{{.BaseURL}}/a/{{pick "pool" "x"}}`},
				{Method: "GET", URL: `{{.BaseURL}}/b/{{pick "pool" "x"}}`},
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
	// Each step is its own JS block scope, so both independently declare
	// __pick0 — no cross-step collision, and no cross-step correlation.
	if strings.Count(string(data), `const __pick0 = pick(state["pool"]);`) != 2 {
		t.Errorf("expected each step to independently hoist its own __pick0, got:\n%s", data)
	}
}

func TestGenerateCorrelatedPickTooManyArgsFails(t *testing.T) {
	cfg := &config.Config{
		Service: config.ServiceConfig{BaseURL: "http://localhost:8080"},
		K6: config.K6Config{
			Steps: []config.K6Step{
				{Method: "GET", URL: `{{.BaseURL}}/x/{{pick "pool" "a" "b"}}`},
			},
		},
	}

	if _, _, err := Generate(cfg); err == nil {
		t.Fatal("expected error for pick called with more than one field argument")
	}
}

func TestGenerateRendersSetupOnceAndSharesStateWithSteps(t *testing.T) {
	cfg := &config.Config{
		Service: config.ServiceConfig{BaseURL: "http://localhost:8080"},
		K6: config.K6Config{
			Setup: []config.K6SetupStep{
				{
					Name:   "create_revision",
					Method: "POST",
					URL:    "{{.BaseURL}}/revisions",
					Body:   `{"name":"bench-{{uniqueId}}"}`,
					Extract: []config.Extract{
						{Path: "name", As: "version_name"},
					},
				},
			},
			Steps: []config.K6Step{
				{
					Name:   "list_by_revision",
					Method: "GET",
					URL:    `{{.BaseURL}}/revisions/{{pick "version_name"}}/items`,
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
		"function extractPath(obj, path) {",
		"export function setup() {",
		"http.request(\"POST\", `http://localhost:8080/revisions`",
		"bench-${typeof __VU !== 'undefined' ? __VU : 'setup'}-${typeof __ITER !== 'undefined' ? __ITER : 0}-${Date.now()}",
		"const body = res.json();",
		`state["version_name"] = state["version_name"] || [];`,
		`state["version_name"].push(extractPath(body, "name"));`,
		"  return state;\n}",
		"export default function (data) {",
		"  const state = data;",
		"${pick(state[\"version_name\"])}",
	} {
		if !strings.Contains(js, snippet) {
			t.Errorf("generated script missing %q\n--- full script ---\n%s", snippet, js)
		}
	}

	// setup() must come before the default function in the generated file.
	if strings.Index(js, "export function setup()") > strings.Index(js, "export default function") {
		t.Errorf("expected setup() to be emitted before the default function, got:\n%s", js)
	}
}

func TestGenerateRendersSetupStepTimeout(t *testing.T) {
	cfg := &config.Config{
		Service: config.ServiceConfig{BaseURL: "http://localhost:8080"},
		K6: config.K6Config{
			Setup: []config.K6SetupStep{
				{Method: "POST", URL: "{{.BaseURL}}/revisions", Timeout: "45s"},
			},
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
	if !strings.Contains(string(data), "timeout: `45s`") {
		t.Errorf("expected timeout: `45s` in generated setup request params, got:\n%s", data)
	}
}

func TestGenerateOmitsSetupByDefault(t *testing.T) {
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
	js := string(data)

	for _, absent := range []string{"setup(", "extractPath", "const state = data"} {
		if strings.Contains(js, absent) {
			t.Errorf("expected no %q when k6.setup is unset, got:\n%s", absent, js)
		}
	}
	if !strings.Contains(js, "export default function () {") {
		t.Errorf("expected the default function to take no parameter, got:\n%s", js)
	}
}

func TestGenerateWiresPromscrapeWhenMetricsURLConfigured(t *testing.T) {
	t.Setenv("MYRTILLE_K6_BIN", "/fake/k6") // only os.LookupEnv is checked; no need for the file to exist

	cfg := &config.Config{
		Service: config.ServiceConfig{
			BaseURL: "http://localhost:8080",
			Metrics: config.MetricsConfig{
				URL:      "http://localhost:8080/metrics",
				Interval: config.Duration(5 * time.Second),
			},
		},
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
	js := string(data)

	for _, snippet := range []string{
		"import promscrape from 'k6/x/promscrape';",
		`const __promscrape = new promscrape.Scraper("http://localhost:8080/metrics");`,
		"export function setup() {\n  __promscrape.start(5000);\n",
		"  return state;\n}",
		"export default function (data) {",
		"  const state = data;",
	} {
		if !strings.Contains(js, snippet) {
			t.Errorf("generated script missing %q\n--- full script ---\n%s", snippet, js)
		}
	}

	// The Scraper must be constructed at module scope (init context, needed
	// for metric registration), before setup() — not inside it.
	scraperIdx := strings.Index(js, "new promscrape.Scraper")
	setupIdx := strings.Index(js, "export function setup()")
	if scraperIdx < 0 || setupIdx < 0 || scraperIdx > setupIdx {
		t.Errorf("expected promscrape.Scraper construction before setup(), got:\n%s", js)
	}
}

func TestGenerateOmitsPromscrapeByDefault(t *testing.T) {
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
	js := string(data)

	if strings.Contains(js, "promscrape") {
		t.Errorf("expected no promscrape wiring when service.metrics.url is unset, got:\n%s", js)
	}
}

// TestGenerateOmitsPromscrapeWithoutCustomBinary is the regression this
// step caught: examples/demo-service/myrtille.yaml sets service.metrics.url
// (still read by the separate, not-yet-removed Go-side scraper in
// internal/metrics), and a real run of it against stock k6 (no
// MYRTILLE_K6_BIN) failed — the generated script unconditionally imported
// k6/x/promscrape, which stock k6 doesn't have. service.metrics.url alone
// must not be enough to wire promscrape in; MYRTILLE_K6_BIN must be set too.
func TestGenerateOmitsPromscrapeWithoutCustomBinary(t *testing.T) {
	t.Setenv("MYRTILLE_K6_BIN", "")

	cfg := &config.Config{
		Service: config.ServiceConfig{
			BaseURL: "http://localhost:8080",
			Metrics: config.MetricsConfig{
				URL:      "http://localhost:8080/metrics",
				Interval: config.Duration(5 * time.Second),
			},
		},
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
	js := string(data)

	if strings.Contains(js, "promscrape") {
		t.Errorf("expected no promscrape wiring without MYRTILLE_K6_BIN, even with service.metrics.url set, got:\n%s", js)
	}
}

// TestGenerateWiresPromscrapeForCoLocatedBinaryWithoutEnvVar is the
// regression this found: examples/demo-service, run exactly the way its
// own README instructs (bin/myrtille run ... with bin/k6 sitting right
// next to it, no MYRTILLE_K6_BIN set), got a live dashboard but never
// actually scraped service metrics into it — k6run.HasCustomBinary() used
// to only check MYRTILLE_K6_BIN, missing the co-located case entirely, so
// Generate never wired promscrape in even though Run itself would have
// resolved and used the co-located custom binary.
func TestGenerateWiresPromscrapeForCoLocatedBinaryWithoutEnvVar(t *testing.T) {
	t.Setenv("MYRTILLE_K6_BIN", "")

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(self)
	if err != nil {
		t.Fatalf("filepath.EvalSymlinks: %v", err)
	}
	sibling := filepath.Join(filepath.Dir(resolved), "k6")
	if err := os.WriteFile(sibling, []byte("fake"), 0o755); err != nil {
		t.Fatalf("writing fake sibling k6: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(sibling) })

	cfg := &config.Config{
		Service: config.ServiceConfig{
			BaseURL: "http://localhost:8080",
			Metrics: config.MetricsConfig{
				URL:      "http://localhost:8080/metrics",
				Interval: config.Duration(5 * time.Second),
			},
		},
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
	js := string(data)

	if !strings.Contains(js, "import promscrape from 'k6/x/promscrape';") {
		t.Errorf("expected promscrape wiring for a co-located k6 binary even without MYRTILLE_K6_BIN, got:\n%s", js)
	}
}

func TestGenerateSetupExtractPathHandlesNestedField(t *testing.T) {
	cfg := &config.Config{
		Service: config.ServiceConfig{BaseURL: "http://localhost:8080"},
		K6: config.K6Config{
			Setup: []config.K6SetupStep{
				{
					Method: "POST",
					URL:    "{{.BaseURL}}/users",
					Extract: []config.Extract{
						{Path: "user.id", As: "user_ids"},
					},
				},
			},
			Steps: []config.K6Step{
				{Method: "GET", URL: `{{.BaseURL}}/users/{{pick "user_ids"}}`},
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
	if !strings.Contains(string(data), `extractPath(body, "user.id")`) {
		t.Errorf("expected the nested path to be passed through verbatim, got:\n%s", data)
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
