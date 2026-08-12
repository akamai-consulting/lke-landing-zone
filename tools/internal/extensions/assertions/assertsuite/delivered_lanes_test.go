package assertsuite

// delivered_lanes_test.go — the lane names an ADOPTER'S workflow names must exist.
//
// ────────────────────────────────────────────────────────────────────────────
// NOTHING CHECKED THEM, AND THEY SHIP.
//
// `llz ci assert-suite --only <names>` refuses an unknown lane loudly, which is
// right — but the refusal happens where the command RUNS, and one caller is
// instance-template/.github/workflows/llz-cluster-health.yml: a vendored workflow
// that lands in every adopter's repo and runs on their schedule against their
// cluster. Renaming a lane here compiles, passes every test, passes `llz ci
// gates`, and breaks the scheduled health gate of every instance already built —
// discovered by an operator reading a failed run, not by anyone who made the
// change.
//
// The delivered surface is where this repo's rot concentrates, for exactly this
// reason: the tests all run against THIS tree, and the thing that broke lives in a
// copy of a file somewhere else.
//
// SO THE NAMES ARE RESOLVED AGAINST THE LIVE LANE SET at build time. This is the
// same shape as docs-guard resolving documented `llz …` invocations against the
// cobra tree, applied to the one caller docs-guard cannot see because it is YAML
// rather than Markdown.
// ────────────────────────────────────────────────────────────────────────────

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestDeliveredWorkflowsNameRealLanes(t *testing.T) {
	const wfDir = "../../../../../instance-template/.github/workflows"
	entries, err := os.ReadDir(filepath.FromSlash(wfDir))
	if err != nil {
		t.Fatalf("reading %s: %v — the delivered workflows are this test's whole subject", wfDir, err)
	}

	known := map[string]bool{}
	for _, l := range Lanes("us-ord") {
		known[l.Name] = true
	}
	if len(known) == 0 {
		t.Fatal("Lanes() returned nothing — every name below would read as unknown")
	}

	// `--only a,b,c`, allowing the YAML line-continuations these workflows use.
	only := regexp.MustCompile(`--only[ \t]+([a-z0-9,_-]+)`)
	var checked int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(filepath.FromSlash(wfDir), e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		body := string(b)
		for _, m := range only.FindAllStringSubmatch(body, -1) {
			// `--only` is also a flag on `llz ci gates`, whose vocabulary is gate
			// names rather than lanes. Judge only the assert-suite calls.
			idx := strings.Index(body, m[0])
			window := body[max(0, idx-200):idx]
			if !strings.Contains(window, "assert-suite") {
				continue
			}
			for _, name := range strings.Split(m[1], ",") {
				name = strings.TrimSpace(name)
				if name == "" {
					continue
				}
				checked++
				if !known[name] {
					t.Errorf("%s names lane %q in `assert-suite --only`, and no such lane exists "+
						"(known: %s).\n\tThis file is VENDORED into every adopter's repo. A rename "+
						"here passes every test in this tree and breaks their scheduled health gate "+
						"at run time, where only an operator sees it.", e.Name(), name, knownLaneList(known))
				}
			}
		}
	}

	// ZERO IS NOW THE EXPECTED ANSWER, and that is the point of the change this
	// guard survived rather than a hole in it.
	//
	// It was written because llz-cluster-health.yml hand-wrote a lane list in
	// `--only`, vendored into every adopter's repo, where a rename here would break
	// their gate at run time. That list is gone: both delivered callers now pass
	// `--day2` and the set lives on the lane table, so there is no restated name
	// left to rot. The guard stays because the hazard returns the moment someone
	// writes a list into a delivered file again — which is exactly what it fails on.
	//
	// It does still fail closed on the case that would make it meaningless: a
	// delivered workflow that narrows the suite by name AND names something fake is
	// caught by the loop above.
	if checked > 0 {
		t.Logf("resolved %d delivered lane name(s) against %d live lanes", checked, len(known))
	}

	// The positive half, so this file is not merely dormant: the delivered callers
	// must select lanes through the FLAG, not by restating the set.
	day2 := 0
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(filepath.FromSlash(wfDir), e.Name()))
		if err != nil {
			continue
		}
		if strings.Contains(string(b), "assert-suite --day2") {
			day2++
		}
	}
	if day2 == 0 {
		t.Fatal("no delivered workflow runs `assert-suite --day2`. Either the allow-list flag was renamed " +
			"(update this guard with it) or a delivered caller went back to naming lanes by hand — which is " +
			"the vendored-rename hazard this file exists for")
	}
	t.Logf("%d delivered workflow(s) select lanes via --day2", day2)
}

func knownLaneList(known map[string]bool) string {
	out := make([]string, 0, len(known))
	for n := range known {
		out = append(out, n)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}
