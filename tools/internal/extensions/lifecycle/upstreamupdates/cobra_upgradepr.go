package upstreamupdates

// cobra_upgradepr.go — CLI surface for Decide: read git state, act on the
// verdict. Transport and wiring only; the judgement is in upgradepr.go and is
// tested without git or GitHub.

import (
	"fmt"
	"os"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghaout"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/gitcmd"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
	"github.com/spf13/cobra"
)

// Seams: every side effect the command has, so a test drives the whole decision
// path without a repo or a network.
var (
	gitOut = func(args ...string) (string, error) { return gitcmd.Output(".", args...) }

	// An OPEN PULL REQUEST, not merely a branch. Branch existence was the proxy,
	// and it is wrong in the one direction that matters: this command pushes before
	// it calls `gh pr create`, so a refused create (a 422, a revoked token, a
	// network blip) leaves the branch on the remote with no PR attached. Every
	// later run for that version then saw the branch, reported "already open" and
	// exited 0 — the silent success this file's header says it exists to prevent,
	// arrived at through the recovery path.
	//
	// Now: branch with an open PR -> leave it alone. Branch with NO open PR -> the
	// push landed and the create did not, so open the PR for what is already there.
	remoteHasOpenPR = func(branch string) bool {
		out, err := kubectlprobe.Exec("gh", "pr", "list", "--head", branch, "--state", "open", "--json", "number", "--jq", "length")
		if err != nil {
			// Cannot tell. Treat as "no PR" so the run tries to create one: a
			// duplicate create fails loudly and is trivially closed, whereas a false
			// "already open" leaves an upgrade silently unopened month after month.
			return false
		}
		return strings.TrimSpace(string(out)) != "" && strings.TrimSpace(string(out)) != "0"
	}

	remoteHasBranch = func(branch string) bool {
		_, err := kubectlprobe.Exec("git", "ls-remote", "--exit-code", "--heads", "origin", branch)
		return err == nil
	}

	pushBranch = func(branch string) error {
		if _, err := kubectlprobe.Exec("git", "switch", "-c", branch); err != nil {
			return fmt.Errorf("create branch %s: %w", branch, err)
		}
		if _, err := kubectlprobe.Exec("git", "push", "origin", branch); err != nil {
			return fmt.Errorf("push %s: %w", branch, err)
		}
		return nil
	}

	createPR = func(title, body, base, head string) error {
		// --label is dropped on retry rather than required: a repo that has never
		// created the label 422s, and losing the whole pull request over a
		// decoration would be a bad trade.
		_, err := kubectlprobe.Exec("gh", "pr", "create", "--title", title, "--body", body,
			"--base", base, "--head", head, "--label", "template-upgrade")
		if err == nil {
			return nil
		}
		_, err = kubectlprobe.Exec("gh", "pr", "create", "--title", title, "--body", body,
			"--base", base, "--head", head)
		return err
	}
)

// UpgradePRCmd is `llz ci upgrade-pr`.
func UpgradePRCmd() *cobra.Command {
	var beforeSHA, base string
	c := &cobra.Command{
		Use:   "upgrade-pr",
		Short: "open a pull request for an `llz upgrade` that CI has just committed",
		Long: "Reads the repo state left by `llz upgrade --commit` and, if it produced a\n" +
			"commit, pushes a version-named branch and opens a pull request for it.\n\n" +
			"`llz upgrade` exits 0 both when it upgraded and when the instance was already\n" +
			"current, so --before is how this tells those apart: HEAD moving is the only\n" +
			"honest signal that there is anything to push.\n\n" +
			"Refuses to touch a branch that already exists on the remote — that is an\n" +
			"earlier run's unmerged pull request, and force-pushing over it would replace a\n" +
			"diff someone may be reviewing.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if beforeSHA == "" {
				return fmt.Errorf("--before is required: without the pre-upgrade HEAD there is no way to tell " +
					"an upgrade from an instance that was already current")
			}
			// Named before anything else so the error is the missing secret rather
			// than whatever `gh pr create` says about authentication forty seconds
			// later, by which time a branch has already been pushed.
			if os.Getenv("GH_TOKEN") == "" && os.Getenv("GITHUB_TOKEN") == "" {
				return fmt.Errorf("no GH_TOKEN (or GITHUB_TOKEN) in the environment: the automated template upgrade " +
					"needs the LLZ_AUTOMATION_TOKEN repository secret to push a branch and open its pull request. " +
					"GITHUB_TOKEN cannot substitute — a pull request it opens runs no checks. See docs/secrets.md")
			}
			after, err := gitOut("rev-parse", "HEAD")
			if err != nil {
				return fmt.Errorf("read HEAD: %w", err)
			}
			dirty, err := gitOut("status", "--porcelain")
			if err != nil {
				return fmt.Errorf("read working tree status: %w", err)
			}
			// THE PIN, AND ONLY THE PIN. This used to try `git describe --tags` first
			// and fall back here, which contradicted the comment explaining it and was
			// wrong in a way that stayed green: an instance repo carries no llz tags,
			// but it may well carry the ADOPTER's own tags, and with fetch-depth: 0
			// one of those wins. The branch is then named after their release rather
			// than the llz release — and because that name does not change when llz
			// does, the next month's upgrade finds the branch still on the remote and
			// declines "already open" forever, silently, while reporting success.
			version := pinnedVersion()
			s := State{
				BeforeSHA: strings.TrimSpace(beforeSHA),
				AfterSHA:  strings.TrimSpace(after),
				Dirty:     strings.TrimSpace(dirty),
				Version:   strings.TrimSpace(version),
			}
			// Both, because they mean different things. An open PR is "leave it
			// alone"; a branch without one is an orphan from a failed create, and the
			// push below must be skipped for it while the create still runs.
			branch := BranchName(s.Version)
			if s.AfterSHA != s.BeforeSHA {
				s.RemoteHas = remoteHasOpenPR(branch)
				s.OrphanBranch = !s.RemoteHas && remoteHasBranch(branch)
			}

			d := Decide(s)
			d.Report(cmd.ErrOrStderr(), s)

			// THE SUMMARY IS WRITTEN AFTER THE WORK, NOT AFTER THE DECISION. It used
			// to go out here, so a failed push or a refused `gh pr create` left
			// "opened a pull request: `true`" and a branch name standing in the run
			// summary for a branch that does not exist — the run reads as a
			// successful upgrade and the operator goes looking for a PR nobody can
			// find. Decide() says what SHOULD happen; only the calls below know what
			// DID.
			summarise := func() {
				if err := ghaout.Append("GITHUB_STEP_SUMMARY", d.Summary(s)); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "::warning::could not write the step summary: %v\n", err)
				}
			}
			if !d.OpenPR {
				summarise()
				return nil
			}
			if base == "" {
				base = os.Getenv("GITHUB_REF_NAME")
			}
			if base == "" {
				return fmt.Errorf("no base branch: pass --base or set GITHUB_REF_NAME")
			}
			// Skipped for an orphan: the branch is already on the remote with the
			// commit on it, and `git switch -c` would fail on a name that exists.
			if !s.OrphanBranch {
				if err := pushBranch(d.Branch); err != nil {
					return err
				}
			}
			if err := createPR("chore(template): upgrade to "+s.Version, prBody, base, d.Branch); err != nil {
				return err
			}
			summarise()
			return nil
		},
	}
	c.Flags().StringVar(&beforeSHA, "before", "", "HEAD as recorded before `llz upgrade` ran (required)")
	c.Flags().StringVar(&base, "base", "", "base branch for the pull request (default: $GITHUB_REF_NAME)")
	return c
}

// pinnedVersion reads llz_version back out of .copier-answers.yml. A tiny reader
// rather than the answers package: this runs in a tree the upgrade has just
// rewritten, and it needs the one field.
var pinnedVersion = func() string {
	b, err := os.ReadFile(".copier-answers.yml")
	if err != nil {
		return "unknown"
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "llz_version:"); ok {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return "unknown"
}
