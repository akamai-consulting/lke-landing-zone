package main

import "os/exec"

// This file holds the single seam through which llz shells out to external
// tools (git, gh, kubectl, ssh-keyscan, …). The output-capturing and
// tool-presence helpers route through these package-level function variables so
// tests can stub the shell-out and exercise the surrounding parse/branch logic
// without the real binaries. Genuinely interactive call sites (those that wire
// os.Stdin/os.Stdout or pipe a secret over stdin) deliberately keep calling
// os/exec directly — they are exercised by the e2e workflow, not unit tests.

// execOutput runs name with args and returns its standard output, exactly as
// (*exec.Cmd).Output would (stderr is surfaced via *exec.ExitError on failure).
var execOutput = func(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}

// execCombined runs name with args and returns their combined stdout+stderr as a
// string, ignoring exit status. Diagnostics-only: on a failure path we want the
// tool's own error text — "No resources found" (an empty namespace), a NotFound,
// a describe's Events block — to surface, all of which execOutput (stdout-only,
// error-gated) would swallow.
var execCombined = func(name string, args ...string) string {
	out, _ := exec.Command(name, args...).CombinedOutput()
	return string(out)
}

// execLookPath reports a binary's location on PATH, like exec.LookPath.
var execLookPath = func(file string) (string, error) {
	return exec.LookPath(file)
}
