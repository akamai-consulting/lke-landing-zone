package cli

// Prompt, ReadEnvFile and SortedKeys arrived from internal/extensions/onboard
// uncovered here — their tests were in cmd/llz, exercising them through main's
// wrappers rather than directly.

import (
	"bufio"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPromptTrimsAndHandlesEOF(t *testing.T) {
	in := bufio.NewScanner(strings.NewReader("  spaced value  \n"))
	if got := Prompt(in, "label"); got != "spaced value" {
		t.Errorf("Prompt = %q, want it trimmed", got)
	}
	// EOF (no terminal, closed stdin) yields "" rather than blocking or panicking —
	// this runs in CI and over `ssh host 'llz ...'` where there is nothing to read.
	empty := bufio.NewScanner(strings.NewReader(""))
	if got := Prompt(empty, "label"); got != "" {
		t.Errorf("Prompt at EOF = %q, want empty", got)
	}
}

func TestReadEnvFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".env")
	body := "A=1\n" +
		"B = two \n" +
		"# a comment\n" +
		"\n" +
		"C=has=equals\n" +
		"NOEQUALS\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got := ReadEnvFile(p)
	// C keeps everything after the FIRST `=`: a value containing `=` is ordinary
	// (base64, a query string), and splitting on all of them would truncate it.
	//
	// B PINS AN ASYMMETRY RATHER THAN ENDORSING IT. The whole LINE is trimmed and
	// then the KEY is trimmed again, but the value is not — so `B = two ` yields
	// " two", with the leading space kept. Trailing space is removed only because
	// the line-level trim already took it. A .env value that begins with a space is
	// almost certainly a typo rather than an intent, so this is worth knowing about;
	// it is pinned as-is because changing it is a behaviour change for every caller
	// and belongs in its own commit, not in a package move.
	for k, want := range map[string]string{"A": "1", "B": " two", "C": "has=equals"} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q (full: %v)", k, got[k], want, got)
		}
	}
	for _, absent := range []string{"# a comment", "NOEQUALS", ""} {
		if _, ok := got[absent]; ok {
			t.Errorf("%q should not be a key: %v", absent, got)
		}
	}
	// A missing file is an empty map, not a crash: callers treat "no .env yet" as
	// a normal first-run state.
	if m := ReadEnvFile(filepath.Join(dir, "absent")); len(m) != 0 {
		t.Errorf("missing file = %v, want empty", m)
	}
}

func TestSortedKeys(t *testing.T) {
	got := SortedKeys(map[string]string{"c": "", "a": "", "b": ""})
	if want := []string{"a", "b", "c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("SortedKeys = %v, want %v — the order is what makes rendered output diffable", got, want)
	}
	if got := SortedKeys(nil); len(got) != 0 {
		t.Errorf("SortedKeys(nil) = %v, want empty", got)
	}
}
