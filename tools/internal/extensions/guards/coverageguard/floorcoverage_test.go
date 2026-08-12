package coverageguard

// floorcoverage_test.go — which packages the coverage gate does NOT gate.
//
// ────────────────────────────────────────────────────────────────────────────
// "Packages without a threshold are not gated." — this command's own --help.
//
// That is the right default and it has no bookkeeping. COVERAGE_MINS is a
// hand-maintained list in the Makefile, check-coverage iterates the FLOORS rather
// than the profile, and a package absent from the list is not reported as
// unchecked — it is simply not mentioned. So the gate cannot distinguish "this
// package is deliberately ungated" from "this package fell off the list".
//
// IT HAS FALLEN OFF THE LIST BEFORE, AND WENT GREEN. The Makefile records it: a
// `#` comment placed inside the backslash-continued assignment consumes the
// continuation, "annotating internal/shared/capability dropped the list from 119
// entries to 100, and `make coverage` went GREEN because the packages it stopped
// knowing about were the ones it stopped checking." Nineteen packages left the
// gate in one edit and nothing said so. The mitigation added then was a comment
// telling the next author not to do it.
//
// Every other "which of these are covered" list in this tree is a two-directional
// ratchet with a reason per entry — undrivenGates, unbackedGrants, componentless,
// allowedSeamCalls, allowedEdges. This one was prose. Now it is a ratchet too.
//
// WHAT IT DOES NOT DO: invent floors. What coverage a package SHOULD hold is a
// judgement, and `make coverage-bank` is the tool for making it — this only
// refuses to let a package become ungated silently.
// ────────────────────────────────────────────────────────────────────────────

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// ungated is every package with tests that COVERAGE_MINS does not gate, and why.
//
// SEEDED WITH WHAT WAS ALREADY TRUE, not with a claim that it is right. Only
// `provider` has a reason of its own; the rest are the population nobody has
// ruled on, in the sense enablement_coverage_test.go uses the phrase. Six of them
// are candidates for `make coverage-bank`, which would set each floor to what the
// package measures today — `internal/shared/forge` most of all, at ~1,700 lines
// across eight files with eleven test files and no floor at all.
//
// EDITING THIS DOWN IS THE POINT. Banking a floor deletes its line here.
var ungated = map[string]string{
	"internal/shared/provider": "interface declaration only — its own boundary_test.go argues a " +
		"unit test would be ceremony, and the invariant it does have is asserted against the build graph",
	"internal/shared/apl/identity": "unruled — nobody has decided; candidate for coverage-bank",
	"internal/shared/apl/overlay":  "unruled — nobody has decided; candidate for coverage-bank",
	"internal/shared/credpaths":    "unruled — nobody has decided; candidate for coverage-bank",
	"internal/shared/forge":        "unruled — the largest of these by far; candidate for coverage-bank",
	"internal/shared/tfroots":      "unruled — nobody has decided; candidate for coverage-bank",
	"internal/shared/validate":     "unruled — nobody has decided; candidate for coverage-bank",
	// NOT "unruled" — ruled out. The package declares data and no functions at all
	// (`go test -cover` reports "[no statements]"), so a floor would be a number
	// with nothing behind it. Its tests assert facts ABOUT that data — that
	// docs/local.md is delivered and classed `owned` — which is exactly the kind of
	// invariant a percentage cannot express.
	"internal/shared/platform": "data declarations only, no statements to cover — its tests assert " +
		"the delivered keep-set and manifest classes, which a coverage number cannot",
}

func TestEveryTestedPackageIsGatedOrListed(t *testing.T) {
	root := filepath.FromSlash("../../../../../")

	raw, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("reading Makefile: %v — COVERAGE_MINS lives there", err)
	}
	floors, err := parseCoverageMins(string(raw))
	if err != nil {
		t.Fatalf("%v — this guard would report every package ungated", err)
	}
	// The truncation this exists for would show up here first.
	if len(floors) < 50 {
		t.Fatalf("parsed only %d COVERAGE_MINS entries — the list was truncated (a `#` inside the "+
			"backslash-continued assignment does exactly that) or the parse broke", len(floors))
	}

	// Packages under tools/internal that ship tests.
	tested := map[string]bool{}
	base := filepath.Join(root, "tools", "internal")
	err = filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return err
		}
		rel, rerr := filepath.Rel(filepath.Join(root, "tools"), filepath.Dir(path))
		if rerr != nil {
			return rerr
		}
		tested[filepath.ToSlash(rel)] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", base, err)
	}
	if len(tested) == 0 {
		t.Fatal("found no tested packages under tools/internal — the walk lost its subject")
	}

	var appeared, banked []string
	for pkg := range tested {
		if !floors[pkg] && ungated[pkg] == "" {
			appeared = append(appeared, pkg)
		}
	}
	for pkg, why := range ungated {
		if floors[pkg] {
			banked = append(banked, pkg)
		}
		if strings.TrimSpace(why) == "" {
			t.Errorf("ungated[%q] has no reason — an entry without one cannot be told from an "+
				"oversight, which is the whole thing this list exists to distinguish", pkg)
		}
		if !tested[pkg] && !floors[pkg] {
			t.Errorf("ungated names %q, which has no tests — the package moved or lost them, so "+
				"the exemption describes nothing", pkg)
		}
	}
	sort.Strings(appeared)
	sort.Strings(banked)

	if len(appeared) > 0 {
		t.Errorf("%d package(s) ship tests and are gated by nothing: %v\n"+
			"\tcheck-coverage iterates the FLOORS, so a package with no COVERAGE_MINS entry is "+
			"not reported as unchecked — it is simply never mentioned, and `make coverage` is "+
			"green either way. Add a floor (`make coverage-bank` sets it to what the package "+
			"measures), or add a line to `ungated` saying why it should not have one.", len(appeared), appeared)
	}
	if len(banked) > 0 {
		t.Errorf("%v now have COVERAGE_MINS floors — DELETE their `ungated` entries in this "+
			"commit. A stale exemption reads as a decision that was made, and hides the next "+
			"package that quietly loses its gate.", banked)
	}
	t.Logf("%d tested package(s), %d floors, %d listed as ungated", len(tested), len(floors), len(ungated))
}

// parseCoverageMins reads the assignment THE WAY MAKE DOES, which is the whole
// point of this file and was wrong in its first cut.
//
// A regex that scanned to the next line starting with a letter or `#` parsed all
// 120 entries out of a block Make would have stopped reading at entry six — so the
// guard passed while `make coverage` silently gated a fraction of the tree, which
// is precisely the incident it cites. A checker that does not share the semantics
// of the thing it checks agrees with itself and nothing else.
//
// Make's rule: a line continues only if its LAST character is a backslash.
// `internal/shared/cli=98 \  # note` does not continue — the backslash is no
// longer last — so the variable ends there and every floor below it is dropped.
func parseCoverageMins(makefile string) (map[string]bool, error) {
	lines := strings.Split(makefile, "\n")
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "COVERAGE_MINS :=") {
			start = i
			break
		}
	}
	if start < 0 {
		return nil, fmt.Errorf("no COVERAGE_MINS assignment found — it was renamed or restructured")
	}
	var block []string
	for i := start; i < len(lines); i++ {
		block = append(block, lines[i])
		if !strings.HasSuffix(lines[i], "\\") {
			break // Make stops here, and so must this
		}
	}
	floors := map[string]bool{}
	entry := regexp.MustCompile(`([a-zA-Z][\w./-]*)=(\d+)`)
	for _, m := range entry.FindAllStringSubmatch(strings.Join(block, "\n"), -1) {
		floors[m[1]] = true
	}
	return floors, nil
}
