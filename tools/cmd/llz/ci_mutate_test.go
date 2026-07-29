package main

import (
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
			name:    "red control invalidates everything",
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
