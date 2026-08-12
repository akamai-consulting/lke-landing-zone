package upstreamupdates

// cobra_prtouches.go — CLI surface for Classify: fetch the PR's files, classify
// them, write the GitHub Actions output. Transport and wiring only; the judgement
// is in prtouches.go and is tested without a network.

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghaout"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
	"github.com/spf13/cobra"
)

// listPRFiles returns every path a pull request changed. Seamed so the command
// is testable without GitHub.
//
// --paginate with per_page=100, not a bare call: the files endpoint defaults to
// 30 per page, and a template-upgrade PR routinely changes more than that. A
// truncated first page would drop exactly the Terraform files this is looking
// for and answer `false` with total confidence.
var listPRFiles = func(repo string, pr int) ([]string, error) {
	b, err := kubectlprobe.Exec("gh", "api", "--paginate",
		fmt.Sprintf("repos/%s/pulls/%d/files?per_page=100", repo, pr))
	if err != nil {
		return nil, err
	}
	var files []struct {
		Filename string `json:"filename"`
	}
	if err := json.Unmarshal(b, &files); err != nil {
		return nil, fmt.Errorf("decode files response: %w", err)
	}
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Filename)
	}
	return out, nil
}

// PRTouchesCmd is `llz ci pr-touches`.
func PRTouchesCmd() *cobra.Command {
	var (
		prefixes   []string
		outputName string
		repo       string
		pr         int
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
			if repo == "" {
				repo = os.Getenv("GITHUB_REPOSITORY")
			}
			if repo == "" {
				return fmt.Errorf("no repository: pass --repo or set GITHUB_REPOSITORY")
			}
			if pr == 0 {
				return fmt.Errorf("no pull request: pass --pr")
			}
			files, err := listPRFiles(repo, pr)
			if err != nil {
				return fmt.Errorf("list files for %s#%d: %w\n"+
					"  this is a 'could not tell', not a 'nothing changed' — the caller must fail rather than skip", repo, pr, err)
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
	c.Flags().StringVar(&repo, "repo", "", "owner/repo (default: $GITHUB_REPOSITORY)")
	c.Flags().IntVar(&pr, "pr", 0, "pull request number")
	return c
}
