package cli

// promote.go — the cobra surface for the `promote-pipeline` extension
// (internal/extensions/lifecycle/promote), and the one thing that extension is not
// permitted to do for itself: the os.WriteFile.
//
// The extension declares `read-repo`, so writing .github/workflows/promote.yml
// lives on THIS side of the boundary — see extension.go for why `write-repo` was
// not invented to move it back.
//
// The Deps wiring used to be built here too, and is not any more: `llz doctor`
// asks the same question of the same tree, and one production constructor
// (promote.DefaultDeps) is what stops the pre-flight and the CI gate reading
// different files.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/promote"
	"github.com/spf13/cobra"
)

// promoteDeps is the production wiring, and it lives in the promote package —
// `llz doctor` reads the same tree through the same seams, and two hand-built
// copies is how the pre-flight and the CI gate come to disagree about one file.
func promoteDeps() promote.Deps { return promote.DefaultDeps() }

func envNextCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "next <deployment>",
		Short: "print the deployment promoted into after <deployment> (the next promotion_rank); errors on the last stage",
		Long: "Reads each deployment's promotion_rank (cluster tfvars) and prints the\n" +
			"next stage in the pipeline — what a promote-on-green CI job builds after\n" +
			"<deployment> goes green. Errors if <deployment> is unranked (not in a\n" +
			"pipeline) or is the final stage. Pair with `llz env list --ordered`.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			d := promoteDeps()
			tfDir, _, _ := d.Layout()
			stages, err := promote.ReadPromotion(d, tfDir)
			if err != nil {
				return err
			}
			if _, ok := promote.FindStage(stages, args[0]); !ok {
				return fmt.Errorf("deployment %q has no promotion_rank — it is not in a promotion pipeline (set promotion_rank in cluster/%s.tfvars)", args[0], args[0])
			}
			next, ok := promote.NextStage(stages, args[0])
			if !ok {
				return fmt.Errorf("deployment %q is the last stage — nothing to promote to", args[0])
			}
			cmd.Println(next)
			return nil
		},
	}
}

// syncPromoteWorkflow performs the write internal/promote deliberately does not.
//
// The extension declares `read-repo`, and the model has no `write-repo` grant —
// `own-paths` is a copier FENCE, not a write permit, and Validate() rejects it at
// `promoted` anyway. So the os.WriteFile lives here, on the side of the boundary
// whose declaration permits it. Same split `llz ci gen-toc` uses; the catalog
// records why nothing was invented instead.
func syncPromoteWorkflow(tfDir, relPrefix string, check bool) (promote.Plan, error) {
	plan, err := promote.PlanWorkflow(promoteDeps(), tfDir, relPrefix)
	if err != nil {
		return promote.Plan{}, err
	}
	if plan.Note != "" && !check {
		fmt.Println(plan.Note)
	}
	if !plan.Changed || check {
		return plan, nil
	}
	if err := os.MkdirAll(filepath.Dir(plan.Path), 0o755); err != nil {
		return promote.Plan{}, err
	}
	if err := os.WriteFile(plan.Path, []byte(plan.Content), 0o644); err != nil {
		return promote.Plan{}, err
	}
	if len(plan.Order) > 0 {
		fmt.Printf("promote.yml: regenerated pipeline %s\n", strings.Join(plan.Order, " → "))
	} else {
		fmt.Println("promote.yml: regenerated as the empty pipeline (no stages) — see the note above.")
	}
	// Re-derive from what was actually written; see Plan.Applied for the bug that
	// reporting on the pre-write plan produced.
	return plan.Applied(), nil
}
