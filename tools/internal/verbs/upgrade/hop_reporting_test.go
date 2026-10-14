package upgrade

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A harness failure and a check failure are DIFFERENT VERDICTS. "the upgrade is
// broken" and "that hop could not be measured" send an operator to different
// places, and a run that produced both has to say both — the loop used to return
// on the first harness error, throwing away every check failure the earlier hops
// had already found.
func TestUpgradeTestFailureReportsChecksAndHarnessSeparately(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failures  []string
		harness   []string
		wantIn    []string
		wantNotIn []string
		wantNil   bool
	}{
		{
			name:    "neither — a helper that invents a failure is a trap for the next caller",
			wantNil: true,
		},
		{
			name:      "checks only",
			failures:  []string{"converges-with-fresh [from v0.0.42]: ..."},
			wantIn:    []string{"1 check(s) failed", "day-2 path"},
			wantNotIn: []string{"could not be measured"},
		},
		{
			name:      "harness only — must not read as a pass",
			harness:   []string{"from v0.0.40: scaffold failed"},
			wantIn:    []string{"1 hop(s) could not be measured", "reached no verdict"},
			wantNotIn: []string{"check(s) failed"},
		},
		{
			name:     "both — neither may be swallowed",
			failures: []string{"pin-advanced [from v0.0.42]: ..."},
			harness:  []string{"from v0.0.40: scaffold failed"},
			wantIn:   []string{"1 check(s) failed", "a further 1 hop(s) could not be measured"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := UpgradeTestFailure(tc.failures, tc.harness)
			if tc.wantNil {
				if err != nil {
					t.Fatalf("nothing went wrong, but it returned: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("no error — a run with failures or unmeasured hops must not exit 0")
			}
			for _, want := range tc.wantIn {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not mention %q:\n  %s", want, err.Error())
				}
			}
			for _, no := range tc.wantNotIn {
				if strings.Contains(err.Error(), no) {
					t.Errorf("error should not mention %q:\n  %s", no, err.Error())
				}
			}
		})
	}
}

// THE REGRESSION. At depth the report interleaves three releases, and four of the
// five checks rendered identically for each — three lines saying "answers-preserved:"
// with nothing saying which upgrade produced them. Only converges-with-fresh named
// its hop.
//
// Asserted against the SOURCE rather than by running the checks: each one fires
// only on a genuinely broken upgrade, so reaching all four behaviourally would
// mean standing up four deliberately-corrupted scaffolds and a copier run apiece.
// The thing being pinned is a property of the call sites, and that is what this
// reads.
func TestEveryCheckFailureNamesItsHop(t *testing.T) {
	// ENUMERATED, NOT LISTED. A hardcoded list of today's checks goes green the day a
	// sixth is added unlabeled, which is the regression itself. A check failure is
	// written as a literal opening `"<check-name>...`, so this compares the two
	// spellings that literal can take: labelled, and bare. Any new check shows up on
	// whichever side it was written on, with no list to maintain.
	src := readSource(t, "cobra_upgrade_test_gate.go")
	labelled := uniqueMatches(regexp.MustCompile(`"([a-z][a-z0-9-]+) \[from `), src)
	bare := uniqueMatches(regexp.MustCompile(`"([a-z][a-z0-9-]+): `), src)

	if len(labelled) < 4 {
		t.Fatalf("only %d labelled check(s) found (%v) — the scan stopped matching the code it polices",
			len(labelled), labelled)
	}
	// `upgrade-test:` is the run summary's own prefix, not a per-hop check. It is
	// the ONE bare name allowed, and naming it here rather than skipping all bare
	// names is what keeps the test able to fail.
	for _, name := range bare {
		if name != "upgrade-test" {
			t.Errorf("check %q is reported without naming its hop — at --depth 3 its failures are\n"+
				"indistinguishable between releases. Write it as %q.", name, name+" [from <ref>]: ")
		}
	}
	// converges-with-fresh names its hop through FormatConvergenceGaps instead, so
	// it is asserted where it actually happens.
	if !strings.Contains(FormatConvergenceGaps("v0.0.41", []ConvergenceGap{{Kind: "stale", Class: "managed", Path: "AGENTS.md"}}), "v0.0.41") {
		t.Error("converges-with-fresh stopped naming its hop")
	}
}

func readSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func uniqueMatches(re *regexp.Regexp, src string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range re.FindAllStringSubmatch(src, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out
}

// THE MISCLASSIFICATION. assertTasksRan runs twice: once on the fresh scaffold
// before the loop, where a failure genuinely IS a harness problem ("this machine
// cannot render"), and once on each upgraded instance, where by definition the
// first call already succeeded — so a failure there means the UPGRADE did not
// deliver. Reporting the second as a harness error summarised a real regression,
// of exactly the class this branch exists to fix, as "could not be measured", and
// pointed the operator at their own --llz binary.
//
// Pinned by name because it pins one specific past mistake: the check has to be
// spelled as a per-hop CHECK failure, which is what puts it in the labelled set.
func TestPostUpgradeTaskDeliveryIsACheckNotAHarnessError(t *testing.T) {
	src := readSource(t, "cobra_upgrade_test_gate.go")
	labelled := uniqueMatches(regexp.MustCompile(`"([a-z][a-z0-9-]+) \[from `), src)
	found := false
	for _, n := range labelled {
		if n == "tasks-delivered" {
			found = true
		}
	}
	if !found {
		t.Errorf("no `tasks-delivered` CHECK among %v — a degraded render in an upgraded instance is\n"+
			"a finding about the upgrade, not a harness failure. Reverting it to `return failures, err`\n"+
			"reports the regression this branch fixes as \"could not be measured\".", labelled)
	}
	// And it must not also be returned as a harness error from that site.
	if regexp.MustCompile(`assertTasksRan\(o\.root, inst\); err != nil \{\s*\n\s*return failures, err`).MatchString(src) {
		t.Error("the post-upgrade assertTasksRan still returns a harness error")
	}
}
