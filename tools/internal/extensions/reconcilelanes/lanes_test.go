package reconcilelanes

import "testing"

// fixedNow pins the clock seam. A local copy of package main's helper: nowUnix is
// this package's own seam now, and the es-store-recovery lane writes a
// revalidation annotation whose value would otherwise change every run.
func fixedNow(t *testing.T, v int64) {
	t.Helper()
	orig := nowUnix
	nowUnix = func() int64 { return v }
	t.Cleanup(func() { nowUnix = orig })
}
