package config

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"
)

// ParseEnvFile parses the contents of a .env file into a map of key/value
// pairs. It supports plain KEY=value lines, "#" comment lines, and blank
// lines; a single layer of matching quotes (single or double) is stripped
// around the value. There is no $VAR expansion, no "export" prefix, and no
// multi-line values — this is a static list of defaults, not a shell
// script.
func ParseEnvFile(data []byte) (map[string]string, error) {
	result := make(map[string]string)

	scanner := bufio.NewScanner(bytes.NewReader(data))
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		before, after, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("env file: line %d: missing '=' in %q", lineNum, line)
		}

		key := strings.TrimSpace(before)
		if key == "" {
			return nil, fmt.Errorf("env file: line %d: empty key in %q", lineNum, line)
		}
		value := strings.TrimSpace(after)
		result[key] = stripMatchingQuotes(value)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("env file: %w", err)
	}

	return result, nil
}

// stripMatchingQuotes removes a single layer of matching single or double
// quotes surrounding value, if present.
func stripMatchingQuotes(value string) string {
	if len(value) < 2 {
		return value
	}
	first, last := value[0], value[len(value)-1]
	if (first == '"' || first == '\'') && first == last {
		return value[1 : len(value)-1]
	}
	return value
}
