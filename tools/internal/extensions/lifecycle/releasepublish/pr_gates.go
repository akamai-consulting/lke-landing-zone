package releasepublish

// pr_gates.go implements `llz ci assert-instance-pr-gates` — the release-e2e
// instantiate job's PROOF THAT THE DELIVERED pull_request-GATED CI ACTUALLY RUNS,
// moved out of inline workflow bash into unit-tested Go (the repo's untestable-loc
// design principle, the same move pin_images.go records).
//
// WHY THE ASSERTION EXISTS. The instance's Terraform Lint and Checkov jobs are
// gated `if: github.event_name == 'pull_request'` behind a paths: filter. The
// throwaway fixture repo is driven entirely by force-push to main, workflow_dispatch
// and schedule — so those two jobs had never executed, here or in any real instance,
// and shipped broken for several releases: they ran `make tf-lint` against a scaffold
// that has no Makefile. A GATE NOTHING TRIGGERS IS INDISTINGUISHABLE FROM A GATE
// THAT PASSES, which is the vacuous-green shape this tree refuses everywhere.
//
// TestDeliveredWorkflowCommands (internal/cli) catches that specific spelling
// statically, at PR time, in the template repo. This verb covers what a static gate
// structurally cannot: that the commands RESOLVE AND SUCCEED in the pinned image, on
// the real scaffold. It runs after pin-instance-images so TF_IMAGE is this commit's.
//
// IT ASSERTS THE pull_request-GATED JOBS, AND THE PR IS OPENED AS A DRAFT SO ONLY
// THOSE CAN RUN. Not tidiness — a correctness requirement. The same paths:
// filter also selects `Plan Cluster (PR)`, and that job runs `llz ci tf-import`,
// which WRITES cluster/<env>/terraform.tfstate. The e2e's provision job dispatches
// an apply against that very state the moment this one returns, the two run under
// DIFFERENT concurrency groups (terraform-infra-pr vs terraform-infra-<env>), and
// the s3 backend has no lock — so a plan still in flight and the apply would race
// each other's state writes, last write wins. Closing the PR does not cancel a
// running job either. The delivered pipeline therefore skips its credential- and
// state-touching plan on DRAFT pull requests (llz-terraform.yml plan-cluster-pr),
// and this verb opens a draft: the two cheap, local, read-only lint jobs still run,
// and nothing this verb triggers can touch Terraform state.
//
// ── THE BUG THE BASH HAD, AND WHY THE PORT IS NOT A TRANSCRIPTION ──────────────
// The inline version polled with `gh pr checks ... || echo '[]'`. `gh pr checks`
// EXITS NON-ZERO BY DESIGN: 1 when a check has failed, 8 when checks are still
// pending. So the `||` fallback fired on exactly the two outcomes the step exists to
// distinguish, replaced them both with an empty set, and the empty set was then
// reported as "the gates never ran" — the WRONG diagnosis for a gate that ran and
// FAILED, pointing an operator at the paths: filter instead of at the broken job.
// Here the exit status is ignored and stdout is parsed on its own merits, which is
// what `gh` intends: the status is a summary of the checks, not an I/O error.
//
// I/O is behind package-var seams (gatesGH / gatesGit / gatesSleep / gatesTempDir /
// gatesAppend / gatesRemoveAll) so the decision logic and the wait loop are tested
// without a forge.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// DefaultPRGateChecks are the delivered check names this verb requires. They are
// the `name:` of the pull_request-gated jobs in
// instance-template/.github/workflows/llz-terraform.yml; renaming a job there
// without renaming it here makes this verb report "never appeared", which is the
// correct and loud answer — and TestPRGateCheckNamesMatchTheDeliveredJobs fails
// in the template repo first, so the rename never reaches an e2e.
//
// REPO READINESS IS ASSERTED TOO, not just the two linters. It is a
// pull_request-gated job that had never run anywhere either — the same starting
// position as tf-lint and checkov — and it was added by the very change set that
// built this probe. Triggering it on the throwaway PR while asserting nothing
// about its verdict would leave the newest delivered gate in exactly the
// unobserved state this verb exists to end.
var DefaultPRGateChecks = []string{
	"Terraform Lint",
	"Checkov IaC Security Scan",
	"Repo readiness (upgrade prerequisites)",
}

// DefaultPRGateTouchPath is a file inside the terraform pipeline's paths: filter
// (`terraform-iac-bootstrap/**`). Touching it is what SELECTS the gated jobs — a PR
// that changes nothing the filter watches opens cleanly and runs neither job, which
// would look identical to the regression this verb hunts.
//
// IT MUST BE A *TRACKED* FILE, AND ALMOST NOTHING UNDER THAT ROOT IS. The first
// draft touched `terraform-iac-bootstrap/cluster/versions.tf`, which cannot work:
// that tree's .gitignore excludes `*/*.tf` because an instance commits ZERO
// Terraform code — the roots are rendered on the fly by `llz render` from the
// embedded tfroots package. The file is not merely unstaged, it is not there at
// all, so the commit would have been empty and the step would have failed on every
// single run. Four files are tracked under the root: .gitignore, AGENTS.md, and two
// .terraform.lock.hcl provider pins. AGENTS.md is the only one that is pure prose,
// so appending to it cannot change what tflint or checkov parse.
//
// IT IS ALSO `managed` AND DIGEST-LOCKED (.template-managed.lock), which is
// latent rather than broken: nothing runs `llz ci managed-fresh` on an instance
// PR today, so appending to it trips no gate. The day a delivered job DOES check
// the lock on pull requests, this probe fails itself — the throwaway PR would be
// reported as drifting from the template it came from. Whoever adds that gate
// needs to pick a different touch target (a tracked, unlocked file under the
// same paths: filter) or teach the gate to ignore this probe's branch.
const DefaultPRGateTouchPath = "terraform-iac-bootstrap/AGENTS.md"

// Seams (package vars) so tests drive the flow without a forge, a clone or a clock.
var (
	// gatesGH runs `gh <args>` with GH_TOKEN set. Stdout comes back even when the
	// command exits non-zero — see the header: `gh pr checks` uses its exit status
	// to report the CHECKS' verdict, not its own success.
	//
	// STDERR IS FOLDED INTO THE ERROR, and that is the whole diagnostic. `Output()`
	// captures stdout only, so without this every `gh` failure — an under-scoped
	// token, an unparseable flag, a repo that does not exist — collapses into the
	// identical, useless `exit status 1`, while the one line gh wrote saying which
	// of those it was is discarded. The ARGV is deliberately not echoed: `gh` takes
	// its credential from the environment, but keeping the two error builders in
	// this file to the same rule is what stops the next one from echoing a command
	// line that does carry one (see gatesGit).
	// GH_HOST IS PASSED, NOT ASSUMED. --host reached the clone URL and nothing
	// else, so every `gh` call went to whatever host the ambient environment
	// named. That worked only because e2e-instantiate.yml happens to export
	// GH_HOST at workflow level — i.e. the flag was inert and the lane was
	// carrying it, which is exactly the arrangement that breaks the first time
	// this verb is called from anywhere else (a GHES lane, an operator's laptop,
	// a future caller). The flag now means what it says.
	gatesGH = func(token, host string, args ...string) ([]byte, error) {
		cmd := exec.Command("gh", args...)
		cmd.Env = append(os.Environ(), "GH_TOKEN="+token)
		if host != "" {
			cmd.Env = append(cmd.Env, "GH_HOST="+host)
		}
		out, err := cmd.Output()
		return out, ghError(args, err)
	}
	// gatesGit runs `git -C dir <args>`. Errors fold in stderr but NEVER the argv:
	// the clone URL carries the token, so an echoed command line would print a
	// credential into a public actions log.
	gatesGit = func(dir string, args ...string) ([]byte, error) {
		full := append([]string{"-C", dir}, args...)
		out, err := exec.Command("git", full...).CombinedOutput()
		if err != nil {
			return out, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
		}
		return out, nil
	}
	gatesSleep     = func(d time.Duration) { time.Sleep(d) }
	gatesTempDir   = func() (string, error) { return os.MkdirTemp("", "llz-pr-gates-") }
	gatesRemoveAll = func(path string) error { return os.RemoveAll(path) }
	// gatesAppend appends to a file, creating it if absent.
	gatesAppend = func(path, s string) error {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		_, err = f.WriteString(s)
		return err
	}
)

// ghError names the failed `gh` call and folds its STDERR into the message.
// Pure, so the folding is tested without a forge — see TestGHErrorCarriesStderr.
// nil in, nil out: a successful call must not be turned into an error.
func ghError(args []string, err error) error {
	if err == nil {
		return nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(bytes.TrimSpace(ee.Stderr)) > 0 {
		err = fmt.Errorf("%w: %s", err, truncate(strings.TrimSpace(string(ee.Stderr)), 500))
	}
	return fmt.Errorf("gh %s: %w", strings.Join(args, " "), err)
}

// PRGatesOpts is the verb's input.
type PRGatesOpts struct {
	Instance  string        // owner/name of the throwaway instance repo
	SHA       string        // the template commit under test — names the branch
	Host      string        // forge host for the clone URL and every gh call (github.com, or a GHES appliance)
	Base      string        // base branch for the throwaway PR; empty = ask the repo for its default
	Token     string        // contents:write + pull-requests:write on the instance repo
	TouchPath string        // repo-relative file to touch so the paths: filter selects the jobs
	Checks    []string      // check names that must appear AND succeed
	Interval  time.Duration // between polls
	Retries   int           // max polls
	Keep      bool          // leave the branch/PR behind for a human to read
}

// check is one row of `gh pr checks --json name,state`.
type check struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

// pendingStates are the states that mean "not yet decided". Anything else — including
// a state this list does not know — counts as settled, so an unfamiliar terminal state
// ends the wait instead of burning the whole budget and then reporting a timeout.
var pendingStates = map[string]bool{"PENDING": true, "QUEUED": true, "IN_PROGRESS": true, "WAITING": true, "REQUESTED": true}

func isPending(state string) bool { return pendingStates[strings.ToUpper(state)] }

// matchesCheck reports whether an observed check name is the wanted job.
//
// A BARE NAME IS NOT ENOUGH, AND ASSUMING IT WAS WOULD HAVE MADE THIS VERB REPORT
// "never ran" FOREVER. The instance's tf-lint and checkov jobs live in the REUSABLE
// llz-terraform.yml, which terraform.yml invokes from a job named `call`. GitHub
// names a called workflow's checks `<caller job> / <called job>`, so what actually
// arrives is `call / Terraform Lint` — and an exact match against "Terraform Lint"
// finds nothing, which this verb reports as the very regression it hunts. A gate
// that cannot pass is worth no more than one that cannot fail.
//
// The suffix is anchored on the " / " separator rather than matched loosely, so
// "Terraform Lint (extra)" still does not satisfy "Terraform Lint": the point is to
// tolerate the caller-job prefix GitHub prepends, not to accept any name that
// happens to contain the wanted one.
func matchesCheck(observed, want string) bool {
	return observed == want || strings.HasSuffix(observed, "/ "+want)
}

// partitionChecks splits a `gh pr checks` payload into the wanted checks that
// APPEARED and the wanted names that did not.
func partitionChecks(raw []byte, want []string) (found []check, missing []string, err error) {
	var all []check
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, nil, fmt.Errorf("parsing gh pr checks output: %w", err)
	}
	for _, w := range want {
		hit := false
		for _, c := range all {
			if matchesCheck(c.Name, w) {
				found = append(found, c)
				hit = true
				break
			}
		}
		if !hit {
			missing = append(missing, w)
		}
	}
	return found, missing, nil
}

// settled reports whether every found check has reached a terminal state.
func settled(found []check) bool {
	for _, c := range found {
		if isPending(c.State) {
			return false
		}
	}
	return true
}

// pendingAfterWait returns the found checks still un-decided once the wait is over.
func pendingAfterWait(found []check) []check {
	var out []check
	for _, c := range found {
		if isPending(c.State) {
			out = append(out, c)
		}
	}
	return out
}

func namesOf(cs []check) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

// inconclusiveStates are terminal states that are NOT a verdict on the gate.
//
// CANCELLED is the one that matters and the one that was miscounted. It is NOT
// concurrency-group supersession — llz-terraform.yml sets cancel-in-progress:
// false, so that group queues and never cancels, and an earlier draft of this
// comment said the opposite. It is a run somebody or something cancelled: a
// force-push to the throwaway branch while the checks were in flight, a manual
// cancel, or the whole workflow run being cancelled. SKIPPED and NEUTRAL mean the job did
// not execute — its `if:` excluded it — which is a real regression in the
// delivered gating but a different one from "the check ran and the command
// inside it failed". Both still FAIL the verb (nothing was proven either way);
// they just must not be reported as "a delivered CI gate does not work in the
// scaffold it ships to", which sends an operator to debug a job that never ran.
// This is the same distinction the header insists on for PENDING, applied to the
// terminal states that carry no verdict either.
var inconclusiveStates = map[string]bool{"CANCELLED": true, "SKIPPED": true, "NEUTRAL": true, "STALE": true}

// inconclusive returns the found checks that reached a terminal state carrying no
// verdict about the gate.
func inconclusive(found []check) []check {
	var out []check
	for _, c := range found {
		if inconclusiveStates[strings.ToUpper(c.State)] {
			out = append(out, c)
		}
	}
	return out
}

// failures returns the found checks that RAN, FINISHED, and did not succeed — a
// verdict about the gate, as distinct from the inconclusive states above and from
// the ones that have not finished at all.
//
// EXCLUDING THE PENDING STATES IS LOAD-BEARING, and it was previously done for it
// by branch ORDER: the caller checked "still pending" first, so `failures` never
// saw an IN_PROGRESS check. Moving the failure branch above the timeout one — so
// a real failure outranks a merely stuck sibling — made that accident visible,
// and every unfinished check briefly counted as a broken gate. A predicate that
// only holds because of the order it is called in is a predicate waiting to be
// reordered.
func failures(found []check) []check {
	var out []check
	for _, c := range found {
		switch {
		case strings.EqualFold(c.State, "SUCCESS"), isPending(c.State),
			inconclusiveStates[strings.ToUpper(c.State)]:
			continue
		}
		out = append(out, c)
	}
	return out
}

// observedNames lists every check name in the payload, for the "never appeared"
// diagnostic. Being told WHAT ran is the difference between "the paths: filter no
// longer covers the touched path" and "the job was renamed" — the two causes of a
// missing gate, which the bash version could not tell apart because its --jq filter
// discarded every name it was not looking for.
func observedNames(raw []byte) []string {
	var all []check
	if json.Unmarshal(raw, &all) != nil {
		return nil
	}
	names := make([]string, 0, len(all))
	for _, c := range all {
		names = append(names, c.Name)
	}
	return names
}

// prBranch names the throwaway branch after the commit under test, so a leaked
// branch (--keep, or a killed run) says which e2e produced it.
func prBranch(sha string) string {
	if len(sha) > 8 {
		sha = sha[:8]
	}
	return "e2e/ci-gates-" + sha
}

// RunAssertInstancePRGates opens a throwaway PR on the instance repo, waits for the
// delivered pull_request-gated checks, and asserts they both ran and passed.
func RunAssertInstancePRGates(o PRGatesOpts) error {
	for _, v := range []struct{ name, val string }{
		{"--instance", o.Instance}, {"--sha", o.SHA}, {"GH_TOKEN", o.Token},
	} {
		if v.val == "" {
			return fmt.Errorf("assert-instance-pr-gates: %s is required", v.name)
		}
	}
	if len(o.Checks) == 0 {
		o.Checks = DefaultPRGateChecks
	}
	if o.TouchPath == "" {
		o.TouchPath = DefaultPRGateTouchPath
	}
	if o.Host == "" {
		o.Host = "github.com"
	}
	if o.Retries < 1 {
		o.Retries = 60
	}
	// A zero interval turns the retry budget into back-to-back `gh` calls at the
	// forge. The cobra layer rejects it outright; this is the floor for a
	// programmatic caller that never went through a flag.
	if o.Interval <= 0 {
		o.Interval = 20 * time.Second
	}
	if o.Base == "" {
		o.Base = defaultBranch(o)
	}

	work, err := gatesTempDir()
	if err != nil {
		return fmt.Errorf("assert-instance-pr-gates: temp dir: %w", err)
	}
	defer func() { _ = gatesRemoveAll(work) }()

	branch := prBranch(o.SHA)
	// REGISTERED BEFORE THE BRANCH EXISTS, because openGatePR pushes it and can
	// then fail: `gh pr create` with a token provisioned from the older
	// contents+actions wording (the case this lane's own docs describe) leaves
	// e2e/ci-gates-<sha> on the fixture repo with nothing that removes it. The
	// PR-close defer below cannot cover that window — there is no PR yet. Deleting
	// a branch that was never pushed is a harmless no-op, so this is registered
	// unconditionally and its result ignored, exactly like the close.
	if !o.Keep {
		defer func() {
			_, _ = gatesGH(o.Token, o.Host, "api", "-X", "DELETE",
				fmt.Sprintf("repos/%s/git/refs/heads/%s", o.Instance, branch))
		}()
	}
	pr, err := openGatePR(o, work, branch)
	if err != nil {
		return err
	}
	fmt.Printf("Opened %s#%s\n", o.Instance, pr)
	if !o.Keep {
		// Cleanup is best-effort and deliberately unchecked: the fixture repo's
		// default branch is force-pushed every run, so a leaked throwaway branch
		// costs nothing — while failing the verb on a cleanup error would turn a
		// PASSING assertion into a red e2e for a reason that is not the subject.
		defer func() {
			_, _ = gatesGH(o.Token, o.Host, "pr", "close", pr, "--repo", o.Instance, "--delete-branch")
		}()
	}

	obs := awaitGateChecks(o, pr)
	for _, c := range obs.found {
		fmt.Printf("  %s: %s\n", c.Name, c.State)
	}

	// FIRST, because it invalidates every verdict below. A poll loop that never once
	// read `gh` cleanly knows nothing about the checks — reporting that silence as
	// "the gates never ran" blames the instance's CI for this verb's own blindness.
	// Note this cannot fire for a PR that simply has no checks: that path records an
	// empty observation, so raw is non-nil and the "never ran" branch owns it.
	if obs.parseErr != nil && obs.raw == nil {
		return fmt.Errorf("::error title=Instance PR gates could not be read::could not read the checks on %s#%s "+
			"after %d attempt(s) — this says nothing about the gates themselves: %w",
			o.Instance, pr, o.Retries, redact(obs.parseErr, o.Token))
	}
	if len(obs.missing) > 0 {
		seen := observedNames(obs.raw)
		// NO CHECKS AT ALL IS A DIFFERENT STORY FROM SOME-BUT-NOT-OURS, and the
		// old wording told only the second one. If OTHER checks appeared and ours
		// did not, the paths: filter or a rename really is the cause. If NOTHING
		// appeared, the likeliest explanation is that the run has not started —
		// the pipeline's concurrency group is cancel-in-progress: false, so a run
		// QUEUES behind whatever is already in flight, and this lane's own
		// force-push to the fixture repo puts one there. Naming a filter and a
		// rename first, for a run that simply had not begun, is the same
		// I/O-as-verdict conflation this file rejects for PENDING, one level up.
		seenMsg := "no checks appeared at all — most often the run has not STARTED " +
			"(the pipeline's concurrency group does not cancel, so a run queues behind one already in " +
			"flight); after that, a paths: filter that no longer selects the touched file"
		if len(seen) > 0 {
			seenMsg = "checks that DID appear: " + strings.Join(seen, ", ")
		}
		// gh's own words on an empty answer, when there are any. "no checks reported
		// on the 'x' branch" is a confirmation of the diagnosis, not competition
		// with it — and it is the difference between a filter that missed and a PR
		// that was never opened against the right base.
		seenMsg += gateContext(obs, o.Token)
		return fmt.Errorf("::error title=Instance PR gates never ran::%s did not appear on %s#%s "+
			"within %s. The jobs are pull_request-gated behind a paths: filter watching %s (%s)",
			strings.Join(obs.missing, " / "), o.Instance, pr,
			time.Duration(o.Retries-1)*o.Interval, o.TouchPath, seenMsg)
	}
	if bad := failures(obs.found); len(bad) > 0 {
		var parts []string
		for _, c := range bad {
			parts = append(parts, fmt.Sprintf("%s=%s", c.Name, c.State))
		}
		return fmt.Errorf("::error title=Instance PR gates failed::a delivered CI gate does not work in the "+
			"scaffold it ships to (%s). See %s#%s", strings.Join(parts, ", "), o.Instance, pr)
	}
	// PENDING IS NOT FAILED. Reporting a check that simply never finished as "a
	// delivered CI gate does not work in the scaffold it ships to" sends an operator
	// to debug a job that may be perfectly healthy and merely slow, or queued behind
	// a busy runner. The two need different words because they need different work.
	//
	// AFTER the failure branch, for the reason the inconclusive branch already
	// records: a gate that RAN AND FAILED alongside one that is merely stuck was
	// reported as "This is a TIMEOUT, not a failing gate" and never named. A
	// failure is knowledge; an unfinished check is its absence. Same ordering bug,
	// one branch up, left unfixed when the other was pinned.
	if stuck := pendingAfterWait(obs.found); len(stuck) > 0 {
		// The I/O context belongs HERE TOO, not only on the never-appeared branch.
		// A good first observation followed by a run of failing polls leaves the
		// checks looking permanently pending, and reporting that as a plain
		// timeout is the same I/O-as-verdict mistake this file records fixing
		// twice already: the checks may well have settled while we could not ask.
		return fmt.Errorf("::error title=Instance PR gates did not finish::%s still %s after %s%s. "+
			"This is a TIMEOUT, not a failing gate — raise --timeout, or look for a queued/stuck run on %s#%s",
			strings.Join(namesOf(stuck), " / "), strings.ToLower(stuck[0].State),
			time.Duration(o.Retries-1)*o.Interval, gateContext(obs, o.Token), o.Instance, pr)
	}
	// AFTER the failure branch, and that ordering was wrong the first time. A run
	// with one FAILURE and one CANCELLED check reported "This is NOT a failing
	// gate" and never named the gate that actually broke — the inconclusive
	// sibling hid the definite verdict. A failure is knowledge; an inconclusive
	// state is the absence of it, so the failure is the headline whenever there is
	// one, and this branch owns only the case where NOTHING failed outright.
	if odd := inconclusive(obs.found); len(odd) > 0 {
		var parts []string
		for _, c := range odd {
			parts = append(parts, fmt.Sprintf("%s=%s", c.Name, c.State))
		}
		return fmt.Errorf("::error title=Instance PR gates returned no verdict::%s. This is NOT a failing gate. "+
			"CANCELLED means the run was cancelled while the checks were in flight — a force-push to the "+
			"throwaway branch, or a manual cancel; the pipeline's concurrency group does NOT cancel "+
			"(cancel-in-progress: false), so re-running is the fix. SKIPPED or NEUTRAL means the job's `if:` "+
			"excluded it, which IS a regression in the delivered gating: check the pull_request / head-repo "+
			"conditions on %s#%s", strings.Join(parts, ", "), o.Instance, pr)
	}
	fmt.Printf("All %d PR-gated CI check(s) passed on %s#%s\n", len(o.Checks), o.Instance, pr)
	return nil
}

// openGatePR clones the instance repo, commits a no-op change under the paths:
// filter, pushes the branch and opens the PR. Returns the PR number.
func openGatePR(o PRGatesOpts, work, branch string) (string, error) {
	repo := filepath.Join(work, "repo")
	cloneURL := fmt.Sprintf("https://x-access-token:%s@%s/%s.git", o.Token, o.Host, o.Instance)
	// CLONED AT THE BASE BRANCH, not at whatever the repo defaults to. `--depth 1`
	// with no --branch lands on the default branch, and the throwaway branch is cut
	// from wherever we land — so with any non-default --base the PR's head would be
	// unrelated to its base: either a diff carrying every commit on default-but-not-
	// in-base (so the paths: filter selects far more than the one file we touched,
	// and the "gates ran" verdict stops being about our change) or an outright
	// `gh pr create` "No commits between" failure. o.Base is resolved before this
	// point precisely so it can be used here.
	if _, err := gatesGit(work, "clone", "-q", "--depth", "1", "--branch", o.Base, cloneURL, repo); err != nil {
		return "", fmt.Errorf("assert-instance-pr-gates: cloning %s: %w", o.Instance, redact(err, o.Token))
	}
	for _, argv := range [][]string{
		{"config", "user.name", "llz-release-e2e[bot]"},
		{"config", "user.email", "llz-release-e2e@users.noreply.github.com"},
		{"checkout", "-q", "-b", branch},
	} {
		if _, err := gatesGit(repo, argv...); err != nil {
			return "", fmt.Errorf("assert-instance-pr-gates: %w", redact(err, o.Token))
		}
	}
	// A trailing comment changes no behavior and still lands in the diff, which is
	// all the paths: filter needs to select the gated jobs.
	touch := filepath.Join(repo, o.TouchPath)
	// An HTML comment: the touch target is Markdown, so this renders as nothing and
	// changes no document. gatesAppend does NOT create the file — a missing touch
	// target means the paths: filter's tracked surface moved, which must fail loudly
	// rather than silently commit a new file the filter may not even watch.
	line := fmt.Sprintf("\n<!-- e2e: exercise the pull_request-gated CI gates (%s) -->\n", shortSHA(o.SHA))
	if err := gatesAppend(touch, line); err != nil {
		return "", fmt.Errorf("assert-instance-pr-gates: touching %s (is it still in the paths: filter?): %w",
			o.TouchPath, err)
	}
	for _, argv := range [][]string{
		{"commit", "-qam", "e2e: trigger PR-gated CI gates"},
		// FORCE, because the branch is named after the commit under test: a re-run
		// of the same sha (a retry, or a run whose cleanup was killed) finds its own
		// leaked branch and a plain push dies on non-fast-forward. The branch is
		// this verb's own throwaway and nothing else may write it, so there is no
		// other history to lose.
		{"push", "-q", "--force", "origin", branch},
	} {
		if _, err := gatesGit(repo, argv...); err != nil {
			return "", fmt.Errorf("assert-instance-pr-gates: %w", redact(err, o.Token))
		}
	}
	// --draft is load-bearing, not cosmetic: it is what keeps the state-writing
	// Plan Cluster job off this PR. See the file header.
	out, err := gatesGH(o.Token, o.Host, "pr", "create", "--repo", o.Instance, "--base", o.Base, "--head", branch,
		"--draft",
		"--title", "e2e: PR-gated CI gates",
		"--body", "Throwaway: proves tf-lint + checkov run in the pinned image. Draft on purpose — a draft PR does not run the state-writing plan job.")
	if pr := prNumber(out); pr != "" {
		return pr, nil
	}
	// A re-run of the same commit finds its branch and PR already there; `gh pr
	// create` then fails with "already exists". Ask for the existing one rather
	// than treating a resumable state as fatal.
	//
	// `pr list --state open`, NOT `pr view <branch>`, AND THAT IS A FALSE-GREEN
	// FIX. The branch is named after the commit under test and this verb CLOSES
	// its PR on the way out, so a second run at the same sha leaves a closed PR
	// on exactly that branch. `gh pr view <branch>` resolves closed PRs happily —
	// so the re-run would have adopted the PREVIOUS run's number and reported its
	// old, already-settled checks as this run's verdict. A gate whose entire
	// purpose is refusing a green that proves nothing must not be able to inherit
	// one.
	viewOut, viewErr := gatesGH(o.Token, o.Host, "pr", "list", "--repo", o.Instance,
		"--head", branch, "--state", "open", "--json", "number", "--jq", ".[0].number")
	if viewErr == nil {
		if pr := prNumber(viewOut); pr != "" {
			return pr, nil
		}
	}
	// err IS NIL ON A REACHABLE PATH — `gh pr create` exits 0 but prints something
	// prNumber cannot read (a survey prompt, a "no commits between" notice, an
	// enterprise banner). Wrapping a nil error with %w renders the literal
	// `%!w(<nil>)` and says nothing about what actually happened, so synthesize the
	// error from what we DID see instead.
	if err == nil {
		fallback := fmt.Sprintf("returned %q", strings.TrimSpace(string(viewOut)))
		if viewErr != nil {
			fallback = "failed: " + viewErr.Error()
		}
		err = fmt.Errorf("`gh pr create` succeeded but printed no PR URL (got %q), and no OPEN pull request "+
			"was found for head %s (%s)", strings.TrimSpace(string(out)), branch, fallback)
	}
	return "", fmt.Errorf("assert-instance-pr-gates: opening a PR on %s: %w", o.Instance, redact(err, o.Token))
}

// awaitGateChecks polls until every wanted check has appeared and settled, or the
// budget runs out. It returns the LAST observation rather than an error on timeout:
// "still pending after N polls" and "never appeared" are reported by the caller from
// the same data, so a timeout cannot masquerade as a clean run.
//
// parseErr IS RETURNED, NOT DROPPED, and it is the same lesson twice. An earlier
// draft ignored partitionChecks' error on every poll, so output `gh` produced but
// this verb could not read — a changed --json shape, an HTML error page, a proxy
// banner ahead of the JSON — left `missing` untouched and `raw` nil, and the caller
// reported that as "the gates never ran". That is the exact misdiagnosis the file
// header records the bash version making: an I/O problem dressed up as a verdict
// about the instance's CI, pointing an operator at a paths: filter that is fine.
// nil once ANY poll parses, so a single transient bad read is not held against a
// run that went on to observe cleanly.
//
// GARBAGE IS NOT SILENCE, AND THE DISTINCTION IS THE WHOLE VERB. `gh pr checks`
// on a PR with NO checks at all exits non-zero with EMPTY stdout — and a PR that
// triggered nothing is precisely the regression this verb hunts (a paths: filter
// that stopped covering the touched path). The first draft of the parseErr
// handling treated empty-stdout-plus-error as "could not read", which put the
// I/O diagnosis in front of the "never ran" one in the one case that matters:
// the verb would have reported its own blindness instead of the missing gates.
// So empty stdout is recorded as an OBSERVATION OF ZERO CHECKS; only non-empty
// output this verb cannot parse counts as unreadable. gh's own error is kept
// separately, as context for whichever verdict the caller reaches.
// SLEEPS BETWEEN POLLS, NOT AFTER THE LAST ONE. With a trailing sleep the loop
// spends Retries*Interval waiting on top of Retries polls, so a 900s budget
// actually consumed 900s PLUS the polling — overrunning the job-timeout
// arithmetic that budget was chosen to fit, and making the timeout message
// understate the wait it reports. The last iteration has nothing left to wait
// for.
func awaitGateChecks(o PRGatesOpts, pr string) gateObservation {
	obs := gateObservation{missing: append([]string(nil), o.Checks...)}
	sleepUnlessLast := func(i int) {
		if i < o.Retries-1 {
			gatesSleep(o.Interval)
		}
	}
	for i := 0; i < o.Retries; i++ {
		// Exit status ignored as a verdict — see the file header — but kept as
		// context: it is the only place gh explains an empty answer.
		out, ghErr := gatesGH(o.Token, o.Host, "pr", "checks", pr, "--repo", o.Instance, "--json", "name,state")
		// TrimSpace, not len(out): a bare "\n" from a future gh, or from a PATH
		// shim, is silence rather than garbage — classifying it as unreadable would
		// report "could not read the checks" for a PR that genuinely triggered
		// nothing, which is the very regression this verb hunts.
		if len(bytes.TrimSpace(out)) == 0 {
			// Zero checks reported. Record it as an observation (so the caller
			// reaches "never appeared" rather than "could not read"), but never
			// over an earlier REAL observation — checks do not un-appear, and a
			// late blank answer must not erase what we already saw.
			//
			// AND IT MUST CLEAR parseErr, which is subtler than it looks. The
			// caller's unreadable branch is `parseErr != nil && raw == nil`; an
			// unreadable poll followed by a blank one used to leave parseErr SET
			// while raw became "[]", so neither branch owned the case and the
			// never-ran diagnosis was printed for what was really an I/O problem —
			// the exact misdiagnosis this plumbing exists to prevent, one poll
			// later. A newer, readable answer supersedes an older unreadable one;
			// the discarded payload is folded into the context so nothing is lost.
			if obs.parseErr != nil {
				// STICKY, and kept apart from ghErr on purpose: ghErr reflects the
				// LATEST poll and is overwritten by the next one, so folding the
				// discarded payload into it loses the payload on the very next
				// empty answer — which is how the first attempt at this still
				// dropped it. priorUnreadable is written once and only cleared by a
				// genuinely successful read.
				obs.priorUnreadable = fmt.Errorf("an earlier poll was unreadable: %w", obs.parseErr)
				obs.parseErr = nil
			}
			obs.ghErr = ghErr
			if obs.raw == nil {
				obs.raw = []byte("[]")
			}
			sleepUnlessLast(i)
			continue
		}
		f, m, err := partitionChecks(out, o.Checks)
		if err != nil {
			obs.parseErr = fmt.Errorf("%w (gh printed %q)", err, truncate(strings.TrimSpace(string(out)), 300))
			sleepUnlessLast(i)
			continue
		}
		obs.found, obs.missing, obs.raw = f, m, out
		obs.parseErr, obs.ghErr, obs.priorUnreadable = nil, nil, nil
		if len(m) == 0 && settled(f) {
			return obs
		}
		sleepUnlessLast(i)
	}
	return obs
}

// gateObservation is the last thing the poll loop saw. It carries the two
// failure modes SEPARATELY because they demand different words from the caller:
// parseErr means this verb could not read gh's answer and knows nothing about the
// gates; ghErr is gh's own complaint about a PR that reported no checks, which is
// a fact ABOUT the gates and belongs in that diagnosis rather than in place of it.
type gateObservation struct {
	found    []check
	missing  []string
	raw      []byte // last payload; non-nil once anything at all was observed
	parseErr error  // gh produced output this verb could not parse
	ghErr    error  // gh could not answer on the LATEST poll (context, not a verdict)
	// priorUnreadable remembers a payload an EARLIER poll could not parse, once a
	// later readable answer superseded it. Sticky, so a run of empty polls cannot
	// quietly drop the one piece of evidence that something was wrong with the I/O.
	priorUnreadable error
}

// truncate bounds an untrusted payload before it lands in an error message, so a
// megabyte of HTML cannot bury the sentence explaining it.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// prNumber pulls the PR number out of `gh pr create` (which prints the PR URL) or
// `gh pr list --jq .[0].number` (which prints the bare number).
func prNumber(out []byte) string {
	s := strings.TrimSpace(string(out))
	if s == "" {
		return ""
	}
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimSpace(s)
	for _, r := range s {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return s
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// redact strips the token from an error's text. The clone URL embeds it, and git
// echoes the remote back on failure — so without this a bad credential prints the
// credential into a public actions log.
func redact(err error, token string) error {
	if err == nil || token == "" {
		return err
	}
	if s := err.Error(); strings.Contains(s, token) {
		return fmt.Errorf("%s", strings.ReplaceAll(s, token, "***"))
	}
	return err
}

// gateContext renders whatever I/O trouble the poll loop met, as a suffix for
// whichever verdict the caller reached. Empty when the polling was clean.
//
// It is shared by the never-appeared and the timeout branches on purpose: both
// verdicts are only as good as the reads behind them, and an operator told
// "still pending after 15m" deserves to know the last four polls errored.
func gateContext(obs gateObservation, token string) string {
	var out string
	// parseErr BELONGS HERE TOO. It was left out, so one good poll followed by a
	// run of UNPARSEABLE ones produced a bare "still in_progress after 15m — this
	// is a TIMEOUT" with no hint that every later read had failed. That is the
	// omission this helper was extracted to fix, reintroduced by only carrying two
	// of the three ways a poll can go wrong.
	for _, ctx := range []error{obs.ghErr, obs.parseErr, obs.priorUnreadable} {
		if ctx != nil {
			out += " — " + strings.TrimSpace(redact(ctx, token).Error())
		}
	}
	return out
}

// defaultBranch asks the instance repo which branch to open the PR against.
//
// `--base main` WAS HARDCODED, and the delivered terraform.yml watches
// `branches: [main, master]` — so against a master-default instance this verb
// force-pushed a throwaway branch and could then never open a PR for it, leaving
// the branch behind and reporting a forge error instead of a verdict. Asking is
// one API call and cannot be wrong; "main" remains the fallback when the call
// fails, which is the previous behavior rather than a new failure mode.
func defaultBranch(o PRGatesOpts) string {
	out, err := gatesGH(o.Token, o.Host, "repo", "view", o.Instance, "--json", "defaultBranchRef", "--jq", ".defaultBranchRef.name")
	if name := strings.TrimSpace(string(out)); err == nil && name != "" {
		return name
	}
	// err IS NIL on the reachable "succeeded but said nothing" path, and `%v` would
	// then print the literal `(<nil>)` — naming no cause at all for a fallback that
	// may well target a branch this repo does not have.
	why := "it returned no branch name"
	if err != nil {
		why = redact(err, o.Token).Error()
	}
	fmt.Fprintf(os.Stderr, "::warning::could not read %s's default branch (%s) — opening the throwaway PR "+
		"against `main`, which may not exist on this repo\n", o.Instance, why)
	return "main"
}
