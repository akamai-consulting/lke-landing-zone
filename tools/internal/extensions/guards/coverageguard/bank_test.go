package coverageguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

func res(suffix string, min float64, minStr string, pct float64, hasData bool) covResult {
	return covResult{
		Threshold: covThreshold{Suffix: suffix, Min: min, MinStr: minStr},
		Pct:       pct, HasData: hasData, OK: hasData && pct+1e-9 >= min,
	}
}

// ── the declaration ─────────────────────────────────────────────────────────

// `--bank` writes, and a Gate may hold read-repo and nothing else. The write half
// therefore needs its own binding — the same call template-sustain's lock-refresh
// made. Pinned here so a later edit cannot quietly widen the gate instead.
func TestBankHasItsOwnWriteBinding(t *testing.T) {
	if errs := extension.ValidateSet([]extension.Extension{Extension()}); len(errs) > 0 {
		t.Fatalf("declaration must validate: %v", errs)
	}
	b := bankBinding()
	if b.Kind != extension.Transition {
		t.Errorf("the write half mutates, so it is a transition; got %q", b.Kind)
	}
	var hasWrite bool
	for _, g := range b.Grants {
		if g == extension.WriteRepo {
			hasWrite = true
		}
	}
	if !hasWrite {
		t.Error("floor-bank must declare write-repo — it rewrites COVERAGE_MINS")
	}
	for _, g := range coverageBinding().Grants {
		if g == extension.WriteRepo {
			t.Error("the GATE must not hold write-repo; that is what the second binding is for")
		}
	}
}

// ── the plan ────────────────────────────────────────────────────────────────

// The whole point: slack is invisible, so find it.
func TestPlanBankRaisesFloorsWithSlack(t *testing.T) {
	plan, err := planBank([]covResult{
		res("a", 80, "80", 86.5, true),
		res("b", 70, "70", 70.0, true),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 1 || plan[0].Suffix != "a" || plan[0].From != 80 || plan[0].To != 86 {
		t.Fatalf("expected a: 80 -> 86, got %+v", plan)
	}
}

// FLOOR, NOT ROUND. Banking 87 against a measurement that displays as 86.9 would
// fail the very next run on unchanged code.
func TestPlanBankFloorsRatherThanRounds(t *testing.T) {
	plan, _ := planBank([]covResult{res("a", 80, "80", 86.9, true)})
	if len(plan) != 1 || plan[0].To != 86 {
		t.Fatalf("86.9%% banks 86, not 87; got %+v", plan)
	}
}

// TIGHTEN-ONLY is the safety property. A tool that can lower a floor is a tool
// for making red go green.
func TestPlanBankNeverLowersAFloor(t *testing.T) {
	plan, err := planBank([]covResult{res("a", 80, "80", 80.0, true)})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 0 {
		t.Errorf("a floor that exactly matches has no slack to bank; got %+v", plan)
	}
}

// Banking a red run would raise the floors that PASSED and leave the failure in
// place, which reads afterwards as a commit that merely updated coverage.
func TestPlanBankRefusesWhileAnythingIsBelowItsFloor(t *testing.T) {
	_, err := planBank([]covResult{
		res("a", 80, "80", 90.0, true),
		res("b", 70, "70", 48.2, true),
	})
	if err == nil {
		t.Fatal("banking must refuse while a package is below its floor")
	}
	if !strings.Contains(err.Error(), "b at 48.2%") {
		t.Errorf("the error should name the failing package, got %v", err)
	}
}

// A package with no coverage data is the renamed-or-removed case the gate exists
// to catch; banking around it would paper over exactly that.
func TestPlanBankRefusesOnMissingData(t *testing.T) {
	if _, err := planBank([]covResult{res("gone", 70, "70", 0, false)}); err == nil {
		t.Fatal("no coverage data must block banking")
	}
}

// ── the rewrite ─────────────────────────────────────────────────────────────

const mk = `COVERAGE_MINS := \
	internal/a=80 \
	internal/b=70 \
	internal/c=60

other: stuff
`

func TestApplyBankRewritesOnlyThePlannedEntries(t *testing.T) {
	out, n := applyBank(mk, []banked{{Suffix: "internal/a", From: 80, To: 86}})
	if n != 1 {
		t.Fatalf("expected one rewrite, got %d", n)
	}
	if !strings.Contains(out, "internal/a=86 \\") {
		t.Error("internal/a should have been raised to 86")
	}
	for _, untouched := range []string{"internal/b=70 \\", "internal/c=60", "other: stuff"} {
		if !strings.Contains(out, untouched) {
			t.Errorf("%q should be untouched", untouched)
		}
	}
}

// The line continuation and indentation are load-bearing in a Makefile: losing
// either turns the block into a syntax error rather than a wrong number.
func TestApplyBankPreservesMakefileLineShape(t *testing.T) {
	out, _ := applyBank(mk, []banked{{Suffix: "internal/a", From: 80, To: 86}})
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, "internal/a=") {
			if !strings.HasPrefix(ln, "\t") || !strings.HasSuffix(ln, " \\") {
				t.Errorf("line shape lost: %q", ln)
			}
		}
	}
}

// Belt and braces on the tighten-only rule: even a plan that somehow asked for a
// lower number must not produce one.
func TestApplyBankRefusesToLowerEvenIfAsked(t *testing.T) {
	out, n := applyBank(mk, []banked{{Suffix: "internal/a", From: 80, To: 70}})
	if n != 0 || !strings.Contains(out, "internal/a=80 \\") {
		t.Error("applyBank must never write a lower floor")
	}
}

// ── end to end ──────────────────────────────────────────────────────────────

// bankTree writes a coverprofile and a Makefile and returns the directory.
func bankTree(t *testing.T, profile, makefile string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	pp := filepath.Join(dir, "cover.out")
	mp := filepath.Join(dir, "Makefile")
	if err := os.WriteFile(pp, []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mp, []byte(makefile), 0o644); err != nil {
		t.Fatal(err)
	}
	return pp, mp
}

// One package at 3/4 statements = 75%, against a floor of 60.
const bankProfile = `mode: set
example.com/x/internal/a/f.go:1.1,2.2 1 1
example.com/x/internal/a/f.go:3.1,4.2 1 1
example.com/x/internal/a/f.go:5.1,6.2 1 1
example.com/x/internal/a/f.go:7.1,8.2 1 0
`

func TestRunBankRaisesTheFloorAndWritesIt(t *testing.T) {
	pp, mp := bankTree(t, bankProfile, "COVERAGE_MINS := \\\n\tinternal/a=60\n")
	var out strings.Builder
	if err := RunBank(pp, mp, []string{"internal/a=60"}, &out); err != nil {
		t.Fatalf("bank should succeed: %v", err)
	}
	got, err := os.ReadFile(mp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "internal/a=75") {
		t.Errorf("the floor should have been raised to 75; got %q", got)
	}
	if !strings.Contains(out.String(), "60 -> 75") {
		t.Errorf("the run should report what it raised; got %q", out.String())
	}
}

func TestRunBankIsANoOpWhenThereIsNoSlack(t *testing.T) {
	pp, mp := bankTree(t, bankProfile, "COVERAGE_MINS := \\\n\tinternal/a=75\n")
	var out strings.Builder
	if err := RunBank(pp, mp, []string{"internal/a=75"}, &out); err != nil {
		t.Fatalf("no slack is success, not failure: %v", err)
	}
	if !strings.Contains(out.String(), "already matches") {
		t.Errorf("expected a no-op message, got %q", out.String())
	}
}

// The block's shape changing must not silently bank nothing: the plan said there
// was slack, so finding no line to edit is a broken assumption, not a clean run.
func TestRunBankFailsWhenNoCoverageMinsEntryMatches(t *testing.T) {
	pp, mp := bankTree(t, bankProfile, "# no COVERAGE_MINS block here at all\n")
	err := RunBank(pp, mp, []string{"internal/a=60"}, &strings.Builder{})
	if err == nil {
		t.Fatal("slack with nothing to rewrite must fail loudly")
	}
	if !strings.Contains(err.Error(), "no COVERAGE_MINS entry matched") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRunBankRequiresAProfile(t *testing.T) {
	if err := RunBank("", "Makefile", []string{"a=1"}, &strings.Builder{}); err == nil {
		t.Fatal("a missing --profile must fail")
	}
}
