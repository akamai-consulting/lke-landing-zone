package mutate

// ci_mutate.go implements `llz ci mutate` — a mutation-testing run over a Go
// package that CHECKS ITSELF before it reports a score.
//
// Why it exists: every failure mode we hit running gremlins by hand surfaced as
// a FLATTERING NUMBER, never as an error. Three separate bugs each produced a
// confident "100% efficacy":
//
//   1. `package main` — gremlins never spawns a test process for a main package
//      and marks every mutant KILLED. cmd/llz reported 4998 killed in 12 SECONDS
//      against an 11.5s suite. Fixed by --integration.
//   2. Module-copy isolation — gremlins copies the module to <tmp>/gremlins-N/wd-M
//      and runs `go test -failfast ./...` there. cmd/llz's tests reach OUTSIDE the
//      module (../../../platform-apl), which does not exist in the copy, so the
//      first such test dies, -failfast aborts, and every mutant is "killed".
//      Fixed by staging the repo root around the workdir.
//   3. Timeouts — gremlins derives the per-mutant timeout from the coverage-run
//      elapsed time. On a fast package that budget is under the compile step, so
//      mutants report TIMED OUT. Timeouts sit outside both KILLED and LIVED, so
//      efficacy (killed / (killed+lived)) is computed over a shrunken denominator
//      and reads high. internal/kube reported 100% while hiding 9 real survivors.
//
// The score is the one thing you cannot use to detect any of these. So this
// command refuses to print a score until three independent checks pass:
//
//   CONTROL  the suite is color.Green on unmutated source. A color.Red suite makes every
//            mutant trivially "killed".
//   CANARY   a mutant that CANNOT be killed must be reported LIVED. If the run
//            says KILLED, the harness is not really running tests. See canary.
//   TIMEOUTS a non-zero timeout count means unmeasured mutants; 100% efficacy
//            alongside timeouts is the signature of a shrunken denominator.
//
// Only then does it diff survivors against a baseline. The absolute score is not
// actionable — equivalent mutants cap it below 100% by construction — but "a
// survivor appeared that is not in the baseline" is.
//
// The parsing, validation and baseline diff are pure and unit-tested; gremlins is
// reached only through the execOutput seam.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// mutantStatus is gremlins' verdict for one mutation.
type mutantStatus string

const (
	statusKilled     mutantStatus = "KILLED"
	statusLived      mutantStatus = "LIVED"
	statusTimedOut   mutantStatus = "TIMED OUT"
	statusNotCovered mutantStatus = "NOT COVERED"
)

// mutant is one reported mutation: its verdict, operator and position.
type mutant struct {
	Status  mutantStatus `json:"status"`
	Mutator string       `json:"mutator"`
	File    string       `json:"file"`
	Line    int          `json:"line"`
	Col     int          `json:"col"`
}

func (m mutant) key() string { return fmt.Sprintf("%s:%d:%d:%s", m.File, m.Line, m.Col, m.Mutator) }

// mutationRun is everything a run reports, already counted.
type mutationRun struct {
	Mutants    []mutant `json:"mutants"`
	Killed     int      `json:"killed"`
	Lived      int      `json:"lived"`
	TimedOut   int      `json:"timedOut"`
	NotCovered int      `json:"notCovered"`
}

// Efficacy is killed / (killed + lived) as a percentage. NOTE the denominator
// deliberately excludes timeouts, matching gremlins — which is exactly why a
// non-zero timeout count makes this number untrustworthy on its own, and why
// validate() treats timeouts as a first-class finding rather than a footnote.
func (r mutationRun) Efficacy() float64 {
	d := r.Killed + r.Lived
	if d == 0 {
		return 0
	}
	return 100 * float64(r.Killed) / float64(d)
}

func (r mutationRun) survivors() []mutant {
	var out []mutant
	for _, m := range r.Mutants {
		if m.Status == statusLived {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out
}

// gremlinsLine matches one result line, e.g.
//
//	"       LIVED CONDITIONALS_NEGATION at merge.go:92:10"
//	" NOT COVERED ARITHMETIC_BASE at apl_app.go:70:24"
//
// The status may contain a space ("NOT COVERED", "TIMED OUT"), so it is matched
// as an alternation rather than \S+.
var gremlinsLine = regexp.MustCompile(`^\s*(KILLED|LIVED|TIMED OUT|NOT COVERED)\s+(\S+)\s+at\s+(\S+?):(\d+):(\d+)\s*$`)

// parseGremlins turns gremlins' stdout into a counted run. Lines it does not
// recognise (banners, the summary block) are ignored: the counts are derived
// from the mutant lines themselves rather than from gremlins' own summary, so a
// mismatch between the two cannot silently pass through.
func parseGremlins(out string) mutationRun {
	var r mutationRun
	for _, ln := range strings.Split(out, "\n") {
		m := gremlinsLine.FindStringSubmatch(ln)
		if m == nil {
			continue
		}
		line, _ := strconv.Atoi(m[4])
		col, _ := strconv.Atoi(m[5])
		mu := mutant{Status: mutantStatus(m[1]), Mutator: m[2], File: m[3], Line: line, Col: col}
		r.Mutants = append(r.Mutants, mu)
		switch mu.Status {
		case statusKilled:
			r.Killed++
		case statusLived:
			r.Lived++
		case statusTimedOut:
			r.TimedOut++
		case statusNotCovered:
			r.NotCovered++
		}
	}
	return r
}

// canary names a mutant that is EQUIVALENT — provably incapable of changing
// observable behaviour — and therefore MUST come back LIVED from a working
// harness. It is the only check that catches a harness which is not running
// tests at all, because that failure mode reports every mutant as KILLED.
type canary struct {
	File    string
	Line    int
	Mutator string
	Why     string
}

// canaries are per-package. Each entry must be justified: a reader has to be able
// to confirm the mutation cannot matter, or the check is worthless.
var canaries = map[string]canary{
	"cmd/llz": {
		File:    "ci_rotate_dbadmin.go",
		Line:    262,
		Mutator: "ARITHMETIC_BASE",
		Why: "make(map[string]string, len(dbAdminCarriedFields)+3) — a MAP size hint. " +
			"Unlike a slice capacity it is not observable through cap() or any other API, " +
			"and len(dbAdminCarriedFields)==4 so the mutant computes 1, which is positive " +
			"and cannot panic. Verified by hand: applied, suite still color.Green.",
	},
}

// validationError is a harness fault: the run cannot be trusted, so no score is
// reported. Distinct from "there are new survivors", which is a real finding.
type validationError struct{ reasons []string }

func (e *validationError) Error() string { return strings.Join(e.reasons, "; ") }

// validateRun applies the three checks that must pass before a score means anything.
// controlGreen is whether `go test` passed on unmutated source.
func validateRun(r mutationRun, controlGreen bool, c *canary) error {
	var bad []string
	if !controlGreen {
		bad = append(bad, "CONTROL failed: the suite is color.Red on unmutated source, so every mutant is trivially killed")
	}
	if len(r.Mutants) == 0 {
		bad = append(bad, "no mutants were reported at all — gremlins found nothing to run")
	}
	if c != nil {
		found := false
		for _, m := range r.Mutants {
			if m.File == c.File && m.Line == c.Line && m.Mutator == c.Mutator {
				found = true
				if m.Status != statusLived {
					bad = append(bad, fmt.Sprintf(
						"CANARY %s:%d %s reported %s, want LIVED — the harness is not running tests. %s",
						c.File, c.Line, c.Mutator, m.Status, c.Why))
				}
			}
		}
		if !found {
			bad = append(bad, fmt.Sprintf(
				"CANARY %s:%d %s was not reported at all — it may have moved; re-verify it by hand before trusting this run",
				c.File, c.Line, c.Mutator))
		}
	}
	if r.TimedOut > 0 {
		bad = append(bad, fmt.Sprintf(
			"%d mutant(s) TIMED OUT: unmeasured, NOT passes. They are excluded from the efficacy denominator, "+
				"so the score reads higher than the evidence supports. Raise --timeout-coefficient, or classify "+
				"them (a loop-counter mutant is non-terminating by construction and belongs in the baseline)", r.TimedOut))
	}
	if len(bad) > 0 {
		return &validationError{reasons: bad}
	}
	return nil
}

// baseline is the checked-in record of survivors that are known and accepted.
// The absolute efficacy score is not a useful gate — equivalent mutants cap it
// below 100% — so the gate is "no survivor outside this set".
type baseline struct {
	Accepted []struct {
		File    string `json:"file"`
		Line    int    `json:"line"`
		Col     int    `json:"col"`
		Mutator string `json:"mutator"`
		Why     string `json:"why"`
	} `json:"accepted"`
}

func loadBaseline(path string) (map[string]string, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading baseline %s: %w", path, err)
	}
	var bl baseline
	if err := json.Unmarshal(b, &bl); err != nil {
		return nil, fmt.Errorf("parsing baseline %s: %w", path, err)
	}
	out := map[string]string{}
	for _, a := range bl.Accepted {
		out[fmt.Sprintf("%s:%d:%d:%s", a.File, a.Line, a.Col, a.Mutator)] = a.Why
	}
	return out, nil
}

// newSurvivors returns the survivors not present in the baseline. These are the
// actionable output: a survivor that appeared since the baseline was written.
func newSurvivors(r mutationRun, accepted map[string]string) []mutant {
	var out []mutant
	for _, m := range r.survivors() {
		if _, ok := accepted[m.key()]; !ok {
			out = append(out, m)
		}
	}
	return out
}

// testsReachOutsideModule reports whether any _test.go file under dir refers to a
// path that climbs above the module root. Such a package CANNOT be measured in
// gremlins' isolated module copy without the repo root being staged around it —
// the tests die on a missing directory and -failfast turns every mutant into a
// false KILLED.
func testsReachOutsideModule(dir string) (bool, error) {
	hit := false
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, "_test.go") {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), "../../..") {
			hit = true
		}
		return nil
	})
	return hit, err
}

// execOutput captures a command's stdout — `go test` for the control run and
// `gremlins` for the mutation run.
//
// A PACKAGE VAR, NOT A Deps FIELD, and the difference matters here. This package
// has exactly one capability and its tests already drive it by swapping the seam;
// a one-field Deps struct threaded through two call sites would be ceremony around
// the same thing. internal/argodiag's diagStream made the same call for the same
// reason. When a package needs two or more capabilities, Deps earns its keep.
var execOutput = func(name string, args ...string) ([]byte, error) {
	out, err := exec.Command(name, args...).Output()
	return out, err
}
