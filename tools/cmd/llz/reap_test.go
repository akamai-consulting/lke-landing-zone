package main

import "testing"

// TestEnvObjKeyLabelsMatchRotationTable guards that the per-env obj-key reaper
// targets EXACTLY the labels mint-bootstrap-objkeys creates — so a rename of a
// minted key label can never silently leak keys past the account's 100-key cap.
func TestEnvObjKeyLabelsMatchRotationTable(t *testing.T) {
	const env = "e2e"
	reaped := map[string]bool{}
	for _, l := range envObjKeyLabels(env) {
		reaped[l] = true
	}
	minted := 0
	for _, e := range buildRotationTable(env, "us-ord-1") {
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
		for _, e := range buildRotationTable(env, "us-ord-1") {
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
	if got := inclusterPATLabel("e2e"); got != "llz-incluster-e2e" {
		t.Errorf("inclusterPATLabel(e2e) = %q, want llz-incluster-e2e (reaper matches this exactly)", got)
	}
}
