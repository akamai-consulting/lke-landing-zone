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
// Three-dot: the PR's own changes against the merge base, so a base branch that
// moved since the PR opened does not read as part of its diff.
var listChangedFiles = func(baseSHA string) ([]string, error) {
	b, err := kubectlprobe.Exec("git", "diff", "--name-only", baseSHA+"...HEAD")
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
		prefixes   []string
		outputName string
		baseSHA    string
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
			if baseSHA == "" {
				return fmt.Errorf("--base-sha is required: it is the commit this pull request is diffed against " +
					"(github.event.pull_request.base.sha). Without it there is nothing to compare and every PR " +
					"would classify the same way")
			}
			files, err := listChangedFiles(baseSHA)
			if err != nil {
				return fmt.Errorf("diff against %s: %w\n"+
					"  this is a 'could not tell', not a 'nothing changed' — the caller must fail rather than skip.\n"+
					"  the checkout needs full history (fetch-depth: 0) for the base commit to be present", baseSHA, err)
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
	c.Flags().StringVar(&baseSHA, "base-sha", "", "commit the PR is diffed against (github.event.pull_request.base.sha)")
	return c
}
