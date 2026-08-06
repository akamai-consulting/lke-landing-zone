package main

// A COUPLING TEST THAT SPANS THE BOUNDARY, so it lives on the side that can see
// both halves.
//
// It asserts that the "canary" kind the VAP lane classifies is one the Go guard's
// allowlist REJECTS — the two must not drift, or a kind vetted in one place
// silently passes in the other. waveHealthAllowedKinds is in
// ci_wave_health_guard.go (the `wave-health` candidate, not yet extracted); when
// it moves, this test moves with it and the assertion becomes a cross-package one.

import (
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/assertnetwork"
)

func TestWaveHealthCanaryIsAKindTheGuardMustReject(t *testing.T) {
	// The canary only proves anything if the guard would genuinely deny it: a
	// health-checked kind ABSENT from the allowlist, at a NEGATIVE sync-wave, and
	// not an Argo hook (hooks are exempt via the VAP's not-argo-hook
	// matchCondition). Guard against someone "fixing" the canary into an
	// allowlisted kind, which would make this assert silently unfalsifiable.
	if !strings.Contains(assertnetwork.WaveHealthCanaryManifest, `argocd.argoproj.io/sync-wave: "-5"`) {
		t.Error("canary must carry a NEGATIVE sync-wave or the VAP's matchConditions skip it")
	}
	if strings.Contains(assertnetwork.WaveHealthCanaryManifest, "argocd.argoproj.io/hook") {
		t.Error("canary must not be an Argo hook — the VAP exempts hooks")
	}
	if _, allowlisted := waveHealthAllowedKinds["apps/Deployment"]; allowlisted {
		t.Error("apps/Deployment became allowlisted — the canary would now be ADMITTED and this assert would fail on a healthy cluster; pick another unvetted health-checked kind")
	}
}
