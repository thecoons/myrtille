package config

import (
	"os"
	"testing"
)

func TestLoadVarsExpandsSetEnvVar(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeTempIn(t, dir, "myrtille.yaml", minimalConfig+"vars:\n  domain: \"${VARSEXPAND_DOMAIN}\"\n")

	t.Setenv("VARSEXPAND_DOMAIN", "alpha")

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got := cfg.Vars["domain"]; got != "alpha" {
		t.Errorf("cfg.Vars[domain] = %v, want %q", got, "alpha")
	}
}

func TestLoadVarsExpandsUnsetVarToEmptyString(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeTempIn(t, dir, "myrtille.yaml", minimalConfig+"vars:\n  domain: \"${VARSEXPAND_UNSET}\"\n")

	if _, ok := os.LookupEnv("VARSEXPAND_UNSET"); ok {
		t.Fatal("precondition: VARSEXPAND_UNSET must not already be set")
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got := cfg.Vars["domain"]; got != "" {
		t.Errorf("cfg.Vars[domain] = %v, want empty string", got)
	}
}

func TestLoadVarsExpandsDefaultWhenUnset(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeTempIn(t, dir, "myrtille.yaml", minimalConfig+"vars:\n  domain: \"${VARSEXPAND_UNSET_WITH_DEFAULT:-fallback}\"\n")

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got := cfg.Vars["domain"]; got != "fallback" {
		t.Errorf("cfg.Vars[domain] = %v, want %q", got, "fallback")
	}
}

func TestLoadVarsExpandsDefaultSyntaxPrefersSetValue(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeTempIn(t, dir, "myrtille.yaml", minimalConfig+"vars:\n  domain: \"${VARSEXPAND_SET_WITH_DEFAULT:-fallback}\"\n")

	t.Setenv("VARSEXPAND_SET_WITH_DEFAULT", "beta")

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got := cfg.Vars["domain"]; got != "beta" {
		t.Errorf("cfg.Vars[domain] = %v, want %q", got, "beta")
	}
}

func TestLoadVarsExpandsReferencesEnvFileValue(t *testing.T) {
	dir := t.TempDir()
	writeTempIn(t, dir, ".env", "VARSEXPAND_FROM_ENVFILE=from_dotenv\n")
	cfgPath := writeTempIn(t, dir, "myrtille.yaml", minimalConfig+"env_file: .env\nvars:\n  domain: \"${VARSEXPAND_FROM_ENVFILE}\"\n")
	t.Cleanup(func() { os.Unsetenv("VARSEXPAND_FROM_ENVFILE") })

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if got := cfg.Vars["domain"]; got != "from_dotenv" {
		t.Errorf("cfg.Vars[domain] = %v, want %q (expansion must see values merged from .env)", got, "from_dotenv")
	}
}

func TestLoadVarsExpandLeavesNonStringValuesUntouched(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeTempIn(t, dir, "myrtille.yaml", minimalConfig+"vars:\n  users_count: 5\n  enabled: true\n")

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.Vars["users_count"] != 5 {
		t.Errorf("cfg.Vars[users_count] = %v, want 5", cfg.Vars["users_count"])
	}
	if cfg.Vars["enabled"] != true {
		t.Errorf("cfg.Vars[enabled] = %v, want true", cfg.Vars["enabled"])
	}
}
