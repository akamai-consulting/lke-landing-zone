package clusterspec

// overlay_prior_corpus_test.go couples OverlayField.Prior to the recorded
// brownfield object it claims to have been read off.
//
// WITHOUT IT, THE GATE MOVED THE TRUST RATHER THAN REMOVING IT. The appliability
// lane exists because CreateOnly is a hand-set boolean nobody verifies. It answers
// that by seeding a fixture with Prior — and Prior is a hand-set STRING nobody
// verified either, whose wrong value does not produce a red. It makes the row
// unaskable: the lane sees fixture == declared, takes its no-transition arm, and
// the row is never probed at all.
//
// The corpus was already in the tree and already loaded — by
// brownfield/livecorpus_test.go, which pins DECLARED-vs-live and never
// PRIOR-vs-live. So the one file that could contradict a wrong Prior was being
// read for a different question. This asks that question.
//
// WHAT IT CANNOT DO, said plainly so the next reader does not over-trust it: the
// corpus is a recording, and apl-core is upstream and unpinned. This holds Prior
// to what was observed, not to what the chart currently defaults to. Refreshing
// the recording is still a human act — but a stale Prior now has to survive
// somebody editing this file, instead of nothing at all.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// brownfieldCorpus is the pre-overlay loki-ingester, as recorded off a cluster
// that predates the overlay.
const brownfieldCorpus = "loki-ingester.brownfield.json"

func TestEveryScalarRowsPriorIsWhatTheRecordedBrownfieldObjectCarries(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "live", brownfieldCorpus))
	if err != nil {
		t.Fatalf("read the brownfield corpus: %v", err)
	}
	var live map[string]any
	if err := json.Unmarshal(raw, &live); err != nil {
		t.Fatalf("decode the brownfield corpus: %v", err)
	}

	checked := 0
	for _, f := range OverlayFields() {
		if f.Match != MatchScalar {
			continue
		}
		// The corpus is ONE object. A scalar row pointing at a different object has no
		// recording to be checked against, and saying so is better than silently
		// counting it as verified.
		if f.Kind != "statefulset" || f.Namespace != "monitoring" || f.Name != "loki-ingester" {
			t.Errorf("%s is a scalar row on %s %s/%s, which no recorded brownfield object covers. "+
				"Record one under testdata/live/ and extend this test, or the row's Prior is a "+
				"hand-set string nothing can contradict — which is the hole this test exists to close",
				OverlayFieldPath(f), f.Kind, f.Namespace, f.Name)
			continue
		}
		if f.Prior == nil {
			// The emitter refuses this too, but it refuses at runtime in a lane that needs
			// a cluster. Saying it here means a PR that adds the row learns at `go test`.
			t.Errorf("%s is a scalar row with no Prior", OverlayFieldPath(f))
			continue
		}

		got, found, missedSelector := LiveValue(live, f.Live)
		if missedSelector {
			t.Errorf("%s: the row's Live path %v does not resolve on the recorded brownfield object — "+
				"the selector matches nothing, so this row points at something the chart does not have",
				OverlayFieldPath(f), f.Live)
			continue
		}
		if !found {
			t.Errorf("%s: the recorded brownfield object carries NOTHING at %v, but the row declares "+
				"Prior %v. A fixture seeded with that value tests a transition no cluster performs",
				OverlayFieldPath(f), f.Live, f.Prior)
			continue
		}
		if !OverlayScalarEqual(got, f.Prior) {
			t.Errorf("%s: Prior is %v but the recorded brownfield %s carries %v at %v.\n"+
				"Prior is the fixture's whole claim to represent a real pre-overlay cluster. A wrong one "+
				"does not fail the appliability lane — it seeds a value nothing runs, so the lane probes a "+
				"transition nobody performs and grades the row on a question nobody asked.\n"+
				"Either correct Prior, or re-record %s if apl-core's chart default has genuinely moved.",
				OverlayFieldPath(f), f.Prior, f.Kind, got, f.Live, brownfieldCorpus)
		}
		checked++
	}

	// VACUITY, for the same reason the lane itself refuses it. A test that verifies
	// zero priors passes just as quietly as one that verifies four, and this file's
	// whole value is that it verified them.
	if checked == 0 {
		t.Fatal("no scalar overlay row was checked against the recorded brownfield object — this test " +
			"proved nothing, which is not the same as finding nothing wrong")
	}
}
