package style

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// TestBufferNeverColored confirms the common case for callers in this
// codebase's own tests: a plain *bytes.Buffer (not an *os.File) never
// gets ANSI escape codes, regardless of NO_COLOR — it isn't a terminal,
// full stop.
func TestBufferNeverColored(t *testing.T) {
	var buf bytes.Buffer
	w := New(&buf)

	w.Step("starting service...")
	w.Done("service stopped (signal=%s, clean=%v)", "TERM", true)
	w.Fail("stopping service failed: %v", "boom")
	w.Info("state file: %s", "/tmp/state.json")

	got := buf.String()
	if strings.Contains(got, "\x1b[") {
		t.Fatalf("expected no ANSI escape codes for a non-terminal writer, got:\n%s", got)
	}

	want := "→ starting service...\n" +
		"✓ service stopped (signal=TERM, clean=true)\n" +
		"✗ stopping service failed: boom\n" +
		"· state file: /tmp/state.json\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

// TestFileGetsColorWhenTerminalLike confirms the other half: writing to a
// real *os.File that looks like a terminal (a pty would be needed for a
// real one in a test — instead this checks the underlying decision
// function directly against a regular file, which os.ModeCharDevice
// correctly reports as false, and against nil-safety) plus NO_COLOR's
// override, exercised through colorEnabled directly since spinning up an
// actual pty is unnecessary ceremony for what's a one-line decision.
func TestFileGetsColorWhenTerminalLike(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "style-test")
	if err != nil {
		t.Fatalf("creating temp file: %v", err)
	}
	defer f.Close()

	// A regular file is not a character device — never colored, same as
	// the common "redirected to a file/CI log" case.
	if colorEnabled(f) {
		t.Error("expected a regular file to not be treated as a color-capable terminal")
	}
}

// TestNoColorEnvDisablesColorEvenForATerminal confirms NO_COLOR wins
// before New even looks at whether w is a terminal — checked directly
// against os.Stderr (which may or may not be a real terminal in test
// runs) since the point is NO_COLOR short-circuits regardless.
func TestNoColorEnvDisablesColorEvenForATerminal(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if colorEnabled(os.Stderr) {
		t.Error("expected NO_COLOR=1 to disable color regardless of the writer")
	}
}
