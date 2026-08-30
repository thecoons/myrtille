package config

import (
	"os"
	"path/filepath"
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
