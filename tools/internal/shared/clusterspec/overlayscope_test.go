package clusterspec

// overlayscope_test.go — LLZ may only speak for apl-core apps it owns.
//
// The apps overlay is a fragment the in-cluster reconciler deep-merges onto
// apl-core's OWN per-app CRs on the machine-owned apl-<env> branch. So the set
// of apps it names is a claim of ownership, and every OTHER renderer gates that
// claim on Component.EmitOnManaged — kustomize.go and render.go both do.
// RenderAppsOverlayEnv walked the registry unfiltered.
//
// Three ManagedSkip components carry AplCoreApps, and on managed all three
// belong to apl-core:
//
//   - gitea, which is DefaultDisabled, so the overlay wrote `enabled: false` —
//     onto the in-cluster gitea that IS the values-repo backend the overlay
//     travels through. A write that disables its own transport.
//   - policyEngine (kyverno, policy-reporter) and imageScanning (trivy), written
//     `enabled: true` on every managed cluster whether or not anyone asked.
//
// Absent is the correct answer for all of them: the overlay merges, so a key LLZ
// omits leaves apl-core's own value standing.

import (
	"testing"

	yaml "gopkg.in/yaml.v3"
)

func appsOverlay(t *testing.T, boot Bootstrap, comps map[string]ComponentToggle) map[string]appToggle {
	t.Helper()
	var doc appsOverlayDoc
	if err := yaml.Unmarshal([]byte(RenderAppsOverlayEnv(boot, comps)), &doc); err != nil {
		t.Fatalf("unmarshal apps overlay: %v", err)
	}
	return doc.Apps
}

// defaultBoot is what Defaults() stamps on an env whose author named no
// managedApps — the input every stock instance renders with.
func defaultBoot() Bootstrap {
	return Bootstrap{ManagedApps: append([]string(nil), DefaultManagedApps...)}
}

// allComponentsOn is the widest possible toggle map: every registry component
// present and enabled. If a ManagedSkip app can leak into the overlay at all, it
// leaks here.
func allComponentsOn() map[string]ComponentToggle {
	m := map[string]ComponentToggle{}
	for _, c := range Components {
		m[c.Name] = ComponentToggle{}
	}
	return m
}

// THE OWNERSHIP RULE, DERIVED FROM THE REGISTRY RATHER THAN LISTED. Naming the
// three apps by hand would pass unchanged the day a fourth ManagedSkip component
// gains an AplCoreApps entry — which is exactly how this one arrived.
func TestTheAppsOverlayNamesNoAppLLZDoesNotOwn(t *testing.T) {
	boot := defaultBoot()
	comps := allComponentsOn()
	got := appsOverlay(t, boot, comps)

	for _, c := range Components {
		if len(c.AplCoreApps) == 0 || c.EmitOnManaged(boot, comps) {
			continue
		}
		for _, app := range c.AplCoreApps {
			if tog, ok := got[app]; ok {
				t.Errorf("overlay writes %q: enabled=%v, but component %q does not emit on managed — "+
					"that key lands on apl-core's own env/apps/%s.yaml and overrides the value apl-core set",
					app, tog.Enabled, c.Name, app)
			}
		}
	}
}

// THE ONE THAT BREAKS THE INSTANCE, called out on its own because the failure is
// not "an app is wrong" but "the overlay disabled the repo it is delivered
// through". A generic ownership assertion states it too quietly to survive a
// future reader deciding the rule is over-strict.
func TestTheAppsOverlayNeverDisablesTheValuesRepoBackend(t *testing.T) {
	got := appsOverlay(t, defaultBoot(), allComponentsOn())
	if tog, ok := got["gitea"]; ok {
		t.Fatalf("overlay writes gitea: enabled=%v — on managed, apl-core's in-cluster gitea is the "+
			"values-repo backend this very overlay is committed to. Writing enabled:false there "+
			"disables the transport mid-delivery", tog.Enabled)
	}
}

// AND IT STILL SAYS WHAT IT SHOULD. A gate that only forbids passes just as
// happily against an overlay that names nothing at all — which would silently
// stop driving every app toggle LLZ genuinely owns.
func TestTheAppsOverlayStillNamesTheAppsLLZDoesOwn(t *testing.T) {
	boot := defaultBoot()
	comps := allComponentsOn()
	got := appsOverlay(t, boot, comps)
	if len(got) == 0 {
		t.Fatal("the apps overlay is empty — LLZ has stopped driving every app toggle it owns")
	}
	// observability and harbor are ManagedConditionalOn loki/harbor, both in
	// DefaultManagedApps, so a stock managed instance does own these.
	for _, app := range []string{"prometheus", "loki", "grafana", "otel", "alertmanager", "harbor"} {
		tog, ok := got[app]
		if !ok {
			t.Errorf("app %q missing: LLZ owns it on a stock managed instance", app)
			continue
		}
		if !tog.Enabled {
			t.Errorf("app %q: want enabled:true for an enabled component", app)
		}
	}
}

// AN UNDECLARED OPTIONAL APP IS NOT LLZ's EITHER. ManagedConditionalOn is the
// other half of the same gate: an operator who did not declare `harbor` in
// managedApps has not asked LLZ to drive apl-core's harbor, so the overlay must
// not name it — in either direction.
func TestTheAppsOverlaySaysNothingAboutUndeclaredOptionalApps(t *testing.T) {
	// managedApps with harbor removed.
	var without []string
	for _, a := range DefaultManagedApps {
		if a != "harbor" {
			without = append(without, a)
		}
	}
	got := appsOverlay(t, Bootstrap{ManagedApps: without}, allComponentsOn())
	if tog, ok := got["harbor"]; ok {
		t.Errorf("overlay writes harbor: enabled=%v with harbor absent from managedApps — "+
			"LLZ has no opinion about an optional app the operator did not declare", tog.Enabled)
	}
	// loki is still declared, so the gate is discriminating rather than off.
	if _, ok := got["loki"]; !ok {
		t.Error("loki is still in managedApps and must still be driven")
	}
}
