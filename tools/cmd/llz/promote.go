package main

// promote.go — the capability wiring and cobra surface for the `promote-pipeline`
// extension (internal/promote).
//
// Every field is a READ of the instance repo. What the extension does that is not
// a read — writing .github/workflows/promote.yml — it does directly, and the
// declaration says so with `write-repo`. See internal/promote/extension.go for why
// that grant had to be invented rather than approximated with `own-paths`.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/envtopology"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/instancelayout"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/promote"
	"github.com/spf13/cobra"
)

func promoteDeps() promote.Deps {
	return promote.Deps{
		Layout:          instancelayout.Detect,
		ListDeployments: envtopology.ListDeployments,
		LoadSpec:        func() (*clusterspec.LandingZone, bool, error) { return clusterspec.Detected() },
		// Narrowed to the one field the extension reads. Handing over the whole
		// `answers` struct would put package main's copier-answers model on the
		// other side of the boundary to answer a one-line question.
		InstanceRepo: func() string {
			a, _ := readAnswers(".")
			if a == nil {
				return ""
			}
			return a.InstanceRepo
		},
	}
}

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
func syncPromoteWorkflow(tfDir, relPrefix string, check bool) (bool, error) {
	plan, err := promote.PlanWorkflow(promoteDeps(), tfDir, relPrefix)
	if err != nil {
		return false, err
	}
	if plan.Note != "" && !check {
		fmt.Println(plan.Note)
	}
	if !plan.Changed || check {
		return plan.Changed, nil
	}
	if err := os.MkdirAll(filepath.Dir(plan.Path), 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(plan.Path, []byte(plan.Content), 0o644); err != nil {
		return false, err
	}
	fmt.Printf("promote.yml: regenerated pipeline %s\n", strings.Join(plan.Order, " → "))
	return true, nil
}
