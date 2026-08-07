// Package color centralizes terminal color for human-facing output (doctor,
// readiness, the token wizard, and every extracted extension that prints).
// Everything degrades to plain text when stdout is not a TTY, NO_COLOR is set, or
// TERM=dumb — so piped output and CI logs stay clean. CLICOLOR_FORCE overrides the
// TTY check.
//
// It became a package on the sixth extraction rather than the first: colour is
// what a verb does at the very end, so every extension that prints anything needs
// it, and duplicating a paint() per package would mean six copies of the NO_COLOR
// rule that could disagree about when to stay quiet.
package color

import (
	"os"

	"golang.org/x/term"
)

// colorOn is computed once: ANSI is emitted only for an interactive stdout that
// hasn't opted out. Zero-width escapes mean callers can wrap fixed-width labels
// without breaking column alignment.
var colorOn = func() bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	if os.Getenv("CLICOLOR_FORCE") != "" {
		return true
	}
	return term.IsTerminal(int(os.Stdout.Fd()))
}()

func paint(code, s string) string {
	if !colorOn {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}

func Green(s string) string   { return paint("32", s) }
func Yellow(s string) string  { return paint("33", s) }
func Red(s string) string     { return paint("31", s) }
func Cyan(s string) string    { return paint("36", s) }
func Magenta(s string) string { return paint("35", s) } // instance-owned (escape-hatch) findings — distinct from the platform categories
func Bold(s string) string    { return paint("1", s) }
func Dim(s string) string     { return paint("90", s) } // bright-black: de-emphasized hint text
