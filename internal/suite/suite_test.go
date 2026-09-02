package suite

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "suite.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing temp suite file: %v", err)
	}
	return path
}

func TestLoadValidResolvesRelativeScenarioPaths(t *testing.T) {
	path := writeTemp(t, `
scenarios:
  - smoke.yaml
  - perimeter-list.yaml
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	want := []string{
		filepath.Join(filepath.Dir(path), "smoke.yaml"),
		filepath.Join(filepath.Dir(path), "perimeter-list.yaml"),
	}
	got := cfg.ScenarioPaths()
	if len(got) != len(want) {
		t.Fatalf("ScenarioPaths() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ScenarioPaths()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLoadAbsoluteScenarioPathPassesThrough(t *testing.T) {
	path := writeTemp(t, `
scenarios:
  - /abs/path/scenario.yaml
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	got := cfg.ScenarioPaths()
	if len(got) != 1 || got[0] != "/abs/path/scenario.yaml" {
		t.Errorf("ScenarioPaths() = %v, want [/abs/path/scenario.yaml]", got)
	}
}

func TestLoadEmptyScenariosFails(t *testing.T) {
	path := writeTemp(t, `scenarios: []`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for empty scenarios, got nil")
	}
	if !strings.Contains(err.Error(), "at least one config path") {
		t.Errorf("expected an empty-scenarios error, got %q", err.Error())
	}
}

func TestLoadMissingScenariosFieldFails(t *testing.T) {
	path := writeTemp(t, `restart_between_runs: true`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for missing scenarios field, got nil")
	}
}

func TestLoadEmptyScenarioEntryFails(t *testing.T) {
	path := writeTemp(t, `
scenarios:
  - smoke.yaml
  - ""
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for an empty scenario entry, got nil")
	}
	if !strings.Contains(err.Error(), "scenarios[1]") {
		t.Errorf("expected the error to identify the faulty index, got %q", err.Error())
	}
}

// writeScenario writes a minimal, loadable myrtille.yaml-shaped config
// next to the suite file at path (same dir), for tests exercising
// restart_between_runs: false — which loads scenario configs during
// Load itself to check their service blocks.
func writeScenario(t *testing.T, suitePath, name, baseURL, startCommand string) {
	t.Helper()
	svc := "service:\n  base_url: " + baseURL + "\n"
	if startCommand != "" {
		svc += "  start_command: " + startCommand + "\n"
		svc += "  readiness:\n    url: /\n"
	}
	content := svc + "k6:\n  script: ./scenario.js\n"
	if err := os.WriteFile(filepath.Join(filepath.Dir(suitePath), name), []byte(content), 0o644); err != nil {
		t.Fatalf("writing scenario %s: %v", name, err)
	}
}

func TestLoadRestartBetweenRunsFalseValid(t *testing.T) {
	path := writeTemp(t, `
scenarios:
  - a.yaml
  - b.yaml
restart_between_runs: false
`)
	writeScenario(t, path, "a.yaml", "http://localhost:8080", "./start.sh")
	writeScenario(t, path, "b.yaml", "http://localhost:8080", "")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.RestartsBetweenRuns() {
		t.Error("expected RestartsBetweenRuns() to be false")
	}
}

func TestLoadRestartBetweenRunsFalseRequiresFirstScenarioStartCommand(t *testing.T) {
	path := writeTemp(t, `
scenarios:
  - a.yaml
restart_between_runs: false
`)
	writeScenario(t, path, "a.yaml", "http://localhost:8080", "")

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error when the first scenario has no service.start_command, got nil")
	}
	if !strings.Contains(err.Error(), "requires the first scenario") {
		t.Errorf("expected a requires-first-scenario error, got %q", err.Error())
	}
}

func TestLoadRestartBetweenRunsFalseRequiresMatchingBaseURL(t *testing.T) {
	path := writeTemp(t, `
scenarios:
  - a.yaml
  - b.yaml
restart_between_runs: false
`)
	writeScenario(t, path, "a.yaml", "http://localhost:8080", "./start.sh")
	writeScenario(t, path, "b.yaml", "http://localhost:9090", "")

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for a mismatched service.base_url, got nil")
	}
	if !strings.Contains(err.Error(), "scenarios[1]") || !strings.Contains(err.Error(), "b.yaml") {
		t.Errorf("expected the error to identify the faulty scenario, got %q", err.Error())
	}
}

func TestLoadRestartBetweenRunsTrueSkipsSharedServiceValidation(t *testing.T) {
	// Mismatched base URLs are fine when each scenario restarts its own
	// service independently (the default) — the shared-service checks
	// only apply under restart_between_runs: false.
	path := writeTemp(t, `
scenarios:
  - a.yaml
  - b.yaml
`)
	writeScenario(t, path, "a.yaml", "http://localhost:8080", "")
	writeScenario(t, path, "b.yaml", "http://localhost:9090", "")

	if _, err := Load(path); err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
}

func TestLoadMissingFileFails(t *testing.T) {
	_, err := Load("/does/not/exist/suite.yaml")
	if err == nil {
		t.Fatal("expected error for a missing suite file, got nil")
	}
}
