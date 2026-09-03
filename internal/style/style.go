// Package style adds lightweight, optional visual formatting — a symbol
// prefix and, when the output looks like an interactive terminal, ANSI
// color — to myrtille's own progress messages (service start/stop, init
// phase transitions, k6 run). Every line still carries the exact same
// text a plain fmt.Fprintln would have printed, just with a symbol (and,
// interactively, color) in front — so nothing that already matches on
// message content (tests, log scrapers, `grep`) needs to change, and
// piped/CI output stays plain, readable text with no escape codes.
package style

import (
	"fmt"
	"io"
	"os"
)

// Writer wraps an io.Writer (myrtille's own stderr, typically) with a few
// small helpers for status/success/warning/failure lines.
type Writer struct {
	w     io.Writer
	color bool
}

// New wraps w for styled output. Color is enabled only when w is backed
// by a real terminal and NO_COLOR isn't set (https://no-color.org) —
// anything else (a pipe, a redirected file, a bytes.Buffer in a test)
// gets the plain, uncolored form, symbol prefix still included.
func New(w io.Writer) *Writer {
	return &Writer{w: w, color: colorEnabled(w)}
}

func colorEnabled(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

const (
	colorCyan   = "\x1b[36m"
	colorGreen  = "\x1b[32m"
	colorYellow = "\x1b[33m"
	colorRed    = "\x1b[31m"
	colorReset  = "\x1b[0m"
)

func (s *Writer) line(symbol, color, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if s.color {
		fmt.Fprintf(s.w, "%s%s%s %s\n", color, symbol, colorReset, msg)
		return
	}
	fmt.Fprintf(s.w, "%s %s\n", symbol, msg)
}

// Step announces a phase starting — in progress, not yet resolved (e.g.
// "starting service...", "running k6...").
func (s *Writer) Step(format string, args ...any) { s.line("→", colorCyan, format, args...) }

// Done announces a phase finishing successfully (e.g. "init phase
// complete", "service stopped (signal=TERM, clean=true)").
func (s *Writer) Done(format string, args ...any) { s.line("✓", colorGreen, format, args...) }

// Info prints a neutral, informational line — not a phase transition,
// just a note worth keeping visible (e.g. "state file: <path>", for
// recovering a `myrtille teardown` if the process gets killed hard).
// Never colored, even interactively — there's no state (in progress,
// succeeded, failed) to signal here, just a fact.
func (s *Writer) Info(format string, args ...any) {
	fmt.Fprintf(s.w, "· %s\n", fmt.Sprintf(format, args...))
}

// Fail announces a specific action failing without necessarily failing
// the whole run (e.g. servicelifecycle's best-effort Stop sending its
// signal) — most fatal run failures already surface as a returned Go
// error, reported once by the CLI itself, so this is used sparingly here.
func (s *Writer) Fail(format string, args ...any) { s.line("✗", colorRed, format, args...) }
