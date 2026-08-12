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
//   - An upgrade that leaves the tree dirty changed files the commit did not
//     capture. That is not "nothing to do"; it is a manifest class that was
//     neither restored nor committed, and it must be said out loud rather than
//     swept into the PR or dropped.

import (
	"fmt"
	"io"
	"strings"
)

// State is what the repo looked like after `llz upgrade` ran.
type State struct {
	BeforeSHA    string // HEAD recorded before the upgrade
	AfterSHA     string // HEAD now
	Dirty        string // `git status --porcelain` output; empty when clean
	Version      string // the llz version the upgrade targeted
	RemoteHas    bool   // an OPEN pull request already exists for the computed branch
	OrphanBranch bool   // the branch exists but no PR does — a create that failed after the push
}

// Decision is what to do about it.
type Decision struct {
	OpenPR  bool
	Branch  string
	Reason  string // why not, when OpenPR is false
	Warning string // said regardless of the verdict
}

// BranchName is the branch an upgrade to version lands on.
//
// It carries the VERSION rather than a date or a fixed name so that a later
// release opens its own pull request instead of retargeting an older one that is
// still open — and so a re-run for the same version is recognisably the same
// branch rather than a second copy of it.
func BranchName(version string) string {
	v := strings.TrimSpace(version)
	if v == "" {
		v = "unknown"
	}
	// Slashes would nest a ref under the branch namespace and collide; spaces are
	// not legal in a ref at all.
	v = strings.NewReplacer("/", "-", " ", "", "\n", "").Replace(v)
	return "chore/template-upgrade-" + v
}

// Decide reads the post-upgrade state and returns what should happen.
func Decide(s State) Decision {
	d := Decision{Branch: BranchName(s.Version)}
	if s.Dirty != "" {
		// Deliberately a warning on BOTH paths, not an error: refusing to open the
		// PR would discard a good upgrade over residue, and swallowing it would ship
		// a PR whose diff is not the whole change.
		d.Warning = "llz upgrade left uncommitted changes, which are NOT in this PR:\n" + s.Dirty
	}
	switch {
	case s.AfterSHA == s.BeforeSHA:
		d.Reason = "llz upgrade produced no commit — this instance is already on the target release"
	case s.RemoteHas:
		d.Reason = fmt.Sprintf("%s already has an open pull request: an earlier run opened it and it has not been "+
			"merged. Leaving it alone rather than force-pushing over a diff someone may be reviewing", d.Branch)
	default:
		d.OpenPR = true
	}
	return d
}

// Report writes the decision as GitHub Actions annotations plus a step summary.
func (d Decision) Report(w io.Writer, s State) {
	if d.Warning != "" {
		fmt.Fprintf(w, "::warning title=Uncommitted residue::%s\n", strings.ReplaceAll(d.Warning, "\n", "%0A"))
	}
	if !d.OpenPR {
		fmt.Fprintf(w, "::notice title=No pull request::%s\n", d.Reason)
		return
	}
	fmt.Fprintf(w, "::notice title=Upgrade ready::opening %s for %s\n", d.Branch, s.Version)
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
	if d.Warning != "" {
		fmt.Fprintf(&b, "\n> **Uncommitted residue** — not included in this PR:\n>\n> ```\n> %s\n> ```\n",
			strings.ReplaceAll(strings.TrimSpace(s.Dirty), "\n", "\n> "))
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
const prBody = "Automated template upgrade — opened by `.github/workflows/template-upgrade.yml`.\n" +
	"\n" +
	"### What this is\n" +
	"The scaffold, the Terraform module refs, the `platform-apl` kustomize refs and\n" +
	"the in-cluster image tag all render from one answer (`llz_version` in\n" +
	"`.copier-answers.yml`), so they move together.\n" +
	"\n" +
	"### Review it like a human upgrade, because it is one\n" +
	"- Every `owned` file was restored and every `managed` file overwritten from a\n" +
	"  clean render. The upgrade refuses to commit over a merge conflict or an\n" +
	"  answer regression, so neither is hiding in this diff.\n" +
	"- The Terraform plan, lint and **repo-readiness** checks on this PR are the\n" +
	"  gates that matter. repo-readiness is what catches a newly mandatory secret\n" +
	"  this release needs and this repo does not have.\n" +
	"- Anything under `apl-values/<env>/` or `environments/` is yours and was left\n" +
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
