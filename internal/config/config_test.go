package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "myrtille.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing temp config: %v", err)
	}
	return path
}

func TestLoadValidConfigAppliesDefaults(t *testing.T) {
	path := writeTemp(t, `
name: demo
service:
  base_url: http://localhost:8080
init:
  steps:
    - name: create_user
      method: post
      url: "{{.BaseURL}}/users"
      body: '{"name":"user-{{.Index}}"}'
      count: 3
      extract:
        - path: id
          as: user_ids
k6:
  script: ./scenario.js
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if cfg.Service.Metrics.Interval.Duration() != 5*time.Second {
		t.Errorf("expected default metrics interval of 5s, got %v", cfg.Service.Metrics.Interval.Duration())
	}
	if cfg.K6.StateEnv != "STATE_FILE" {
		t.Errorf("expected default state_env STATE_FILE, got %q", cfg.K6.StateEnv)
	}
	if cfg.Init.Steps[0].Method != "POST" {
		t.Errorf("expected method to be uppercased, got %q", cfg.Init.Steps[0].Method)
	}
	if len(cfg.Report.Formats) != 2 {
		t.Errorf("expected default report formats, got %v", cfg.Report.Formats)
	}

	wantScript := filepath.Join(filepath.Dir(path), "scenario.js")
	if cfg.K6ScriptPath() != wantScript {
		t.Errorf("K6ScriptPath() = %q, want %q", cfg.K6ScriptPath(), wantScript)
	}
}

func TestLoadCustomMetricsInterval(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
  metrics:
    url: http://localhost:8080/metrics
    interval: 2s
k6:
  script: ./scenario.js
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.Service.Metrics.Interval.Duration() != 2*time.Second {
		t.Errorf("expected 2s interval, got %v", cfg.Service.Metrics.Interval.Duration())
	}
}

func TestLoadMissingBaseURLFails(t *testing.T) {
	path := writeTemp(t, `
k6:
  script: ./scenario.js
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for missing service.base_url, got nil")
	}
}

func TestLoadMissingScriptFails(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for missing k6.script, got nil")
	}
}

func TestLoadInvalidStepMethodFails(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
init:
  steps:
    - name: bad
      method: BOGUS
      url: http://localhost:8080/x
k6:
  script: ./scenario.js
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for invalid method, got nil")
	}
}

func TestLoadInvalidExtractFails(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
init:
  steps:
    - name: bad
      url: http://localhost:8080/x
      extract:
        - path: ""
          as: ""
k6:
  script: ./scenario.js
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for invalid extract, got nil")
	}
}

func TestLoadInvalidReportFormatFails(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
k6:
  script: ./scenario.js
report:
  formats: ["pdf"]
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for invalid report format, got nil")
	}
}

func TestLoadMissingFileFails(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml")); err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestLoadInvalidDurationFails(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
  metrics:
    interval: "not-a-duration"
k6:
  script: ./scenario.js
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for invalid duration, got nil")
	}
}

func TestLoadVars(t *testing.T) {
	path := writeTemp(t, `
name: demo
vars:
  users_count: 5
  label: "hello"
service:
  base_url: http://localhost:8080
k6:
  script: ./scenario.js
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.Vars["users_count"] != 5 {
		t.Errorf("cfg.Vars[users_count] = %v, want 5", cfg.Vars["users_count"])
	}
	if cfg.Vars["label"] != "hello" {
		t.Errorf("cfg.Vars[label] = %v, want hello", cfg.Vars["label"])
	}
}

func TestLoadNestedChildrenValid(t *testing.T) {
	path := writeTemp(t, `
name: demo
vars:
  products_per_category: 3
service:
  base_url: http://localhost:8080
init:
  steps:
    - name: create_categories
      method: POST
      url: "{{.BaseURL}}/categories"
      count: 2
      extract:
        - path: id
          as: category_ids
      children:
        - name: create_products
          method: POST
          url: "{{.BaseURL}}/categories/{{.Parent.id}}/products"
          count: "{{.Vars.products_per_category}}"
          extract:
            - path: id
              as: product_ids
k6:
  script: ./scenario.js
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	parent := cfg.Init.Steps[0]
	if len(parent.Children) != 1 {
		t.Fatalf("expected 1 child, got %d", len(parent.Children))
	}
	child := parent.Children[0]
	if child.Method != "POST" {
		t.Errorf("expected child method to default+uppercase to POST, got %q", child.Method)
	}
	if child.Count != "{{.Vars.products_per_category}}" {
		t.Errorf("expected child count template preserved, got %q", child.Count)
	}
}

func TestLoadNestedChildInvalidFails(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
init:
  steps:
    - name: parent
      url: http://localhost:8080/x
      children:
        - name: bad_child
          url: ""
k6:
  script: ./scenario.js
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid nested child, got nil")
	}
	if !strings.Contains(err.Error(), "children[0]") {
		t.Errorf("expected error to reference children[0], got: %v", err)
	}
}

func TestLoadCountAsTemplateStringPassesValidation(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
init:
  steps:
    - name: create_users
      url: http://localhost:8080/users
      count: "{{random 1 5}}"
k6:
  script: ./scenario.js
`)
	if _, err := Load(path); err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
}

func TestLoadNegativeLiteralCountFails(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
init:
  steps:
    - name: create_users
      url: http://localhost:8080/users
      count: -1
k6:
  script: ./scenario.js
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for negative count, got nil")
	}
}

func TestLoadTeardownStepsValid(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
init:
  steps:
    - name: create_users
      method: POST
      url: "{{.BaseURL}}/users"
      count: 3
      extract:
        - path: id
          as: user_ids
teardown:
  steps:
    - name: delete_users
      method: delete
      url: "{{.BaseURL}}/users/{{index .Dict.user_ids .Index}}"
      count: "{{len .Dict.user_ids}}"
k6:
  script: ./scenario.js
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(cfg.Teardown.Steps) != 1 {
		t.Fatalf("expected 1 teardown step, got %d", len(cfg.Teardown.Steps))
	}
	if cfg.Teardown.Steps[0].Method != "DELETE" {
		t.Errorf("expected method to be uppercased, got %q", cfg.Teardown.Steps[0].Method)
	}
}

func TestLoadTeardownStepInvalidMethodFails(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
teardown:
  steps:
    - url: http://localhost:8080/x
      method: BOGUS
k6:
  script: ./scenario.js
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for invalid teardown step method, got nil")
	}
}

func TestLoadTeardownStepInvalidExtractFails(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
teardown:
  steps:
    - url: http://localhost:8080/x
      extract:
        - path: ""
          as: ""
k6:
  script: ./scenario.js
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for invalid teardown extract, got nil")
	}
}

func TestLoadK6StepsValid(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
k6:
  steps:
    - name: place_order
      method: post
      url: "{{.BaseURL}}/orders"
      body: '{"userId":"{{pick "user_ids"}}"}'
      checks:
        "status is 201": "r.status === 201"
      sleep: 200ms
  options:
    vus: 10
    duration: 30s
    thresholds:
      http_req_failed: ["rate<0.01"]
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(cfg.K6.Steps) != 1 {
		t.Fatalf("expected 1 k6 step, got %d", len(cfg.K6.Steps))
	}
	if cfg.K6.Steps[0].Method != "POST" {
		t.Errorf("expected method to be uppercased, got %q", cfg.K6.Steps[0].Method)
	}
	if cfg.K6.Options.VUs != 10 {
		t.Errorf("cfg.K6.Options.VUs = %d, want 10", cfg.K6.Options.VUs)
	}
}

func TestLoadK6ScriptAndStepsBothSetFails(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
k6:
  script: ./scenario.js
  steps:
    - url: http://localhost:8080/x
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error when both k6.script and k6.steps are set, got nil")
	}
}

func TestLoadK6ScriptAndOptionsFails(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
k6:
  script: ./scenario.js
  options:
    vus: 5
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error when k6.options is set alongside k6.script, got nil")
	}
}

func TestLoadK6StepInvalidMethodFails(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
k6:
  steps:
    - url: http://localhost:8080/x
      method: BOGUS
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for invalid k6 step method, got nil")
	}
}

func TestLoadK6StepEmptyCheckExpressionFails(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
k6:
  steps:
    - url: http://localhost:8080/x
      checks:
        "status ok": ""
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for empty check expression, got nil")
	}
}

func TestLoadK6OptionsInvalidStageFails(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
k6:
  steps:
    - url: http://localhost:8080/x
  options:
    stages:
      - duration: 0s
        target: 10
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for zero-duration stage, got nil")
	}
}

func TestLoadK6OptionsInvalidThresholdFails(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
k6:
  steps:
    - url: http://localhost:8080/x
  options:
    thresholds:
      http_req_failed: []
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for empty threshold expression list, got nil")
	}
}

func TestLoadInitCommandAppliesDefaultTimeout(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
init:
  command: ./seed.sh
k6:
  script: ./scenario.js
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.Init.CommandTimeout.Duration() != 5*time.Minute {
		t.Errorf("expected default init.command_timeout of 5m, got %v", cfg.Init.CommandTimeout.Duration())
	}
}

func TestLoadInitCommandAndStepsBothSetFails(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
init:
  command: ./seed.sh
  steps:
    - url: http://localhost:8080/x
k6:
  script: ./scenario.js
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error when both init.command and init.steps are set, got nil")
	}
}

func TestLoadInitCommandNegativeTimeoutFails(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
init:
  command: ./seed.sh
  command_timeout: -1s
k6:
  script: ./scenario.js
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for negative init.command_timeout, got nil")
	}
}
