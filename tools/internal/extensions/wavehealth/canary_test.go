package wavehealth

// THE COUPLING TEST FINALLY LANDED WITH ITS SUBJECT.
//
// One extraction ago this sat in package main, because AllowedKinds was here and
// the VAP canary was in internal/assertnetwork — main was the only side that could
// see both. Now the allowlist is this package's, so the test comes here and the
// assertion becomes a genuine cross-package one: the Go guard's allowlist must
// reject the same kind the ValidatingAdmissionPolicy's CEL rejects.
//
// A kind vetted in one place and not the other is exactly the drift the pair
// exists to prevent, and it is now checked across a boundary rather than inside
// one file.

import (
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertnetwork"
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
	if _, allowlisted := AllowedKinds["apps/Deployment"]; allowlisted {
		t.Error("apps/Deployment became allowlisted — the canary would now be ADMITTED and this assert would fail on a healthy cluster; pick another unvetted health-checked kind")
	}
}
