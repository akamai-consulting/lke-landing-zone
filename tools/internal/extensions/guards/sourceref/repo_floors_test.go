package sourceref

// repo_floors_test.go — run the guard against THIS repo and assert it looked at
// something.
//
// THE HOLE THIS CLOSES IS THE ONE THE GUARD'S OWN DESIGN OPENED. `Size == 0`
// disables a vocabulary, and that rule is right: a tree with no Makefile, no `ci`
// group or no ADR index cannot answer those questions, and an instance repo is
// exactly such a tree. But the consequence is that a BROKEN INDEX and an ABSENT
// SUBJECT are indistinguishable from the outside — both produce a vocabulary that
// judges nothing, and the summary line simply omits it. A run with five of six
// checks live reads exactly like a run with six.
//
// Every unit test in this package supplies its own tiny tree, so none of them can
// see it either: they assert behaviour GIVEN an index, never that the real repo
// produces one. That is the same shape as the defect the whole campaign has been
// finding — a check that cannot fail for the thing it was built to catch — and it
// was sitting in the checker.
//
// FLOORS, NOT EXACT COUNTS. The numbers below are roughly half what the repo
// currently measures. An exact assertion would fail on every honest edit and be
// deleted within a week; a floor fails only on a COLLAPSE, which is the only
// event that matters here. They are deliberately not ratcheted: this is a
// smoke-detector, not a budget.

import (
	"os"
	"path/filepath"
	"testing"
)

// repoRootForTest walks up for the repository root, the same way the registry's
// tests find the extension tree — this package has already moved once, and a
// relative literal pointed at nothing afterwards.
func repoRootForTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if fi, err := os.Stat(filepath.Join(dir, ".core-surface-budget.yaml")); err == nil && !fi.IsDir() {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Skip("repository root not found from the test's working directory")
	return ""
}

// minimum is the floor for each vocabulary: names indexed, and citations judged.
// Both matter — an index can be built and then matched against nothing, which is
// the failure emptyVocabulary catches per-run but only for an ENABLED vocabulary.
// THE ci-verb VOCABULARY IS ABSENT ON PURPOSE, and the reason is a real
// distinction rather than an exemption. Its index is CALLER-SUPPLIED — the live
// cobra tree, threaded in — so a floor asserted here would measure the fixture
// this test constructs, not the repository. Passing a partial tree would also
// manufacture findings for the ~150 real verbs the fixture omits, which is a test
// reporting its own setup as a defect.
//
// Where that vocabulary can genuinely go quiet is the WIRING, and that is checked
// where the wiring lives: ciVerbs returns nil for a tree with no `ci` group, and
// internal/cli's command tests are what pin the group being attached.
var minimum = map[string]struct{ indexed, refs int }{
	"symbol":      {800, 250},
	"test":        {1500, 40},
	"make target": {30, 50},
	"ADR":         {7, 100},
}

// EVERY FILE-DERIVED VOCABULARY MUST BE LIVE IN THIS REPO. It has Go packages,
// tests, a Makefile and an ADR index, so a disabled one means a broken index
// rather than a different tree — and that is the distinction nothing else could
// draw.
//
// The tree is nil, which disables the ci-verb vocabulary. See `minimum`.
func TestEveryVocabularyIsLiveAgainstThisRepo(t *testing.T) {
	root := repoRootForTest(t)

	rep, err := RunSymbolsReport(root, nil)
	// A finding is not this test's business — a genuinely stale reference should
	// fail the GATE, not this. What matters is that the run examined things.
	if err != nil && len(rep.Vocab) == 0 {
		t.Fatalf("the guard could not run against the repo at all: %v", err)
	}

	if rep.Scanned < 400 {
		t.Errorf("only %d file(s) scanned — the corpus walk collapsed", rep.Scanned)
	}

	seen := map[string]bool{}
	for _, v := range rep.Vocab {
		seen[v.Name] = true
		min, known := minimum[v.Name]
		if !known {
			// A vocabulary with no floor is one this test cannot fail for, which is
			// the hole it exists to close. ci-verb is the one deliberate omission and
			// is disabled here, so it never reaches this branch.
			if v.Indexed > 0 {
				t.Errorf("vocabulary %q has no floor — add one, or this test cannot fail for it", v.Name)
			}
			continue
		}
		if v.Indexed == 0 {
			t.Errorf("vocabulary %q indexed NOTHING, so it was silently disabled — in this repo "+
				"that means its index broke, not that the subject is absent", v.Name)
			continue
		}
		if v.Indexed < min.indexed {
			t.Errorf("vocabulary %q indexed %d name(s), floor %d — the index shrank by more than "+
				"an honest edit explains", v.Name, v.Indexed, min.indexed)
		}
		if v.Refs < min.refs {
			t.Errorf("vocabulary %q judged %d citation(s), floor %d — it indexed names and then "+
				"matched almost nothing, which is what a broken extraction looks like",
				v.Name, v.Refs, min.refs)
		}
	}

	// The other direction: a vocabulary DELETED from the table would simply stop
	// appearing, and every assertion above iterates what is present.
	for name := range minimum {
		if !seen[name] {
			t.Errorf("vocabulary %q is gone from the table — if that is deliberate, delete its "+
				"floor in the same commit", name)
		}
	}
}
