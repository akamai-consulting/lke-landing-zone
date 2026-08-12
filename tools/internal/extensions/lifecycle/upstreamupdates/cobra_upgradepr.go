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
			version, err := gitOut("describe", "--tags", "--abbrev=0")
			if err != nil || strings.TrimSpace(version) == "" {
				// The pin, not a git tag, is the version that matters — an instance repo
				// carries no llz tags at all. Read it back from the answers file the
				// upgrade just rewrote.
				version = pinnedVersion()
			}
			s := State{
				BeforeSHA: strings.TrimSpace(beforeSHA),
				AfterSHA:  strings.TrimSpace(after),
				Dirty:     strings.TrimSpace(dirty),
				Version:   strings.TrimSpace(version),
			}
			s.RemoteHas = s.AfterSHA != s.BeforeSHA && remoteHasBranch(BranchName(s.Version))

			d := Decide(s)
			d.Report(cmd.ErrOrStderr(), s)
			if err := ghaout.Append("GITHUB_STEP_SUMMARY", d.Summary(s)); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "::warning::could not write the step summary: %v\n", err)
			}
			if !d.OpenPR {
				return nil
			}
			if base == "" {
				base = os.Getenv("GITHUB_REF_NAME")
			}
			if base == "" {
				return fmt.Errorf("no base branch: pass --base or set GITHUB_REF_NAME")
			}
			if err := pushBranch(d.Branch); err != nil {
				return err
			}
			return createPR("chore(template): upgrade to "+s.Version, prBody, base, d.Branch)
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
