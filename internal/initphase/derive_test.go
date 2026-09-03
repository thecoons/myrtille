package initphase

import (
	"encoding/json"
	"testing"

	"github.com/thecoons/myrtille/internal/config"
	"github.com/thecoons/myrtille/internal/state"
)

// perimeterItemsFixture mirrors the tranche 0 spike fixture: two domains,
// each with one parentless root and children pointing back at it via
// spec.parent, as init.steps' Extract (path: "items", as:
// "perimeter_items") would have accumulated them across two iterations.
func perimeterItemsFixture() []any {
	return []any{
		map[string]any{"metadata": map[string]any{"domain": "alpha", "name": "root-1"}, "spec": map[string]any{"parent": nil}},
		map[string]any{"metadata": map[string]any{"domain": "alpha", "name": "child-1"}, "spec": map[string]any{"parent": map[string]any{"domain": "alpha", "name": "root-1"}}},
		map[string]any{"metadata": map[string]any{"domain": "alpha", "name": "child-2"}, "spec": map[string]any{"parent": map[string]any{"domain": "alpha", "name": "root-1"}}},
		map[string]any{"metadata": map[string]any{"domain": "beta", "name": "root-2"}, "spec": map[string]any{"parent": nil}},
		map[string]any{"metadata": map[string]any{"domain": "beta", "name": "child-3"}, "spec": map[string]any{"parent": map[string]any{"domain": "beta", "name": "root-2"}}},
	}
}

const leafKeysExpr = `
(map(select(.spec.parent != null) | (.spec.parent.domain + "/" + .spec.parent.name))) as $parent_keys
| [.[] | select(((.metadata.domain + "/" + .metadata.name) as $k | $parent_keys | index($k) | not))
    | {domain: .metadata.domain, name: .metadata.name}]
`

func assertJSONEqual(t *testing.T, got any, want any) {
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

func TestDeriveComputesLeafKeysFromNamedInput(t *testing.T) {
	dict := state.New()
	dict.AppendMany("perimeter_items", perimeterItemsFixture())

	cfg := &config.Config{Init: config.InitConfig{Derive: []config.DeriveRule{
		{As: "leaf_keys", Input: "perimeter_items", Expr: leafKeysExpr},
	}}}

	if err := Derive(cfg, dict); err != nil {
		t.Fatalf("Derive returned error: %v", err)
	}

	want := []any{
		map[string]any{"domain": "alpha", "name": "child-1"},
		map[string]any{"domain": "alpha", "name": "child-2"},
		map[string]any{"domain": "beta", "name": "child-3"},
	}
	assertJSONEqual(t, dict.Snapshot()["leaf_keys"], want)
}

func TestDeriveWithoutInputRunsAgainstWholeDict(t *testing.T) {
	dict := state.New()
	dict.AppendMany("user_ids", []any{"u1", "u2", "u3"})

	cfg := &config.Config{Init: config.InitConfig{Derive: []config.DeriveRule{
		{As: "user_count", Expr: "[.user_ids | length]"},
	}}}

	if err := Derive(cfg, dict); err != nil {
		t.Fatalf("Derive returned error: %v", err)
	}
	assertJSONEqual(t, dict.Snapshot()["user_count"], []any{float64(3)})
}

func TestDeriveMissingInputKeyFailsFast(t *testing.T) {
	dict := state.New()
	cfg := &config.Config{Init: config.InitConfig{Derive: []config.DeriveRule{
		{As: "leaf_keys", Input: "never_populated", Expr: "."},
	}}}

	err := Derive(cfg, dict)
	if err == nil {
		t.Fatal("expected error for missing input key, got nil")
	}
}

func TestDeriveRulesRunSequentiallyAgainstMutableDict(t *testing.T) {
	dict := state.New()
	dict.AppendMany("user_ids", []any{"u1", "u2"})

	cfg := &config.Config{Init: config.InitConfig{Derive: []config.DeriveRule{
		{As: "doubled", Expr: "[.user_ids[], .user_ids[]]"},
		{As: "doubled_count", Input: "doubled", Expr: "[length]"},
	}}}

	if err := Derive(cfg, dict); err != nil {
		t.Fatalf("Derive returned error: %v", err)
	}
	assertJSONEqual(t, dict.Snapshot()["doubled_count"], []any{float64(4)})
}

func TestDeriveSetReplacesRatherThanAppendsExistingKey(t *testing.T) {
	dict := state.New()
	dict.AppendMany("user_ids", []any{"stale-1", "stale-2"})

	cfg := &config.Config{Init: config.InitConfig{Derive: []config.DeriveRule{
		{As: "user_ids", Expr: `["fresh"]`},
	}}}

	if err := Derive(cfg, dict); err != nil {
		t.Fatalf("Derive returned error: %v", err)
	}
	assertJSONEqual(t, dict.Snapshot()["user_ids"], []any{"fresh"})
}

func TestDeriveNonArrayResultFails(t *testing.T) {
	dict := state.New()
	cfg := &config.Config{Init: config.InitConfig{Derive: []config.DeriveRule{
		{As: "bad", Expr: `"not an array"`},
	}}}

	err := Derive(cfg, dict)
	if err == nil {
		t.Fatal("expected error for a non-array derive result, got nil")
	}
}

func TestDeriveInvalidExprFailsToParse(t *testing.T) {
	dict := state.New()
	cfg := &config.Config{Init: config.InitConfig{Derive: []config.DeriveRule{
		{As: "bad", Expr: "this is not jq {{{"},
	}}}

	err := Derive(cfg, dict)
	if err == nil {
		t.Fatal("expected a parse error, got nil")
	}
}
