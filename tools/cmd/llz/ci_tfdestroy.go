package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
)

// ci_tfdestroy.go — `llz ci tf-destroy`, the assimilation of the inline
// `terraform plan -destroy … && terraform apply destroy-plan.bin` blocks (the
// cluster and object-storage teardown steps in llz-terraform.yml) and the
// `terraform apply -refresh-only` step in llz-secret-rotation.yml. Completes the
// tf-* verb family (tf-plan/tf-apply/tf-import already exist); a GitLab
// .gitlab-ci.yml calls the same verb. See docs/designs/forge-abstraction.md
// (Phase 5). This is a mutating verb: the workflow keeps it under the existing
// assert-destroy-confirm guard.

// tfDestroyRunFn runs `terraform <args...>` with output combined into w.
// Package var so tests stub the terraform exec.
var tfDestroyRunFn = func(w io.Writer, args ...string) error {
	cmd := exec.Command("terraform", args...)
	cmd.Stdout = w
	cmd.Stderr = w
	return cmd.Run()
}

func ciTFDestroyCmd() *cobra.Command {
	var varFile, planOut string
	var refreshOnly bool
	c := &cobra.Command{
		Use:   "tf-destroy --var-file <f>",
		Short: "terraform destroy via an explicit -destroy plan, or --refresh-only",
		Long: "Native port of the inline destroy/refresh terraform steps. Default: a\n" +
			"two-phase destroy — `terraform plan -destroy -out=<plan> -no-color\n" +
			"-var-file=<f>` then `terraform apply <plan>` (the explicit-plan form the\n" +
			"workflow used, so the destroy that runs is exactly the one shown). With\n" +
			"--refresh-only it instead runs `terraform apply -refresh-only\n" +
			"-auto-approve -var-file=<f>` (repopulate state, no resource changes —\n" +
			"the post-rotation kubeconfig refresh). Mutating: keep it behind the\n" +
			"caller's assert-destroy-confirm guard.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runCITFDestroy(os.Stdout, varFile, planOut, refreshOnly)
		},
	}
	c.Flags().StringVar(&varFile, "var-file", "", "terraform -var-file (required)")
	c.Flags().StringVar(&planOut, "plan-out", "destroy-plan.bin", "file for the saved -destroy plan")
	c.Flags().BoolVar(&refreshOnly, "refresh-only", false, "apply -refresh-only instead of destroying")
	_ = c.MarkFlagRequired("var-file")
	return c
}

func runCITFDestroy(w io.Writer, varFile, planOut string, refreshOnly bool) error {
	if varFile == "" {
		return fmt.Errorf("tf-destroy: --var-file is required")
	}
	if refreshOnly {
		if err := tfDestroyRunFn(w, "apply", "-refresh-only", "-auto-approve", "-no-color", "-var-file="+varFile); err != nil {
			return fmt.Errorf("tf-destroy --refresh-only: %w", err)
		}
		return nil
	}
	// Two-phase: save an explicit -destroy plan, then apply exactly that plan.
	if err := tfDestroyRunFn(w, "plan", "-destroy", "-var-file="+varFile, "-out="+planOut, "-no-color"); err != nil {
		return fmt.Errorf("tf-destroy: plan -destroy: %w", err)
	}
	if err := tfDestroyRunFn(w, "apply", planOut); err != nil {
		return fmt.Errorf("tf-destroy: apply %s: %w", planOut, err)
	}
	return nil
}
