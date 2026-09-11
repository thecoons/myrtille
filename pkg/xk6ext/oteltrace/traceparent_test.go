package oteltrace

import (
	"testing"
)

func TestNewTraceparentFormat(t *testing.T) {
	tp, err := newTraceparent()
	if err != nil {
		t.Fatalf("newTraceparent: %v", err)
	}
	if !traceparentPattern.MatchString(tp) {
		t.Errorf("newTraceparent() = %q, does not match W3C traceparent format", tp)
	}
}

func TestNewTraceparentNoCollisionsAcrossManyCalls(t *testing.T) {
	const n = 10_000
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		tp, err := newTraceparent()
		if err != nil {
			t.Fatalf("newTraceparent: %v", err)
		}
		if !traceparentPattern.MatchString(tp) {
			t.Fatalf("newTraceparent() = %q, does not match W3C traceparent format", tp)
		}
		if _, dup := seen[tp]; dup {
			t.Fatalf("newTraceparent() produced a duplicate value: %q", tp)
		}
		seen[tp] = struct{}{}
	}
}

func TestRandNonZeroNeverAllZero(t *testing.T) {
	const n = 10_000
	for i := 0; i < n; i++ {
		b, err := randNonZero(16)
		if err != nil {
			t.Fatalf("randNonZero: %v", err)
		}
		if isAllZero(b) {
			t.Fatalf("randNonZero(16) returned an all-zero slice")
		}
	}
}

func TestParseTraceIDExtractsTraceIDField(t *testing.T) {
	got, err := parseTraceID("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	if err != nil {
		t.Fatalf("parseTraceID: %v", err)
	}
	if got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("parseTraceID() = %q, want %q", got, "4bf92f3577b34da6a3ce929d0e0e4736")
	}
}

func TestParseTraceIDRoundTripsWithNewTraceparent(t *testing.T) {
	tp, err := newTraceparent()
	if err != nil {
		t.Fatalf("newTraceparent: %v", err)
	}
	traceID, err := parseTraceID(tp)
	if err != nil {
		t.Fatalf("parseTraceID(%q): %v", tp, err)
	}
	if want := tp[3:35]; traceID != want {
		t.Errorf("parseTraceID(%q) = %q, want %q", tp, traceID, want)
	}
}

func TestParseTraceIDRejectsMalformedInput(t *testing.T) {
	for _, tp := range []string{
		"",
		"not-a-traceparent",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-extra",
		"00-tooshort-00f067aa0ba902b7-01",
		"00-4bf92f3577b34da6a3ce929d0e0e4736ZZ-00f067aa0ba902b7-01", // non-hex chars, wrong length too
		"01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",   // wrong version
	} {
		if _, err := parseTraceID(tp); err == nil {
			t.Errorf("parseTraceID(%q) succeeded, expected an error", tp)
		}
	}
}
