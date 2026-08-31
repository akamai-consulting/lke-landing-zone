package doctor

// capability_conditionality_test.go — a grant is demanded only where its
// consumer is deployed, asserted against what the renderer really emits.
//
// The repo-level Secrets check exists for one consumer: the
// harbor-robot-provisioner CronJob, which ships inside the `harbor` component.
// An instance can opt out of Harbor two ways — spec.components.harbor.enabled on
// self-managed, or omitting `harbor` from bootstrap.managedApps on the Managed
// App Platform (ManagedConditionalOn) — and either way runs no provisioner and
// needs no grant. Demanding it there is a denial with nothing behind it, on a
// credential the operator cannot fix in any way that helps.
//
// THIS TEST LIVES HERE because only this package can import both sides:
// clusterspec (which decides what renders) and tokenprobe (which decides what is
// demanded). It calls both REAL functions rather than restating either rule —
// a restatement would keep passing while the shipped renderer and the shipped
// preflight disagreed, which is the whole failure mode.

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/envreq"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/tokenprobe"
)

const harborGrant = "read/write REPO-level Actions secrets"

// offSet builds the disabled-component set by calling clusterspec's REAL emit
// predicate over a hand-built toggle map + bootstrap — the same conjunction
// RenderManifestKustomization applies to decide whether the provisioner's
// manifests are written at all. Restating "harbor off means skip" here instead
// would pass forever while the renderer changed underneath it.
func offSet(t *testing.T, harborEnabled bool, managedApps []string) map[string]bool {
	t.Helper()
	en := harborEnabled
	toggles := map[string]clusterspec.ComponentToggle{"harbor": {Enabled: &en}}
	boot := clusterspec.Bootstrap{ManagedApps: managedApps}
	off := map[string]bool{}
	if !clusterspec.ComponentEmits(toggles, boot, "harbor") {
		off["harbor"] = true
	}
	return off
}

// probeRepoSecrets runs the repo-Secrets check against a component set, with the
// HTTP seam answering 403 — so an unskipped check is unambiguously a DENIAL and
// the test cannot pass by the probe quietly not happening.
func probeRepoSecrets(t *testing.T, off map[string]bool) tokenprobe.CapabilityResult {
	t.Helper()
	orig := tokenprobe.GHCapabilityProbe
	t.Cleanup(func() { tokenprobe.GHCapabilityProbe = orig })
	tokenprobe.GHCapabilityProbe = func(_, _, _ string) (int, error) { return 403, nil }

	reqs := []envreq.Requirement{{Name: "OPENBAO_SECRETS_WRITE_TOKEN", Secret: true, Required: true}}
	caps, _ := ProbeTokenCapabilities(reqs, map[string]string{"OPENBAO_SECRETS_WRITE_TOKEN": "tok"},
		map[string]string{}, envreq.NewLiveState(nil, nil, nil, nil),
		tokenprobe.CapContext{Repo: "acme/platform", Region: "prod", ComponentOff: off})
	for _, cr := range caps["OPENBAO_SECRETS_WRITE_TOKEN"] {
		if cr.Op == harborGrant {
			return cr
		}
	}
	t.Fatalf("no result for %q", harborGrant)
	return tokenprobe.CapabilityResult{}
}

// SELF-MANAGED OPT-OUT: components.harbor.enabled = false.
func TestHarborDisabledSelfManagedDoesNotDemandRepoSecrets(t *testing.T) {
	off := offSet(t, false, []string{"harbor"})
	cr := probeRepoSecrets(t, off)
	if cr.Status == tokenprobe.CapDenied {
		t.Fatal("a deployment with Harbor switched off must not be denied a grant whose only consumer it never deploys")
	}
	if cr.Status != tokenprobe.CapNotApplicable {
		t.Errorf("status = %v, want CapNotApplicable", cr.Status)
	}
	// NOT CapSkipped. Skipped means "we could not ask" and renders as a standing
	// yellow "· partial" plus a "scope NOT verified" note — a permanent warning on
	// a correct, unchanging configuration, which is how a column stops being read.
	if cr.Status == tokenprobe.CapSkipped {
		t.Error("an inapplicable check must not reuse the could-not-ask verdict")
	}
	if cr.Detail == "" {
		t.Error("it must say WHY the question did not apply")
	}
}

// MANAGED OPT-OUT: harbor absent from bootstrap.managedApps (ManagedConditionalOn).
// The other route to the same state, and the one a components-only check misses.
func TestHarborNotInManagedAppsDoesNotDemandRepoSecrets(t *testing.T) {
	off := offSet(t, true, []string{"loki", "grafana"})
	if !off["harbor"] {
		t.Fatal("fixture: harbor must be off when it is absent from managedApps — otherwise this asserts nothing")
	}
	if cr := probeRepoSecrets(t, off); cr.Status != tokenprobe.CapNotApplicable {
		t.Errorf("status = %v, want CapNotApplicable — managedApps is the second way to opt out", cr.Status)
	}
}

// AND THE INVERSE, so the conditionality cannot pass by switching the check off
// everywhere: with Harbor enabled the grant is still demanded and still blocks.
func TestHarborEnabledStillDemandsRepoSecrets(t *testing.T) {
	off := offSet(t, true, []string{"harbor"})
	if off["harbor"] {
		t.Fatal("fixture: harbor must be ON here, or the inverse proves nothing")
	}
	if cr := probeRepoSecrets(t, off); cr.Status != tokenprobe.CapDenied {
		t.Errorf("status = %v, want CapDenied — an enabled Harbor still needs the grant", cr.Status)
	}
}

// NO SPEC → NO OFF-SET → THE CHECK STILL RUNS. `llz doctor` runs in trees at
// every stage of setup; failing toward "ask for the grant" keeps an unreadable
// spec from silently switching the preflight off.
//
// Through the REAL DisabledComponents, chdir'd into an empty tree, so this
// exercises the production path's fail-open rather than a hand-built empty map.
// (clusterspec's own TestDisabledComponentsReadsARealSpec covers the other half:
// that a spec which IS readable produces the right set — without which this
// fail-open would be indistinguishable from the loader never working.)
func TestUnreadableSpecStillDemandsTheGrant(t *testing.T) {
	t.Chdir(t.TempDir())
	if cr := probeRepoSecrets(t, clusterspec.DisabledComponents("prod")); cr.Status != tokenprobe.CapDenied {
		t.Errorf("status = %v, want CapDenied — an unreadable spec must not switch the check off", cr.Status)
	}
}
