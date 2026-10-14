package clusterspec

// patch_targets_field_test.go covers PatchTargetsField where it now LIVES.
//
// THE TESTS DID NOT MOVE WITH THE CODE. This check was written in
// assertplatform, for the PR-time lane. It moved here when a second caller
// appeared — brownfield's pre-delete probe, which had been grading a refusal
// without it and cleared an orphan delete for a patch aimed at an unrelated key.
// Its tests stayed in assertplatform, which left the logic exercised only through
// one of its two callers and dropped this package to within 0.7 points of its
// coverage floor. A shared check needs coverage that does not depend on which
// caller happens to be tested.

import (
	"strings"
	"testing"
)

// scalarRow is a row shaped like the real container-resource rows: a selector
// mid-path, a scalar leaf.
func scalarRow() OverlayField {
	return OverlayField{
		App: "demo", Value: []string{"size"},
		Kind: "widget", Namespace: "ns", Name: "w",
		Live:  []string{"spec", "containers[name=main]", "size"},
		Match: MatchScalar,
	}
}

func TestPatchTargetsFieldAcceptsTheRowsOwnPatch(t *testing.T) {
	p := `{"spec":{"containers":[{"name":"main","size":"3Gi"}]}}`
	if err := PatchTargetsField(p, scalarRow(), "3Gi"); err != nil {
		t.Fatalf("a correct patch was rejected: %v", err)
	}
}

func TestPatchTargetsFieldRefusesEachWayAProbeCanBeAboutSomethingElse(t *testing.T) {
	for _, tc := range []struct{ name, patch, want string }{
		{"writes nothing at the row's path",
			`{"spec":{"serviceName":"other"}}`, "does not write anything at"},
		{"writes the wrong value there",
			`{"spec":{"containers":[{"name":"main","size":"nobody-declared-this"}]}}`, "not the declared"},
		{"also writes at a map level the walk passes through",
			`{"spec":{"containers":[{"name":"main","size":"3Gi"}],"podManagementPolicy":"Parallel"}}`, "also writes"},
		{"selects a container the row does not name",
			`{"spec":{"containers":[{"name":"sidecar","size":"3Gi"}]}}`, "does not write anything at"},
		{"writes the row's field twice through a duplicate element",
			`{"spec":{"containers":[{"name":"main","size":"3Gi"},{"name":"main","size":"64Mi"}]}}`, "also writes"},
		{"is not JSON at all", `{`, "not valid JSON"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := PatchTargetsField(tc.patch, scalarRow(), "3Gi")
			if err == nil {
				t.Fatalf("accepted as testing this row's field: %s", tc.patch)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not say why.\nwant a message containing %q\ngot: %v", tc.want, err)
			}
		})
	}
}

func TestPatchTargetsFieldAllowsWhatAddressesOrCompletesTheRowsOwnElement(t *testing.T) {
	// A merge key the API needs, and a sibling the apiserver requires alongside the
	// row's field, are not second writes — they are inside the list element the row
	// already targets, and neither can produce a spurious whole-spec refusal.
	p := `{"spec":{"containers":[{"name":"main","port":8080,"size":"3Gi"}]}}`
	if err := PatchTargetsField(p, scalarRow(), "3Gi"); err != nil {
		t.Errorf("a patch carrying a merge key inside the row's own element was rejected, which "+
			"would make such a row unfixable: %v", err)
	}
}

func TestPatchTargetsFieldOnAListRowTreatsThePayloadAsOpaque(t *testing.T) {
	// MatchNonEmptyList's payload is a whole list; every field inside an entry is
	// the row's own business and must not be reported as an extra write.
	f := OverlayField{
		App: "demo", Value: []string{"on"}, Kind: "widget", Namespace: "ns", Name: "w",
		Live: []string{"spec", "claims"}, Match: MatchNonEmptyList,
	}
	p := `{"spec":{"claims":[{"metadata":{"name":"data"},"spec":{"resources":{"requests":{"storage":"5Gi"}}}}]}}`
	if err := PatchTargetsField(p, f, true); err != nil {
		t.Fatalf("a list payload's own fields were counted as extra writes: %v", err)
	}
	// …but a key beside the list still is one.
	p2 := `{"spec":{"claims":[{"metadata":{"name":"data"}}],"podManagementPolicy":"Parallel"}}`
	if err := PatchTargetsField(p2, f, true); err == nil {
		t.Error("a key beside the row's list was accepted; on a StatefulSet that key alone draws " +
			"the whole-spec refusal this check exists to attribute correctly")
	}
}

func TestTheInElementExemptionDoesNotExtendToArbitraryDepth(t *testing.T) {
	// THE EXEMPTION IS FOR A MERGE KEY AND A CO-REQUIRED SIBLING — scalars sitting
	// directly on the element the row selects. Carried down the subtree it switched
	// the check off entirely: the real row's patch plus a nested extra was accepted,
	// and `_ = inElement` left the package green, so nothing held the relaxation.
	f := scalarRow()
	for _, tc := range []struct {
		name, patch string
		wantErr     bool
	}{
		{"a scalar merge key on the element",
			`{"spec":{"containers":[{"name":"main","port":8080,"size":"3Gi"}]}}`, false},
		{"a nested subtree beside the row's field",
			`{"spec":{"containers":[{"name":"main","size":"3Gi","securityContext":{"privileged":true}}]}}`, true},
		{"a nested resources sibling",
			`{"spec":{"containers":[{"name":"main","size":"3Gi","resources":{"limits":{"cpu":"99"}}}]}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := PatchTargetsField(tc.patch, f, "3Gi")
			if tc.wantErr && err == nil {
				t.Errorf("a nested extra write inside the row's element was accepted — the exemption "+
					"is for the element's own top-level keys, not for its whole subtree: %s", tc.patch)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("a merge key directly on the element was rejected, which is the false red "+
					"the exemption exists to prevent: %v", err)
			}
		})
	}
}

// ── the walker's own divergent-shape arms ────────────────────────────────────
//
// TESTED DIRECTLY, AND THE REASON IS THE FINDING. PatchTargetsField checks
// LiveValue FIRST, so a patch whose shape diverges from Live is refused with "does
// not write anything at" before extraPatchWrites ever runs — which makes these
// three arms unreachable through the exported function and left all three
// deletable with 146 packages green. They are not dead code: each is what keeps
// the walk honest if that ordering is ever changed, and the non-map arm's own
// comment says so. Defensive code still has to be defended, so it is exercised
// here at the level it is reachable from.

func TestTheWalkerReportsAPositionWhereThePatchDivergesFromLive(t *testing.T) {
	// The patch holds a scalar where Live expects to keep descending. Reached through
	// PatchTargetsField only if the LiveValue guard above it is reordered away.
	live := []string{"spec", "size"}
	got := extraPatchWrites("not-a-map", 0, live, "spec", "", false)
	if len(got) != 1 || got[0] != "spec" {
		t.Errorf("a patch that diverges from Live before reaching it did not report its position: %v", got)
	}
}

func TestTheWalkerTreatsASelectedKeyThatIsNotAListAsAnExtraWrite(t *testing.T) {
	// Live says `containers[name=main]`, so the walk expects a list. A patch holding a
	// MAP there is writing somewhere the row does not claim, and every leaf under it
	// has to be named rather than silently descended into as if it were the payload.
	live := []string{"spec", "containers[name=main]", "size"}
	node := map[string]any{"containers": map[string]any{"size": "3Gi"}}
	got := extraPatchWrites(node, 1, live, "spec", "", false)
	if len(got) != 1 || got[0] != "spec.containers.size" {
		t.Errorf("a non-list at the selector position was not reported as an extra write: %v", got)
	}
}

func TestAnEmptyObjectStillCountsAsAWrite(t *testing.T) {
	// `{"ordinals":{}}` beside the row's own field writes nothing readable, but it is
	// still a key in the patch the apiserver sees — and for a StatefulSet ANY
	// non-whitelisted spec key draws the identical whole-spec refusal, which is the
	// verdict this row would then vouch for. A walker that returned nothing for an
	// empty object would let that key through.
	if got := patchLeaves(map[string]any{}, "spec.ordinals"); len(got) != 1 || got[0] != "spec.ordinals" {
		t.Errorf("an empty object was not counted as a write at its own position: %v", got)
	}
}
