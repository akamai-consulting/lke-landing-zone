package main

// ci_build_failure_summary.go — `llz ci build-failure-summary`: tell an operator
// whose build just failed what state they are in and what to do next.
//
// The apply path had no `if: failure()` step at all. GITHUB_STEP_SUMMARY was
// written by exactly two places — the push-noop notice and the destroy job — so a
// failed first build produced a red X in the Actions UI and raw Terraform logs,
// with nothing anywhere saying whether re-dispatching was safe, what had already
// been created, or that `llz reap` exists. None of the twelve delivered
// runbooks/playbooks covered it either: they all assume a converged cluster.
//
// That is the expensive gap, because the first build is the one most likely to
// fail and the operator hitting it is by definition the one with the least
// context. The information is short and almost entirely stage-independent — the
// apply is idempotent, re-dispatch is the intended recovery, and the two things
// that genuinely need cleaning up have verbs — so it is written once, here, and
// every failing stage calls it.
//
// Deliberately NOT a diagnosis. Nothing here inspects the failure; the log above
// it does that. This answers the questions the log cannot: is my instance broken,
// can I just run it again, and what did I leak.

import (
	"fmt"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghaout"
	"github.com/spf13/cobra"
)

func ciBuildFailureSummaryCmd() *cobra.Command {
	var stage, region string
	c := &cobra.Command{
		Use:   "build-failure-summary",
		Short: "write an operator-facing recovery summary for a failed build stage",
		Long: "Appends a recovery summary to GITHUB_STEP_SUMMARY: what this stage had\n" +
			"created by the time it failed, whether re-dispatching is safe, and the\n" +
			"cleanup verbs for the two things that leak. Called from `if: failure()`\n" +
			"steps in the apply path — it never fails a job (the job has already\n" +
			"failed) and it does not diagnose, it orients.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			// Never propagate: this runs inside an already-failing job, and an
			// error here would replace the real failure in the operator's view.
			if err := ghaout.Append("GITHUB_STEP_SUMMARY", buildFailureSummary(stage, region)); err != nil {
				fmt.Printf("could not write the failure summary (%v); the recovery steps are in docs/runbooks/first-build-failed.md\n", err)
			}
			return nil
		},
	}
	c.Flags().StringVar(&stage, "stage", "", "the stage that failed (vpc, cluster, object-storage, bootstrap)")
	c.Flags().StringVar(&region, "region", "", "the deployment being built")
	return c
}

// stageRecovery is the one thing that differs per stage: what exists now.
var stageRecovery = map[string]string{
	// This job hosts the pipeline-wide preflights AND the shared-VPC apply, and the
	// preflights are what usually fails — they are the point of front-loading them.
	// Saying "the shared VPC apply failed" would misname the common case, and send
	// an operator whose real problem is a PAT scope looking at Terraform.
	"vpc": "The first job failed. It runs every cheap preflight — image pin, committed-render " +
		"drift, secret presence, credential validity and scope, apl-core chart floor — " +
		"BEFORE any cloud mutation, and then applies the shared VPC. A failure here is " +
		"therefore almost always configuration rather than leaked infrastructure: read " +
		"the failing step's own message, which names its fix.",
	"cluster": "The cluster apply failed. Terraform may hold a partially-created LKE " +
		"cluster, VPC, firewall or node pool. Terraform state is authoritative and the " +
		"apply is idempotent, so a re-dispatch continues from where it stopped rather " +
		"than starting over.",
	"object-storage": "The object-storage apply failed. The cluster exists; the Loki/Harbor " +
		"buckets may be partially created. Bucket labels share one namespace per region " +
		"ACROSS Linode accounts, so `already exists` here means the label is taken " +
		"globally — change spec.instance.objLabelPrefix rather than the deployment name.",
	"bootstrap": "The cluster exists and Terraform is done; apl-core install, the " +
		"convergence gate, or OpenBao bootstrap failed. This stage is re-runnable: " +
		"seeded OpenBao paths are skipped, and `helm upgrade --install` is idempotent. " +
		"Re-dispatch before considering a teardown.",
}

// buildFailureSummary renders the Markdown block. Pure, so the wording is
// testable and cannot drift from the commands it prescribes.
func buildFailureSummary(stage, region string) string {
	env := region
	if env == "" {
		env = "<env>"
	}
	what, ok := stageRecovery[stage]
	if !ok {
		what = "A build stage failed. Terraform state is authoritative and the apply is " +
			"idempotent, so re-dispatching continues rather than starting over."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## ❌ Build failed — %s\n\n", firstNonEmpty(stage, "unknown stage"))
	fmt.Fprintf(&b, "%s\n\n", what)
	b.WriteString("### What to do\n\n")
	b.WriteString("1. **Read the failing step above.** The preflights name their own fix; a Terraform\n")
	b.WriteString("   error names the resource. Locally: `gh run view <run-id> --log-failed`.\n")
	fmt.Fprintf(&b, "2. **Fix it in your instance repo and push.** The build renders from the pushed\n"+
		"   default branch, not your working tree — an uncommitted fix is not in the next run.\n")
	fmt.Fprintf(&b, "3. **Re-dispatch:** `llz build %s --yes` (or `llz up %s --yes` to re-check the gates first).\n\n", env, env)
	b.WriteString("### If you want to start clean instead\n\n")
	b.WriteString("Re-running is almost always right — a destroy costs another full build. If you do\n")
	b.WriteString("need to reset, tear down first, then re-dispatch:\n\n")
	b.WriteString("```bash\n")
	fmt.Fprintf(&b, "gh workflow run terraform.yml -f action=destroy -f module=all -f region=%s \\\n", env)
	fmt.Fprintf(&b, "  -f confirm_destroy=destroy:%s:cluster\n", env)
	b.WriteString("```\n\n")
	b.WriteString("`confirm_destroy` is mandatory — without it every destroy job fails its guard.\n\n")
	b.WriteString("### Leaked resources\n\n")
	b.WriteString("A cancelled or failed cycle can strand NodeBalancers, VPCs and Volumes, and a\n")
	b.WriteString("backlog of them makes the NEXT cluster-create hang on account quota. Sweep them:\n\n")
	b.WriteString("```bash\n")
	b.WriteString("llz reap --region <linode-region>            # dry run; add --yes to delete\n")
	b.WriteString("```\n\n")
	b.WriteString("Full walkthrough: `docs/runbooks/first-build-failed.md`.\n")
	return b.String()
}
