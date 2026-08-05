package main

import "testing"

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
	for _, e := range buildRotationTable("acme", env, "us-ord-1") {
		if e.kind != credKindObjKey {
			continue
		}
		minted++
		if !reaped[e.label] {
			t.Errorf("reapEnvObjKeys does not target minted obj-key label %q — it would leak on teardown", e.label)
		}
	}
	if minted == 0 {
		t.Fatal("buildRotationTable produced no obj-key entries — test can't verify coverage")
	}
	// And the reaper must not target a label nothing mints (over-broad delete).
	for l := range reaped {
		found := false
		for _, e := range buildRotationTable("acme", env, "us-ord-1") {
			if e.kind == credKindObjKey && e.label == l {
				found = true
			}
		}
		if !found {
			t.Errorf("reapEnvObjKeys targets label %q that buildRotationTable never mints", l)
		}
	}
}

// TestEnvInclusterPATLabel pins the in-cluster PAT label the reaper deletes to the
// one inclusterPATLabel mints.
func TestEnvInclusterPATLabel(t *testing.T) {
	// Instance-scoped: runCredentialsPATRevokeOld revokes every token with this
	// exact label, so two instances sharing a deployment name on one Linode
	// account would revoke each other's live in-cluster credential monthly.
	if got, want := inclusterPATLabel("acme", "e2e"), "llz-incluster-acme-e2e"; got != want {
		t.Errorf("inclusterPATLabel = %q, want %q (reaper matches this exactly)", got, want)
	}
}
