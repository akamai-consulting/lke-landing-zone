package ghaout_test

// The behaviour worth pinning is the UNSET-ENV NO-OP. Every caller runs both
// under Actions and on a laptop, and returning an error off-Actions would make
// `llz ci` unusable locally — a regression that CI, which always sets the var,
// could never catch.

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghaout"
)

func TestAppendIsANoOpWhenTheEnvVarIsUnset(t *testing.T) {
	t.Setenv("LLZ_TEST_OUT", "")
	if err := ghaout.Append("LLZ_TEST_OUT", "k=v"); err != nil {
		t.Fatalf("unset env must be a no-op, got %v", err)
	}
}

func TestAppendAccumulatesAcrossCalls(t *testing.T) {
	p := filepath.Join(t.TempDir(), "out")
	t.Setenv("LLZ_TEST_OUT", p)
	if err := ghaout.Append("LLZ_TEST_OUT", "a=1", "b=2"); err != nil {
		t.Fatal(err)
	}
	if err := ghaout.Append("LLZ_TEST_OUT", "c=3"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), "a=1\nb=2\nc=3\n"; got != want {
		t.Errorf("file = %q, want %q — O_APPEND, not truncate: a step writes many outputs", got, want)
	}
}

// Mask/MaskLines arrived from baoseed uncovered. The per-line split is the part
// worth pinning: ::add-mask:: matches whole lines, so a multi-line secret masked
// as one blob leaves every individual line visible in the log — which is the
// failure this second function exists to prevent.
func TestMaskLinesEmitsOneDirectivePerLine(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "true")
	out := captureStdout(t, func() { ghaout.MaskLines("alpha\nbeta\n\n  \ngamma") })
	for _, want := range []string{"::add-mask::alpha", "::add-mask::beta", "::add-mask::gamma"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q missing %q", out, want)
		}
	}
	// Blank and whitespace-only lines are skipped: `::add-mask::` with an empty
	// value is rejected by the runner and would mask nothing while looking fine.
	if strings.Contains(out, "::add-mask::\n") || strings.Contains(out, "::add-mask::  ") {
		t.Errorf("blank line produced a mask directive: %q", out)
	}
}

func TestMaskIsSilentOutsideActions(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "")
	if out := captureStdout(t, func() { ghaout.Mask("secret") }); out != "" {
		t.Errorf("printed %q outside GitHub Actions — the directive is noise in a local run", out)
	}
	t.Setenv("GITHUB_ACTIONS", "true")
	if out := captureStdout(t, func() { ghaout.Mask("") }); out != "" {
		t.Errorf("empty value produced %q; masking nothing is a no-op", out)
	}
}

// captureStdout: the campaign's most-copied helper, local again for the usual
// reason — a stream-swapping helper cannot live in a shared package without
// shipping `testing` into production code.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = orig
	b, _ := io.ReadAll(r)
	return string(b)
}
