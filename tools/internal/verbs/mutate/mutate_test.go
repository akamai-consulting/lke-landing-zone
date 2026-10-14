package mutate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A real gremlins fragment, including the two multi-word statuses and the
// summary block that must NOT be parsed as results.
const gremlinsSample = `Starting...
Gathering coverage... done in 787.15ms
 NOT COVERED CONDITIONALS_NEGATION at apl_app.go:67:9
      KILLED ARITHMETIC_BASE at ci_health.go:148:41
       LIVED CONDITIONALS_NEGATION at merge.go:92:10
   TIMED OUT INCREMENT_DECREMENT at ci.go:979:50
      KILLED CONDITIONALS_BOUNDARY at merge.go:95:51

Mutation testing completed in 1 hour 34 minutes
Killed: 4141, Lived: 456, Not covered: 1694
Timed out: 44, Not viable: 0, Skipped: 0
Test efficacy: 90.08%
Mutator coverage: 73.07%
`

func TestParseGremlins(t *testing.T) {
	r := parseGremlins(gremlinsSample)
	if len(r.Mutants) != 5 {
		t.Fatalf("parsed %d mutants, want 5: %+v", len(r.Mutants), r.Mutants)
	}
	if r.Killed != 2 || r.Lived != 1 || r.TimedOut != 1 || r.NotCovered != 1 {
		t.Errorf("counts killed=%d lived=%d timedout=%d notcovered=%d, want 2/1/1/1",
			r.Killed, r.Lived, r.TimedOut, r.NotCovered)
	}
	// The summary block says Killed: 4141. Counting it as results would be a
	// silent corruption, so the counts must come from the mutant lines only.
	if r.Killed == 4141 {
		t.Error("the summary block was parsed as results")
	}
	got := r.Mutants[3]
	if got.Status != statusTimedOut || got.File != "ci.go" || got.Line != 979 || got.Col != 50 {
		t.Errorf("multi-word status parsed wrong: %+v", got)
	}
}

// Efficacy deliberately excludes timeouts, matching gremlins. This test pins
// that, because it is the arithmetic that made internal/kube report 100% while
// hiding 9 survivors.
func TestEfficacyExcludesTimeouts(t *testing.T) {
	r := mutationRun{Killed: 10, Lived: 0, TimedOut: 9}
	if e := r.Efficacy(); e != 100 {
		t.Errorf("efficacy = %v, want 100 (timeouts are outside the denominator)", e)
	}
	if r2 := (mutationRun{}); r2.Efficacy() != 0 {
		t.Errorf("empty run efficacy = %v, want 0", r2.Efficacy())
	}
	// The denominator must be killed PLUS lived. Mutation testing caught this
	// file's own gap: the cases above both use Lived == 0, where `Killed + Lived`
	// and `Killed - Lived` are the same number, so a flipped operator survived in
	// the tool built to find survivors. A case with survivors present is what
	// distinguishes them — 3/(3+1) = 75%, versus 3/(3-1) = 150%, which is not even
	// a percentage.
	if e := (mutationRun{Killed: 3, Lived: 1}).Efficacy(); e != 75 {
		t.Errorf("efficacy with survivors = %v, want 75 (3 killed of 4 measured)", e)
	}
	if e := (mutationRun{Killed: 1, Lived: 3}).Efficacy(); e != 25 {
		t.Errorf("efficacy = %v, want 25", e)
	}
	// A run that killed nothing is 0%, not a division blow-up.
	if e := (mutationRun{Killed: 0, Lived: 5}).Efficacy(); e != 0 {
		t.Errorf("efficacy with no kills = %v, want 0", e)
	}
}

// survivors() sorts so the report is diffable run to run. The comparator carried
// two mutants; this kills the one that reverses the order. Its sibling
// (`<` -> `<=`) is EQUIVALENT and baselined: gremlins never emits two mutants
// sharing file:line:col:mutator — verified across a real 6,639-mutant run, which
// had zero duplicate keys — so the comparator is never called on a tie.
func TestSurvivorsAreSortedAndFilterAllButLived(t *testing.T) {
	r := mutationRun{Mutants: []mutant{
		{Status: statusLived, Mutator: "Z_MUT", File: "z.go", Line: 9, Col: 1},
		{Status: statusKilled, Mutator: "A_MUT", File: "a.go", Line: 1, Col: 1},
		{Status: statusLived, Mutator: "A_MUT", File: "a.go", Line: 1, Col: 2},
		{Status: statusTimedOut, Mutator: "T_MUT", File: "t.go", Line: 5, Col: 1},
		{Status: statusNotCovered, Mutator: "N_MUT", File: "n.go", Line: 5, Col: 1},
		{Status: statusLived, Mutator: "M_MUT", File: "m.go", Line: 3, Col: 1},
	}}
	got := r.survivors()
	if len(got) != 3 {
		t.Fatalf("survivors() returned %d, want the 3 LIVED only: %+v", len(got), got)
	}
	for _, m := range got {
		if m.Status != statusLived {
			t.Errorf("non-survivor leaked in: %+v", m)
		}
	}
	want := []string{"a.go:1:2:A_MUT", "m.go:3:1:M_MUT", "z.go:9:1:Z_MUT"}
	for i, w := range want {
		if got[i].key() != w {
			t.Errorf("survivors()[%d] = %q, want %q — ascending by key, so a report diffs cleanly between runs",
				i, got[i].key(), w)
		}
	}
}

func liveCanary() *canary {
	return &canary{File: "c.go", Line: 10, Mutator: "ARITHMETIC_BASE", Why: "size hint"}
}

func TestValidate(t *testing.T) {
	ok := mutationRun{
		Mutants: []mutant{
			{Status: statusLived, Mutator: "ARITHMETIC_BASE", File: "c.go", Line: 10, Col: 5},
			{Status: statusKilled, Mutator: "CONDITIONALS_NEGATION", File: "c.go", Line: 20, Col: 3},
		},
		Killed: 1, Lived: 1,
	}
	if err := validateRun(ok, true, liveCanary()); err != nil {
		t.Fatalf("a healthy run must validate, got: %v", err)
	}

	for _, tc := range []struct {
		name    string
		run     mutationRun
		control bool
		can     *canary
		want    string
	}{
		{
			// The failure that produced "4998 killed in 12 seconds".
			name: "canary reported KILLED means tests never ran",
			run: mutationRun{Mutants: []mutant{
				{Status: statusKilled, Mutator: "ARITHMETIC_BASE", File: "c.go", Line: 10},
			}, Killed: 1},
			control: true, can: liveCanary(), want: "CANARY",
		},
		{
			name: "canary absent is not a pass",
			run: mutationRun{Mutants: []mutant{
				{Status: statusKilled, Mutator: "CONDITIONALS_NEGATION", File: "z.go", Line: 1},
			}, Killed: 1},
			control: true, can: liveCanary(), want: "not reported at all",
		},
		{
			// 100% efficacy alongside timeouts — the shrunken denominator.
			name:    "timeouts are unmeasured, not passes",
			run:     mutationRun{Mutants: []mutant{{Status: statusKilled, File: "c.go"}}, Killed: 1, TimedOut: 3},
			control: true, can: nil, want: "TIMED OUT",
		},
		{
			name:    "color.Red control invalidates everything",
			run:     ok,
			control: false, can: liveCanary(), want: "CONTROL failed",
		},
		{
			name:    "no mutants at all",
			run:     mutationRun{},
			control: true, can: nil, want: "no mutants",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRun(tc.run, tc.control, tc.can)
			if err == nil {
				t.Fatalf("want a validation error mentioning %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error should mention %q, got: %v", tc.want, err)
			}
		})
	}
}

func TestNewSurvivorsDiffsAgainstBaseline(t *testing.T) {
	r := mutationRun{Mutants: []mutant{
		{Status: statusLived, Mutator: "ARITHMETIC_BASE", File: "a.go", Line: 1, Col: 2},
		{Status: statusLived, Mutator: "CONDITIONALS_BOUNDARY", File: "b.go", Line: 3, Col: 4},
		{Status: statusKilled, Mutator: "CONDITIONALS_NEGATION", File: "c.go", Line: 5, Col: 6},
	}, Lived: 2, Killed: 1}

	accepted := map[string]string{"a.go:1:2:ARITHMETIC_BASE": "map size hint"}
	fresh := newSurvivors(r, accepted)
	if len(fresh) != 1 || fresh[0].File != "b.go" {
		t.Fatalf("want only b.go as new, got %+v", fresh)
	}
	// A killed mutant is never a survivor, baseline or not.
	for _, m := range fresh {
		if m.Status != statusLived {
			t.Errorf("non-survivor leaked into the diff: %+v", m)
		}
	}
	if got := newSurvivors(r, nil); len(got) != 2 {
		t.Errorf("with no baseline every survivor is new, got %d", len(got))
	}
}

func TestLoadBaseline(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "b.json")
	if err := os.WriteFile(p, []byte(`{"accepted":[
	  {"file":"ci_rotate_dbadmin.go","line":262,"col":58,"mutator":"ARITHMETIC_BASE","why":"map size hint"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := loadBaseline(p)
	if err != nil {
		t.Fatalf("loadBaseline: %v", err)
	}
	if got["ci_rotate_dbadmin.go:262:58:ARITHMETIC_BASE"] != "map size hint" {
		t.Errorf("baseline key missing or wrong: %+v", got)
	}
	if m, err := loadBaseline(""); m != nil || err != nil {
		t.Errorf("an empty path means no baseline, got %v %v", m, err)
	}
	if _, err := loadBaseline(filepath.Join(dir, "nope.json")); err == nil {
		t.Error("a missing baseline file must be an error, not a silent empty set")
	}
}

func TestTestsReachOutsideModule(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "self_test.go"), []byte("package p\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if hit, err := testsReachOutsideModule(dir); err != nil || hit {
		t.Fatalf("self-contained tests: hit=%v err=%v, want false/nil", hit, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "outside_test.go"),
		[]byte("package p\nconst root = \"../../../platform-apl\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hit, err := testsReachOutsideModule(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !hit {
		t.Error("a test referencing ../../.. must be flagged — such a package cannot be measured in gremlins' isolated module copy without staging")
	}
}

// The canary registry is only worth having if every entry explains why the
// mutation cannot matter. An unjustified entry is a rubber stamp.
func TestCanariesAreJustified(t *testing.T) {
	for pkg, c := range canaries {
		if c.File == "" || c.Line == 0 || c.Mutator == "" {
			t.Errorf("%s: canary is incomplete: %+v", pkg, c)
		}
		if len(c.Why) < 40 {
			t.Errorf("%s: canary needs a real justification, got %q", pkg, c.Why)
		}
	}
}

// ── ciMutateCmd's RunE ────────────────────────────────────────────────────────
//
// The pure helpers above were at 100% while RunE — where CONTROL, CANARY and the
// baseline diff are actually ASSEMBLED — sat at 21%. That is the wrong way round
// for a command whose whole purpose is refusing to report an unvalidated score:
// the assembly is the part that can silently stop checking.

// stubMutateRun makes execOutput answer both calls RunE makes: the CONTROL
// `go test`, and gremlins itself. gremlinsOut is returned verbatim as the
// gremlins stdout.
func stubMutateRun(t *testing.T, controlErr error, gremlinsOut string) *[]string {
	t.Helper()
	var calls []string
	withExecOutput(t, func(name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if name == "go" {
			return nil, controlErr
		}
		return []byte(gremlinsOut), nil
	})
	return &calls
}

// A gremlins run whose canary comes back LIVED and whose only other survivor is
// in the baseline: the one shape that should report a score and exit 0.
const mutateHealthyOut = `Starting...
      KILLED CONDITIONALS_NEGATION at a.go:1:1
       LIVED ARITHMETIC_BASE at ci_rotate_dbadmin.go:262:58
`

// runMutateCmd drives Run directly rather than through a cobra tree.
//
// The command used to be the only entry point, so the test built it and called
// Execute with argv. Lifting the ninety-seven-line RunE into Run(Opts) made the
// flag parsing package main's business and left this test asserting on the verb
// itself — which is what it was always about. The `--package` argv form is kept so
// the cases read the same as the command an operator types.
func runMutateCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var o Opts
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--package":
			i++
			if i < len(args) {
				o.Pkg = args[i]
			}
		case "--baseline":
			i++
			if i < len(args) {
				o.BaselinePath = args[i]
			}
		case "--out":
			i++
			if i < len(args) {
				o.OutPath = args[i]
			}
		}
	}
	var buf strings.Builder
	o.Out = &buf
	err := Run(o)
	return buf.String(), err
}

func TestMutateCmdRequiresAPackage(t *testing.T) {
	if _, err := runMutateCmd(t); err == nil {
		t.Fatal("--package is required; without it the command has nothing to measure")
	}
}

func TestMutateCmdReportsAScoreWhenTheRunValidates(t *testing.T) {
	calls := stubMutateRun(t, nil, mutateHealthyOut)
	out, err := runMutateCmd(t, "--package", "./cmd/llz",
		"--baseline", filepath.Join("testdata", "mutation-baseline.json"))
	if err != nil {
		t.Fatalf("a validating run with no NEW survivors must exit 0, got %v\n%s", err, out)
	}
	if !strings.Contains(out, "efficacy=") {
		t.Errorf("a validated run must report the score:\n%s", out)
	}
	// The canary survivor is in the baseline, so it is not reported as new.
	if strings.Contains(out, "NEW ") {
		t.Errorf("a baselined survivor must not be reported as new:\n%s", out)
	}
	// CONTROL must actually run, and before gremlins — a score computed without
	// it is the "color.Red suite kills every mutant" failure.
	if len(*calls) < 2 || !strings.HasPrefix((*calls)[0], "go test") {
		t.Errorf("CONTROL `go test` must run first, calls were: %v", *calls)
	}
	if !strings.Contains((*calls)[1], "gremlins") {
		t.Errorf("gremlins must run after the control, calls were: %v", *calls)
	}
}

func TestMutateCmdRefusesToScoreAnUntrustworthyRun(t *testing.T) {
	for _, tc := range []struct {
		name       string
		controlErr error
		out        string
		want       string
	}{
		{
			name:       "color.Red control",
			controlErr: errors.New("suite failed"),
			out:        mutateHealthyOut,
			want:       "CONTROL failed",
		},
		{
			// The failure that produced "4998 killed in 12 seconds".
			name: "canary killed means tests never ran",
			out: "      KILLED ARITHMETIC_BASE at ci_rotate_dbadmin.go:262:58\n" +
				"      KILLED CONDITIONALS_NEGATION at a.go:1:1\n",
			want: "CANARY",
		},
		{
			name: "timeouts are unmeasured",
			out:  mutateHealthyOut + "   TIMED OUT INCREMENT_DECREMENT at b.go:2:2\n",
			want: "TIMED OUT",
		},
		{
			name: "gremlins reported nothing at all",
			out:  "Starting...\nno mutants here\n",
			want: "no mutants",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubMutateRun(t, tc.controlErr, tc.out)
			out, err := runMutateCmd(t, "--package", "./cmd/llz")
			if err == nil {
				t.Fatalf("an untrustworthy run must NOT exit 0:\n%s", out)
			}
			if !strings.Contains(out, "harness is not trustworthy") {
				t.Errorf("the operator must be told the harness is at fault, not shown a score:\n%s", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("reason %q missing from:\n%s", tc.want, out)
			}
			if strings.Contains(out, "efficacy=") {
				t.Errorf("NO score may be printed for an unvalidated run:\n%s", out)
			}
		})
	}
}

func TestMutateCmdFailsOnASurvivorOutsideTheBaseline(t *testing.T) {
	stubMutateRun(t, nil, mutateHealthyOut+"       LIVED CONDITIONALS_BOUNDARY at brandnew.go:7:3\n")
	out, err := runMutateCmd(t, "--package", "./cmd/llz",
		"--baseline", filepath.Join("testdata", "mutation-baseline.json"))
	if err == nil {
		t.Fatalf("a survivor outside the baseline is the actionable event and must fail:\n%s", out)
	}
	if !strings.Contains(out, "NEW ") || !strings.Contains(out, "brandnew.go") {
		t.Errorf("the new survivor must be named:\n%s", out)
	}
	// The baselined canary must still not be reported.
	if strings.Contains(out, "ci_rotate_dbadmin.go") {
		t.Errorf("a baselined survivor leaked into the new list:\n%s", out)
	}
}
