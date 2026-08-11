package releasepublish

// pr_gates_test.go — the decision logic of `llz ci assert-instance-pr-gates`.
//
// THE POINT OF THE PORT WAS TESTABILITY, so these drive the three verdicts the
// inline bash could not be tested for and got one of WRONG: both gates ran and
// passed, a gate never appeared, and a gate ran and FAILED. The bash collapsed the
// last two into "never ran" via `|| echo '[]'` on a `gh pr checks` exit status that
// is a verdict rather than an error — TestFailingChecksAreNotReportedAsMissing is
// that regression, pinned.

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fakeForge stands in for gh + git + the clock. Every seam this package owns is
// swapped, so nothing here reaches a network, a clone or a real sleep.
type fakeForge struct {
	t *testing.T
	// checkPolls are the successive `gh pr checks` payloads; the last repeats.
	checkPolls []string
	// ghCalls records every gh argv, so a test can assert cleanup happened.
	ghCalls [][]string
	// gitCalls records every git argv.
	gitCalls [][]string
	// prCreateErr makes `gh pr create` fail, exercising the pr-view fallback.
	prCreateErr error
	// prViewOut is what the fallback `gh pr view` returns.
	prViewOut string
	// checksErr is returned alongside every checks payload — gh's non-zero exit.
	checksErr error
	polls     int
	slept     int
}

// install swaps the package seams and returns a restore func.
func (f *fakeForge) install() func() {
	origGH, origGit, origSleep := gatesGH, gatesGit, gatesSleep
	origTemp, origAppend, origRemove := gatesTempDir, gatesAppend, gatesRemoveAll

	gatesGH = func(_ string, args ...string) ([]byte, error) {
		f.ghCalls = append(f.ghCalls, args)
		switch {
		case len(args) >= 2 && args[0] == "pr" && args[1] == "create":
			if f.prCreateErr != nil {
				return nil, f.prCreateErr
			}
			return []byte("https://github.com/o/r/pull/7\n"), nil
		case len(args) >= 2 && args[0] == "pr" && args[1] == "view":
			return []byte(f.prViewOut), nil
		case len(args) >= 2 && args[0] == "pr" && args[1] == "checks":
			i := f.polls
			if i >= len(f.checkPolls) {
				i = len(f.checkPolls) - 1
			}
			f.polls++
			return []byte(f.checkPolls[i]), f.checksErr
		}
		return nil, nil
	}
	gatesGit = func(_ string, args ...string) ([]byte, error) {
		f.gitCalls = append(f.gitCalls, args)
		return nil, nil
	}
	gatesSleep = func(time.Duration) { f.slept++ }
	gatesTempDir = func() (string, error) { return f.t.TempDir(), nil }
	gatesAppend = func(string, string) error { return nil }
	gatesRemoveAll = func(string) error { return nil }

	return func() {
		gatesGH, gatesGit, gatesSleep = origGH, origGit, origSleep
		gatesTempDir, gatesAppend, gatesRemoveAll = origTemp, origAppend, origRemove
	}
}

func (f *fakeForge) sawGH(sub ...string) bool {
	for _, c := range f.ghCalls {
		if len(c) < len(sub) {
			continue
		}
		match := true
		for i, s := range sub {
			if c[i] != s {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func gatesBaseOpts() PRGatesOpts {
	return PRGatesOpts{
		Instance: "o/r", SHA: "abcdef1234567890", Token: "tok",
		Interval: time.Millisecond, Retries: 3,
	}
}

const bothPass = `[{"name":"Terraform Lint","state":"SUCCESS"},{"name":"Checkov IaC Security Scan","state":"SUCCESS"}]`

func TestBothGatesRanAndPassed(t *testing.T) {
	f := &fakeForge{t: t, checkPolls: []string{bothPass}}
	defer f.install()()

	if err := RunAssertInstancePRGates(gatesBaseOpts()); err != nil {
		t.Fatalf("both gates SUCCESS should pass, got: %v", err)
	}
	if !f.sawGH("pr", "close") {
		t.Error("the throwaway PR was not closed — a passing run must not leak a branch")
	}
}

// TestFailingChecksAreNotReportedAsMissing is the bash bug, pinned. `gh pr checks`
// exits 1 when a check has failed; the inline version's `|| echo '[]'` turned that
// into an empty set and reported "the gates never ran" — sending an operator to the
// paths: filter instead of to the broken job.
func TestFailingChecksAreNotReportedAsMissing(t *testing.T) {
	f := &fakeForge{
		t: t,
		checkPolls: []string{
			`[{"name":"Terraform Lint","state":"FAILURE"},{"name":"Checkov IaC Security Scan","state":"SUCCESS"}]`,
		},
		checksErr: errors.New("exit status 1"), // gh's verdict, not an I/O error
	}
	defer f.install()()

	err := RunAssertInstancePRGates(gatesBaseOpts())
	if err == nil {
		t.Fatal("a FAILURE check must fail the verb")
	}
	if strings.Contains(err.Error(), "never ran") {
		t.Errorf("a gate that RAN AND FAILED was reported as never having run — "+
			"the exit status was treated as an I/O error again: %v", err)
	}
	if !strings.Contains(err.Error(), "Terraform Lint=FAILURE") {
		t.Errorf("the failing gate and its state should be named, got: %v", err)
	}
}

func TestMissingGateNamesWhatDidAppear(t *testing.T) {
	f := &fakeForge{t: t, checkPolls: []string{`[{"name":"Plan Cluster","state":"SUCCESS"}]`}}
	defer f.install()()

	err := RunAssertInstancePRGates(gatesBaseOpts())
	if err == nil {
		t.Fatal("absent gates must fail closed — 'not found' is the failure this verb exists to catch")
	}
	if !strings.Contains(err.Error(), "never ran") {
		t.Errorf("expected the never-ran diagnosis, got: %v", err)
	}
	// Naming what DID appear is what separates "the paths: filter stopped covering
	// this path" from "the job was renamed".
	if !strings.Contains(err.Error(), "Plan Cluster") {
		t.Errorf("the diagnostic should list the checks that did appear, got: %v", err)
	}
	if !strings.Contains(err.Error(), DefaultPRGateTouchPath) {
		t.Errorf("the diagnostic should name the touched path, got: %v", err)
	}
}

func TestWaitsForPendingThenSucceeds(t *testing.T) {
	f := &fakeForge{t: t, checkPolls: []string{
		`[{"name":"Terraform Lint","state":"IN_PROGRESS"},{"name":"Checkov IaC Security Scan","state":"QUEUED"}]`,
		bothPass,
	}}
	defer f.install()()

	if err := RunAssertInstancePRGates(gatesBaseOpts()); err != nil {
		t.Fatalf("a settling run should pass once both reach SUCCESS: %v", err)
	}
	if f.slept == 0 {
		t.Error("the poll loop never slept — it is spinning on the forge")
	}
}

// A run whose checks never settle must not be reported as clean. The loop returns
// its LAST observation rather than an error, so this pins that the caller still
// fails.
func TestPendingForeverFails(t *testing.T) {
	f := &fakeForge{t: t, checkPolls: []string{
		`[{"name":"Terraform Lint","state":"IN_PROGRESS"},{"name":"Checkov IaC Security Scan","state":"IN_PROGRESS"}]`,
	}}
	defer f.install()()

	if err := RunAssertInstancePRGates(gatesBaseOpts()); err == nil {
		t.Fatal("checks pending past the budget must fail, not pass vacuously")
	}
}

func TestKeepLeavesThePROpen(t *testing.T) {
	f := &fakeForge{t: t, checkPolls: []string{bothPass}}
	defer f.install()()

	o := gatesBaseOpts()
	o.Keep = true
	if err := RunAssertInstancePRGates(o); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if f.sawGH("pr", "close") {
		t.Error("--keep closed the PR anyway, so a failure cannot be inspected by hand")
	}
}

// A re-run of the same commit finds its branch and PR already there. That is
// resumable, not fatal.
func TestExistingPRIsReused(t *testing.T) {
	f := &fakeForge{
		t: t, checkPolls: []string{bothPass},
		prCreateErr: errors.New("a pull request already exists"),
		prViewOut:   "7\n",
	}
	defer f.install()()

	if err := RunAssertInstancePRGates(gatesBaseOpts()); err != nil {
		t.Fatalf("an already-open PR should be reused, got: %v", err)
	}
}

func TestRequiredFlags(t *testing.T) {
	defer (&fakeForge{t: t, checkPolls: []string{bothPass}}).install()()

	for _, tc := range []struct {
		name, want string
		mut        func(*PRGatesOpts)
	}{
		{"no instance", "--instance", func(o *PRGatesOpts) { o.Instance = "" }},
		{"no sha", "--sha", func(o *PRGatesOpts) { o.SHA = "" }},
		{"no token", "GH_TOKEN", func(o *PRGatesOpts) { o.Token = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := gatesBaseOpts()
			tc.mut(&o)
			err := RunAssertInstancePRGates(o)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("expected a %s requirement error, got: %v", tc.want, err)
			}
		})
	}
}

// The touched file must be the one under the paths: filter — touching anything else
// opens a PR that triggers nothing and the verb would report a phantom regression.
func TestTouchesThePathUnderTheFilter(t *testing.T) {
	f := &fakeForge{t: t, checkPolls: []string{bothPass}}
	defer f.install()()

	var touched string
	orig := gatesAppend
	gatesAppend = func(path, _ string) error { touched = path; return nil }
	defer func() { gatesAppend = orig }()

	if err := RunAssertInstancePRGates(gatesBaseOpts()); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if !strings.HasSuffix(touched, DefaultPRGateTouchPath) {
		t.Errorf("touched %q, want a path ending in %q", touched, DefaultPRGateTouchPath)
	}
}

func TestBranchIsNamedForTheCommit(t *testing.T) {
	if got, want := prBranch("abcdef1234567890"), "e2e/ci-gates-abcdef12"; got != want {
		t.Errorf("prBranch = %q, want %q", got, want)
	}
	// A short sha must not panic the slice.
	if got := prBranch("abc"); got != "e2e/ci-gates-abc" {
		t.Errorf("short sha: got %q", got)
	}
}

func TestPartitionChecksMatchesNamesExactly(t *testing.T) {
	raw := []byte(`[{"name":"Terraform Lint","state":"SUCCESS"},{"name":"Terraform Lint (extra)","state":"FAILURE"}]`)
	found, missing, err := partitionChecks(raw, []string{"Terraform Lint"})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(found) != 1 || found[0].State != "SUCCESS" {
		t.Errorf("a prefix match leaked in: %+v", found)
	}
	if len(missing) != 0 {
		t.Errorf("unexpected missing: %v", missing)
	}
}

func TestPartitionChecksRejectsGarbage(t *testing.T) {
	if _, _, err := partitionChecks([]byte("not json"), []string{"x"}); err == nil {
		t.Error("unparseable gh output must be an error, not an empty (vacuously clean) set")
	}
}

// An unfamiliar terminal state must end the wait rather than burn the whole budget
// and then be reported as a timeout.
func TestUnknownStateCountsAsSettled(t *testing.T) {
	if !settled([]check{{Name: "x", State: "NEUTRAL"}}) {
		t.Error("NEUTRAL is terminal; treating it as pending makes the verb time out on a decided check")
	}
	if settled([]check{{Name: "x", State: "IN_PROGRESS"}}) {
		t.Error("IN_PROGRESS is not settled")
	}
}

func TestFailuresIsCaseInsensitiveOnSuccess(t *testing.T) {
	if got := failures([]check{{Name: "a", State: "success"}}); len(got) != 0 {
		t.Errorf("lowercase success counted as a failure: %+v", got)
	}
	if got := failures([]check{{Name: "a", State: "CANCELLED"}}); len(got) != 1 {
		t.Errorf("CANCELLED must count as a failure, got %+v", got)
	}
}

func TestPRNumberParsing(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://github.com/o/r/pull/7\n", "7"},
		{"42\n", "42"},
		{"", ""},
		{"not-a-number\n", ""},
	} {
		if got := prNumber([]byte(tc.in)); got != tc.want {
			t.Errorf("prNumber(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The clone URL embeds the token; git echoes the remote back on failure. Without
// redaction a bad credential prints itself into a public actions log.
func TestRedactStripsTheToken(t *testing.T) {
	err := fmt.Errorf("fatal: could not read from https://x-access-token:s3cret@github.com/o/r.git")
	got := redact(err, "s3cret").Error()
	if strings.Contains(got, "s3cret") {
		t.Errorf("token survived redaction: %s", got)
	}
	if !strings.Contains(got, "***") {
		t.Errorf("expected a redaction marker, got: %s", got)
	}
	if redact(nil, "s3cret") != nil {
		t.Error("redact(nil) must stay nil")
	}
}

func TestCloneFailureDoesNotLeakTheToken(t *testing.T) {
	f := &fakeForge{t: t, checkPolls: []string{bothPass}}
	defer f.install()()

	orig := gatesGit
	gatesGit = func(_ string, args ...string) ([]byte, error) {
		if args[0] == "clone" {
			return nil, fmt.Errorf("could not read from https://x-access-token:tok@github.com/o/r.git")
		}
		return nil, nil
	}
	defer func() { gatesGit = orig }()

	err := RunAssertInstancePRGates(gatesBaseOpts())
	if err == nil {
		t.Fatal("a failed clone must fail the verb")
	}
	if strings.Contains(err.Error(), "tok@") {
		t.Errorf("the token reached the error text: %v", err)
	}
}
