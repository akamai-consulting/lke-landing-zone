package main

import (
	"os"
	"strings"
	"testing"
)

// Every seeding COMMAND must be invoked by the bootstrap workflow.
//
// externalsecret-paths does not cover this and cannot: it proves some Go source
// contains a `baoread.KVPut("secret/…")` call for each ExternalSecret's path, which is
// a statement about the SOURCE, not about anything running. `llz ci seed-ssec-key`
// configreadiness.Satisfied that guard completely while being invoked by nothing — so the path was
// never written, ESO reported SecretSyncedError, the DaemonSet could not mount the
// Secret, and llz-obj-proxy sat OutOfSync/Degraded until the convergence gate gave
// up. That is how the first e2e run of the obj-proxy component failed, and no
// static gate in the repo could have caught it.
//
// This closes the gap for the seeds that have a dedicated command. It reads the
// shipped workflow rather than a fixture, because the bug was precisely that the
// two drifted.
func TestSeedCommandsAreInvokedByBootstrap(t *testing.T) {
	raw, err := os.ReadFile("../../../instance-template/.github/workflows/llz-bootstrap-openbao.yml")
	if err != nil {
		t.Fatalf("could not read the bootstrap workflow (%v) — a test that skips here proves nothing, "+
			"which is the same class of gap it exists to close", err)
	}
	body := string(raw)
	for _, verb := range []string{
		"llz ci seed-broad-pat",
		"llz ci seed-ssec-key",
	} {
		if !strings.Contains(body, verb) {
			t.Errorf("%q is never invoked by llz-bootstrap-openbao.yml — the command exists, the "+
				"ExternalSecret reads the path it writes, and nothing runs it. The consumer will sit "+
				"Degraded on a Secret that is never created", verb)
		}
	}
}

// The seed must run BEFORE the convergence gate, or the consumer is already
// Degraded by the time the key arrives and the gate has failed the run.
func TestSSECSeedRunsBeforeConvergence(t *testing.T) {
	raw, err := os.ReadFile("../../../instance-template/.github/workflows/llz-bootstrap-openbao.yml")
	if err != nil {
		t.Fatalf("could not read the bootstrap workflow (%v) — a test that skips here proves nothing, "+
			"which is the same class of gap it exists to close", err)
	}
	body := string(raw)
	seed := strings.Index(body, "llz ci seed-ssec-key")
	converge := strings.Index(body, "Wait for cluster to converge")
	if seed < 0 {
		t.Fatal("seed-ssec-key is not in the bootstrap workflow at all")
	}
	if converge < 0 {
		t.Fatal("the convergence gate marker moved — this ordering check is now blind, fix the marker")
	}
	if seed > converge {
		t.Error("seed-ssec-key runs AFTER the convergence gate — the ExternalSecret would be " +
			"SecretSyncedError for the whole wait and the gate would fail before the key existed")
	}
}
