package upstreamupdates

// upgradepr.go — turn the result of a CI `llz upgrade` into a reviewable pull
// request, or into an honest no-op.
//
// The decisions here are small individually and were 55 lines of workflow shell
// together. Three of them are worth naming, because each one has a wrong answer
// that looks like success:
//
//   - `llz upgrade` exits 0 both when it upgraded and when the instance was
//     already current. The COMMIT — HEAD moving — is the only honest signal that
//     there is anything to push.
//   - A branch that already exists on the remote is an earlier run's unmerged
//     PR. Force-pushing over it would silently replace a diff someone may be
//     halfway through reviewing.
//   - A branch name that does not move with the commit turns the "already
//     proposed" check from a safety into a permanent stop. Three cuts derived it
//     from the version alone; the SHA is what removed the whole class.
//
// (An earlier cut also warned about a dirty tree "not in this PR". It was dead
// code AND inverted: `llz upgrade --commit` does `git add -A`, so the tree is
// clean by the time this runs and any residue is committed INTO the PR, not left
// out of it.)

import (
	"fmt"
	"io"
	"strings"
)

// State is what the repo looked like after `llz upgrade` ran, plus what the forge
// already has.
type State struct {
	BeforeSHA string // HEAD recorded before the upgrade
	AfterSHA  string // HEAD now
	Version   string // the pin the upgrade left behind
	// OpenUpgradePR: an upgrade pull request from any earlier run is still open.
	OpenUpgradePR bool
	// RejectedThisVersion: a pull request for THIS version was closed unmerged.
	RejectedThisVersion bool
	// Forced: an operator dispatched this run with an explicit --ref. The two
	// guards exist to stop an UNATTENDED monthly run being noisy; a human who named
	// a version has already made that judgement, and silently discarding the
	// upgrade they asked for — after doing all the work — is the opposite of what
	// they requested. No flag could previously un-block a rejected version at all.
	Forced bool
}

// Decision is what to do about it.
type Decision struct {
	OpenPR   bool
	Branch   string
	Reason   string // why not, when OpenPR is false
	Override string // what --force stepped over, when it did
}

// branchStem is the prefix every upgrade branch shares. It is the identity this
// verb queries by — NOT a label, because createPR drops the label on a 422 retry
// and a guard keyed on something optional is a guard that goes missing.
const branchStem = "chore/template-upgrade-"

// VersionStem is the prefix of every branch proposing `version`.
func VersionStem(version string) string {
	v := strings.TrimSpace(version)
	if v == "" {
		v = "unknown"
	}
	return branchStem + sanitizeRef(v) + "-"
}

// sanitizeRef reduces a version to characters git accepts in a ref.
//
// AN ALLOW-LIST, because git's rules are a list of prohibitions that is easy to
// under-read: beyond the obvious slash and space it rejects ~ ^ : ? * [ and
// backslash, the sequence "..", a trailing dot, and a ".lock" suffix. The first
// cut replaced three characters and would have failed at `git push` — AFTER the
// upgrade had been committed, so the run died having done the work and thrown it
// away. Anything outside [A-Za-z0-9._-] becomes a dash.
func sanitizeRef(v string) string {
	var b strings.Builder
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := b.String()
	for strings.Contains(out, "..") {
		out = strings.ReplaceAll(out, "..", ".")
	}
	out = strings.Trim(out, ".-")
	out = strings.TrimSuffix(out, ".lock")
	if out == "" {
		return "unknown"
	}
	return out
}

// BranchName is the branch this run's commit lands on.
//
// ALWAYS UNIQUE, because the commit SHA is in it — and that is the whole design
// rather than a detail. Three previous cuts derived the branch from the version
// alone and used its existence as the interlock, and each fix opened the next
// hole: `--state open` could not see a rejected PR, `--state all` swallowed new
// work at an already-merged pin, and a merged-only rename produced branches no
// later run ever queried, so duplicates stacked and the reviewer guard became
// unreachable.
//
// A name that cannot collide removes all of it: no orphan recovery, no
// force-push, no lease, no spent-branch case. What a run may PROPOSE is decided
// by the two questions in Decide, and the git ref stops carrying that meaning.
func BranchName(version, sha string) string {
	return VersionStem(version) + shortSHA(sha)
}

// shortSHA disambiguates the branch. Seven characters: unique in practice, short
// enough that the branch still reads as an upgrade to a version.
func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// ProposesVersion reports whether `head` is an upgrade branch for EXACTLY
// `version`.
//
// NOT `strings.HasPrefix(head, VersionStem(version))`, which is what the
// rejected-version guard asked first and is wrong in one direction: the stem for
// `v1.2.3` is a prefix of `chore/template-upgrade-v1.2.3-rc1-abc1234`, so ONE
// closed pre-release pull request blocks the GA release of the same number —
// permanently, at exit 0, on a green run. Every month. `+build` metadata collides
// the same way, because sanitizeRef maps `+` to a dash.
//
// What separates the two is the suffix: BranchName appends shortSHA and nothing
// else, so an upgrade branch for THIS version ends in an abbreviated commit id.
// Anything left over that is not one — `rc1-abc1234` — belongs to a different
// version.
func ProposesVersion(head, version string) bool {
	rest, ok := strings.CutPrefix(strings.TrimSpace(head), VersionStem(version))
	if !ok || rest == "" {
		return false
	}
	for _, r := range rest {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// Decide reads the post-upgrade state and returns what should happen.
//
// TWO QUESTIONS, both about pull requests rather than about refs:
//
//	is an upgrade already awaiting review?  -> do not stack a second one
//	was THIS version already rejected?      -> do not hand it back every month
//
// Everything else opens a pull request. A create that fails after a successful
// push leaves a branch with no PR; the next run simply proposes again on a new
// name, so the work is never stranded and the stale ref is inert.
func Decide(s State) Decision {
	d := Decision{Branch: BranchName(s.Version, s.AfterSHA)}
	switch {
	case s.AfterSHA == s.BeforeSHA:
		d.Reason = "llz upgrade produced no commit — this instance is already on the target release"
	case s.OpenUpgradePR && !s.Forced:
		d.Reason = "an upgrade pull request from an earlier run is still open — leaving it to be reviewed " +
			"rather than stacking a second one behind it"
	case s.RejectedThisVersion && !s.Forced:
		d.Reason = fmt.Sprintf("a pull request for %s was closed unmerged: a reviewer rejected this upgrade, and "+
			"reopening it would hand back the same diff every month", strings.TrimSpace(s.Version))
	default:
		d.OpenPR = true
		if s.Forced && (s.OpenUpgradePR || s.RejectedThisVersion) {
			d.Override = "--force: proposing anyway, over an upgrade that is already open or a version that " +
				"was previously rejected"
		}
	}
	return d
}

// Report writes what was DECIDED, before any of it is attempted: the reason
// nothing will be proposed, or the guard an operator's --force stepped over.
//
// IT DOES NOT ANNOUNCE THE PULL REQUEST. That notice used to live here, so a run
// whose push or `gh pr create` failed still left "opening
// chore/template-upgrade-… " standing in the log for a branch that does not
// exist — an operator then goes looking for a pull request nobody can find. It is
// the same decision-vs-outcome class the step summary was moved for, and the
// summary move did not cover the annotation beside it. Opened() is what says a
// pull request exists, and only the caller may say it.
func (d Decision) Report(w io.Writer, s State) {
	if !d.OpenPR {
		fmt.Fprintf(w, "::notice title=No pull request::%s\n", d.Reason)
		return
	}
	if d.Override != "" {
		fmt.Fprintf(w, "::warning title=Guard overridden::%s\n", d.Override)
	}
	fmt.Fprintf(w, "::notice title=Upgrade ready::proposing %s for %s\n", d.Branch, strings.TrimSpace(s.Version))
}

// Opened is the annotation for a pull request that now exists. Called only after
// the push and the create have both returned nil.
func (d Decision) Opened(w io.Writer, s State) {
	fmt.Fprintf(w, "::notice title=Pull request opened::%s proposes %s\n", d.Branch, strings.TrimSpace(s.Version))
}

// Summary is the markdown written to $GITHUB_STEP_SUMMARY.
func (d Decision) Summary(s State) string {
	var b strings.Builder
	b.WriteString("### Template upgrade\n\n")
	fmt.Fprintf(&b, "- to: `%s`\n", strings.TrimSpace(s.Version))
	fmt.Fprintf(&b, "- opened a pull request: `%t`\n", d.OpenPR)
	if d.OpenPR {
		fmt.Fprintf(&b, "- branch: `%s`\n", d.Branch)
	} else {
		fmt.Fprintf(&b, "- why not: %s\n", d.Reason)
	}
	return b.String()
}

// prBody is the pull-request description.
//
// Written here rather than as a heredoc in the workflow for the budget reason,
// and kept here afterwards for a better one: TestDeliveredWorkflowCommands
// resolves every CLI invocation inside a `run:` script against the real cobra
// tree, and it does not exempt prose — so a backticked command in a workflow
// heredoc reds the gate. In Go it is just a string.
// prBody TAKES `envs` because the body's last section names the deployments a
// human has to dispatch after merging — see applyafter.go for why merging alone
// changes nothing, and why that is invisible in the diff.
//
// prBody TAKES `draft` BECAUSE THE BODY MAKES A CLAIM ABOUT IT. createPR drops
// --draft when the repository cannot open draft pull requests (a private repo on a
// Free plan), and a fixed body then tells the reviewer the state-writing
// plan-cluster-pr job is skipped on a pull request that in fact selects it. The
// only other signal is a ::warning in the run log, which nobody reading the pull
// request sees. Composed per attempt, so the body the reviewer reads describes the
// pull request that was actually opened.
func prBody(draft bool, envs []string) string {
	return prBodyHead + draftNote(draft) + prBodyTail + applySection(envs)
}

// draftNote is the one paragraph that differs between the two.
//
// BOTH HALVES WERE STALE, and the draft half was actively misleading. They
// described `plan-cluster-pr` — "marking it ready is what asks for the plan", and
// on the fallback a warning that its `llz ci tf-import` step would write
// cluster/<deployment>/terraform.tfstate against a concurrent apply. That job is
// RETIRED (see the note in llz-terraform.yml where it used to be: a plan against
// live state needs env-scoped credentials a pull_request run cannot hold, and the
// environment form fails the branch-policy lock). So there is no plan to ask for
// on any pull request, and no tf-import on any pull-request path to warn about.
//
// The prose outlived the job because it lives in Go and the job lived in YAML —
// the retirement edited the workflow and this text describes the workflow. What a
// reviewer was told, on every automated upgrade since, was that a check existed
// which they could summon and which would not run.
//
// The draft itself is KEPT and its reason restated rather than removed: a bot's
// pull request opening un-reviewed is worth signalling, and whether to keep
// drafting at all is a behaviour change this correction does not make.
func draftNote(draft bool) string {
	if draft {
		return "- This PR opens as a DRAFT because nothing has reviewed it yet. Note that no\n" +
			"  pull request runs a Terraform plan — that needs deployment-scoped credentials\n" +
			"  a pull_request run cannot hold — so marking it ready summons tflint, checkov\n" +
			"  and repo-readiness, not a diff against live state.\n"
	}
	return "- This repository cannot open draft pull requests (a private repo on a Free\n" +
		"  plan), so this one is not one. Nothing is lost by that: the draft only ever\n" +
		"  signalled that the PR is un-reviewed, and the state-writing job it used to\n" +
		"  hold off is retired.\n"
}

const prBodyHead = "Automated template upgrade — opened by `.github/workflows/template-upgrade.yml`.\n" +
	"\n" +
	"### What this is\n" +
	"The scaffold, the Terraform module refs, the `platform-apl` kustomize refs and\n" +
	"the in-cluster image tag all render from one answer (`llz_version` in\n" +
	"`.copier-answers.yml`), so they move together.\n" +
	"\n" +
	"### FIRST: bump the CI image variables\n" +
	"`repo-readiness` runs its image-freshness check BEFORE its required-config\n" +
	"check, and the image variables are repo-level — this\n" +
	"upgrade cannot push them. So until they move, that job fails on the image\n" +
	"check and never reaches the secrets check, which is the one that catches a\n" +
	"newly mandatory value. Set them, then mark this PR ready for review:\n" +
	"\n" +
	"```\n" +
	"gh variable set TF_IMAGE   --body ghcr.io/<org>/ci-tofu:sha-<the new pin>\n" +
	"gh variable set KUBE_IMAGE --body ghcr.io/<org>/ci-kubernetes:sha-<the new pin>\n" +
	"```\n" +
	"\n" +
	"The exact values are printed by the image-freshness step on this PR, and by\n" +
	"the upgrade run that opened it.\n" +
	"\n" +
	"### Review it like a human upgrade, because it is one\n" +
	"- Every `owned` file was restored and every `managed` file overwritten from a\n" +
	"  clean render. The upgrade refuses to commit over a merge conflict or an\n" +
	"  answer regression, so neither is hiding in this diff.\n"

const prBodyTail = "- Anything under `apl-values/<env>/` or `environments/` is yours and was left\n" +
	"  alone.\n" +
	"\n" +
	"### If this PR looks wrong\n" +
	"Close it and reproduce the same diff interactively:\n" +
	"\n" +
	"```\n" +
	"llz self-update && llz upgrade\n" +
	"```\n" +
	"\n" +
	"Nothing here is load-bearing until it merges.\n"
