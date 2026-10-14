package coverageguard

// bank.go — `--bank`: raise each floor to what the package actually measures.
//
// THE FLOORS WERE A MANUAL RATCHET, AND MANUAL RATCHETS SLIP DOWNHILL. Adding one
// guard in a single session moved its floor four times — 80, 83, 84, 86 — each
// bump made by hand AFTER a red run, because the only signal that a floor had
// fallen behind was the gate passing with slack. Slack is invisible: a package at
// 86% against a floor of 80 reports `ok`, and the six points it gained are free
// for the next change to spend without anyone deciding to spend them.
//
// TIGHTEN-ONLY, AND THAT IS THE WHOLE SAFETY PROPERTY. This never lowers a floor.
// A tool that could lower one is a tool for making red go green, which is the
// opposite of the gate; `.untestable-budget.yaml`'s doctrine says the same thing
// about its own numbers — "Do NOT raise the budget to make this pass".
//
// AND IT REFUSES TO RUN WHILE ANY PACKAGE IS BELOW ITS FLOOR. Banking a red run
// would raise the floors that PASSED while leaving the failure untouched, which
// reads afterwards as a commit that "updated coverage" and quietly moved on. Fix
// the failure, then bank.
//
// WHY IT REWRITES THE MAKEFILE rather than a config of its own: COVERAGE_MINS is
// where the floors already live and where a reviewer already looks for them. A
// second home would be one more copy of a number, which is the defect this repo
// keeps finding.

import (
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
)

// covMinLine matches one `  <suffix>=<min> \` entry in the COVERAGE_MINS block.
var covMinLine = regexp.MustCompile(`^(\s*)([A-Za-z0-9_./-]+)=(\d+)(\s*\\?)$`)

// banked is one floor this run would raise.
type banked struct {
	Suffix   string
	From, To int
}

// planBank returns the floors that measured strictly above their current value.
//
// FLOOR, NOT ROUND. A package at 86.9% banks 86: the gate compares `pct >= min`,
// so banking 87 against a measurement that rounds down would fail the very next
// run on unchanged code. The point is to capture what is already true.
func planBank(results []covResult) ([]banked, error) {
	var below []string
	var out []banked
	for _, r := range results {
		if !r.HasData {
			below = append(below, r.Threshold.Suffix+" (no data)")
			continue
		}
		if !r.OK {
			below = append(below, fmt.Sprintf("%s at %.1f%% (floor %s)", r.Threshold.Suffix, r.Pct, r.Threshold.MinStr))
			continue
		}
		got := int(math.Floor(r.Pct + 1e-9))
		cur := int(r.Threshold.Min)
		if got > cur {
			out = append(out, banked{Suffix: r.Threshold.Suffix, From: cur, To: got})
		}
	}
	if len(below) > 0 {
		return nil, fmt.Errorf("refusing to bank while %d package(s) are below their floor:\n\t%s\n"+
			"\tBanking now would raise the floors that PASSED and leave the failure in place, which "+
			"reads afterwards as a commit that updated coverage. Fix the failure, then bank",
			len(below), strings.Join(below, "\n\t"))
	}
	return out, nil
}

// applyBank rewrites the COVERAGE_MINS entries in makefile, returning the new
// contents. Entries not in the plan are untouched, so an unrelated edit in the
// block survives.
func applyBank(makefile string, plan []banked) (string, int) {
	to := map[string]int{}
	for _, b := range plan {
		to[b.Suffix] = b.To
	}
	lines := strings.Split(makefile, "\n")
	changed := 0
	for i, ln := range lines {
		m := covMinLine.FindStringSubmatch(ln)
		if m == nil {
			continue
		}
		want, ok := to[m[2]]
		if !ok {
			continue
		}
		cur, err := strconv.Atoi(m[3])
		if err != nil || want <= cur {
			continue // never lower; see this file's header
		}
		lines[i] = m[1] + m[2] + "=" + strconv.Itoa(want) + m[4]
		changed++
	}
	return strings.Join(lines, "\n"), changed
}

// RunBank evaluates the profile and raises every floor that has slack.
func RunBank(profile, makefile string, args []string, out io.Writer) error {
	if profile == "" {
		return fmt.Errorf("--profile (path to the Go coverprofile) is required")
	}
	repo, rel := capability.RepoContaining(bankBinding(), profile)
	raw, err := repo.ReadFile(rel)
	if err != nil {
		return fmt.Errorf("coverage: profile not found: %s", profile)
	}
	thresholds, err := parseCovThresholds(args)
	if err != nil {
		return err
	}
	plan, err := planBank(evaluateCoverage(string(raw), thresholds))
	if err != nil {
		return fmt.Errorf("coverage --bank: %w", err)
	}
	if len(plan) == 0 {
		fmt.Fprintln(out, "coverage --bank: every floor already matches what its package measures.")
		return nil
	}

	// The reader and the writer are built from the SAME binding, so the grant that
	// permits the edit is the one that permitted the read of what is being edited.
	mread, mrel := capability.RepoContaining(bankBinding(), makefile)
	body, err := mread.ReadFile(mrel)
	if err != nil {
		return fmt.Errorf("coverage --bank: %s: %w", makefile, err)
	}
	mwrite, _ := capability.RepoContainingWriter(bankBinding(), makefile)
	updated, n := applyBank(string(body), plan)
	if n == 0 {
		return fmt.Errorf("coverage --bank: %d floor(s) have slack but no COVERAGE_MINS entry matched "+
			"in %s — the block's shape changed and this would silently bank nothing", len(plan), makefile)
	}
	if err := mwrite.WriteFile(mrel, []byte(updated), 0o644); err != nil {
		return fmt.Errorf("coverage --bank: writing %s: %w", makefile, err)
	}
	for _, b := range plan {
		fmt.Fprintf(out, "  %-40s %d -> %d\n", b.Suffix, b.From, b.To)
	}
	fmt.Fprintf(out, "coverage --bank: raised %d floor(s) in %s. Commit this with the tests that earned it.\n",
		n, makefile)
	return nil
}
