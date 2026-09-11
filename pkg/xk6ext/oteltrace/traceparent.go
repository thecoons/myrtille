package oteltrace

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
)

// newTraceparent generates a fresh W3C traceparent header value
// (https://www.w3.org/TR/trace-context/#traceparent-header): version "00",
// a random 16-byte trace-id, a random 8-byte parent-id, and trace-flags
// "01" (sampled) — the value a k6 script sets on an outgoing request so the
// service's OTel SDK, if it extracts incoming trace context (see
// docs/plans/otel-span-metrics.md tranche 8), emits spans under this same
// trace-id. Exported to JS as a plain `oteltrace.newTraceparent()` function
// rather than a Receiver method: generation needs no shared state, no VU
// context, and no init-time metric registration (unlike Receiver, which
// must be constructed in the init context) — so it works from anywhere a
// script calls it, including inside the default function per request.
//
// crypto/rand, not math/rand: multiple concurrent k6 VUs generate
// traceparents independently and must never collide.
func newTraceparent() (string, error) {
	traceID, err := randNonZero(16)
	if err != nil {
		return "", fmt.Errorf("oteltrace: generating trace-id: %w", err)
	}
	spanID, err := randNonZero(8)
	if err != nil {
		return "", fmt.Errorf("oteltrace: generating parent-id: %w", err)
	}
	return fmt.Sprintf("00-%s-%s-01", hex.EncodeToString(traceID), hex.EncodeToString(spanID)), nil
}

// randNonZero returns n random bytes, never all-zero — the W3C spec
// reserves an all-zero trace-id/parent-id as invalid. crypto/rand.Read
// returning n zero bytes is astronomically unlikely (1 in 2^(8n)), but
// retrying costs nothing and makes the guarantee actual rather than assumed.
func randNonZero(n int) ([]byte, error) {
	b := make([]byte, n)
	for {
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		if !isAllZero(b) {
			return b, nil
		}
	}
}

func isAllZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

// traceparentPattern matches a well-formed W3C traceparent value emitted by
// newTraceparent (version "00", 32-hex trace-id, 16-hex parent-id, flags
// "01") and captures the trace-id field — the only piece parseTraceID
// needs. Anchored, not just a substring match: a malformed value (wrong
// field count, wrong-length hex) must fail to parse rather than silently
// matching a truncated prefix.
var traceparentPattern = regexp.MustCompile(`^00-([0-9a-f]{32})-[0-9a-f]{16}-01$`)

// parseTraceID extracts the trace-id field from a traceparent header value
// — used by receiver.linkFailure (see failures.go) to key its
// trace_id -> failure map, since that's the only part of a traceparent
// spans emitted under it also carry (span.TraceId in the OTLP wire format).
func parseTraceID(traceparent string) (string, error) {
	m := traceparentPattern.FindStringSubmatch(traceparent)
	if m == nil {
		return "", fmt.Errorf("oteltrace: malformed traceparent %q", traceparent)
	}
	return m[1], nil
}
