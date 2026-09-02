package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTempIn writes name (e.g. "myrtille.yaml" or ".env") with content
// into dir and returns its path.
func writeTempIn(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	return path
}

const minimalConfig = `
service:
  base_url: http://localhost:8080
k6:
  script: ./scenario.js
`

func TestLoadEnvFileMergesUnsetVars(t *testing.T) {
	dir := t.TempDir()
	writeTempIn(t, dir, ".env", "ENVFILE_FOO=from_file\nENVFILE_BAR=also_from_file\n")
	cfgPath := writeTempIn(t, dir, "myrtille.yaml", minimalConfig+"env_file: .env\n")

	for _, key := range []string{"ENVFILE_FOO", "ENVFILE_BAR"} {
		if _, ok := os.LookupEnv(key); ok {
			t.Fatalf("precondition: %s must not already be set in the test process", key)
		}
		t.Cleanup(func(k string) func() { return func() { os.Unsetenv(k) } }(key))
	}

	if _, err := Load(cfgPath); err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if got := os.Getenv("ENVFILE_FOO"); got != "from_file" {
		t.Errorf("ENVFILE_FOO = %q, want %q", got, "from_file")
	}
	if got := os.Getenv("ENVFILE_BAR"); got != "also_from_file" {
		t.Errorf("ENVFILE_BAR = %q, want %q", got, "also_from_file")
	}
}

func TestLoadEnvFilePreservesExistingProcessVar(t *testing.T) {
	dir := t.TempDir()
	writeTempIn(t, dir, ".env", "ENVFILE_PRESET=from_file\n")
	cfgPath := writeTempIn(t, dir, "myrtille.yaml", minimalConfig+"env_file: .env\n")

	t.Setenv("ENVFILE_PRESET", "from_process")

	if _, err := Load(cfgPath); err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if got := os.Getenv("ENVFILE_PRESET"); got != "from_process" {
		t.Errorf("ENVFILE_PRESET = %q, want the pre-existing process value %q (not overwritten by .env)", got, "from_process")
	}
}

func TestLoadWithoutEnvFileFieldIsNoop(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeTempIn(t, dir, "myrtille.yaml", minimalConfig)

	if _, ok := os.LookupEnv("ENVFILE_UNTOUCHED"); ok {
		t.Fatal("precondition: ENVFILE_UNTOUCHED must not already be set")
	}

	if _, err := Load(cfgPath); err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if _, ok := os.LookupEnv("ENVFILE_UNTOUCHED"); ok {
		t.Error("Load without env_file set an unrelated env var; expected a pure no-op")
		os.Unsetenv("ENVFILE_UNTOUCHED")
	}
}

func TestLoadEnvFileExplicitPathMissingFails(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeTempIn(t, dir, "myrtille.yaml", minimalConfig+"env_file: does-not-exist.env\n")

	_, err := Load(cfgPath)
	if err == nil {
		t.Fatal("expected error for missing env_file, got nil")
	}
	wantPath := filepath.Join(dir, "does-not-exist.env")
	if got := err.Error(); !strings.Contains(got, wantPath) {
		t.Errorf("error = %q, want it to mention resolved path %q", got, wantPath)
	}
}

func TestLoadEnvFileOverrideWinsOverYAMLField(t *testing.T) {
	dir := t.TempDir()
	writeTempIn(t, dir, "a.env", "ENVFILE_FROM=yaml_field\n")
	writeTempIn(t, dir, "b.env", "ENVFILE_FROM=cli_flag\n")
	cfgPath := writeTempIn(t, dir, "myrtille.yaml", minimalConfig+"env_file: a.env\n")
	overridePath := filepath.Join(dir, "b.env")

	t.Cleanup(func() { os.Unsetenv("ENVFILE_FROM") })

	if _, err := Load(cfgPath, WithEnvFileOverride(overridePath)); err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if got := os.Getenv("ENVFILE_FROM"); got != "cli_flag" {
		t.Errorf("ENVFILE_FROM = %q, want %q (the --env-file override, not the YAML env_file)", got, "cli_flag")
	}
}
