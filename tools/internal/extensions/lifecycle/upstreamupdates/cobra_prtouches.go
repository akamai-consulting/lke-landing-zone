package upstreamupdates

// cobra_prtouches.go — CLI surface for Classify: fetch the PR's files, classify
// them, write the GitHub Actions output. Transport and wiring only; the judgement
// is in prtouches.go and is tested without a network.

import (
	"fmt"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghaout"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
	"github.com/spf13/cobra"
)

// listChangedFiles returns every path a pull request changed, computed from the
// LOCAL CHECKOUT rather than from the pulls API. Seamed so the command is
// testable without git.
//
// WHY NOT `gh api repos/{}/pulls/{}/files`, WHICH IS THE OBVIOUS CALL. That
// endpoint needs `pull-requests: read`, and this verb runs in a job of a called
// reusable workflow. A callee job may never ask for more than the CALLING job
// holds — GitHub validates that while parsing the graph, before any `if:` is
// evaluated — and every caller of llz-terraform.yml holds `contents: read`. So
// the API version turned the whole pipeline into a `startup_failure`: no jobs, no
// logs, no annotations, on every PR plan, every dispatched apply and every
// promotion stage. It shipped, and the caller-permissions guard passed it,
// because that guard only compared WRITE escalations.
//
// The fix could have been `pull-requests: read` on each caller. It was not, for a
// delivery reason: terraform.yml is a `merge`-class file, so that line arrives
// through a 3-way merge an adopter's local edit can decline — and the failure it
// would reintroduce is the silent one. Staying inside `contents: read` needs no
// caller to cooperate.
//
// BOTH ENDS ARE EXPLICIT, and HEAD is not one of them. On a pull_request event
// actions/checkout leaves HEAD at the MERGE ref, whose first parent is the base
// tip — so `base...HEAD` compares the base against a commit that already contains
// the base's own newer commits, and three-dot degenerates because the merge base
// of those two IS base. Base-branch drift then reads as part of the PR's diff, and
// a stale bot upgrade PR classifies terraform=true and takes the unserialized
// tfstate write this job exists to prevent.
//
// base.sha...head.sha is the PR's own changes by construction: three-dot against
// the real merge base of the two branch tips, and neither end depends on what the
// checkout happens to have put at HEAD.
var listChangedFiles = func(baseSHA, headSHA string) ([]string, error) {
	b, err := kubectlprobe.Exec("git", "diff", "--name-only", baseSHA+"..."+headSHA)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.TrimSpace(line); f != "" {
			out = append(out, f)
		}
	}
	return out, nil
}

// PRTouchesCmd is `llz ci pr-touches`.
func PRTouchesCmd() *cobra.Command {
	var (
		prefixes         []string
		outputName       string
		baseSHA, headSHA string
	)
	c := &cobra.Command{
		Use:   "pr-touches",
		Short: "classify whether a pull request's diff touches given path prefixes",
		Long: "Writes `<name>=true|false` to $GITHUB_OUTPUT for use in a downstream job or\n" +
			"step `if:`. A prefix ending in `/` matches a subtree; anything else must match\n" +
			"the whole path.\n\n" +
			"Built for one caller: llz-terraform.yml gates its `llz ci tf-import` step on\n" +
			"this, because that step WRITES cluster/<deployment>/terraform.tfstate with\n" +
			"nothing serializing it against a concurrent apply. Confining the write to PRs\n" +
			"that change Terraform or the spec is what makes it safe to run this pipeline on\n" +
			"an automated template-upgrade PR at all.\n\n" +
			"FAILS rather than answering `false` when it cannot tell: an unreachable API or\n" +
			"an empty file list is an error, because a silent `false` skips the import on\n" +
			"every PR and looks exactly like a clean tree.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if baseSHA == "" || headSHA == "" {
				return fmt.Errorf("--base-sha and --head-sha are both required: they are the pull request's " +
					"base and head tips (github.event.pull_request.base.sha / .head.sha). HEAD is NOT a " +
					"substitute — on a pull_request event it is the merge ref, which already contains the " +
					"base's newer commits, so the diff would count base-branch drift as this PR's changes")
			}
			files, err := listChangedFiles(baseSHA, headSHA)
			if err != nil {
				return fmt.Errorf("diff %s...%s: %w\n"+
					"  this is a 'could not tell', not a 'nothing changed' — the caller must fail rather than skip.\n"+
					"  the checkout needs full history (fetch-depth: 0) for both commits to be present", baseSHA, headSHA, err)
			}
			cl, err := Classify(files, prefixes)
			if err != nil {
				return err
			}
			cl.Report(cmd.ErrOrStderr())
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", strings.Join(cl.Files, "\n"))
			return ghaout.Append("GITHUB_OUTPUT", fmt.Sprintf("%s=%t", outputName, cl.Touches))
		},
	}
	c.Flags().StringArrayVar(&prefixes, "prefix", nil, "path prefix to match; trailing / means a subtree (repeatable)")
	c.Flags().StringVar(&outputName, "output-name", "touches", "name of the GITHUB_OUTPUT key to write")
	c.Flags().StringVar(&baseSHA, "base-sha", "", "the PR's base tip (github.event.pull_request.base.sha)")
	c.Flags().StringVar(&headSHA, "head-sha", "", "the PR's head tip (github.event.pull_request.head.sha)")
	return c
}
