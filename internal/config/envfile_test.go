package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseEnvFileBasic(t *testing.T) {
	data := []byte(`
# a comment
BASE_URL=http://localhost:8080

SEED=true
`)
	got, err := ParseEnvFile(data)
	if err != nil {
		t.Fatalf("ParseEnvFile returned error: %v", err)
	}
	want := map[string]string{
		"BASE_URL": "http://localhost:8080",
		"SEED":     "true",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseEnvFile() = %v, want %v", got, want)
	}
}

func TestParseEnvFileQuotedValues(t *testing.T) {
	data := []byte(`
DOUBLE="hello world"
SINGLE='hello world'
UNQUOTED=hello world
EMPTY=
`)
	got, err := ParseEnvFile(data)
	if err != nil {
		t.Fatalf("ParseEnvFile returned error: %v", err)
	}
	want := map[string]string{
		"DOUBLE":   "hello world",
		"SINGLE":   "hello world",
		"UNQUOTED": "hello world",
		"EMPTY":    "",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseEnvFile() = %v, want %v", got, want)
	}
}

func TestParseEnvFileMismatchedQuotesKeptLiteral(t *testing.T) {
	got, err := ParseEnvFile([]byte(`KEY="mismatched'`))
	if err != nil {
		t.Fatalf("ParseEnvFile returned error: %v", err)
	}
	if got["KEY"] != `"mismatched'` {
		t.Errorf("KEY = %q, want literal value with mismatched quotes kept", got["KEY"])
	}
}

func TestParseEnvFileMalformedLineFails(t *testing.T) {
	_, err := ParseEnvFile([]byte("BASE_URL=http://localhost:8080\nnot-a-valid-line\n"))
	if err == nil {
		t.Fatal("expected error for malformed line, got nil")
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Errorf("error = %v, want it to reference line 2", err)
	}
}

func TestParseEnvFileEmptyKeyFails(t *testing.T) {
	_, err := ParseEnvFile([]byte("=value"))
	if err == nil {
		t.Fatal("expected error for empty key, got nil")
	}
}

func TestParseEnvFileDuplicateKeyLastWins(t *testing.T) {
	got, err := ParseEnvFile([]byte("KEY=first\nKEY=second\n"))
	if err != nil {
		t.Fatalf("ParseEnvFile returned error: %v", err)
	}
	if got["KEY"] != "second" {
		t.Errorf("KEY = %q, want %q", got["KEY"], "second")
	}
}

func TestParseEnvFileEmptyInputReturnsEmptyMap(t *testing.T) {
	got, err := ParseEnvFile([]byte(""))
	if err != nil {
		t.Fatalf("ParseEnvFile returned error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ParseEnvFile(\"\") = %v, want empty map", got)
	}
}
