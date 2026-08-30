// Package state holds the dictionary of values gathered during the init
// phase (e.g. IDs created in the target service), so that k6 scenarios can
// randomize requests against a known, real state. It is safe for concurrent
// use since init steps may run with a count > 1.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
)

// Dict accumulates named lists of extracted values, keyed by the `as` field
// of an init step's extract rule. Repeated extractions under the same key
// are appended, producing arrays a k6 script can index/randomize over.
type Dict struct {
	mu   sync.Mutex
	data map[string][]any
}

// New returns an empty, ready to use Dict.
func New() *Dict {
	return &Dict{data: make(map[string][]any)}
}

// Append adds a single value under key.
func (d *Dict) Append(key string, value any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.data[key] = append(d.data[key], value)
}

// AppendMany adds multiple values under key, in order.
func (d *Dict) AppendMany(key string, values []any) {
	if len(values) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.data[key] = append(d.data[key], values...)
}

// Count returns the number of values accumulated under key.
func (d *Dict) Count(key string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.data[key])
}

// Keys returns the accumulated keys in sorted order.
func (d *Dict) Keys() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	keys := make([]string, 0, len(d.data))
	for k := range d.data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Summary returns the number of values accumulated per key, useful for
// reporting what the init phase produced.
func (d *Dict) Summary() map[string]int {
	d.mu.Lock()
	defer d.mu.Unlock()
	summary := make(map[string]int, len(d.data))
	for k, v := range d.data {
		summary[k] = len(v)
	}
	return summary
}

// MarshalJSON renders the dict as a plain JSON object of key -> array,
// suitable for a k6 script to `JSON.parse(open(...))`.
func (d *Dict) MarshalJSON() ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return json.Marshal(d.data)
}

// WriteTempFile serializes the dict to JSON and writes it to a new
// temporary file, returning its path. The caller is responsible for
// removing the file once it is no longer needed (e.g. after the k6 run
// completes).
func (d *Dict) WriteTempFile() (string, error) {
	data, err := json.Marshal(d)
	if err != nil {
		return "", fmt.Errorf("marshaling state dict: %w", err)
	}

	f, err := os.CreateTemp("", "myrtille-state-*.json")
	if err != nil {
		return "", fmt.Errorf("creating state file: %w", err)
	}
	defer f.Close()

	if _, err := f.Write(data); err != nil {
		return "", fmt.Errorf("writing state file: %w", err)
	}

	return f.Name(), nil
}
