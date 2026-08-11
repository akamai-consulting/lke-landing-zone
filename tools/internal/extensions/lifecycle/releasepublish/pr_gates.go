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
// IT ASSERTS ONLY THE TWO LINT GATES. The PR also triggers Plan Cluster, which wants
// cloud credentials and state — out of scope here, and asserting on it would make
// this verb fail for reasons that have nothing to do with it.
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
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// DefaultPRGateChecks are the delivered check names this verb requires. They are
// the `name:` of the two jobs in instance-template/.github/workflows/llz-terraform.yml
// that are pull_request-gated; renaming a job there without renaming it here makes
// this verb report "never appeared", which is the correct and loud answer.
var DefaultPRGateChecks = []string{"Terraform Lint", "Checkov IaC Security Scan"}

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
const DefaultPRGateTouchPath = "terraform-iac-bootstrap/AGENTS.md"

// Seams (package vars) so tests drive the flow without a forge, a clone or a clock.
var (
	// gatesGH runs `gh <args>` with GH_TOKEN set. Stdout comes back even when the
	// command exits non-zero — see the header: `gh pr checks` uses its exit status
	// to report the CHECKS' verdict, not its own success.
	gatesGH = func(token string, args ...string) ([]byte, error) {
		cmd := exec.Command("gh", args...)
		cmd.Env = append(os.Environ(), "GH_TOKEN="+token)
		out, err := cmd.Output()
		return out, err
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

// PRGatesOpts is the verb's input.
type PRGatesOpts struct {
	Instance  string        // owner/name of the throwaway instance repo
	SHA       string        // the template commit under test — names the branch
	Host      string        // forge host for the clone URL (github.com, or a GHES appliance)
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

// failures returns the found checks that did not succeed.
func failures(found []check) []check {
	var out []check
	for _, c := range found {
		if !strings.EqualFold(c.State, "SUCCESS") {
			out = append(out, c)
		}
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

	work, err := gatesTempDir()
	if err != nil {
		return fmt.Errorf("assert-instance-pr-gates: temp dir: %w", err)
	}
	defer func() { _ = gatesRemoveAll(work) }()

	branch := prBranch(o.SHA)
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
			_, _ = gatesGH(o.Token, "pr", "close", pr, "--repo", o.Instance, "--delete-branch")
		}()
	}

	found, missing, raw := awaitGateChecks(o, pr)
	for _, c := range found {
		fmt.Printf("  %s: %s\n", c.Name, c.State)
	}

	if len(missing) > 0 {
		seen := observedNames(raw)
		seenMsg := "no checks appeared at all"
		if len(seen) > 0 {
			seenMsg = "checks that DID appear: " + strings.Join(seen, ", ")
		}
		return fmt.Errorf("::error title=Instance PR gates never ran::%s did not appear on %s#%s. "+
			"The jobs are pull_request-gated behind a paths: filter — either the filter no longer covers %s, "+
			"or the jobs were removed or renamed (%s)",
			strings.Join(missing, " / "), o.Instance, pr, o.TouchPath, seenMsg)
	}
	// PENDING IS NOT FAILED. Reporting a check that simply never finished as "a
	// delivered CI gate does not work in the scaffold it ships to" sends an operator
	// to debug a job that may be perfectly healthy and merely slow, or queued behind
	// a busy runner. The two need different words because they need different work.
	if stuck := pendingAfterWait(found); len(stuck) > 0 {
		return fmt.Errorf("::error title=Instance PR gates did not finish::%s still %s after %s. "+
			"This is a TIMEOUT, not a failing gate — raise --timeout, or look for a queued/stuck run on %s#%s",
			strings.Join(namesOf(stuck), " / "), strings.ToLower(stuck[0].State),
			time.Duration(o.Retries)*o.Interval, o.Instance, pr)
	}
	if bad := failures(found); len(bad) > 0 {
		var parts []string
		for _, c := range bad {
			parts = append(parts, fmt.Sprintf("%s=%s", c.Name, c.State))
		}
		return fmt.Errorf("::error title=Instance PR gates failed::a delivered CI gate does not work in the "+
			"scaffold it ships to (%s). See %s#%s", strings.Join(parts, ", "), o.Instance, pr)
	}
	fmt.Printf("Both PR-gated CI checks passed on %s#%s\n", o.Instance, pr)
	return nil
}

// openGatePR clones the instance repo, commits a no-op change under the paths:
// filter, pushes the branch and opens the PR. Returns the PR number.
func openGatePR(o PRGatesOpts, work, branch string) (string, error) {
	repo := filepath.Join(work, "repo")
	cloneURL := fmt.Sprintf("https://x-access-token:%s@%s/%s.git", o.Token, o.Host, o.Instance)
	// Cloned via `git -C work clone <url> repo` so the seam stays one shape.
	if _, err := gatesGit(work, "clone", "-q", "--depth", "1", cloneURL, repo); err != nil {
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
	out, err := gatesGH(o.Token, "pr", "create", "--repo", o.Instance, "--base", "main", "--head", branch,
		"--title", "e2e: PR-gated CI gates",
		"--body", "Throwaway: proves tf-lint + checkov run in the pinned image.")
	if pr := prNumber(out); pr != "" {
		return pr, nil
	}
	// A re-run of the same commit finds its branch and PR already there; `gh pr
	// create` then fails with "already exists". Ask for the existing one rather
	// than treating a resumable state as fatal.
	if out, viewErr := gatesGH(o.Token, "pr", "view", branch, "--repo", o.Instance, "--json", "number", "--jq", ".number"); viewErr == nil {
		if pr := prNumber(out); pr != "" {
			return pr, nil
		}
	}
	return "", fmt.Errorf("assert-instance-pr-gates: opening a PR on %s: %w", o.Instance, redact(err, o.Token))
}

// awaitGateChecks polls until every wanted check has appeared and settled, or the
// budget runs out. It returns the LAST observation rather than an error on timeout:
// "still pending after N polls" and "never appeared" are reported by the caller from
// the same data, so a timeout cannot masquerade as a clean run.
func awaitGateChecks(o PRGatesOpts, pr string) (found []check, missing []string, raw []byte) {
	missing = append([]string(nil), o.Checks...)
	for i := 0; i < o.Retries; i++ {
		// Exit status ignored on purpose — see the file header.
		out, _ := gatesGH(o.Token, "pr", "checks", pr, "--repo", o.Instance, "--json", "name,state")
		if len(out) > 0 {
			f, m, err := partitionChecks(out, o.Checks)
			if err == nil {
				found, missing, raw = f, m, out
				if len(m) == 0 && settled(f) {
					return found, missing, raw
				}
			}
		}
		gatesSleep(o.Interval)
	}
	return found, missing, raw
}

// prNumber pulls the PR number out of `gh pr create` (which prints the PR URL) or
// `gh pr view --jq .number` (which prints the bare number).
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
