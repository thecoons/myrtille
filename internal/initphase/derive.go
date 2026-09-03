package initphase

import (
	"encoding/json"
	"fmt"

	"github.com/itchyny/gojq"
	"github.com/thecoons/myrtille/internal/config"
	"github.com/thecoons/myrtille/internal/state"
)

// Derive runs cfg.Init.Derive's rules, in declaration order, against dict —
// once the init phase (--state-file, init.command, or init.steps) has
// already produced it. Each rule's jq expression is evaluated against
// dict[Input] if Input is set, or the whole dict (as JSON) otherwise, and
// must produce exactly one JSON array, written into dict[As] via Dict.Set
// (replacing, not appending — see Dict.Set). Rules run against a mutable
// dict, so a later rule can read a key an earlier one just wrote.
//
// It fails fast: an unknown Input key, an expression that fails to parse or
// run, or a result that isn't a single JSON array, all abort immediately
// rather than leaving the dict partially derived.
func Derive(cfg *config.Config, dict *state.Dict) error {
	for _, rule := range cfg.Init.Derive {
		result, err := deriveOne(rule, dict)
		if err != nil {
			return fmt.Errorf("init.derive: rule %q: %w", rule.As, err)
		}
		dict.Set(rule.As, result)
	}
	return nil
}

func deriveOne(rule config.DeriveRule, dict *state.Dict) ([]any, error) {
	query, err := gojq.Parse(rule.Expr)
	if err != nil {
		return nil, fmt.Errorf("parsing expr: %w", err)
	}

	input, err := deriveInput(rule.Input, dict)
	if err != nil {
		return nil, err
	}

	iter := query.Run(input)
	v, ok := iter.Next()
	if !ok {
		return nil, fmt.Errorf("expr produced no output")
	}
	if err, ok := v.(error); ok {
		return nil, fmt.Errorf("running expr: %w", err)
	}
	if _, hasMore := iter.Next(); hasMore {
		return nil, fmt.Errorf("expr produced more than one output value; wrap the result in [...] to produce a single array")
	}

	result, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("expr must produce a JSON array, got %T", v)
	}
	return result, nil
}

// deriveInput resolves what a rule's expression runs against: dict[input]
// alone when input is set (erroring if that key was never populated), or
// the whole dict, marshaled to JSON and back into a plain any so gojq sees
// the same map[string]any/[]any shapes encoding/json would produce, when
// input is empty.
func deriveInput(input string, dict *state.Dict) (any, error) {
	if input == "" {
		data, err := json.Marshal(dict)
		if err != nil {
			return nil, fmt.Errorf("marshaling dict: %w", err)
		}
		var v any
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("unmarshaling dict: %w", err)
		}
		return v, nil
	}

	values, ok := dict.Snapshot()[input]
	if !ok {
		return nil, fmt.Errorf("input %q was never populated in the state dict", input)
	}
	return values, nil
}
