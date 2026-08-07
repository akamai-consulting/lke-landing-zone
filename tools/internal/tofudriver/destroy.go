package tofudriver

import (
	"fmt"
	"io"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/tfbin"
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
	cmd := tfbin.Command(args...)
	cmd.Stdout = w
	cmd.Stderr = w
	return cmd.Run()
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
