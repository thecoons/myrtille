package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/thecoons/myrtille/internal/config"
)

// installFakeK6 writes a shell-script stand-in for the k6 binary onto PATH
// for the duration of the test, so Run() can be exercised end-to-end
// without a real k6 install. If $FAKE_K6_STATE_CAPTURE is set, the fake
// also copies $STATE_FILE's content there, so a test can assert on the
// state dict k6 actually received.
func installFakeK6(t *testing.T, exitCode int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake k6 shim is a POSIX shell script")
	}

	dir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
summary_path=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "--summary-export" ]; then
    summary_path="$arg"
  fi
  prev="$arg"
done
if [ -n "$summary_path" ]; then
  cat > "$summary_path" <<'SUMMARY_EOF'
{"metrics":{"http_req_duration":{"avg":10,"thresholds":{"p(95)<500":{"ok":true}}}}}
SUMMARY_EOF
fi
if [ -n "$FAKE_K6_STATE_CAPTURE" ] && [ -n "$STATE_FILE" ]; then
  cat "$STATE_FILE" > "$FAKE_K6_STATE_CAPTURE"
fi
exit %d
`, exitCode)

	path := filepath.Join(dir, "k6")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake k6 script: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func writeConfig(t *testing.T, yaml string) *config.Config {
	t.Helper()
	dir := t.TempDir()

	scriptPath := filepath.Join(dir, "scenario.js")
	if err := os.WriteFile(scriptPath, []byte("export default function() {}"), 0o644); err != nil {
		t.Fatalf("writing scenario.js: %v", err)
	}

	cfgPath := filepath.Join(dir, "myrtille.yaml")
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("loading config: %v", err)
	}
	return cfg
}

func newFakeService(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"%s-id"}`, body.Name)
	})
	mux.HandleFunc("/fails", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "# TYPE memory_usage_bytes gauge\nmemory_usage_bytes 12345\n")
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func TestRunEndToEndSuccess(t *testing.T) {
	installFakeK6(t, 0)
	ts := newFakeService(t)

	yaml := fmt.Sprintf(`
name: demo
ref: JIRA-1
service:
  base_url: %s
init:
  steps:
    - name: create_user
      method: POST
      url: "{{.BaseURL}}/users"
      body: '{"name":"user-{{.Index}}"}'
      count: 2
      extract:
        - path: id
          as: user_ids
k6:
  script: ./scenario.js
`, ts.URL)
	cfg := writeConfig(t, yaml)

	var stdout, stderr bytes.Buffer
	rpt, err := Run(context.Background(), cfg, "", &stdout, &stderr)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if rpt.Init == nil || len(rpt.Init.Steps) != 1 || rpt.Init.Steps[0].Requests != 2 {
		t.Fatalf("unexpected init summary: %+v", rpt.Init)
	}
	if rpt.K6 == nil || !rpt.K6.Passed {
		t.Fatalf("expected k6 to pass, got %+v", rpt.K6)
	}
	if rpt.FinishedAt.Before(rpt.StartedAt) {
		t.Fatal("expected FinishedAt >= StartedAt")
	}
	if rpt.Error != "" {
		t.Fatalf("expected no error on report, got %q", rpt.Error)
	}
}

func TestRunEndToEndWithTeardown(t *testing.T) {
	installFakeK6(t, 0)

	var deleteCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /users", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"%s-id"}`, body.Name)
	})
	mux.HandleFunc("DELETE /users/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		atomic.AddInt32(&deleteCalls, 1)
		if id == "user-0-id" {
			// One deliberately-missing resource: teardown must still
			// delete the rest and must not fail the overall run.
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	yaml := fmt.Sprintf(`
name: demo
service:
  base_url: %s
init:
  steps:
    - name: create_user
      method: POST
      url: "{{.BaseURL}}/users"
      body: '{"name":"user-{{.Index}}"}'
      count: 3
      extract:
        - path: id
          as: user_ids
teardown:
  steps:
    - name: delete_users
      method: DELETE
      url: "{{.BaseURL}}/users/{{index .Dict.user_ids .Index}}"
      count: "{{len .Dict.user_ids}}"
k6:
  script: ./scenario.js
`, ts.URL)
	cfg := writeConfig(t, yaml)

	var stdout, stderr bytes.Buffer
	rpt, err := Run(context.Background(), cfg, "", &stdout, &stderr)
	if err != nil {
		t.Fatalf("Run returned error despite a teardown-only failure: %v", err)
	}
	if rpt.Error != "" {
		t.Fatalf("expected rpt.Error to stay empty despite the teardown failure, got %q", rpt.Error)
	}

	if calls := atomic.LoadInt32(&deleteCalls); calls != 3 {
		t.Fatalf("expected all 3 users to get a DELETE attempt, got %d", calls)
	}
	// Requests only counts successful calls (matching init's existing
	// semantics); the 404'd deletion is attempted (see deleteCalls above)
	// but doesn't count as a successful request.
	if rpt.Teardown == nil || len(rpt.Teardown.Steps) != 1 || rpt.Teardown.Steps[0].Requests != 2 {
		t.Fatalf("unexpected teardown summary: %+v", rpt.Teardown)
	}
	if len(rpt.TeardownErrors) != 1 {
		t.Fatalf("expected exactly 1 collected teardown error, got %v", rpt.TeardownErrors)
	}
	if !strings.Contains(stderr.String(), "state file:") {
		t.Errorf("expected the state file path to be printed to stderr, got: %q", stderr.String())
	}
}

func TestRunEndToEndWithGeneratedScenario(t *testing.T) {
	installFakeK6(t, 0)
	ts := newFakeService(t)

	yaml := fmt.Sprintf(`
name: demo
ref: JIRA-1
service:
  base_url: %s
  metrics:
    url: %s/metrics
    interval: 20ms
init:
  steps:
    - name: create_user
      method: POST
      url: "{{.BaseURL}}/users"
      body: '{"name":"user-{{.Index}}"}'
      count: 2
      extract:
        - path: id
          as: user_ids
k6:
  steps:
    - name: get_user
      method: GET
      url: '{{.BaseURL}}/users/{{pick "user_ids"}}'
      checks:
        "status is 200": "r.status === 200"
      sleep: 10ms
  options:
    vus: 1
    iterations: 1
`, ts.URL, ts.URL)
	cfg := writeConfig(t, yaml)

	before, err := filepath.Glob(filepath.Join(os.TempDir(), "myrtille-scenario-*.js"))
	if err != nil {
		t.Fatalf("globbing temp dir before run: %v", err)
	}

	var stdout, stderr bytes.Buffer
	rpt, err := Run(context.Background(), cfg, "", &stdout, &stderr)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if rpt.K6 == nil || !rpt.K6.Passed {
		t.Fatalf("expected k6 to pass, got %+v", rpt.K6)
	}

	after, err := filepath.Glob(filepath.Join(os.TempDir(), "myrtille-scenario-*.js"))
	if err != nil {
		t.Fatalf("globbing temp dir after run: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("expected generated scenario file to be cleaned up, before=%v after=%v", before, after)
	}
}

func TestRunAbortsWhenInitFails(t *testing.T) {
	installFakeK6(t, 0)
	ts := newFakeService(t)

	yaml := fmt.Sprintf(`
service:
  base_url: %s
init:
  steps:
    - name: broken_step
      method: GET
      url: "{{.BaseURL}}/fails"
k6:
  script: ./scenario.js
`, ts.URL)
	cfg := writeConfig(t, yaml)

	var stdout, stderr bytes.Buffer
	rpt, err := Run(context.Background(), cfg, "", &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error when init step fails")
	}
	if rpt.K6 != nil {
		t.Fatalf("expected k6 to not have run, got %+v", rpt.K6)
	}
	if !strings.Contains(rpt.Error, "init phase failed") {
		t.Fatalf("expected report Error to mention init phase failure, got %q", rpt.Error)
	}
}

func TestRunReturnsErrorWhenK6ThresholdsFail(t *testing.T) {
	installFakeK6(t, 99)
	ts := newFakeService(t)

	yaml := fmt.Sprintf(`
service:
  base_url: %s
k6:
  script: ./scenario.js
`, ts.URL)
	cfg := writeConfig(t, yaml)

	var stdout, stderr bytes.Buffer
	rpt, err := Run(context.Background(), cfg, "", &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error when k6 thresholds fail")
	}
	if rpt.K6 == nil || !rpt.K6.ThresholdsFailed {
		t.Fatalf("expected ThresholdsFailed=true, got %+v", rpt.K6)
	}
	if !strings.Contains(rpt.Error, "did not pass") {
		t.Fatalf("expected report Error to mention failure, got %q", rpt.Error)
	}
}

func TestRunLoadsPreloadedStateFile(t *testing.T) {
	installFakeK6(t, 0)
	ts := newFakeService(t)

	yaml := fmt.Sprintf(`
service:
  base_url: %s
k6:
  script: ./scenario.js
`, ts.URL)
	cfg := writeConfig(t, yaml)

	dir := t.TempDir()
	statePath := filepath.Join(dir, "preloaded-state.json")
	if err := os.WriteFile(statePath, []byte(`{"user_ids":["user-42","user-43"]}`), 0o644); err != nil {
		t.Fatalf("writing preloaded state file: %v", err)
	}

	captured := filepath.Join(dir, "captured-state.json")
	t.Setenv("FAKE_K6_STATE_CAPTURE", captured)

	var stdout, stderr bytes.Buffer
	rpt, err := Run(context.Background(), cfg, statePath, &stdout, &stderr)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if rpt.Init != nil {
		t.Fatalf("expected Init to stay nil when init.steps was skipped, got %+v", rpt.Init)
	}
	if rpt.K6 == nil || !rpt.K6.Passed {
		t.Fatalf("expected k6 to pass, got %+v", rpt.K6)
	}

	data, err := os.ReadFile(captured)
	if err != nil {
		t.Fatalf("reading captured state file: %v", err)
	}
	if !strings.Contains(string(data), "user-42") || !strings.Contains(string(data), "user-43") {
		t.Fatalf("expected k6 to receive the preloaded dict, got %s", data)
	}
}

func TestRunRejectsStateFileWithInitSteps(t *testing.T) {
	installFakeK6(t, 0)
	ts := newFakeService(t)

	yaml := fmt.Sprintf(`
service:
  base_url: %s
init:
  steps:
    - name: create_user
      method: POST
      url: "{{.BaseURL}}/users"
      body: '{"name":"user-{{.Index}}"}'
      extract:
        - path: id
          as: user_ids
k6:
  script: ./scenario.js
`, ts.URL)
	cfg := writeConfig(t, yaml)

	dir := t.TempDir()
	statePath := filepath.Join(dir, "preloaded-state.json")
	if err := os.WriteFile(statePath, []byte(`{"user_ids":["user-42"]}`), 0o644); err != nil {
		t.Fatalf("writing preloaded state file: %v", err)
	}

	var stdout, stderr bytes.Buffer
	rpt, err := Run(context.Background(), cfg, statePath, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error when --state-file and init.steps are both set")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected a mutual-exclusivity error, got %v", err)
	}
	if !strings.Contains(rpt.Error, "mutually exclusive") {
		t.Fatalf("expected report Error to mention the conflict, got %q", rpt.Error)
	}
}

func TestRunRejectsMissingStateFile(t *testing.T) {
	installFakeK6(t, 0)
	ts := newFakeService(t)

	yaml := fmt.Sprintf(`
service:
  base_url: %s
k6:
  script: ./scenario.js
`, ts.URL)
	cfg := writeConfig(t, yaml)

	var stdout, stderr bytes.Buffer
	rpt, err := Run(context.Background(), cfg, filepath.Join(t.TempDir(), "does-not-exist.json"), &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error when the state file does not exist")
	}
	if !strings.Contains(err.Error(), "loading state file failed") {
		t.Fatalf("expected a load-failure error, got %v", err)
	}
	if rpt.K6 != nil {
		t.Fatalf("expected k6 to not have run, got %+v", rpt.K6)
	}
}

func TestRunEndToEndWithInitCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("init.command runs via `sh -c`, a POSIX shell")
	}
	installFakeK6(t, 0)
	ts := newFakeService(t)

	yaml := fmt.Sprintf(`
service:
  base_url: %s
init:
  command: |
    echo '{"user_ids":["cmd-user-1","cmd-user-2"]}' > "$MYRTILLE_STATE_OUTPUT"
k6:
  script: ./scenario.js
`, ts.URL)
	cfg := writeConfig(t, yaml)

	dir := t.TempDir()
	captured := filepath.Join(dir, "captured-state.json")
	t.Setenv("FAKE_K6_STATE_CAPTURE", captured)

	var stdout, stderr bytes.Buffer
	rpt, err := Run(context.Background(), cfg, "", &stdout, &stderr)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if rpt.Init == nil || rpt.Init.Command == nil {
		t.Fatalf("expected rpt.Init.Command to be set, got %+v", rpt.Init)
	}
	if rpt.Init.Command.ExitCode != 0 || rpt.Init.Command.TimedOut {
		t.Fatalf("unexpected command summary: %+v", rpt.Init.Command)
	}
	if len(rpt.Init.Steps) != 0 {
		t.Fatalf("expected no init steps when init.command is used, got %+v", rpt.Init.Steps)
	}
	if rpt.K6 == nil || !rpt.K6.Passed {
		t.Fatalf("expected k6 to pass, got %+v", rpt.K6)
	}

	data, err := os.ReadFile(captured)
	if err != nil {
		t.Fatalf("reading captured state file: %v", err)
	}
	if !strings.Contains(string(data), "cmd-user-1") || !strings.Contains(string(data), "cmd-user-2") {
		t.Fatalf("expected k6 to receive the dict produced by init.command, got %s", data)
	}
}

func TestRunAbortsWhenInitCommandFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("init.command runs via `sh -c`, a POSIX shell")
	}
	installFakeK6(t, 0)
	ts := newFakeService(t)

	yaml := fmt.Sprintf(`
service:
  base_url: %s
init:
  command: exit 3
k6:
  script: ./scenario.js
`, ts.URL)
	cfg := writeConfig(t, yaml)

	var stdout, stderr bytes.Buffer
	rpt, err := Run(context.Background(), cfg, "", &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error when init.command exits non-zero")
	}
	if !strings.Contains(err.Error(), "exited with code 3") {
		t.Fatalf("expected the exit code in the error, got %v", err)
	}
	if rpt.K6 != nil {
		t.Fatalf("expected k6 to not have run, got %+v", rpt.K6)
	}
	if !strings.Contains(rpt.Error, "init command failed") {
		t.Fatalf("expected report Error to mention the init command failure, got %q", rpt.Error)
	}
}

func TestRunRejectsStateFileWithInitCommand(t *testing.T) {
	installFakeK6(t, 0)
	ts := newFakeService(t)

	yaml := fmt.Sprintf(`
service:
  base_url: %s
init:
  command: ./seed.sh
k6:
  script: ./scenario.js
`, ts.URL)
	cfg := writeConfig(t, yaml)

	dir := t.TempDir()
	statePath := filepath.Join(dir, "preloaded-state.json")
	if err := os.WriteFile(statePath, []byte(`{"user_ids":["user-42"]}`), 0o644); err != nil {
		t.Fatalf("writing preloaded state file: %v", err)
	}

	var stdout, stderr bytes.Buffer
	rpt, err := Run(context.Background(), cfg, statePath, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error when --state-file and init.command are both set")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected a mutual-exclusivity error, got %v", err)
	}
	if !strings.Contains(rpt.Error, "mutually exclusive") {
		t.Fatalf("expected report Error to mention the conflict, got %q", rpt.Error)
	}
}

func TestRunAppliesInitDeriveAfterInitSteps(t *testing.T) {
	installFakeK6(t, 0)
	ts := newFakeService(t)

	yaml := fmt.Sprintf(`
service:
  base_url: %s
init:
  steps:
    - name: create_user
      method: POST
      url: "{{.BaseURL}}/users"
      body: '{"name":"user-{{.Index}}"}'
      count: 2
      extract:
        - path: id
          as: user_ids
  derive:
    - as: user_count
      expr: "[.user_ids | length]"
k6:
  script: ./scenario.js
`, ts.URL)
	cfg := writeConfig(t, yaml)

	dir := t.TempDir()
	captured := filepath.Join(dir, "captured-state.json")
	t.Setenv("FAKE_K6_STATE_CAPTURE", captured)

	var stdout, stderr bytes.Buffer
	rpt, err := Run(context.Background(), cfg, "", &stdout, &stderr)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if rpt.K6 == nil || !rpt.K6.Passed {
		t.Fatalf("expected k6 to pass, got %+v", rpt.K6)
	}

	data, err := os.ReadFile(captured)
	if err != nil {
		t.Fatalf("reading captured state file: %v", err)
	}
	var got map[string][]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("parsing captured state file: %v", err)
	}
	assertJSONEqualOrch(t, got["user_count"], []any{float64(2)})
}

// TestRunAppliesInitDeriveWithPreloadedStateFile exercises the decision
// that derive runs uniformly after all three branches that can produce a
// dict (--state-file, init.command, init.steps), not only after
// init.steps — see docs/plans/init-derive-and-env-vars.md.
func TestRunAppliesInitDeriveWithPreloadedStateFile(t *testing.T) {
	installFakeK6(t, 0)
	ts := newFakeService(t)

	yaml := fmt.Sprintf(`
service:
  base_url: %s
init:
  derive:
    - as: user_count
      expr: "[.user_ids | length]"
k6:
  script: ./scenario.js
`, ts.URL)
	cfg := writeConfig(t, yaml)

	dir := t.TempDir()
	statePath := filepath.Join(dir, "preloaded-state.json")
	if err := os.WriteFile(statePath, []byte(`{"user_ids":["user-42","user-43","user-44"]}`), 0o644); err != nil {
		t.Fatalf("writing preloaded state file: %v", err)
	}

	captured := filepath.Join(dir, "captured-state.json")
	t.Setenv("FAKE_K6_STATE_CAPTURE", captured)

	var stdout, stderr bytes.Buffer
	rpt, err := Run(context.Background(), cfg, statePath, &stdout, &stderr)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if rpt.K6 == nil || !rpt.K6.Passed {
		t.Fatalf("expected k6 to pass, got %+v", rpt.K6)
	}

	data, err := os.ReadFile(captured)
	if err != nil {
		t.Fatalf("reading captured state file: %v", err)
	}
	var got map[string][]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("parsing captured state file: %v", err)
	}
	assertJSONEqualOrch(t, got["user_count"], []any{float64(3)})
}

func TestRunFailsWhenDeriveInputKeyMissing(t *testing.T) {
	installFakeK6(t, 0)
	ts := newFakeService(t)

	yaml := fmt.Sprintf(`
service:
  base_url: %s
init:
  derive:
    - as: leaf_keys
      input: never_populated
      expr: "."
k6:
  script: ./scenario.js
`, ts.URL)
	cfg := writeConfig(t, yaml)

	var stdout, stderr bytes.Buffer
	rpt, err := Run(context.Background(), cfg, "", &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error when a derive rule references a never-populated input key")
	}
	if !strings.Contains(err.Error(), "init derive failed") {
		t.Fatalf("expected an init-derive-failed error, got %v", err)
	}
	if rpt.K6 != nil {
		t.Fatalf("expected k6 to not have run, got %+v", rpt.K6)
	}
}

// TestRunReproducesSeedAndSnapshotAggregation is the durable, committed
// counterpart to the tranche 0/2 spikes: it proves that init.steps +
// init.derive can reproduce the snapshot/aggregation half of
// seed-and-snapshot.sh (see docs/plans/init-derive-and-env-vars.md)
// end to end — domain_names, perimeter_keys, root_keys via Extract,
// leaf_keys via a derive rule, and domain_count read from ${DOMAIN_COUNT}
// rather than hardcoded in vars: — against a fake perimeters API and the
// real orchestrator.Run.
func TestRunReproducesSeedAndSnapshotAggregation(t *testing.T) {
	installFakeK6(t, 0)

	// Two domains, each with one parentless root and children pointing
	// back at it via spec.parent — same shape as the tranche 0/2 fixtures,
	// now served by a fake HTTP API keyed by the {{.Index}}-derived URL
	// (domain-0, domain-1) rather than hardcoded domain names, so the
	// config below never needs to know the real domain names up front.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /domains/domain-0/perimeters", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"items": [
			{"metadata": {"domain": "alpha", "name": "root-1"}, "spec": {"parent": null}},
			{"metadata": {"domain": "alpha", "name": "child-1"}, "spec": {"parent": {"domain": "alpha", "name": "root-1"}}},
			{"metadata": {"domain": "alpha", "name": "child-2"}, "spec": {"parent": {"domain": "alpha", "name": "root-1"}}}
		]}`)
	})
	mux.HandleFunc("GET /domains/domain-1/perimeters", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"items": [
			{"metadata": {"domain": "beta", "name": "root-2"}, "spec": {"parent": null}},
			{"metadata": {"domain": "beta", "name": "child-3"}, "spec": {"parent": {"domain": "beta", "name": "root-2"}}}
		]}`)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	t.Setenv("DOMAIN_COUNT", "2")

	yaml := fmt.Sprintf(`
service:
  base_url: %s
vars:
  domain_count: "${DOMAIN_COUNT}"
init:
  steps:
    - name: collect_perimeters
      url: "{{.BaseURL}}/domains/domain-{{.Index}}/perimeters"
      count: "{{.Vars.domain_count}}"
      extract:
        - path: "items.0.metadata.domain"
          as: domain_names
        - path: items
          as: perimeter_items
        - path: "items.#.{domain:metadata.domain,name:metadata.name}"
          as: perimeter_keys
        - path: "items.#(spec.parent==~null)#.{domain:metadata.domain,name:metadata.name}"
          as: root_keys
  derive:
    - as: leaf_keys
      input: perimeter_items
      expr: |
        (map(select(.spec.parent != null) | (.spec.parent.domain + "/" + .spec.parent.name))) as $parent_keys
        | [.[] | select(((.metadata.domain + "/" + .metadata.name) as $k | $parent_keys | index($k) | not))
            | {domain: .metadata.domain, name: .metadata.name}]
k6:
  script: ./scenario.js
`, ts.URL)
	cfg := writeConfig(t, yaml)

	dir := t.TempDir()
	captured := filepath.Join(dir, "captured-state.json")
	t.Setenv("FAKE_K6_STATE_CAPTURE", captured)

	var stdout, stderr bytes.Buffer
	rpt, err := Run(context.Background(), cfg, "", &stdout, &stderr)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if rpt.K6 == nil || !rpt.K6.Passed {
		t.Fatalf("expected k6 to pass, got %+v", rpt.K6)
	}

	data, err := os.ReadFile(captured)
	if err != nil {
		t.Fatalf("reading captured state file: %v", err)
	}
	var got map[string][]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("parsing captured state file: %v", err)
	}

	assertJSONEqualOrch(t, got["domain_names"], []any{"alpha", "beta"})
	assertJSONEqualOrch(t, got["root_keys"], []any{
		map[string]any{"domain": "alpha", "name": "root-1"},
		map[string]any{"domain": "beta", "name": "root-2"},
	})
	assertJSONEqualOrch(t, got["perimeter_keys"], []any{
		map[string]any{"domain": "alpha", "name": "root-1"},
		map[string]any{"domain": "alpha", "name": "child-1"},
		map[string]any{"domain": "alpha", "name": "child-2"},
		map[string]any{"domain": "beta", "name": "root-2"},
		map[string]any{"domain": "beta", "name": "child-3"},
	})
	assertJSONEqualOrch(t, got["leaf_keys"], []any{
		map[string]any{"domain": "alpha", "name": "child-1"},
		map[string]any{"domain": "alpha", "name": "child-2"},
		map[string]any{"domain": "beta", "name": "child-3"},
	})
}

func assertJSONEqualOrch(t *testing.T, got any, want any) {
	t.Helper()
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshaling got: %v", err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshaling want: %v", err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("got %s, want %s", gotJSON, wantJSON)
	}
}
