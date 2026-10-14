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

	// A corpus guard that matched nothing reports clean over a workflow it never
	// read — the failure this whole tree refuses.
	if checked == 0 {
		t.Fatal("no `assert-suite --only` lane name found in the delivered workflows. Either " +
			"the health workflow stopped narrowing the suite (delete this guard and say so in " +
			"the same commit), or the flag spelling changed and this guard has been vacuous since")
	}
	t.Logf("resolved %d delivered lane name(s) against %d live lanes", checked, len(known))
}

func knownLaneList(known map[string]bool) string {
	out := make([]string, 0, len(known))
	for n := range known {
		out = append(out, n)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}
