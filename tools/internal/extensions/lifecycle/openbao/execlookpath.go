package openbao

import "os/exec"

// execLookPath reports a binary's location on PATH. A package var so the
// breakglass age-encryption path can be tested on a machine without `age`
// installed — and, more usefully, on one WITH it, where the real LookPath would
// let the test reach a live binary.
//
// It arrived with the lifecycle merge. Its file also carried a private
// `firstNonEmpty`, which this package already had a copy of in quote.go — the
// merge deleted one of the two, which is the smallest possible instance of why
// four packages describing one subject were worth collapsing.
var execLookPath = func(file string) (string, error) { return exec.LookPath(file) }
