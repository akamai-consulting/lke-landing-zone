package ghaout_test

// The behaviour worth pinning is the UNSET-ENV NO-OP. Every caller runs both
// under Actions and on a laptop, and returning an error off-Actions would make
// `llz ci` unusable locally — a regression that CI, which always sets the var,
// could never catch.

import (
	"os"
	"path/filepath"
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
