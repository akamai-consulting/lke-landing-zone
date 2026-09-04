package clusterspec

import "testing"

// The history's LAST element is the baseline. This is the whole anti-rot
// mechanism: bumping BaselineAplChartVersion without appending the version it
// replaced fails here, at the same moment and in the same package as the bump.
// A "previous baselines" list maintained separately would go stale silently,
// because a missing entry only shows up as an old pin no longer being recognised
// as one of ours — which looks exactly like an operator's deliberate pin.
func TestAplBaselineHistoryEndsAtTheBaseline(t *testing.T) {
	if len(AplBaselineHistory) == 0 {
		t.Fatal("AplBaselineHistory must not be empty — WasAplBaseline would recognise nothing and every pin would read as deliberate")
	}
	if got := AplBaselineHistory[len(AplBaselineHistory)-1]; got != BaselineAplChartVersion {
		t.Errorf("AplBaselineHistory ends at %q, want the current baseline %q — append the version it replaced when you bump", got, BaselineAplChartVersion)
	}
}

// Strictly ascending, and every entry parseable. An unparseable entry silently
// matches nothing; a duplicate or an out-of-order entry means someone appended
// without reading, which is the same inattention the ordering is here to catch.
func TestAplBaselineHistoryIsOrderedAndParseable(t *testing.T) {
	for i, v := range AplBaselineHistory {
		if _, _, _, ok := AplSemver(v); !ok {
			t.Errorf("AplBaselineHistory[%d] = %q does not parse — it can never match a pin", i, v)
		}
		if i > 0 && !AplSemverLess(AplBaselineHistory[i-1], v) {
			t.Errorf("AplBaselineHistory must be strictly ascending: %q does not sort before %q", AplBaselineHistory[i-1], v)
		}
	}
}

func TestWasAplBaseline(t *testing.T) {
	// Every baseline we have ever shipped is ours, prefix or not: an instance that
	// wrote the bare form is still tracking us and must be recognised as such.
	for _, pin := range []string{"6.0.0", "v6.1.0", "6.1.0", "v6.2.0", "6.2.0", BaselineAplChartVersion, "6.2.1"} {
		if !WasAplBaseline(pin) {
			t.Errorf("WasAplBaseline(%q) = false, want true — a version llz targeted must read as ours", pin)
		}
	}
	// Everything else is the operator's. These are the pins an upgrade must not
	// touch: a deliberate hold, a version we never shipped, and junk.
	for _, pin := range []string{"6.0.1", "6.3.0", "5.0.0", "7.0.0", "", "latest", "6.2"} {
		if WasAplBaseline(pin) {
			t.Errorf("WasAplBaseline(%q) = true, want false — only versions llz itself targeted may be treated as ours", pin)
		}
	}
}

// A PRE-RELEASE IS NOT THE RELEASE. AplSemver strips `-rc.1` on purpose — the
// DRIFT question cares about the numeric triple, not the release channel — and
// routing this question through it made `v6.2.1-rc.1` read as a version llz had
// targeted. llz has never targeted an rc, so a pin naming one is an operator
// deliberately riding a release candidate, and `llz upgrade` would have deleted it
// as "one of ours".
func TestWasAplBaselineRejectsPreReleases(t *testing.T) {
	for _, pin := range []string{
		BaselineAplChartVersion + "-rc.1", "6.2.1-rc.1", "v6.2.0-rc.4", "6.1.0-rc.7", "v6.2.1+build.5",
	} {
		if WasAplBaseline(pin) {
			t.Errorf("WasAplBaseline(%q) = true — llz never targeted a pre-release, so this is the operator's pin and must survive an upgrade", pin)
		}
	}
	// The release itself is still ours, in both spellings.
	for _, pin := range []string{"v6.2.1", "6.2.1", " v6.2.0 "} {
		if !WasAplBaseline(pin) {
			t.Errorf("WasAplBaseline(%q) = false, want true", pin)
		}
	}
}

// The blocking predicate is the ONE copy of "does this drift stop the world",
// shared by the spec preflight and the lane that reads a live cluster. Pin both
// arms, including the override — the arm the live lane was missing.
func TestAplChartDriftBlocks(t *testing.T) {
	blocking := []AplChartDrift{AplChartDriftMajorBehind, AplChartDriftMajorAhead}
	permitted := []AplChartDrift{AplChartDriftNone, AplChartDriftMinor}

	for _, d := range blocking {
		if !AplChartDriftBlocks(d) {
			t.Errorf("drift %v must block by default", d)
		}
	}
	for _, d := range permitted {
		if AplChartDriftBlocks(d) {
			t.Errorf("drift %v must never block — minor/patch lag is routine mid-rollout", d)
		}
	}

	t.Setenv(AllowMajorDriftEnv, "1")
	for _, d := range blocking {
		if AplChartDriftBlocks(d) {
			t.Errorf("%s=1 must release drift %v for a staged upgrade", AllowMajorDriftEnv, d)
		}
	}
}

// And the spec-side gate must DECIDE through that predicate, not beside it — the
// coupling that stops the two gates from drifting apart.
func TestSpecGateDecidesThroughTheSharedPredicate(t *testing.T) {
	for _, pin := range []string{"5.0.0", "7.0.0", "6.0.0", "6.1.0", BaselineAplChartVersion} {
		wantBlocked := AplChartDriftBlocks(AplChartDriftOf(pin))
		gotBlocked := aplChartVersionError("prod", pin) != nil
		if gotBlocked != wantBlocked {
			t.Errorf("pin %q: spec gate blocked=%v, shared predicate says %v — the two must not diverge", pin, gotBlocked, wantBlocked)
		}
	}
}
