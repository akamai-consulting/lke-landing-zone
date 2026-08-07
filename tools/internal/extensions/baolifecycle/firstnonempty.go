package baolifecycle

import "os/exec"

// firstNonEmpty is copied, not shared. Ninth package in this campaign to keep its
// own three lines: a shared package for it would be an import in every file for a
// loop with no decision in it.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// execLookPath reports a binary's location on PATH. A package var so the
// breakglass age-encryption path can be tested on a machine without `age`
// installed — and, more usefully, on one WITH it, where the real LookPath would
// let the test reach a live binary.
var execLookPath = func(file string) (string, error) { return exec.LookPath(file) }
