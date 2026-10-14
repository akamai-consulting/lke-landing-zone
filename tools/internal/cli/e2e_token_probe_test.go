package cli

// e2e_token_probe_test.go — the e2e lane must PROBE its dispatch token, before
// it spends anything on the assumption that the token works.
//
// ── THE RUN THIS COMES FROM ───────────────────────────────────────────────────
//
// 2026-08-23. The Preconditions step printed "E2E config OK" and the run died 20
// seconds later at step 10, "Stamp provenance + force-push instantiation":
//
//	remote: Invalid username or token. Password authentication is not supported
//	fatal: Authentication failed for '.../lke-landing-zone-example.git/'
//	##[error]Process completed with exit code 128
//
// An expired PAT, reported as a git exit code, after four steps and a Go build
// had already run on the assumption it was good. Nothing in that message names
// the credential, says it expired, or says where to rotate it.
//
// Preconditions could not have caught it: it runs BEFORE llz is built, so it has
// nothing to probe with, and a presence check is all it can honestly do.
// `llz ci validate-tokens` is the verb for exactly this — it existed already,
// wired into the instance pipeline and not into the lane that instantiates it.
//
// ── WHAT THIS GATE HOLDS ──────────────────────────────────────────────────────
//
// Three properties, and the third is the one that would rot silently.

import (
	"os"
	"strings"
	"testing"
)

const e2eInstantiate = "../../../.github/workflows/e2e-instantiate.yml"

// pushStepName is the step the probe exists to protect: the first thing in the
// lane that USES the dispatch token against the instance repo.
const pushStepName = "Stamp provenance + force-push instantiation"

// probeInvocation is the RUN LINE, not any mention of the verb. Anchoring on the
// bare verb name matched the explanatory comment above the step first, and every
// index computed from it was measuring prose — the same class of mistake this
// file's own gates are written to prevent, caught by them on the first run.
const probeInvocation = "run: bin/llz ci validate-tokens"

func TestE2ELaneProbesItsDispatchTokenBeforeSpending(t *testing.T) {
	raw, err := os.ReadFile(e2eInstantiate)
	if err != nil {
		t.Fatalf("read %s: %v", e2eInstantiate, err)
	}
	body := string(raw)

	probe := strings.Index(body, probeInvocation)
	if probe < 0 {
		t.Fatalf("%s never runs `llz ci validate-tokens`.\n"+
			"    Presence is not validity: the Preconditions step can only check that the secret is\n"+
			"    non-empty, because it runs before llz is built. Without a probe, an expired\n"+
			"    E2E_DISPATCH_TOKEN reaches the force-push and reports as `exit code 128`.",
			e2eInstantiate)
	}

	// ORDER IS THE WHOLE POINT. A probe placed after the push protects nothing —
	// the push has already failed, and the probe's precise message arrives as a
	// second failure nobody reads.
	push := strings.Index(body, pushStepName)
	if push < 0 {
		t.Fatalf("cannot find the step named %q in %s — this gate anchors its ordering check on it, "+
			"so a rename turns the check below into a comparison against -1 that always passes",
			pushStepName, e2eInstantiate)
	}
	if probe > push {
		t.Errorf("the token probe runs AFTER %q, so it protects nothing — the push has already "+
			"failed by then. Move it up to just after the llz build.", pushStepName)
	}
}

// THE ARM THAT WOULD ROT SILENTLY. `llz ci validate-tokens` probes only what is
// in the ENVIRONMENT and SKIPS the rest — with no secret mapped it reports
// "probed 0 credential(s)" and exits 0. So a probe step whose `env:` block loses
// E2E_DISPATCH_TOKEN keeps passing forever while checking nothing, which is
// exactly the shape of the failure it was added to prevent.
//
// GitHub has no way to splat secrets, so that `env:` mapping is hand-maintained
// and can only be held by reading the YAML — the same reason
// TestDeliveredJobCoversRepoLevelRequirements exists for repo-readiness.
func TestE2ETokenProbeActuallyHasATokenToProbe(t *testing.T) {
	raw, err := os.ReadFile(e2eInstantiate)
	if err != nil {
		t.Fatalf("read %s: %v", e2eInstantiate, err)
	}
	body := string(raw)
	probe := strings.Index(body, probeInvocation)
	if probe < 0 {
		t.Skip("no probe step — the sibling test reports that")
	}

	// Look back from the invocation to the start of its step, and require the
	// secret to be mapped inside it.
	stepStart := strings.LastIndex(body[:probe], "- name:")
	if stepStart < 0 {
		t.Fatalf("could not find the step containing the probe")
	}
	step := body[stepStart:probe]
	if !strings.Contains(step, "E2E_DISPATCH_TOKEN: ${{ secrets.E2E_DISPATCH_TOKEN }}") {
		t.Errorf("the probe step does not map E2E_DISPATCH_TOKEN into env:, so `validate-tokens` "+
			"probes NOTHING and exits 0 — a green check over no evidence, which is the exact "+
			"failure this step was added to prevent. Step body:\n%s", step)
	}
}
