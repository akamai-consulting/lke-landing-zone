package teardown

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/credrotate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"
)

// TestEnvObjKeyLabelsMatchRotationTable guards that the per-env obj-key reaper
// targets EXACTLY the labels mint-bootstrap-objkeys creates — so a rename of a
// minted key label can never silently leak keys past the account's 100-key cap.
func TestEnvObjKeyLabelsMatchRotationTable(t *testing.T) {
	const env = "e2e"
	reaped := map[string]bool{}
	for _, l := range envObjKeyLabels("acme", env) {
		reaped[l] = true
	}
	minted := 0
	for _, e := range credrotate.BuildRotationTable("acme", env, "us-ord-1") {
		if e.Kind != credrotate.CredKindObjKey {
			continue
		}
		minted++
		if !reaped[e.Label] {
			t.Errorf("ReapEnvObjKeys does not target minted obj-key label %q — it would leak on teardown", e.Label)
		}
	}
	if minted == 0 {
		t.Fatal("credrotate.BuildRotationTable produced no obj-key entries — test can't verify coverage")
	}
	// And the reaper must not target a label nothing mints (over-broad delete) —
	// EXCEPT a deliberately retired one, which it must keep targeting.
	//
	// The invariant is mint ⊆ reap, not mint == reap. A key stops being minted the
	// moment its entry leaves the rotation table, but every cluster bootstrapped
	// before that still holds one, and teardown is the only thing that removes it.
	// Equality here would have forced the retirement to leak two keys per destroyed
	// deployment against a 100-key account cap.
	retired := map[string]string{
		"acme-loki-" + env:            "per-app Loki obj key; ExternalSecret deleted when object storage went apl-core-native",
		"acme-harbor-registry-" + env: "per-app Harbor registry obj key; same retirement",
	}
	for l := range reaped {
		found := false
		for _, e := range credrotate.BuildRotationTable("acme", env, "us-ord-1") {
			if e.Kind == credrotate.CredKindObjKey && e.Label == l {
				found = true
			}
		}
		if !found {
			if _, ok := retired[l]; !ok {
				t.Errorf("ReapEnvObjKeys targets label %q that credrotate.BuildRotationTable never "+
					"mints and that is not a named retired label — an over-broad delete", l)
			}
		}
	}
	// A retired label that stops being reaped leaks on every teardown, silently.
	for l, why := range retired {
		if !reaped[l] {
			t.Errorf("retired obj-key label %q is no longer reaped (%s) — clusters bootstrapped "+
				"before the retirement still hold that key, and teardown is the only thing that "+
				"deletes it", l, why)
		}
	}
}

// TestEnvInclusterPATLabel pins the in-cluster PAT label the reaper deletes to the
// one linode.InClusterPATLabel mints.
func TestEnvInclusterPATLabel(t *testing.T) {
	// Instance-scoped: runCredentialsPATRevokeOld revokes every token with this
	// exact label, so two instances sharing a deployment name on one Linode
	// account would revoke each other's live in-cluster credential monthly.
	if got, want := linode.InClusterPATLabel("acme", "e2e"), "llz-incluster-acme-e2e"; got != want {
		t.Errorf("linode.InClusterPATLabel = %q, want %q (reaper matches this exactly)", got, want)
	}
}
