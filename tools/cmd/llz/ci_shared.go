package main

// ci_shared.go holds the primitives the ci gate verbs share: the kubectl/clock
// seam constructor (aplGateDeps / kyvernoDeps), the deadline poll loop every
// gate spells the same way, the env-with-default reader, and the kubeconfig
// tempfile spill. Each of these had drifted into three-to-five near-identical
// inline copies across the verbs; collapsing them here keeps the incident
// history (the comments below) in exactly one place.

import (
	"fmt"
	"os"
	"os/exec"
	"time"
)

// aplGateKubectl builds the kubectl runner for the gate seams. kubeconfig == ""
// inherits the ambient environment so kubectl resolves $KUBECONFIG /
// ~/.kube/config itself; a non-empty path pins KUBECONFIG to that file.
//
// runCombined runs the command BEFORE reading its buffer. The original
// `return buf.String(), c.Run() == nil` evaluated buf.String() first (Go's
// left-to-right operand order), snapshotting the buffer EMPTY before the command
// ever ran — so every gate read back blank output and sat reading sync= health=
// forever. Do not "simplify" this back into a single return expression.
func aplGateKubectl(kubeconfig string) func(args ...string) (string, bool) {
	return func(args ...string) (string, bool) {
		c := exec.Command("kubectl", args...)
		if kubeconfig == "" {
			c.Env = os.Environ()
		} else {
			c.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig)
		}
		return runCombined(c)
	}
}

// newAplGateDeps builds the real kubectl/clock seam against the ambient
// KUBECONFIG. Package var so tests can swap the whole seam out.
var newAplGateDeps = func() aplGateDeps { return newAplGateDepsFor("") }

// newAplGateDepsFor is newAplGateDeps with KUBECONFIG pinned to kubeconfig (the
// tempfile the KUBECONFIG_RAW-driven verbs spill).
func newAplGateDepsFor(kubeconfig string) aplGateDeps {
	return aplGateDeps{
		kubectl: aplGateKubectl(kubeconfig),
		now:     time.Now,
		sleep:   time.Sleep,
	}
}

// pollUntil calls cond immediately, then every interval until it returns true or
// timeout elapses (mirrors the bash `until … sleep N` loops). now/sleep are
// injected so tests run without real waiting.
//
// Boundary: the loop gives up when !now().Before(deadline), so a probe landing
// EXACTLY on the deadline is the last one tried. The three loops collapsed into
// this one disagreed here — waitPoll used now().After(deadline), which grants one
// extra sleep+probe round at the exact boundary. !Before is kept: it is the
// stricter reading of "within the budget", it was already what two of the three
// call sites did, and it makes a fake clock that lands exactly on the deadline
// terminate instead of spinning. With a real clock the two spellings differ only
// on an exact-nanosecond tie, which is why nothing observable changes here.
func pollUntil(now func() time.Time, sleep func(time.Duration), timeout, interval time.Duration, cond func() bool) bool {
	deadline := now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if !now().Before(deadline) {
			return false
		}
		sleep(interval)
	}
}

// envOrDefault returns getenv(key), or def when it is unset/empty. getenv is
// injected so env parsing is unit-testable without mutating the process.
func envOrDefault(getenv func(string) string, key, def string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return def
}

// envOr is envOrDefault against the real process environment.
func envOr(key, def string) string { return envOrDefault(os.Getenv, key, def) }

// writeTempKubeconfig writes raw kubeconfig bytes to a tempfile named with the
// given CreateTemp pattern and returns its path plus a remove cleanup. A failed
// write cleans up its own partial file, so the caller only has to defer cleanup
// on the success path.
func writeTempKubeconfig(pattern string, raw []byte) (string, func(), error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", nil, fmt.Errorf("create kubeconfig tempfile: %w", err)
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", nil, fmt.Errorf("write kubeconfig: %w", err)
	}
	f.Close()
	return f.Name(), func() { os.Remove(f.Name()) }, nil
}
