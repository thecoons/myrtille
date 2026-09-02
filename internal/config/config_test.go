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

func TestLoadServiceLifecycleValidAppliesDefaults(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
  start_command: ./start.sh
  readiness:
    url: /healthz
k6:
  script: ./scenario.js
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.Service.StopSignal != "TERM" {
		t.Errorf("expected default stop_signal TERM, got %q", cfg.Service.StopSignal)
	}
	if cfg.Service.Readiness.Timeout.Duration() != 5*time.Minute {
		t.Errorf("expected default readiness.timeout 5m, got %v", cfg.Service.Readiness.Timeout.Duration())
	}
	if cfg.Service.Readiness.Interval.Duration() != time.Second {
		t.Errorf("expected default readiness.interval 1s, got %v", cfg.Service.Readiness.Interval.Duration())
	}
	if cfg.Service.StopTimeout.Duration() != 30*time.Second {
		t.Errorf("expected default stop_timeout 30s, got %v", cfg.Service.StopTimeout.Duration())
	}
}

func TestLoadServiceLifecycleCustomValuesPreserved(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
  start_command: ./start.sh
  stop_signal: KILL
  stop_timeout: 10s
  readiness:
    url: /healthz
    timeout: 15s
    interval: 500ms
k6:
  script: ./scenario.js
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.Service.StopSignal != "KILL" {
		t.Errorf("expected stop_signal KILL, got %q", cfg.Service.StopSignal)
	}
	if cfg.Service.Readiness.Timeout.Duration() != 15*time.Second {
		t.Errorf("expected readiness.timeout 15s, got %v", cfg.Service.Readiness.Timeout.Duration())
	}
	if cfg.Service.Readiness.Interval.Duration() != 500*time.Millisecond {
		t.Errorf("expected readiness.interval 500ms, got %v", cfg.Service.Readiness.Interval.Duration())
	}
	if cfg.Service.StopTimeout.Duration() != 10*time.Second {
		t.Errorf("expected stop_timeout 10s, got %v", cfg.Service.StopTimeout.Duration())
	}
}

func TestLoadServiceStopSignalWithoutStartCommandFails(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
  stop_signal: TERM
k6:
  script: ./scenario.js
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for stop_signal without start_command, got nil")
	}
	if !strings.Contains(err.Error(), "service.stop_signal is only used with service.start_command") {
		t.Errorf("expected a stop_signal-without-start_command error, got %q", err.Error())
	}
}

func TestLoadServiceReadinessWithoutStartCommandFails(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
  readiness:
    url: /healthz
k6:
  script: ./scenario.js
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for readiness without start_command, got nil")
	}
	if !strings.Contains(err.Error(), "service.readiness is only used with service.start_command") {
		t.Errorf("expected a readiness-without-start_command error, got %q", err.Error())
	}
}

func TestLoadServiceStartCommandWithoutReadinessURLFails(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
  start_command: ./start.sh
k6:
  script: ./scenario.js
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for start_command without readiness.url, got nil")
	}
	if !strings.Contains(err.Error(), "service.readiness.url is required when service.start_command is set") {
		t.Errorf("expected a readiness.url-required error, got %q", err.Error())
	}
}

func TestLoadServiceInvalidStopSignalFails(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
  start_command: ./start.sh
  stop_signal: BOGUS
  readiness:
    url: /healthz
k6:
  script: ./scenario.js
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for unsupported stop_signal, got nil")
	}
	if !strings.Contains(err.Error(), `service.stop_signal: unsupported signal "BOGUS"`) {
		t.Errorf("expected an unsupported-signal error, got %q", err.Error())
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

func TestLoadAcceptsDashboardHTMLFormat(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
k6:
  script: ./scenario.js
report:
  formats: ["markdown", "dashboard-html"]
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(cfg.Report.Formats) != 2 || cfg.Report.Formats[1] != "dashboard-html" {
		t.Fatalf("unexpected formats: %+v", cfg.Report.Formats)
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

func TestLoadDeriveRulesValid(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
init:
  steps:
    - name: collect_perimeters
      url: "{{.BaseURL}}/perimeters"
      extract:
        - path: items
          as: perimeter_items
  derive:
    - as: leaf_keys
      input: perimeter_items
      expr: |
        map(select(.spec.parent != null) | .spec.parent)
k6:
  script: ./scenario.js
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(cfg.Init.Derive) != 1 {
		t.Fatalf("expected 1 derive rule, got %d", len(cfg.Init.Derive))
	}
	rule := cfg.Init.Derive[0]
	if rule.As != "leaf_keys" {
		t.Errorf("expected as %q, got %q", "leaf_keys", rule.As)
	}
	if rule.Input != "perimeter_items" {
		t.Errorf("expected input %q, got %q", "perimeter_items", rule.Input)
	}
	if !strings.Contains(rule.Expr, "select(.spec.parent != null)") {
		t.Errorf("expected expr to contain the jq filter, got %q", rule.Expr)
	}
}

func TestLoadDeriveRuleMissingAsFails(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
init:
  derive:
    - expr: "."
k6:
  script: ./scenario.js
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for derive rule missing as, got nil")
	}
	if !strings.Contains(err.Error(), "init.derive[0]: as is required") {
		t.Errorf("expected error to identify the faulty rule by index, got %q", err.Error())
	}
}

func TestLoadDeriveRuleMissingExprFails(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
init:
  derive:
    - as: leaf_keys
k6:
  script: ./scenario.js
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for derive rule missing expr, got nil")
	}
	if !strings.Contains(err.Error(), "init.derive[0] (leaf_keys): expr is required") {
		t.Errorf("expected error to identify the faulty rule by its as name, got %q", err.Error())
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

func TestLoadK6SetupValid(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
k6:
  setup:
    - name: create_revision
      method: post
      url: "{{.BaseURL}}/revisions"
      body: '{"name":"bench-{{uniqueId}}"}'
      extract:
        - path: name
          as: version_name
  steps:
    - url: "{{.BaseURL}}/revisions/{{pick \"version_name\"}}/items"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.K6.Setup[0].Method != "POST" {
		t.Errorf("expected method to be uppercased, got %q", cfg.K6.Setup[0].Method)
	}
	if cfg.K6.Setup[0].Extract[0].As != "version_name" {
		t.Errorf("expected extract to be preserved, got %+v", cfg.K6.Setup[0].Extract)
	}
}

func TestLoadK6SetupMissingURLFails(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
k6:
  setup:
    - method: POST
  steps:
    - url: http://localhost:8080/x
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for missing k6.setup url, got nil")
	}
}

func TestLoadK6SetupEmptyExtractPathFails(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
k6:
  setup:
    - url: http://localhost:8080/revisions
      extract:
        - path: ""
          as: version_name
  steps:
    - url: http://localhost:8080/x
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for empty extract path, got nil")
	}
}

func TestLoadK6SetupAndScriptFails(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
k6:
  script: ./scenario.js
  setup:
    - url: http://localhost:8080/revisions
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error when k6.setup is set alongside k6.script, got nil")
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

func TestLoadK6StepTagsValid(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
k6:
  steps:
    - url: http://localhost:8080/x
      tags:
        endpoint: get
        selector: "{{.Vars.selector}}"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.K6.Steps[0].Tags["endpoint"] != "get" {
		t.Errorf("expected tags to be preserved, got %+v", cfg.K6.Steps[0].Tags)
	}
}

func TestLoadK6StepEmptyTagNameFails(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
k6:
  steps:
    - url: http://localhost:8080/x
      tags:
        "": get
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for empty tag name, got nil")
	}
}

func TestLoadK6StepRepeatTemplateExpressionPassesValidation(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
k6:
  steps:
    - url: http://localhost:8080/x
      repeat: "{{.Vars.perimeters_per_version}}"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.K6.Steps[0].Repeat != "{{.Vars.perimeters_per_version}}" {
		t.Errorf("expected repeat to be preserved, got %q", cfg.K6.Steps[0].Repeat)
	}
}

func TestLoadK6StepNegativeLiteralRepeatFails(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
k6:
  steps:
    - url: http://localhost:8080/x
      repeat: "-1"
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for negative literal repeat, got nil")
	}
}

func TestLoadK6StepBodyFromPatchValid(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
k6:
  steps:
    - name: touch_root
      method: PUT
      url: '{{.BaseURL}}/domains/{{pick "root_perimeters" "metadata.domain"}}/perimeters/{{pick "root_perimeters" "metadata.name"}}'
      body_from: root_perimeters
      body_patch:
        metadata.labels.touched: "{{uniqueId}}"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	step := cfg.K6.Steps[0]
	if step.BodyFrom != "root_perimeters" {
		t.Errorf("expected body_from %q, got %q", "root_perimeters", step.BodyFrom)
	}
	if got := step.BodyPatch["metadata.labels.touched"]; got != "{{uniqueId}}" {
		t.Errorf("expected body_patch entry %q, got %q", "{{uniqueId}}", got)
	}
}

func TestLoadK6StepBodyFromAloneValid(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
k6:
  steps:
    - url: http://localhost:8080/x
      body_from: some_pool
`)
	if _, err := Load(path); err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
}

func TestLoadK6StepBodyAndBodyFromBothSetFails(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
k6:
  steps:
    - url: http://localhost:8080/x
      body: '{"x":1}'
      body_from: some_pool
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error when both body and body_from are set, got nil")
	}
	if !strings.Contains(err.Error(), "exactly one of body or body_from must be set") {
		t.Errorf("expected a mutual-exclusivity error, got %q", err.Error())
	}
}

func TestLoadK6StepBodyPatchWithoutBodyFromFails(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
k6:
  steps:
    - url: http://localhost:8080/x
      body_patch:
        foo: bar
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for body_patch without body_from, got nil")
	}
	if !strings.Contains(err.Error(), "body_patch requires body_from") {
		t.Errorf("expected a body_patch-requires-body_from error, got %q", err.Error())
	}
}

func TestLoadK6StepBodyPatchEmptyValueFails(t *testing.T) {
	path := writeTemp(t, `
service:
  base_url: http://localhost:8080
k6:
  steps:
    - url: http://localhost:8080/x
      body_from: some_pool
      body_patch:
        foo: ""
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for empty body_patch value, got nil")
	}
	if !strings.Contains(err.Error(), `body_patch["foo"]: value is required`) {
		t.Errorf("expected a value-is-required error identifying the path, got %q", err.Error())
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
