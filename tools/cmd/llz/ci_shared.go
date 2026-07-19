package main

// ci_shared.go holds the primitives the ci gate verbs share: the kubectl/clock
// seam type and constructor (aplGateDeps), the deadline poll loop every gate
// spells the same way, the env-with-default reader, and the kubeconfig tempfile
// spill. Each of these had drifted into three-to-five near-identical inline
// copies across the verbs; collapsing them here keeps the incident history (the
// comments below) in exactly one place.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
	tf "github.com/akamai-consulting/lke-landing-zone/tools/internal/terraform"
)

// kubectlRunner runs one kubectl invocation (KUBECONFIG already wired by the
// caller) and returns its combined output plus whether it exited 0.
type kubectlRunner func(args ...string) (string, bool)

// aplGateDeps are the seams every ci gate drives: one kubectl invocation plus
// now/sleep for the testable deadline loop. One type for all the gates — the
// kyverno and destroy-unwedge verbs each used to declare their own structurally
// identical copy, to the point that one call site had to write
// `kyvernoDeps(newAplGateDepsFor(…))` to convert between them.
type aplGateDeps struct {
	kubectl kubectlRunner
	now     func() time.Time
	sleep   func(time.Duration)
}

// aplGateKubectl builds the kubectl runner for the gate seams. kubeconfig == ""
// inherits the ambient environment so kubectl resolves $KUBECONFIG /
// ~/.kube/config itself; a non-empty path pins KUBECONFIG to that file.
//
// runCombined runs the command BEFORE reading its buffer. The original
// `return buf.String(), c.Run() == nil` evaluated buf.String() first (Go's
// left-to-right operand order), snapshotting the buffer EMPTY before the command
// ever ran — so every gate read back blank output and sat reading sync= health=
// forever. Do not "simplify" this back into a single return expression.
func aplGateKubectl(kubeconfig string) kubectlRunner {
	return func(args ...string) (string, bool) {
		c := exec.Command("kubectl", args...)
		if kubeconfig == "" {
			c.Env = os.Environ()
		} else {
			c.Env = envWithKubeconfig(kubeconfig)
		}
		return runCombined(c)
	}
}

// envWithKubeconfig returns the process env with KUBECONFIG set to exactly `path`
// — dropping any inherited KUBECONFIG first so kubectl/helm can't read a duplicate
// (often empty) entry instead. Duplicate KUBECONFIG env keys are resolved
// inconsistently, which is how an empty placeholder $KUBECONFIG shadowed the real
// resolved path in the e2e.
//
// Do not "simplify" this to `append(os.Environ(), "KUBECONFIG="+path)` — that is
// the spelling with the bug, and it is what aplGateKubectl above used until this
// helper (written for the bootstrap-cluster runners after that e2e failure) was
// lifted here and shared.
func envWithKubeconfig(path string) []string {
	src := os.Environ()
	env := make([]string, 0, len(src)+1)
	for _, e := range src {
		if strings.HasPrefix(e, "KUBECONFIG=") {
			continue
		}
		env = append(env, e)
	}
	return append(env, "KUBECONFIG="+path)
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

// readRegionTFVars resolves the tfvars for a region — preferring
// <tfDir>/<region>.tfvars and falling back to the committed .example — parses
// it, and asserts a cluster_label is present. tfDir == "" resolves relative to
// the current working directory (the tf-root the verb already chdir'd into).
//
// Four verbs (tf-import, firewall discovery, teardown, destroy-unwedge) each
// open-coded this same stat-then-fallback + "read %s" wrap + "%s has no
// cluster_label" guard; two of them carried comments saying they mirrored
// runCITFImport, which is how you know it was copied. The guard is on
// ClusterLabel rather than DeriveLabels().Cluster because those are the same
// field (Labels.Cluster is assigned straight from v.ClusterLabel) — callers that
// want labels derive them from the returned vars.
func readRegionTFVars(tfDir, region string) (tf.TFVars, string, error) {
	prefix := ""
	if tfDir != "" {
		prefix = tfDir + "/"
	}
	varFile := prefix + region + ".tfvars"
	if _, err := os.Stat(varFile); err != nil {
		varFile = prefix + region + ".tfvars.example"
	}
	content, err := os.ReadFile(varFile)
	if err != nil {
		return tf.TFVars{}, varFile, fmt.Errorf("read %s: %w", varFile, err)
	}
	vars := tf.ParseTFVars(string(content))
	if vars.ClusterLabel == "" {
		return tf.TFVars{}, varFile, fmt.Errorf("%s has no cluster_label", varFile)
	}
	return vars, varFile, nil
}

// ciClient builds the Linode API client the ci verbs share, with the 60s
// per-request timeout they all used, plus the background context they all then
// created. Four verbs (tf-import, the two reap gates, teardown) open-coded this
// exact ciToken → err-check → NewClient(token, 60*time.Second) →
// context.Background() sequence.
//
// This is deliberately NOT applied to the narrow per-command interfaces
// (teardownClient, firewallDiscoverer, credLister, …) — those are intentional
// test seams, not duplication.
func ciClient() (*linode.Client, context.Context, error) {
	token, err := ciToken()
	if err != nil {
		return nil, nil, err
	}
	return linode.NewClient(token, 60*time.Second), context.Background(), nil
}
