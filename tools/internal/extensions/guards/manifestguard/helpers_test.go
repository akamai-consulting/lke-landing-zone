package manifestguard

// helpers_test.go — fixtures the moved tests need.

import (
	"io"
	"os"
	"strings"
	"testing"
)

// captureStderr captures the os.Stderr path — the remediation / warning printers
// write there.
//
// Its stdout twin and a mustWrite fixture lived here too, and went with the
// apl-schema tests: `llz ci validate-apl-values` was retired when its only input,
// a rendered apl-core values.yaml, stopped being rendered on the managed
// platform. Deleting rather than keeping them "in case": an unused test helper is
// indistinguishable from one whose test was accidentally dropped.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	fn()
	w.Close()
	os.Stderr = orig
	var b strings.Builder
	if _, err := io.Copy(&b, r); err != nil {
		t.Fatal(err)
	}
	return b.String()
}
