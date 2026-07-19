package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	tf "github.com/akamai-consulting/lke-landing-zone/tools/internal/terraform"
	"github.com/spf13/cobra"
)

// tfPlanFlakeSettle is how long tf-plan waits before its one retry when the plan
// failed on a transient control-plane API flake. Package var so tests zero it.
var tfPlanFlakeSettle = 30 * time.Second

// ci_tfplan.go ports instance-scripts/terraform/terraform-plan.sh +
// terraform-summarize-plan.sh: run `terraform plan -no-color`, tee the combined
// output to a file, and on success append the tail of the plan to
// GITHUB_STEP_SUMMARY under a heading. A failed plan fails the command and
// writes no summary (the scripts' `set -e` had the same semantics).

// tfPlanRunFn runs `terraform plan -no-color <flags...>` with stdout+stderr
// combined into w. Package-level var so tests can stub the terraform exec.
var tfPlanRunFn = func(w io.Writer, tfFlags []string) error {
	cmd := exec.Command("terraform", append([]string{"plan", "-no-color"}, tfFlags...)...)
	cmd.Stdout = w
	cmd.Stderr = w
	return cmd.Run()
}

func ciTFPlanCmd() *cobra.Command {
	var out, title string
	var lines int
	c := &cobra.Command{
		Use:   "tf-plan --out <tee-file> --title <title> [--lines N] [-- terraform flags...]",
		Short: "terraform plan, teed to a file + tail appended to the step summary",
		Long: "Native port of terraform-plan.sh + terraform-summarize-plan.sh.\n" +
			"Runs `terraform plan -no-color [flags...]` with stdout+stderr combined,\n" +
			"streamed live and teed to --out. On success the last --lines lines are\n" +
			"appended to $GITHUB_STEP_SUMMARY as a fenced code block under --title\n" +
			"(skipped when the env var is unset). Flags after `--` pass through to\n" +
			"terraform. A failed plan fails the command and writes no summary.",
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			return runCITFPlan(out, title, lines, args)
		},
	}
	c.Flags().StringVar(&out, "out", "", "file to tee the plan output to")
	c.Flags().StringVar(&title, "title", "", "step-summary heading for the plan tail")
	c.Flags().IntVar(&lines, "lines", 80, "trailing lines of plan output to put in the summary")
	_ = c.MarkFlagRequired("out")
	_ = c.MarkFlagRequired("title")
	return c
}

func runCITFPlan(out, title string, lines int, tfFlags []string) error {
	tee, err := os.Create(out)
	if err != nil {
		return fmt.Errorf("tf-plan: %w", err)
	}
	// Capture in memory as well so the summary tail doesn't re-read the tee file.
	var buf strings.Builder
	planErr := tfPlanRunFn(io.MultiWriter(os.Stdout, tee, &buf), tfFlags)

	// A plan can fail purely because a data-source read (e.g.
	// data.kubernetes_service.coredns) hit a control-plane API flake moments
	// after the cluster came up — no state to repair, the fix is to settle and
	// retry once. Same self-heal class tf-apply has, extended to plan. Anchored
	// on the API endpoint so a genuine plan error is not retried.
	if planErr != nil && tf.TransientAPIFlake(buf.String()) {
		fmt.Fprintf(os.Stderr, "::warning::Plan hit a transient control-plane API flake (TLS handshake/timeout against :6443). Waiting %s, then retrying the plan once.\n", tfPlanFlakeSettle)
		time.Sleep(tfPlanFlakeSettle)
		buf.Reset()
		planErr = tfPlanRunFn(io.MultiWriter(os.Stdout, tee, &buf), tfFlags)
	}

	if err := tee.Close(); err != nil {
		return fmt.Errorf("tf-plan: close %s: %w", out, err)
	}
	if planErr != nil {
		return fmt.Errorf("tf-plan: terraform plan %s: %w", strings.Join(tfFlags, " "), planErr)
	}

	// A plan that could not apply a `moved` block exits 0 with only a WARNING, but
	// terraform has planned to DESTROY the object at the source address. When both
	// addresses alias the same cloud object that destroy deletes it for real — see
	// tf.UnresolvedMoveConflicts for the incident this gates. Always a bug in an
	// automated pipeline, so fail here (seconds) instead of mid-apply (after the
	// 20-30 minute cluster create).
	if conflicts, found := tf.UnresolvedMoveConflicts(buf.String()); found && len(conflicts) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "tf-plan: terraform could not apply %d `moved` migration(s) — it has planned to DESTROY the source object(s):\n", len(conflicts))
		for _, c := range conflicts {
			fmt.Fprintf(&b, "    %s\n      could not move to %s (destination already occupied)\n", c.From, c.To)
		}
		b.WriteString("  If both addresses refer to the SAME cloud object (e.g. `llz ci tf-import` adopted it\n")
		b.WriteString("  at the destination while the source entry was still in state), applying this plan\n")
		b.WriteString("  DELETES that live object. Reconcile state before re-planning, e.g.:\n")
		for _, c := range conflicts {
			fmt.Fprintf(&b, "    terraform state rm '%s'\n", c.From)
		}
		fmt.Fprintf(os.Stderr, "::error::%s\n", strings.ReplaceAll(strings.TrimSpace(b.String()), "\n", " "))
		return fmt.Errorf("%s", strings.TrimSuffix(b.String(), "\n"))
	}

	return appendGHAFile("GITHUB_STEP_SUMMARY", strings.TrimSuffix(tfPlanSummary(title, buf.String(), lines), "\n"))
}

// tfPlanSummary renders the GITHUB_STEP_SUMMARY block: a heading, then the last
// n lines of the plan output fenced as code. The tail always ends in a newline
// so the closing fence never glues onto the last plan line.
func tfPlanSummary(title, planOutput string, n int) string {
	var b strings.Builder
	b.WriteString("### " + title + "\n```\n")
	if tail := tailLines(planOutput, n); tail != "" {
		b.WriteString(tail + "\n")
	}
	b.WriteString("```\n")
	return b.String()
}

// tailLines returns the last n lines of s without a trailing newline
// (`tail -N` semantics: a final newline terminates the last line rather than
// starting an empty one). n <= 0 yields "".
func tailLines(s string, n int) string {
	if n <= 0 || s == "" {
		return ""
	}
	all := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	if n < len(all) {
		all = all[len(all)-n:]
	}
	return strings.Join(all, "\n")
}
