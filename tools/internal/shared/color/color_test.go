package color

// color_test.go — paint's on/off behaviour, moved here with the package. It was in
// package main's coverage_tier1_test.go, which is a grab-bag rather than a home.

import (
	"strings"
	"testing"
)

func TestPaint(t *testing.T) {
	defer func(o bool) { colorOn = o }(colorOn)

	colorOn = false
	if got := paint("32", "x"); got != "x" {
		t.Errorf("paint with color off = %q, want x", got)
	}
	colorOn = true
	if got := paint("32", "x"); got != "\033[32mx\033[0m" {
		t.Errorf("paint with color on = %q", got)
	}
}

// Every wrapper, so a copy-paste error in the table (two names sharing a code)
// fails here. They are one line each, which is exactly why nothing would notice.
func TestEachColorUsesItsOwnCode(t *testing.T) {
	orig := colorOn
	colorOn = true
	t.Cleanup(func() { colorOn = orig })

	seen := map[string]string{}
	for name, fn := range map[string]func(string) string{
		"Green": Green, "Yellow": Yellow, "Red": Red,
		"Cyan": Cyan, "Magenta": Magenta, "Bold": Bold, "Dim": Dim,
	} {
		got := fn("x")
		if !strings.HasPrefix(got, "\033[") || !strings.HasSuffix(got, "x\033[0m") {
			t.Errorf("%s(%q) = %q, want an ANSI-wrapped x", name, "x", got)
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("%s and %s emit the same escape %q — one of them is a copy-paste", name, prev, got)
		}
		seen[got] = name
	}
}

// The opt-outs are the reason this is a package rather than seven fmt calls: a
// piped or CI-captured stream must stay clean, and every extracted extension now
// depends on that being decided in one place.
func TestColorOffMeansPlainText(t *testing.T) {
	orig := colorOn
	colorOn = false
	t.Cleanup(func() { colorOn = orig })

	for name, fn := range map[string]func(string) string{
		"Green": Green, "Yellow": Yellow, "Red": Red,
		"Cyan": Cyan, "Magenta": Magenta, "Bold": Bold, "Dim": Dim,
	} {
		if got := fn("x"); got != "x" {
			t.Errorf("%s with color off = %q, want plain x", name, got)
		}
	}
}
