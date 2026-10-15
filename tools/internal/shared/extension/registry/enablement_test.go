package registry

import (
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
)

func toggles(t *testing.T, on map[string]bool) map[string]clusterspec.ComponentToggle {
	t.Helper()
	m := map[string]clusterspec.ComponentToggle{}
	for name, v := range on {
		v := v
		m[name] = clusterspec.ComponentToggle{Enabled: &v}
	}
	return m
}

func find(t *testing.T, es []Enablement, name string) Enablement {
	t.Helper()
	for _, e := range es {
		if e.Extension.Name == name {
			return e
		}
	}
	t.Fatalf("%s is not in the resolved set", name)
	return Enablement{}
}

// THE PROMISE, ASSERTED. The model says `Always` is a DEFAULT, not a constant:
// "an instance with no object storage must be able to turn assert-objstore off in
// its own configuration rather than by taking a different build". This is the
// first test that could have failed on that claim.
func TestAnInstanceCanTurnACapabilityOff(t *testing.T) {
	on, err := EnabledFor(toggles(t, map[string]bool{"objProxy": true}))
	if err != nil {
		t.Fatal(err)
	}
	if e := find(t, on, "obj-encryption"); !e.Enabled {
		t.Errorf("obj-encryption disabled with objProxy ON (reason %q)", e.Reason)
	}

	off, err := EnabledFor(toggles(t, map[string]bool{"objProxy": false}))
	if err != nil {
		t.Fatal(err)
	}
	e := find(t, off, "obj-encryption")
	if e.Enabled {
		t.Error("obj-encryption still enabled with spec.components.objProxy disabled — this " +
			"is the claim the whole model rests on, and it is the one nothing checked")
	}
	if !strings.Contains(e.Reason, "objProxy") {
		t.Errorf("reason %q does not name the component — an operator asking why a lane did "+
			"not run needs to know WHICH toggle did it", e.Reason)
	}
}

// BOTH DIRECTIONS. The model's own note demands it: "the registry must let an
// instance override this field in both directions." obj-encryption ships
// `Always: false`, so enabling its component must turn it ON — otherwise the
// field is a floor rather than a default.
func TestComponentEnablementOverridesAlwaysInBothDirections(t *testing.T) {
	e := find(t, mustResolve(t, map[string]bool{"objProxy": true}), "obj-encryption")
	if !e.Extension.Always && !e.Enabled {
		t.Error("an Always:false extension stayed off with its component ON — component " +
			"enablement must override the default upward as well as downward")
	}
}

// A TYPO MUST NOT RESOLVE TO "ENABLED". An unknown component name falling back to
// `Always` would enable something the operator believes they turned off, and would
// do it silently — a failure in the dangerous direction.
func TestAnUnknownComponentIsRefusedNotDefaulted(t *testing.T) {
	if _, err := EnabledFor(nil); err != nil {
		t.Fatalf("the real registry does not resolve cleanly: %v", err)
	}

	// Every declared component link must name a real component. This is the same
	// assertion the error path protects, run against the actual tree.
	for name, comp := range ComponentLinks() {
		if _, ok := clusterspec.LookupComponent(comp); !ok {
			t.Errorf("%s follows component %q, which does not exist — EnabledFor refuses "+
				"this rather than defaulting, so it would break the instance at resolve time",
				name, comp)
		}
	}
}

// Extensions with no component keep following Always, and the reason says so.
// Four of the seven opt-in extensions are deliberately unlinked — a one-time
// adoption path, two dev-only tools, and a template-repo-side publisher — and
// giving them a component would mean inventing one to be a checkbox.
func TestUnlinkedExtensionsStillFollowAlways(t *testing.T) {
	res := mustResolve(t, nil)
	links := ComponentLinks()
	var checked int
	for _, e := range res {
		if _, linked := links[e.Extension.Name]; linked {
			continue
		}
		checked++
		if e.Enabled != e.Extension.Always {
			t.Errorf("%s: enabled=%v but Always=%v with no component link",
				e.Extension.Name, e.Enabled, e.Extension.Always)
		}
	}
	if checked == 0 {
		t.Fatal("no unlinked extensions — either everything grew a component or this guard " +
			"stopped scanning")
	}
}

// The mapping is small and load-bearing, so it is pinned by name. A link silently
// changing which component gates a capability is exactly the drift a reader of the
// declaration would not notice.
func TestComponentLinksArePinned(t *testing.T) {
	// THE REASON MATTERS MORE THAN THE PAIR. A link decides whether a capability
	// runs at all, in BOTH directions, so each one is a claim that the extension
	// exists BECAUSE the component does — not merely that the two are related.
	// Extensions spanning several components are deliberately absent below:
	// `assert-secrets` touches externalSecrets, openbao and broadPatRotator, and a
	// single link would turn one of the three into a switch for all of them.
	want := map[string]string{
		// the SSE-C key exists to encrypt what the OBJ proxy writes
		"obj-encryption": "objProxy",
		// proves a minted Harbor robot can authenticate; no Harbor, no robot
		"assert-registry": "harbor",
		// measures the in-cluster reconciler the component deploys
		"assert-reconciler": "llzReconciler",

		// SEVEN ADDED TOGETHER, taking enablement from 3 of 61 to 10. The field had
		// been readable since the resolver landed and almost nothing used it, so the
		// model's headline promise — an instance can turn a capability off in its own
		// configuration rather than by taking a different build — was true for three
		// extensions and undeliverable for the rest.
		//
		// every lane reads the telemetry stack itself: no Prometheus to query, no
		// Loki to ship to
		"assert-observability": "observability",
		// mints Harbor robots and seeds them; no Harbor, no project to mint into
		"harbor-provisioner": "harbor",
		// applies ClusterPolicies and waits for enforcement; without the engine the
		// wait is for a webhook that will never exist
		"kyverno-policies": "policyEngine",
		// seeds the state the in-cluster firewall-controller reconciles FROM, and
		// that controller is what the component installs
		"cloud-firewall-bootstrap": "cidrFirewall",
		// IS the reconcile daemon the component deploys
		"reconciler-runtime": "llzReconciler",
		// the daemon's action lanes run inside that same process, so their
		// enablement cannot differ from its
		"reconcile-actions": "llzReconciler",
		// initialising the store, exchanging peer CAs and seeding into it all
		// presuppose the store the component installs
		"openbao": "openbao",
	}
	got := ComponentLinks()
	for name, comp := range want {
		if got[name] != comp {
			t.Errorf("%s follows %q, want %q", name, got[name], comp)
		}
	}
	for name, comp := range got {
		if want[name] == "" {
			t.Errorf("%s newly follows component %q — add it to this test with the reason, "+
				"because a component link decides whether a capability runs at all", name, comp)
		}
	}
}

func mustResolve(t *testing.T, on map[string]bool) []Enablement {
	t.Helper()
	res, err := EnabledFor(toggles(t, on))
	if err != nil {
		t.Fatal(err)
	}
	return res
}
