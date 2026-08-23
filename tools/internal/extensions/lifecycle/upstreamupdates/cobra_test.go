package upstreamupdates

// cobra_test.go — the command layer, driven through its seams.
//
// The pure judgement is covered in upstreamupdates_test.go. What is covered HERE
// is the wiring around it, which is where the two commands can still go wrong in
// the way that matters: doing something irreversible, or reporting success for
// work they did not do.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// runCmd executes a command with args, capturing stderr, and returns the error.
func runCmd(t *testing.T, c *cobra.Command, args []string, errBuf *bytes.Buffer) error {
	t.Helper()
	c.SetArgs(args)
	c.SetErr(errBuf)
	if c.OutOrStdout() == os.Stdout {
		c.SetOut(&bytes.Buffer{})
	}
	return c.Execute()
}

// inTempRepo runs fn with cwd set to an empty temp dir and the Actions output
// files pointed somewhere writable, so a command's ghaout.Append does not append
// to the developer's environment.
func inTempRepo(t *testing.T, fn func(dir string)) {
	t.Helper()
	dir := t.TempDir()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
	t.Setenv("GITHUB_OUTPUT", filepath.Join(dir, "out"))
	t.Setenv("GITHUB_STEP_SUMMARY", filepath.Join(dir, "summary"))
	fn(dir)
}

// writeAnswers seeds the pin the upgrade would have just rewritten — the single
// source `upgrade-pr` reads the version from.
func writeAnswers(t *testing.T, dir, version string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".copier-answers.yml"),
		[]byte("_commit: abc\nllz_version: \""+version+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// ── upgrade-pr ──────────────────────────────────────────────────────────────

// stubForge installs the whole side-effect surface and returns what was done.
type forge struct {
	pushed, prHead string
	created        bool
}

func stubForge(t *testing.T, headSHA string, open, closed []upgradePR, listErr error) *forge {
	t.Helper()
	og, ob, op, oc := gitOut, upgradePRs, pushBranch, createPR
	t.Cleanup(func() { gitOut, upgradePRs, pushBranch, createPR = og, ob, op, oc })

	f := &forge{}
	gitOut = func(args ...string) (string, error) {
		if args[0] == "rev-parse" {
			return headSHA + "\n", nil
		}
		return "", nil
	}
	upgradePRs = func(state, _ string) ([]upgradePR, error) {
		if listErr != nil {
			return nil, listErr
		}
		if state == "open" {
			return open, nil
		}
		return closed, nil
	}
	pushBranch = func(b string) error { f.pushed = b; return nil }
	createPR = func(_, _, h string) error { f.created = true; f.prHead = h; return nil }
	return f
}

func runUpgradePR(t *testing.T, version string, args ...string) (string, error) {
	t.Helper()
	var errBuf bytes.Buffer
	var err error
	inTempRepo(t, func(dir string) {
		if version != "" {
			writeAnswers(t, dir, version)
		}
		t.Setenv("GH_TOKEN", "x")
		c := UpgradePRCmd()
		c.SilenceUsage, c.SilenceErrors = true, true
		err = runCmd(t, c, append([]string{"--base", "main"}, args...), &errBuf)
	})
	return errBuf.String(), err
}

func TestUpgradePRRequiresBeforeSHA(t *testing.T) {
	// Without it there is no way to tell an upgrade from an instance that was
	// already current, so the command would open an empty PR every month.
	stubForge(t, "def", nil, nil, nil)
	_, err := runUpgradePR(t, "v1.0.0")
	if err == nil || !strings.Contains(err.Error(), "--before is required") {
		t.Errorf("missing --before must be refused, got %v", err)
	}
}

func TestUpgradePRNamesTheMissingTokenBeforeDoingAnything(t *testing.T) {
	// Named up front rather than left to `gh pr create`, which would fail forty
	// seconds later — by which time a branch has been pushed.
	f := stubForge(t, "def", nil, nil, nil)
	var errBuf bytes.Buffer
	inTempRepo(t, func(dir string) {
		writeAnswers(t, dir, "v1.0.0")
		t.Setenv("GH_TOKEN", "")
		t.Setenv("GITHUB_TOKEN", "")
		c := UpgradePRCmd()
		c.SilenceUsage, c.SilenceErrors = true, true
		if err := runCmd(t, c, []string{"--before", "abc", "--base", "main"}, &errBuf); err == nil ||
			!strings.Contains(err.Error(), "LLZ_AUTOMATION_TOKEN") {
			t.Errorf("a missing token must name the secret, got %v", err)
		}
	})
	if f.pushed != "" {
		t.Error("nothing may be pushed before the token check")
	}
}

func TestUpgradePRRejectsGITHUBTOKEN(t *testing.T) {
	// Accepting it would contradict the sentence the error prints: a pull request
	// opened with GITHUB_TOKEN runs no checks, so the run would report a
	// successful upgrade and hand back a PR nothing had verified.
	stubForge(t, "def", nil, nil, nil)
	var errBuf bytes.Buffer
	inTempRepo(t, func(dir string) {
		writeAnswers(t, dir, "v1.0.0")
		t.Setenv("GH_TOKEN", "")
		t.Setenv("GITHUB_TOKEN", "ghs_something")
		c := UpgradePRCmd()
		c.SilenceUsage, c.SilenceErrors = true, true
		err := runCmd(t, c, []string{"--before", "abc", "--base", "main"}, &errBuf)
		if err == nil || !strings.Contains(err.Error(), "deliberately NOT accepted") {
			t.Errorf("GITHUB_TOKEN must be refused with the reason, got %v", err)
		}
	})
}

func TestUpgradePROpensNothingWhenHEADDidNotMove(t *testing.T) {
	// `llz upgrade` exits 0 whether or not it changed anything, so the commit is
	// the only honest signal. It must also not even ASK the forge.
	asked := false
	f := stubForge(t, "abc", nil, nil, nil)
	orig := upgradePRs
	t.Cleanup(func() { upgradePRs = orig })
	upgradePRs = func(string, string) ([]upgradePR, error) { asked = true; return nil, nil }

	out, err := runUpgradePR(t, "v1.2.3", "--before", "abc")
	if err != nil {
		t.Fatalf("already-current must exit 0, got %v", err)
	}
	if asked || f.pushed != "" || f.created {
		t.Error("an unchanged HEAD must touch nothing at all")
	}
	if !strings.Contains(out, "already on the target release") {
		t.Errorf("must say why, got: %s", out)
	}
}

func TestUpgradePRDoesNotStackBehindAnOpenUpgrade(t *testing.T) {
	// The guard that replaced three attempts at deriving it from the branch name.
	// It keys on the branch STEM, not the version and not the label — the label is
	// dropped on a 422 retry, so a guard depending on it would go missing exactly
	// when the forge is having a bad day.
	f := stubForge(t, "def", []upgradePR{{Head: "chore/template-upgrade-v9.9.9-aaaaaaa", State: "OPEN"}}, nil, nil)
	out, err := runUpgradePR(t, "v1.2.3", "--before", "abc")
	if err != nil {
		t.Fatalf("a pending review is a clean no-op, got %v", err)
	}
	if f.pushed != "" || f.created {
		t.Error("must not stack a second upgrade behind one awaiting review")
	}
	if !strings.Contains(out, "still open") {
		t.Errorf("must say a review is pending, got: %s", out)
	}
}

func TestUpgradePRDoesNotReproposeARejectedVersion(t *testing.T) {
	// A reviewer CLOSED this version — read off the STATE, not inferred from the
	// query: `gh pr list --state closed` INCLUDES merged, so only `State ==
	// "CLOSED"` distinguishes rejection from shipment. Handing a rejected version
	// back monthly is noise that trains people to ignore the bot.
	f := stubForge(t, "def", nil, []upgradePR{{Head: "chore/template-upgrade-v1.2.3-0000000", State: "CLOSED"}}, nil)
	out, err := runUpgradePR(t, "v1.2.3", "--before", "abc")
	if err != nil {
		t.Fatalf("a rejected version is a clean no-op, got %v", err)
	}
	if f.pushed != "" || f.created {
		t.Error("must not re-propose a rejected upgrade")
	}
	if !strings.Contains(out, "closed unmerged") {
		t.Errorf("must say it was rejected, got: %s", out)
	}
}

func TestUpgradePRProposesAgainAfterAMergedOne(t *testing.T) {
	// A merged pull request does NOT block: `llz upgrade` also restores drifted
	// `managed` files, so it can legitimately commit again at an unchanged pin.
	// This case reaches that outcome with NO prior pull request at all; the one
	// where a MERGED row does come back from the closed query — which it does, that
	// query includes merged — is TestUpgradePRTreatsAMergedPRAsShippedNotRejected.
	f := stubForge(t, "deadbeefcafe", nil, nil, nil)
	if _, err := runUpgradePR(t, "v5.0.0", "--before", "abc"); err != nil {
		t.Fatalf("work at an already-merged pin must still open a PR, got %v", err)
	}
	if f.pushed != "chore/template-upgrade-v5.0.0-deadbee" || f.prHead != f.pushed {
		t.Errorf("expected a version+sha branch, pushed=%q head=%q", f.pushed, f.prHead)
	}
}

func TestUpgradePRBranchCarriesTheCommit(t *testing.T) {
	// The property that removed orphan recovery, the force-push, the lease and the
	// spent-branch case: two runs can never compute the same branch.
	a := BranchName("v1.0.0", "1111111aaaa")
	b := BranchName("v1.0.0", "2222222bbbb")
	if a == b {
		t.Fatal("two commits at the same version must not share a branch name")
	}
	if !strings.HasPrefix(a, VersionStem("v1.0.0")) {
		t.Errorf("%q must sit under the version stem the rejected-version guard matches", a)
	}
}

func TestUpgradePRRefusesToActWhenItCannotTell(t *testing.T) {
	// Guessing here either stacks a second PR behind one under review, or
	// re-proposes a rejected upgrade. Both silent, both monthly.
	//
	// Drives the REAL upgradeBranches over a failing process hop: stubbing the
	// wrapper would skip the very message under test. (An earlier cut called
	// stubForge first and then "restored" upgradeBranches — capturing the stub,
	// so the real query never ran and the test passed on the stub's own error.)
	og, op, oc, oe := gitOut, pushBranch, createPR, execSeam
	t.Cleanup(func() { gitOut, pushBranch, createPR, execSeam = og, op, oc, oe })

	pushed, created := "", false
	gitOut = func(args ...string) (string, error) {
		if args[0] == "rev-parse" {
			return "def\n", nil
		}
		return "", nil
	}
	pushBranch = func(b string) error { pushed = b; return nil }
	createPR = func(_, _, _ string) error { created = true; return nil }
	execSeam = func(string, ...string) ([]byte, error) { return nil, fmt.Errorf("gh exploded") }

	_, err := runUpgradePR(t, "v1.0.0", "--before", "abc")
	if err == nil {
		t.Fatal("an unreadable PR list must stop the run, not be guessed at")
	}
	if !strings.Contains(err.Error(), "refusing to act on a guess") {
		t.Errorf("error must say why it stopped, got: %v", err)
	}
	if pushed != "" || created {
		t.Error("nothing may be pushed when the forge state is unknown")
	}
}

func TestUpgradePRRefusesAnUnresolvablePin(t *testing.T) {
	// The pin keys the PR title and the rejected-version guard. A placeholder would
	// make every release look like the same one.
	f := stubForge(t, "def", nil, nil, nil)
	_, err := runUpgradePR(t, "", "--before", "abc") // no .copier-answers.yml
	if err == nil || !strings.Contains(err.Error(), "same one") {
		t.Errorf("an unresolvable pin must stop the run and say why, got %v", err)
	}
	if f.pushed != "" {
		t.Error("nothing may be pushed under a placeholder version")
	}
}

func TestUpgradePROpensADraft(t *testing.T) {
	// The draft is what keeps this PR off `llz ci tf-import`. Asserted on the real
	// argv, because the flag living in the seam's body is the only thing that makes
	// the claim true.
	if !strings.Contains(draftFlagProbe(), "--draft") {
		t.Error("`gh pr create` must pass --draft: without it the bot's own PR takes the unserialized " +
			"tfstate write that plan-cluster-pr's draft-skip exists to prevent")
	}
}

// draftFlagProbe captures the argv the REAL createPR builds, by swapping the
// process seam underneath it rather than restating the flag list.
func draftFlagProbe() string {
	orig := execSeam
	defer func() { execSeam = orig }()
	var seen []string
	execSeam = func(bin string, args ...string) ([]byte, error) {
		seen = append([]string{bin}, args...)
		return nil, nil
	}
	_ = createPR("t", "main", "branch")
	return strings.Join(seen, " ")
}

func TestPinnedVersionReadsTheAnswersFile(t *testing.T) {
	inTempRepo(t, func(dir string) {
		if err := os.WriteFile(filepath.Join(dir, ".copier-answers.yml"),
			[]byte("_commit: abc\nllz_version: \"v4.5.6\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := pinnedVersion(); got != "v4.5.6" {
			t.Errorf("pinnedVersion() = %q, want v4.5.6", got)
		}
	})
	// NO PLACEHOLDER: empty, so the caller can refuse.
	inTempRepo(t, func(string) {
		if got := pinnedVersion(); got != "" {
			t.Errorf("an unresolvable pin must be empty so the caller can refuse, got %q", got)
		}
	})
}

func TestUpgradePRForceOverridesBothGuards(t *testing.T) {
	// An operator who dispatched with an explicit --ref has already made the
	// judgement the guards exist to make for an UNATTENDED run. Without an
	// override, that run did the whole upgrade and then discarded it — and no flag
	// could ever un-block a version a reviewer had closed.
	f := stubForge(t, "def0000",
		[]upgradePR{{Head: "chore/template-upgrade-v9.9.9-aaaaaaa", State: "OPEN"}},
		[]upgradePR{{Head: "chore/template-upgrade-v1.2.3-0000000", State: "CLOSED"}},
		nil)
	out, err := runUpgradePR(t, "v1.2.3", "--before", "abc", "--force")
	if err != nil {
		t.Fatalf("--force must proceed, got %v", err)
	}
	if !f.created || f.pushed != "chore/template-upgrade-v1.2.3-def0000" {
		t.Errorf("--force must push and open, pushed=%q created=%v", f.pushed, f.created)
	}
	// And it must SAY so: a run that silently stepped over two guards looks
	// identical to one that had nothing in its way.
	if !strings.Contains(out, "Guard overridden") {
		t.Errorf("an overridden guard must be reported, got: %s", out)
	}
}

func TestUpgradePRTreatsAFullPageAsUnknown(t *testing.T) {
	// A truncated page reads exactly like "no upgrade pull requests", and an older
	// rejected upgrade is what falls off the end — so the bot hands it back. That
	// is the guess the error message says it refuses to make.
	og, op, oc, oe := gitOut, pushBranch, createPR, execSeam
	t.Cleanup(func() { gitOut, pushBranch, createPR, execSeam = og, op, oc, oe })

	pushed := ""
	gitOut = func(args ...string) (string, error) {
		if args[0] == "rev-parse" {
			return "def\n", nil
		}
		return "", nil
	}
	pushBranch = func(b string) error { pushed = b; return nil }
	createPR = func(_, _, _ string) error { return nil }
	// UPGRADE branches, not arbitrary ones. The query is scoped by head stem now,
	// so a page filled with unrelated pull requests no longer counts — which was
	// the bug: this repo returns exactly 300 closed PRs, so the unscoped guard
	// hard-failed every run after the upgrade had already been committed.
	full := make([]string, prListLimitN)
	for i := range full {
		full[i] = fmt.Sprintf("chore/template-upgrade-v1.0.0-%07d CLOSED", i)
	}
	execSeam = func(string, ...string) ([]byte, error) { return []byte(strings.Join(full, "\n")), nil }

	_, err := runUpgradePR(t, "v1.0.0", "--before", "abc")
	if err == nil || !strings.Contains(err.Error(), "may be truncated") {
		t.Errorf("a full page must be treated as unknown, got %v", err)
	}
	if pushed != "" {
		t.Error("nothing may be pushed on a page that might be truncated")
	}
}

func TestCreatePRRetriesOnlyForTheLabel(t *testing.T) {
	// Retrying on ANY error turns a create that SUCCEEDED and then errored late
	// into a second attempt: the retry fails on the pull request the first call
	// already made, the run goes red, and no summary is written for a PR that
	// exists.
	orig := execSeam
	t.Cleanup(func() { execSeam = orig })

	calls := 0
	execSeam = func(string, ...string) ([]byte, error) {
		calls++
		return nil, fmt.Errorf("HTTP 502 gateway timeout")
	}
	if err := createPR("t", "main", "br"); err == nil {
		t.Fatal("a non-label failure must be returned")
	}
	if calls != 1 {
		t.Errorf("a non-label failure must NOT be retried, got %d call(s)", calls)
	}

	calls = 0
	execSeam = func(_ string, args ...string) ([]byte, error) {
		calls++
		if calls == 1 {
			return nil, fmt.Errorf(`could not add label: 'template-upgrade' not found`)
		}
		return nil, nil
	}
	if err := createPR("t", "main", "br"); err != nil {
		t.Fatalf("a label refusal must retry without it, got %v", err)
	}
	if calls != 2 {
		t.Errorf("expected one retry, got %d call(s)", calls)
	}
}

func TestCreatePRDropsTheLabelAndTheDraftTogether(t *testing.T) {
	// THE REPO SHAPE BOTH FALLBACKS EXIST FOR IS THE SAME ONE: a private repo on a
	// Free plan that has never created the label — i.e. every fresh instance —
	// refuses the label AND the draft. The label complaint is what `gh` reports
	// first, and while the label retry kept --draft and returned on failure, the
	// draft fallback below it was unreachable: the run errored with a branch pushed,
	// no pull request, and no summary.
	orig := execSeam
	t.Cleanup(func() { execSeam = orig })
	var argvs []string
	execSeam = func(_ string, args ...string) ([]byte, error) {
		argv := strings.Join(args, " ")
		argvs = append(argvs, argv)
		if strings.Contains(argv, "--label") {
			return nil, fmt.Errorf(`could not add label: 'template-upgrade' not found`)
		}
		if strings.Contains(argv, "--draft") {
			return nil, fmt.Errorf("draft pull requests are not supported in this repository")
		}
		return nil, nil
	}
	if err := createPR("t", "main", "br"); err != nil {
		t.Fatalf("both refusals together must still open a PR rather than strand the pushed branch, got %v", err)
	}
	if len(argvs) != 3 {
		t.Fatalf("expected label-drop then draft-drop, got %d attempt(s): %v", len(argvs), argvs)
	}
	if strings.Contains(argvs[2], "--label") || strings.Contains(argvs[2], "--draft") {
		t.Errorf("the final attempt must carry neither decoration, got %q", argvs[2])
	}
}

func TestUpgradePRTreatsAMergedPRAsShippedNotRejected(t *testing.T) {
	// `gh pr list --state closed` INCLUDES MERGED — measured against gh, not
	// assumed, and I had it backwards. Inferring rejection from the QUERY rather
	// than reading the state made a merged upgrade block a legitimate re-commit at
	// an unchanged pin (a drifted `managed` file restored), reported as "a reviewer
	// rejected this upgrade", exit 0, green run.
	f := stubForge(t, "def0000", nil,
		[]upgradePR{{Head: "chore/template-upgrade-v1.2.3-0000000", State: "MERGED"}}, nil)

	_, err := runUpgradePR(t, "v1.2.3", "--before", "abc")
	if err != nil {
		t.Fatalf("a merged upgrade must not block new work, got %v", err)
	}
	if !f.created || f.pushed != "chore/template-upgrade-v1.2.3-def0000" {
		t.Errorf("work after a merged upgrade must still be proposed, pushed=%q created=%v", f.pushed, f.created)
	}
}

func TestCreatePRFallsBackWhenDraftsAreUnavailable(t *testing.T) {
	// Draft PRs are unavailable on a private repo on a Free plan. The branch is
	// already pushed by the time createPR runs, so refusing would strand the work —
	// but a non-draft PR selects the state-writing plan job, which is exactly what
	// --draft prevents. Open it AND say the protection is absent.
	orig := execSeam
	t.Cleanup(func() { execSeam = orig })
	var argvs []string
	execSeam = func(_ string, args ...string) ([]byte, error) {
		argvs = append(argvs, strings.Join(args, " "))
		if len(argvs) == 1 {
			return nil, fmt.Errorf("draft pull requests are not supported in this repository")
		}
		return nil, nil
	}
	if err := createPR("t", "main", "br"); err != nil {
		t.Fatalf("a draft refusal must fall back rather than strand the pushed branch, got %v", err)
	}
	if len(argvs) != 2 {
		t.Fatalf("expected one retry, got %d attempt(s)", len(argvs))
	}
	if strings.Contains(argvs[1], "--draft") {
		t.Error("the retry must drop --draft; that is the point of it")
	}
}

func TestUpgradePRQueryIsScopedToUpgradeBranches(t *testing.T) {
	// Reads the REAL argv, because every other test in this file hands the query
	// pre-scoped data — so removing `--search head:` from the command left them all
	// green while the guard went back to counting unrelated pull requests. This
	// repo returns exactly 300 closed PRs, so unscoped meant a hard failure on
	// every run, after `llz upgrade --commit` had already done the work.
	orig := execSeam
	t.Cleanup(func() { execSeam = orig })
	var argv string
	execSeam = func(bin string, args ...string) ([]byte, error) {
		argv = bin + " " + strings.Join(args, " ")
		return nil, nil
	}
	if _, err := upgradePRs("closed", "chore/template-upgrade-v1.2.3-"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(argv, "--search head:chore/template-upgrade-v1.2.3-") {
		t.Errorf("the query must be scoped to the head stem, got: %s", argv)
	}
	// And it must ask for the state, because `--state closed` includes MERGED and
	// the two mean opposite things.
	if !strings.Contains(argv, "headRefName,state") {
		t.Errorf("the query must request state — --state closed includes MERGED, so rejection cannot be "+
			"inferred from the query alone. got: %s", argv)
	}
}

func TestUpgradePRIsNotBlockedByARejectedPreRelease(t *testing.T) {
	// THE STEM FOR A VERSION IS A PREFIX OF EVERY PRE-RELEASE OF IT:
	// chore/template-upgrade-v1.2.3- prefixes chore/template-upgrade-v1.2.3-rc1-abc1234.
	// The rejected-version guard matched on that prefix, so one closed rc turned
	// into a permanent refusal of the GA release with the same number — exit 0,
	// green run, upgrades silently stop arriving. The consumer is what is asserted
	// here, not ProposesVersion on its own: the bug was which comparison the guard
	// reached for.
	rejectedRC := []upgradePR{{Head: BranchName("v1.2.3-rc1", "abc1234"), State: "CLOSED"}}
	f := stubForge(t, "def5678", nil, rejectedRC, nil)
	if _, err := runUpgradePR(t, "v1.2.3", "--before", "abc"); err != nil {
		t.Fatal(err)
	}
	if !f.created {
		t.Errorf("a rejected PRE-RELEASE must not block the GA release of the same number; "+
			"pushed=%q created=%v", f.pushed, f.created)
	}

	// The guard itself still has to work at the exact version.
	rejected := []upgradePR{{Head: BranchName("v1.2.3", "abc1234"), State: "CLOSED"}}
	f = stubForge(t, "def5678", nil, rejected, nil)
	if _, err := runUpgradePR(t, "v1.2.3", "--before", "abc"); err != nil {
		t.Fatal(err)
	}
	if f.created {
		t.Error("a rejected upgrade for THIS exact version must still stop the run")
	}
}

func TestUpgradePRCountsTheRawPageForTruncation(t *testing.T) {
	// Truncation is a property of the RESPONSE, so the count has to be taken
	// BEFORE the stem filter. Counting survivors meant one fuzzy non-match on a
	// full page dropped the tally to 299 and waved through a page that was missing
	// rows — the single thing the guard exists to refuse.
	og, op, oc, oe := gitOut, pushBranch, createPR, execSeam
	t.Cleanup(func() { gitOut, pushBranch, createPR, execSeam = og, op, oc, oe })

	pushed := ""
	gitOut = func(args ...string) (string, error) {
		if args[0] == "rev-parse" {
			return "def\n", nil
		}
		return "", nil
	}
	pushBranch = func(b string) error { pushed = b; return nil }
	createPR = func(_, _, _ string) error { return nil }

	// A full page, one row of which the stem filter drops — which is what `gh`'s
	// fuzzy `head:` search really returns.
	full := make([]string, prListLimitN)
	for i := range full {
		full[i] = fmt.Sprintf("chore/template-upgrade-v1.0.0-%07d CLOSED", i)
	}
	full[0] = "feat/something-else OPEN"
	execSeam = func(string, ...string) ([]byte, error) { return []byte(strings.Join(full, "\n")), nil }

	_, err := runUpgradePR(t, "v1.0.0", "--before", "abc")
	if err == nil || !strings.Contains(err.Error(), "may be truncated") {
		t.Errorf("a full page must be unknown even when a row is filtered out, got %v", err)
	}
	if pushed != "" {
		t.Error("nothing may be pushed on a page that might be truncated")
	}
}

func TestCreatePRBodyStopsClaimingDraftOnTheFallback(t *testing.T) {
	// Read off the REAL argv, like TestUpgradePROpensADraft: the body is the only
	// surface the REVIEWER sees, and on the non-draft fallback the fixed body told
	// them plan-cluster-pr was skipped on a pull request that in fact selects it —
	// the state-writing job --draft exists to keep away from. The ::warning goes to
	// a run log nobody reading the PR opens.
	orig := execSeam
	t.Cleanup(func() { execSeam = orig })
	var bodies []string
	execSeam = func(_ string, args ...string) ([]byte, error) {
		for i, a := range args {
			if a == "--body" && i+1 < len(args) {
				bodies = append(bodies, args[i+1])
			}
		}
		if len(bodies) == 1 {
			return nil, fmt.Errorf("draft pull requests are not supported in this repository")
		}
		return nil, nil
	}
	if err := createPR("t", "main", "br"); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 {
		t.Fatalf("expected a draft attempt and a non-draft retry, got %d", len(bodies))
	}
	if !strings.Contains(bodies[0], "opens as a DRAFT") {
		t.Error("the draft attempt must carry the draft explanation")
	}
	if strings.Contains(bodies[1], "opens as a DRAFT") {
		t.Error("the non-draft body must NOT tell the reviewer the PR is a draft")
	}
	if !strings.Contains(bodies[1], "cannot open draft pull requests") {
		t.Errorf("the non-draft body must say WHY it is not a draft, got:\n%s", bodies[1])
	}
	// IT MUST NOT WARN ABOUT A HAZARD THAT NO LONGER EXISTS. This used to require
	// the words "tf-import" and a warning that the fallback exposed an unserialised
	// write to cluster/<deployment>/terraform.tfstate. plan-cluster-pr is retired,
	// so there is no tf-import on any pull-request path — and a scary paragraph
	// about a job that cannot run costs a reviewer the same attention as a real one
	// while teaching them the body is not to be trusted.
	for _, gone := range []string{"tf-import", "terraform.tfstate", "concurrent apply"} {
		if strings.Contains(bodies[1], gone) {
			t.Errorf("the non-draft body still warns about %q, retired with plan-cluster-pr:\n%s",
				gone, bodies[1])
		}
	}
}

func TestUpgradePRAnnouncesThePRAfterOpeningIt(t *testing.T) {
	// "Pull request opened" is an OUTCOME. It used to be printed from the decision,
	// before the push — so a failed push left an annotation naming a branch that
	// does not exist and the operator went looking for a PR nobody could find.
	// Same class as the step-summary move, which did not cover the annotation.
	og, ob, op, oc := gitOut, upgradePRs, pushBranch, createPR
	t.Cleanup(func() { gitOut, upgradePRs, pushBranch, createPR = og, ob, op, oc })
	gitOut = func(args ...string) (string, error) {
		if args[0] == "rev-parse" {
			return "def5678\n", nil
		}
		return "", nil
	}
	upgradePRs = func(string, string) ([]upgradePR, error) { return nil, nil }
	createPR = func(_, _, _ string) error { return nil }

	pushBranch = func(string) error { return fmt.Errorf("remote rejected") }
	out, err := runUpgradePR(t, "v1.0.0", "--before", "abc")
	if err == nil {
		t.Fatal("a failed push must fail the run")
	}
	if strings.Contains(out, "Pull request opened") {
		t.Errorf("a failed push must not announce a pull request, got:\n%s", out)
	}

	pushBranch = func(string) error { return nil }
	out, err = runUpgradePR(t, "v1.0.0", "--before", "abc")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Pull request opened") {
		t.Errorf("a successful create must be announced, got:\n%s", out)
	}
}

func TestUpgradePRTreatsAMergedVersionAsShippedEvenWithAClosedOne(t *testing.T) {
	// `--state closed` returns CLOSED and MERGED, and they mean opposite things. A
	// version can carry BOTH — a first attempt closed, a later --force run merged —
	// and then the version is in the tree. The loop used to break on the first
	// CLOSED row, so any later legitimate re-commit at that pin (a `managed` file
	// restored from drift) was refused as "a reviewer rejected this upgrade",
	// forever, at exit 0.
	both := []upgradePR{
		{Head: BranchName("v1.2.3", "aaaaaaa"), State: "CLOSED"},
		{Head: BranchName("v1.2.3", "bbbbbbb"), State: "MERGED"},
	}
	f := stubForge(t, "ccccccc", nil, both, nil)
	if _, err := runUpgradePR(t, "v1.2.3", "--before", "abc"); err != nil {
		t.Fatal(err)
	}
	if !f.created {
		t.Errorf("a version that SHIPPED must not stay blocked by an earlier closed attempt; "+
			"pushed=%q created=%v", f.pushed, f.created)
	}
}

func TestUpgradePRForceSurvivesAnUnreadableGuardQuery(t *testing.T) {
	// --force overrides both guards, so on that path the query only decorates an
	// annotation. Hard-failing on it discarded an upgrade the operator explicitly
	// asked for, after `llz upgrade --commit` had already done the work, over a
	// question whose answer changes nothing.
	f := stubForge(t, "def5678", nil, nil, fmt.Errorf("gh: API rate limit exceeded"))
	out, err := runUpgradePR(t, "v1.0.0", "--before", "abc", "--force")
	if err != nil {
		t.Fatalf("--force must not discard the upgrade over an unreadable guard query: %v", err)
	}
	if !f.created {
		t.Error("--force must still propose")
	}
	if !strings.Contains(out, "Guard state unknown") {
		t.Errorf("the unreadable query must still be said out loud, got:\n%s", out)
	}

	// Without --force the answer decides, so refusing to guess is still right.
	f = stubForge(t, "def5678", nil, nil, fmt.Errorf("gh: API rate limit exceeded"))
	if _, err := runUpgradePR(t, "v1.0.0", "--before", "abc"); err == nil {
		t.Error("an unforced run must still refuse to act on a guess")
	}
	if f.created {
		t.Error("nothing may be proposed when the guards cannot be read")
	}
}
