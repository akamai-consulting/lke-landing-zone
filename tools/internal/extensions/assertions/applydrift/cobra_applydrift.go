package applydrift

// cobra_applydrift.go — CLI surface: find the last successful apply for this
// deployment, diff main against it, hand the result to Report. Transport and
// wiring only; the judgement is in applydrift.go and is tested without a forge.

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/gitcmd"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
	"github.com/spf13/cobra"
)

// execSeam is the single process hop this file makes, so a test can drive the
// whole path without gh or git.
var execSeam = kubectlprobe.Exec

// runsToScan bounds how far back to look for this deployment's last apply. An
// instance applies at most a few times a month, so 30 covers a year of history
// for a single-deployment instance and several months for a ranked one — and
// running out is an ERROR, never "up to date".
const runsToScan = 30

type ghRun struct {
	ID      int64  `json:"id"`
	HeadSHA string `json:"head_sha"`
}

// lastApply returns the head SHA of the newest successful terraform.yml dispatch
// whose jobs name this deployment.
//
// THE JOB NAME IS THE KEY, because the runs API does not expose a dispatch's
// inputs — there is no field saying which deployment a run applied.
// llz-terraform.yml names the chained job "Bootstrap OpenBao (<deployment>)", so
// that name is the only per-deployment evidence the forge carries. Both calls
// were verified against a live repo before this was written.
var lastApply = func(env string) (string, int, error) {
	out, err := execSeam("gh", "api",
		fmt.Sprintf("repos/{owner}/{repo}/actions/workflows/terraform.yml/runs?event=workflow_dispatch&status=success&per_page=%d", runsToScan),
		"--jq", ".workflow_runs[] | {id, head_sha} | tostring")
	if err != nil {
		return "", 0, fmt.Errorf("could not list successful terraform.yml runs: %w\n"+
			"    refusing to guess: with no apply history this cannot tell 'up to date' from 'never applied'", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	checked := 0
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		var r ghRun
		if jerr := json.Unmarshal([]byte(l), &r); jerr != nil {
			return "", checked, fmt.Errorf("could not read run %q: %w", l, jerr)
		}
		checked++
		jobs, jerr := execSeam("gh", "api",
			fmt.Sprintf("repos/{owner}/{repo}/actions/runs/%d/jobs", r.ID), "--jq", ".jobs[].name")
		if jerr != nil {
			return "", checked, fmt.Errorf("could not read the jobs of run %d: %w\n"+
				"    refusing to guess: a run whose jobs cannot be read might be this deployment's last apply", r.ID, jerr)
		}
		// "(<env>)" and not a bare substring: a deployment called `prod` must not
		// match `Bootstrap OpenBao (prod-web)`.
		if strings.Contains(string(jobs), "("+env+")") {
			return r.HeadSHA, checked, nil
		}
	}
	return "", checked, fmt.Errorf("no successful apply of %q found in the last %d dispatched terraform.yml runs.\n"+
		"    This is NOT 'up to date': it means the history does not reach back to this deployment's last apply,\n"+
		"    or it has never been applied. Either way the question is unanswered, so this fails rather than pass",
		env, checked)
}

// changedSince lists the files main has that the applied commit did not.
var changedSince = func(sha string) ([]string, error) {
	out, err := gitcmd.Output(".", "diff", "--name-only", sha+"...HEAD")
	if err != nil {
		return nil, fmt.Errorf("could not diff against the last applied commit %s: %w\n"+
			"    the checkout needs full history (fetch-depth: 0) for that commit to be present", short(sha), err)
	}
	var files []string
	for _, l := range strings.Split(out, "\n") {
		if f := strings.TrimSpace(l); f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}

// Cmd is `llz ci apply-drift`.
func Cmd() *cobra.Command {
	var env string
	var strict bool
	c := &cobra.Command{
		Use:   "apply-drift",
		Short: "report Terraform or spec changes merged since a deployment's last successful apply",
		Long: "A push to main neither plans nor applies, and the apply is a workflow_dispatch a\n" +
			"human fires — so a merged change that only Terraform can deliver sits undeployed\n" +
			"with nothing reporting it. This reports it.\n\n" +
			"It does NOT apply anything. promote.yml already walks ranked deployments with a\n" +
			"green gate between stages and per-stage environment approval; the gap was never\n" +
			"that nothing can apply, it was that nobody knows they need to.\n\n" +
			"apl-values/ is deliberately not counted: Argo and the in-cluster reconciler pull\n" +
			"it continuously, so a change there is already live.\n\n" +
			"--strict makes a behind deployment fail the job rather than warn.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if env == "" {
				return fmt.Errorf("--env is required: drift is per-deployment, and a repo-wide answer would " +
					"report the deployment applied most recently and call the rest up to date")
			}
			sha, checked, err := lastApply(env)
			if err != nil {
				return err
			}
			changed, err := changedSince(sha)
			if err != nil {
				return err
			}
			v := Verdict{Deployment: env, AppliedSHA: sha, Behind: Relevant(changed), CheckedRuns: checked}
			return v.Report(cmd.OutOrStdout(), strict)
		},
	}
	c.Flags().StringVar(&env, "env", "", "deployment to check (required)")
	c.Flags().BoolVar(&strict, "strict", false, "fail the job when the deployment is behind, instead of warning")
	return c
}
