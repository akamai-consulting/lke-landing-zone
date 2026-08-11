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
	"os"
	"os/exec"
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
	// ghHosts records the host passed alongside each gh argv.
	ghHosts []string
	// gitCalls records every git argv.
	gitCalls [][]string
	// prCreateErr makes `gh pr create` fail, exercising the pr-view fallback.
	prCreateErr error
	// prCreateOut overrides what `gh pr create` prints (empty = a normal PR URL).
	prCreateOut string
	// prViewOut is what the fallback `gh pr list --state open` returns.
	prViewOut string
	// checksErr is returned alongside every checks payload — gh's non-zero exit.
	checksErr error
	// checksErrAfter, when >0, makes `gh pr checks` start ERRORING from that poll
	// onwards — gh going quiet mid-run, rather than at the start.
	checksErrAfter int
	// repoDefaultBranch is what `gh repo view --json defaultBranchRef` returns.
	repoDefaultBranch string
	polls             int
	slept             int
}

// install swaps the package seams and returns a restore func.
func (f *fakeForge) install() func() {
	origGH, origGit, origSleep := gatesGH, gatesGit, gatesSleep
	origTemp, origAppend, origRemove := gatesTempDir, gatesAppend, gatesRemoveAll

	gatesGH = func(_, host string, args ...string) ([]byte, error) {
		f.ghHosts = append(f.ghHosts, host)
		f.ghCalls = append(f.ghCalls, args)
		switch {
		case len(args) >= 2 && args[0] == "pr" && args[1] == "create":
			if f.prCreateErr != nil {
				return nil, f.prCreateErr
			}
			if f.prCreateOut != "" {
				return []byte(f.prCreateOut), nil
			}
			return []byte("https://github.com/o/r/pull/7\n"), nil
		case len(args) >= 2 && args[0] == "pr" && args[1] == "list":
			return []byte(f.prViewOut), nil
		case len(args) >= 2 && args[0] == "repo" && args[1] == "view":
			return []byte(f.repoDefaultBranch), nil
		case len(args) >= 2 && args[0] == "pr" && args[1] == "checks":
			i := f.polls
			if i >= len(f.checkPolls) {
				i = len(f.checkPolls) - 1
			}
			f.polls++
			if f.checksErrAfter > 0 && f.polls > f.checksErrAfter {
				return nil, errors.New("gh pr checks: could not answer: dial tcp: i/o timeout")
			}
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
	// CANCELLED is NOT a failure: the gated jobs share a concurrency group, so a
	// superseded run reports its checks CANCELLED, and calling that "a delivered
	// CI gate does not work in the scaffold it ships to" sends an operator to
	// debug a job that never got to run. It is inconclusive — still fatal to the
	// verb, but under its own words.
	if got := failures([]check{{Name: "a", State: "CANCELLED"}}); len(got) != 0 {
		t.Errorf("CANCELLED was counted as a broken gate: %+v", got)
	}
	if got := inconclusive([]check{{Name: "a", State: "CANCELLED"}}); len(got) != 1 {
		t.Errorf("CANCELLED must be inconclusive, got %+v", got)
	}
	if got := failures([]check{{Name: "a", State: "TIMED_OUT"}}); len(got) != 1 {
		t.Errorf("TIMED_OUT is a verdict about the gate and must count as a failure, got %+v", got)
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

// ── the review's findings, pinned ─────────────────────────────────────────────

// The gated jobs live in the REUSABLE llz-terraform.yml, which terraform.yml calls
// from a job named `call`, so GitHub reports `call / Terraform Lint`. Matching the
// bare name found nothing and reported the verb's own regression forever.
func TestReusableWorkflowPrefixedChecksMatch(t *testing.T) {
	f := &fakeForge{t: t, checkPolls: []string{
		`[{"name":"call / Terraform Lint","state":"SUCCESS"},{"name":"call / Checkov IaC Security Scan","state":"SUCCESS"}]`,
	}}
	defer f.install()()

	if err := RunAssertInstancePRGates(gatesBaseOpts()); err != nil {
		t.Fatalf("caller-job-prefixed check names must match: %v", err)
	}
}

// ...but the prefix tolerance must not become a substring match.
func TestPrefixToleranceIsAnchored(t *testing.T) {
	for _, tc := range []struct {
		observed, want string
		match          bool
	}{
		{"Terraform Lint", "Terraform Lint", true},
		{"call / Terraform Lint", "Terraform Lint", true},
		{"a / b / Terraform Lint", "Terraform Lint", true},
		{"Terraform Lint (extra)", "Terraform Lint", false},
		{"Extra Terraform Lint", "Terraform Lint", false},
		{"call /Terraform Lint", "Terraform Lint", false},
	} {
		if got := matchesCheck(tc.observed, tc.want); got != tc.match {
			t.Errorf("matchesCheck(%q, %q) = %v, want %v", tc.observed, tc.want, got, tc.match)
		}
	}
}

// The touch target must be a TRACKED file. The first draft used
// terraform-iac-bootstrap/cluster/versions.tf, which that tree's .gitignore
// excludes (`*/*.tf` — an instance commits zero Terraform code), so the commit
// would have been empty and the step would have failed on every run.
func TestTouchPathIsNotAGeneratedTFRoot(t *testing.T) {
	if strings.HasSuffix(DefaultPRGateTouchPath, ".tf") {
		t.Errorf("%q is a rendered TF root — gitignored via */*.tf, so it is never tracked "+
			"and the touch commit would be empty", DefaultPRGateTouchPath)
	}
	if !strings.HasPrefix(DefaultPRGateTouchPath, "terraform-iac-bootstrap/") {
		t.Errorf("%q is outside the pipeline's paths: filter, so it would select neither gated job",
			DefaultPRGateTouchPath)
	}
}

// A check that never settles is a TIMEOUT. Reporting it as "a delivered CI gate
// does not work in the scaffold it ships to" sends an operator to debug a job that
// may be healthy and merely queued.
func TestTimeoutIsNotReportedAsAFailingGate(t *testing.T) {
	f := &fakeForge{t: t, checkPolls: []string{
		`[{"name":"Terraform Lint","state":"IN_PROGRESS"},{"name":"Checkov IaC Security Scan","state":"SUCCESS"}]`,
	}}
	defer f.install()()

	err := RunAssertInstancePRGates(gatesBaseOpts())
	if err == nil {
		t.Fatal("an unsettled check must fail the verb")
	}
	if !strings.Contains(err.Error(), "did not finish") {
		t.Errorf("expected a timeout diagnosis, got: %v", err)
	}
	if strings.Contains(err.Error(), "does not work in the scaffold") {
		t.Errorf("a pending check was reported as a broken gate: %v", err)
	}
}

// The branch is named after the commit, so a retry at the same sha meets its own
// leaked branch; a plain push dies on non-fast-forward.
func TestPushIsForcedSoARetryAtTheSameSHAWorks(t *testing.T) {
	f := &fakeForge{t: t, checkPolls: []string{bothPass}}
	defer f.install()()

	if err := RunAssertInstancePRGates(gatesBaseOpts()); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	for _, c := range f.gitCalls {
		if c[0] != "push" {
			continue
		}
		if !slicesContains(c, "--force") {
			t.Errorf("push is not forced (%v) — a re-run at the same sha cannot overwrite its own leaked branch", c)
		}
		return
	}
	t.Error("no push was issued")
}

// ── the second review's findings, pinned ─────────────────────────────────────

// THE PR MUST BE A DRAFT, and this is a correctness test rather than a style one.
// The same paths: filter selects `Plan Cluster (PR)`, which runs `llz ci tf-import`
// — a WRITE to cluster/<env>/terraform.tfstate. The e2e's provision job dispatches
// an apply against that state as soon as this verb returns, under a different
// concurrency group, over a backend with no lock. Drop --draft and the two race,
// last write wins.
func TestPRIsOpenedAsADraft(t *testing.T) {
	f := &fakeForge{t: t, checkPolls: []string{bothPass}}
	defer f.install()()

	if err := RunAssertInstancePRGates(gatesBaseOpts()); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	for _, c := range f.ghCalls {
		if len(c) < 2 || c[0] != "pr" || c[1] != "create" {
			continue
		}
		if !slicesContains(c, "--draft") {
			t.Errorf("`gh pr create` is not --draft (%v) — the throwaway PR would also fire the "+
				"state-writing Plan Cluster job, which races the provision apply on the same tfstate", c)
		}
		return
	}
	t.Error("no `gh pr create` was issued")
}

// Output this verb cannot READ says nothing about the gates. Reporting it as
// "never appeared" is the bash version's misdiagnosis wearing a different hat:
// an operator is sent to inspect a paths: filter that is fine.
func TestUnreadableChecksAreNotReportedAsMissing(t *testing.T) {
	f := &fakeForge{t: t, checkPolls: []string{"<html>502 Bad Gateway</html>"}}
	defer f.install()()

	err := RunAssertInstancePRGates(gatesBaseOpts())
	if err == nil {
		t.Fatal("unparseable gh output must fail the verb, not pass vacuously")
	}
	if strings.Contains(err.Error(), "never ran") || strings.Contains(err.Error(), "paths: filter") {
		t.Errorf("output that could not be parsed was blamed on the instance's CI: %v", err)
	}
	if !strings.Contains(err.Error(), "could not read the checks") {
		t.Errorf("expected an I/O diagnosis, got: %v", err)
	}
	// The payload has to reach the operator, or the message is unactionable.
	if !strings.Contains(err.Error(), "502 Bad Gateway") {
		t.Errorf("the unreadable payload should be quoted, got: %v", err)
	}
}

// ...but ONE bad read must not condemn a run that went on to observe cleanly.
func TestATransientUnreadablePollDoesNotPoisonACleanRun(t *testing.T) {
	f := &fakeForge{t: t, checkPolls: []string{"<html>502</html>", bothPass}}
	defer f.install()()

	if err := RunAssertInstancePRGates(gatesBaseOpts()); err != nil {
		t.Fatalf("a transient bad poll followed by a clean one should pass: %v", err)
	}
}

// `gh pr create` can exit 0 and print something that is not a PR URL. The error
// built from that used to `%w` a nil err, rendering the literal `%!w(<nil>)` and
// naming neither what gh printed nor what the fallback did.
func TestPRCreateWithNoURLIsDiagnosable(t *testing.T) {
	f := &fakeForge{
		t: t, checkPolls: []string{bothPass},
		prCreateOut: "Warning: 1 uncommitted change\n",
		prViewOut:   "",
	}
	defer f.install()()

	err := RunAssertInstancePRGates(gatesBaseOpts())
	if err == nil {
		t.Fatal("a PR that could not be identified must fail the verb")
	}
	if strings.Contains(err.Error(), "%!w") || strings.Contains(err.Error(), "<nil>") {
		t.Errorf("a nil error was formatted as the cause: %v", err)
	}
	if !strings.Contains(err.Error(), "uncommitted change") {
		t.Errorf("what gh actually printed should be quoted, got: %v", err)
	}
}

// Every `gh` failure used to collapse to `exit status 1` — the same words for an
// under-scoped token, a bad flag and a missing repo — because Output() drops
// stderr, which is where gh writes the one line that tells them apart.
func TestGHErrorCarriesStderr(t *testing.T) {
	if got := ghError([]string{"pr", "create"}, nil); got != nil {
		t.Fatalf("a successful call must not become an error: %v", got)
	}
	ee := &exec.ExitError{
		ProcessState: &os.ProcessState{},
		Stderr:       []byte("GraphQL: Resource not accessible by integration (createPullRequest)\n"),
	}
	got := ghError([]string{"pr", "create", "--repo", "o/r"}, ee).Error()
	if !strings.Contains(got, "Resource not accessible by integration") {
		t.Errorf("stderr was dropped, so the real cause is invisible: %s", got)
	}
	if !strings.Contains(got, "pr create") {
		t.Errorf("the failing gh call should be named: %s", got)
	}
}

// ── the third review's findings, pinned ──────────────────────────────────────

// A PR THAT TRIGGERED NOTHING IS THE REGRESSION THIS VERB HUNTS, and it is also
// the case where `gh pr checks` exits non-zero with EMPTY stdout. Treating that
// as "could not read the checks" put the verb's own blindness in front of the
// one diagnosis it exists to deliver: an operator would be told the tooling
// failed instead of that the paths: filter no longer selects the gated jobs.
// Garbage is unreadable; silence is an observation of zero checks.
func TestAPRWithNoChecksIsReportedAsNeverRan(t *testing.T) {
	f := &fakeForge{
		t: t, checkPolls: []string{""},
		checksErr: errors.New("gh pr checks: exit status 1: no checks reported on the 'e2e/ci-gates-abcdef12' branch"),
	}
	defer f.install()()

	err := RunAssertInstancePRGates(gatesBaseOpts())
	if err == nil {
		t.Fatal("a PR that triggered no checks at all must fail — it is the regression this verb hunts")
	}
	if !strings.Contains(err.Error(), "never ran") {
		t.Errorf("a PR with zero checks was not reported as the gates never running: %v", err)
	}
	if strings.Contains(err.Error(), "could not read the checks") {
		t.Errorf("zero checks was reported as an I/O failure, which blames the tooling for a real "+
			"regression in the delivered CI: %v", err)
	}
	if !strings.Contains(err.Error(), DefaultPRGateTouchPath) {
		t.Errorf("the diagnosis should still name the touched path: %v", err)
	}
	// gh's own sentence is the confirmation, and it must survive into the message
	// rather than compete with it.
	if !strings.Contains(err.Error(), "no checks reported") {
		t.Errorf("gh's explanation of the empty answer was dropped: %v", err)
	}
}

// ...but genuinely unreadable output must STILL be reported as unreadable. The
// two branches are one `len(out)` apart, so both directions need holding.
func TestUnparseableOutputIsStillAnIOFailure(t *testing.T) {
	f := &fakeForge{t: t, checkPolls: []string{"<html>502 Bad Gateway</html>"}}
	defer f.install()()

	err := RunAssertInstancePRGates(gatesBaseOpts())
	if err == nil || !strings.Contains(err.Error(), "could not read the checks") {
		t.Fatalf("non-empty output that cannot be parsed must be an I/O diagnosis, got: %v", err)
	}
}

// A blank poll after a real observation must not erase what was already seen —
// checks do not un-appear, and reporting "never ran" for a gate we watched fail
// would be the bash bug wearing the new code's clothes.
func TestABlankPollDoesNotEraseAnEarlierObservation(t *testing.T) {
	f := &fakeForge{t: t, checkPolls: []string{
		`[{"name":"Terraform Lint","state":"FAILURE"},{"name":"Checkov IaC Security Scan","state":"IN_PROGRESS"}]`,
		"", // gh goes quiet on a later poll
	}}
	defer f.install()()

	err := RunAssertInstancePRGates(gatesBaseOpts())
	if err == nil {
		t.Fatal("a FAILURE check must still fail the verb")
	}
	if strings.Contains(err.Error(), "never ran") {
		t.Errorf("a later blank poll erased the observation that the gates DID run: %v", err)
	}
}

// --host reached the clone URL and nothing else, so every `gh` call went to
// whatever host the ambient environment named. It only worked because
// e2e-instantiate.yml exports GH_HOST at workflow level — the flag was inert and
// the lane was carrying it.
func TestHostReachesGH(t *testing.T) {
	f := &fakeForge{t: t, checkPolls: []string{bothPass}}
	defer f.install()()

	o := gatesBaseOpts()
	o.Host = "ghes.example.com"
	if err := RunAssertInstancePRGates(o); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(f.ghHosts) == 0 {
		t.Fatal("no gh calls were made")
	}
	for i, h := range f.ghHosts {
		if h != "ghes.example.com" {
			t.Errorf("gh call %d (%v) went to host %q, not the --host the caller asked for — "+
				"a GHES lane would silently address github.com", i, f.ghCalls[i], h)
		}
	}
}

func slicesContains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// A superseded run reports CANCELLED. Reporting that as a broken delivered gate
// is the same conflation the header rejects for PENDING — the operator is sent to
// debug a job that never got to run.
func TestCancelledIsNotReportedAsABrokenGate(t *testing.T) {
	f := &fakeForge{t: t, checkPolls: []string{
		`[{"name":"Terraform Lint","state":"CANCELLED"},{"name":"Checkov IaC Security Scan","state":"SUCCESS"}]`,
	}}
	defer f.install()()

	err := RunAssertInstancePRGates(gatesBaseOpts())
	if err == nil {
		t.Fatal("an inconclusive check must still fail the verb — nothing was proven")
	}
	if strings.Contains(err.Error(), "does not work in the scaffold") {
		t.Errorf("a cancelled check was reported as a broken gate: %v", err)
	}
	if !strings.Contains(err.Error(), "no verdict") || !strings.Contains(err.Error(), "concurrency group") {
		t.Errorf("expected the superseded-run diagnosis with a re-run hint, got: %v", err)
	}
}

// SKIPPED means the job's `if:` excluded it — a real regression in the delivered
// gating, and a different one from a command that failed inside the job.
func TestSkippedPointsAtTheGatingCondition(t *testing.T) {
	f := &fakeForge{t: t, checkPolls: []string{
		`[{"name":"Terraform Lint","state":"SKIPPED"},{"name":"Checkov IaC Security Scan","state":"SUCCESS"}]`,
	}}
	defer f.install()()

	err := RunAssertInstancePRGates(gatesBaseOpts())
	if err == nil {
		t.Fatal("a skipped gate must fail the verb — it did not run")
	}
	if !strings.Contains(err.Error(), "`if:`") {
		t.Errorf("expected the diagnosis to point at the job's if: condition, got: %v", err)
	}
}

// The push happens before the PR exists, so a `gh pr create` failure would leave
// the branch behind with nothing to remove it — including the under-scoped-token
// case this lane's own docs describe.
func TestBranchIsDeletedEvenWhenThePRIsNeverOpened(t *testing.T) {
	f := &fakeForge{
		t: t, checkPolls: []string{bothPass},
		prCreateErr: errors.New("gh pr create: exit status 1: GraphQL: Resource not accessible by integration"),
		prViewOut:   "",
	}
	defer f.install()()

	if err := RunAssertInstancePRGates(gatesBaseOpts()); err == nil {
		t.Fatal("a PR that could not be opened must fail the verb")
	}
	branch := prBranch(gatesBaseOpts().SHA)
	for _, c := range f.ghCalls {
		if len(c) >= 4 && c[0] == "api" && c[1] == "-X" && c[2] == "DELETE" && strings.HasSuffix(c[3], branch) {
			return
		}
	}
	t.Errorf("the pushed branch %q was never deleted — it leaks on the fixture repo, and a re-run at "+
		"the same sha meets its own leftovers. gh calls were: %v", branch, f.ghCalls)
}

// An unreadable poll followed by a blank one used to leave parseErr set while raw
// became non-nil, so neither the "could not read" branch nor the "never ran" one
// owned the case — and the never-ran wording was printed for an I/O problem, one
// poll after the plumbing that exists to prevent exactly that.
func TestAnUnreadablePollThenABlankOneStillDiagnosesCorrectly(t *testing.T) {
	f := &fakeForge{t: t, checkPolls: []string{"<html>502 Bad Gateway</html>", ""}}
	defer f.install()()

	err := RunAssertInstancePRGates(gatesBaseOpts())
	if err == nil {
		t.Fatal("neither poll saw the gates — the verb must fail")
	}
	// The blank answer is the newer evidence, so "never ran" is the right verdict…
	if !strings.Contains(err.Error(), "never ran") {
		t.Errorf("expected the never-ran verdict from the newer, readable answer, got: %v", err)
	}
	// …but the payload we could not read has to survive into the message, or an
	// operator chasing a paths: filter never learns a poll was garbage.
	if !strings.Contains(err.Error(), "unreadable") {
		t.Errorf("the earlier unreadable poll was dropped from the diagnosis: %v", err)
	}
}

// --base was hardcoded to "main" while the delivered terraform.yml watches
// `branches: [main, master]`. Against a master-default instance the verb
// force-pushed a branch and could then never open its PR.
func TestBaseComesFromTheInstanceDefaultBranch(t *testing.T) {
	f := &fakeForge{t: t, checkPolls: []string{bothPass}, repoDefaultBranch: "master"}
	defer f.install()()

	if err := RunAssertInstancePRGates(gatesBaseOpts()); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	for _, c := range f.ghCalls {
		if len(c) < 2 || c[0] != "pr" || c[1] != "create" {
			continue
		}
		for i, a := range c {
			if a == "--base" && i+1 < len(c) {
				if c[i+1] != "master" {
					t.Errorf("PR opened against %q, but the instance's default branch is master — "+
						"`gh pr create` would reject it and the pushed branch would leak", c[i+1])
				}
				return
			}
		}
		t.Errorf("`gh pr create` carried no --base: %v", c)
		return
	}
	t.Error("no `gh pr create` was issued")
}

// An explicit --base must still win, for an instance whose PR target is neither.
func TestExplicitBaseIsNotOverridden(t *testing.T) {
	f := &fakeForge{t: t, checkPolls: []string{bothPass}, repoDefaultBranch: "master"}
	defer f.install()()

	o := gatesBaseOpts()
	o.Base = "release/v2"
	if err := RunAssertInstancePRGates(o); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	for _, c := range f.ghCalls {
		if len(c) >= 2 && c[0] == "repo" && c[1] == "view" {
			t.Error("--base was given, but the verb still asked the forge for the default branch")
		}
	}
}

// A good first observation followed by failing polls leaves the checks looking
// permanently pending. Reporting that as a plain timeout hides that we simply
// stopped being able to ask — the I/O-as-verdict mistake, in the third branch.
func TestATimeoutSaysWhenThePollsWereFailing(t *testing.T) {
	f := &fakeForge{t: t, checkPolls: []string{
		`[{"name":"Terraform Lint","state":"IN_PROGRESS"},{"name":"Checkov IaC Security Scan","state":"IN_PROGRESS"}]`,
		"", // gh stops answering
	}}
	f.checksErrAfter = 1
	defer f.install()()

	err := RunAssertInstancePRGates(gatesBaseOpts())
	if err == nil {
		t.Fatal("unsettled checks must fail the verb")
	}
	if !strings.Contains(err.Error(), "TIMEOUT") {
		t.Errorf("expected the timeout verdict, got: %v", err)
	}
	if !strings.Contains(err.Error(), "could not answer") {
		t.Errorf("the failing polls were not mentioned, so the timeout reads as certain: %v", err)
	}
}

// `Retries = timeout / interval` is INTEGER division, so a timeout shorter than
// one poll rounds to zero attempts — which RunAssertInstancePRGates then promotes
// to its 60-retry default, turning `--timeout 10 --interval 20` into a
// twenty-MINUTE wait. A zero interval is worse: it removes the sleep and fires
// the whole budget at the forge back to back.
func TestPollBudgetFlagsAreValidated(t *testing.T) {
	defer (&fakeForge{t: t, checkPolls: []string{bothPass}}).install()()

	for _, tc := range []struct {
		name, want string
		args       []string
	}{
		{"timeout below one interval", "shorter than one --interval",
			[]string{"--instance", "o/r", "--sha", "abcdef1234", "--timeout", "10", "--interval", "20"}},
		{"zero interval", "at least 1 second",
			[]string{"--instance", "o/r", "--sha", "abcdef1234", "--interval", "0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GH_TOKEN", "tok")
			c := AssertInstancePRGatesCmd()
			c.SetArgs(tc.args)
			c.SetOut(os.Stderr)
			c.SilenceErrors = true
			err := c.Execute()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("expected a %q rejection, got: %v", tc.want, err)
			}
		})
	}
}

// A programmatic caller that never went through a flag must not busy-spin either.
func TestZeroIntervalGetsAFloorNotATightLoop(t *testing.T) {
	f := &fakeForge{t: t, checkPolls: []string{
		`[{"name":"Terraform Lint","state":"IN_PROGRESS"},{"name":"Checkov IaC Security Scan","state":"IN_PROGRESS"}]`,
	}}
	defer f.install()()

	o := gatesBaseOpts()
	o.Interval = 0
	o.Retries = 2
	var slept []time.Duration
	orig := gatesSleep
	gatesSleep = func(d time.Duration) { slept = append(slept, d) }
	defer func() { gatesSleep = orig }()

	_ = RunAssertInstancePRGates(o)
	for _, d := range slept {
		if d <= 0 {
			t.Fatalf("the poll loop slept %v — a zero interval polls the forge in a tight loop", d)
		}
	}
	if len(slept) == 0 {
		t.Fatal("the poll loop never slept at all")
	}
}

// A real FAILURE outranks an inconclusive sibling. With the branches the other
// way round, one FAILURE plus one CANCELLED reported "This is NOT a failing gate"
// and never named the gate that actually broke.
func TestAFailureOutranksAnInconclusiveSibling(t *testing.T) {
	f := &fakeForge{t: t, checkPolls: []string{
		`[{"name":"Terraform Lint","state":"FAILURE"},{"name":"Checkov IaC Security Scan","state":"CANCELLED"}]`,
	}}
	defer f.install()()

	err := RunAssertInstancePRGates(gatesBaseOpts())
	if err == nil {
		t.Fatal("a FAILURE must fail the verb")
	}
	if !strings.Contains(err.Error(), "Terraform Lint=FAILURE") {
		t.Errorf("the gate that actually broke was not named: %v", err)
	}
	if strings.Contains(err.Error(), "NOT a failing gate") {
		t.Errorf("a cancelled sibling hid a definite failure: %v", err)
	}
}

// The timeout verdict must also say when the polls were UNPARSEABLE, not only
// when they were empty — one good poll then a run of garbage is the same
// I/O-as-verdict trap through the third door.
func TestATimeoutSaysWhenThePollsWereUnparseable(t *testing.T) {
	f := &fakeForge{t: t, checkPolls: []string{
		`[{"name":"Terraform Lint","state":"IN_PROGRESS"},{"name":"Checkov IaC Security Scan","state":"IN_PROGRESS"}]`,
		"<html>502 Bad Gateway</html>",
	}}
	defer f.install()()

	err := RunAssertInstancePRGates(gatesBaseOpts())
	if err == nil {
		t.Fatal("unsettled checks must fail the verb")
	}
	if !strings.Contains(err.Error(), "TIMEOUT") {
		t.Errorf("expected the timeout verdict, got: %v", err)
	}
	if !strings.Contains(err.Error(), "502 Bad Gateway") {
		t.Errorf("the unparseable polls went unmentioned, so the timeout reads as certain: %v", err)
	}
}

// THE RE-RUN FALSE GREEN. The branch is named after the commit under test and
// this verb closes its PR on the way out, so a second run at the same sha finds a
// CLOSED PR on exactly that branch. `gh pr view <branch>` resolves closed PRs, so
// the re-run would have adopted the previous run's number and reported its old,
// already-settled checks as this run's verdict — a gate that refuses vacuous
// green, inheriting one.
func TestTheResumeFallbackOnlyAcceptsAnOpenPR(t *testing.T) {
	f := &fakeForge{
		t: t, checkPolls: []string{bothPass},
		prCreateErr: errors.New("gh pr create: a pull request already exists"),
		prViewOut:   "7\n",
	}
	defer f.install()()

	if err := RunAssertInstancePRGates(gatesBaseOpts()); err != nil {
		t.Fatalf("an already-open PR should still be reused: %v", err)
	}
	for _, c := range f.ghCalls {
		if len(c) < 2 || c[0] != "pr" {
			continue
		}
		if c[1] == "view" {
			t.Errorf("used `gh pr view`, which resolves CLOSED pull requests too: %v", c)
		}
		if c[1] == "list" {
			if !slicesContains(c, "--state") || !slicesContains(c, "open") {
				t.Errorf("the resume lookup is not constrained to OPEN pull requests: %v", c)
			}
			if !slicesContains(c, "--head") {
				t.Errorf("the resume lookup is not scoped to this run's branch: %v", c)
			}
			return
		}
	}
	t.Error("no resume lookup was issued after `gh pr create` failed")
}

// The loop must sleep BETWEEN polls, not after the last one. A trailing sleep
// spends Retries*Interval on top of Retries polls, overrunning the job-timeout
// arithmetic the 900s budget was chosen to fit — and the timeout message then
// reports less waiting than actually happened.
func TestThePollLoopDoesNotSleepAfterItsLastPoll(t *testing.T) {
	f := &fakeForge{t: t, checkPolls: []string{
		`[{"name":"Terraform Lint","state":"IN_PROGRESS"},{"name":"Checkov IaC Security Scan","state":"IN_PROGRESS"}]`,
	}}
	defer f.install()()

	o := gatesBaseOpts()
	o.Retries = 4
	if err := RunAssertInstancePRGates(o); err == nil {
		t.Fatal("unsettled checks must fail the verb")
	}
	if f.polls != o.Retries {
		t.Fatalf("polled %d time(s), want %d", f.polls, o.Retries)
	}
	if f.slept != o.Retries-1 {
		t.Errorf("slept %d time(s) for %d poll(s) — a sleep after the final poll is budget spent "+
			"waiting for something nobody will ask about", f.slept, o.Retries)
	}
}

// The throwaway branch is cut from wherever the clone lands, so the clone has to
// land on the PR's BASE. With a bare `--depth 1` it lands on the repo default,
// and any non-default --base yields a PR whose head is unrelated to its base:
// a diff carrying every commit on default-but-not-in-base, or `gh pr create`
// refusing with "No commits between".
func TestTheCloneLandsOnTheBaseBranch(t *testing.T) {
	f := &fakeForge{t: t, checkPolls: []string{bothPass}, repoDefaultBranch: "master"}
	defer f.install()()

	if err := RunAssertInstancePRGates(gatesBaseOpts()); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	for _, c := range f.gitCalls {
		if c[0] != "clone" {
			continue
		}
		if !slicesContains(c, "--branch") {
			t.Fatalf("clone does not pin a branch (%v) — it lands on the repo default while the PR "+
				"is opened against --base", c)
		}
		for i, a := range c {
			if a == "--branch" && i+1 < len(c) && c[i+1] != "master" {
				t.Errorf("cloned branch %q, but the PR base is the default branch master", c[i+1])
			}
		}
		return
	}
	t.Error("no clone was issued")
}

// A whitespace-only payload is SILENCE, not garbage. Classifying `"\n"` as
// unreadable reports "could not read the checks" for a PR that genuinely
// triggered nothing — the regression this verb exists to catch.
func TestWhitespaceOnlyOutputCountsAsNoChecks(t *testing.T) {
	f := &fakeForge{t: t, checkPolls: []string{"\n  \n"}}
	defer f.install()()

	err := RunAssertInstancePRGates(gatesBaseOpts())
	if err == nil {
		t.Fatal("a PR with no checks must fail the verb")
	}
	if strings.Contains(err.Error(), "could not read the checks") {
		t.Errorf("a blank payload was treated as unreadable rather than as zero checks: %v", err)
	}
	if !strings.Contains(err.Error(), "never ran") {
		t.Errorf("expected the never-ran verdict, got: %v", err)
	}
}
