package render

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cliopts"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/instancelayout"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/tfvars"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/validate"
	"github.com/spf13/cobra"
)

// cobra_render.go — the flag sets for `llz render` and `llz env vpc`. The renderer
// is internal/render.

// EnvVPCCmd prints the shared VPC (spec.networks name) a deployment attaches to,
// or an empty line for a dedicated VPC, so the apply-vpc workflow step can decide
// whether — and which — shared VPC to apply before the cluster. It reads the spec
// when present (the source of truth), falling back to the rendered
// cluster/<env>.tfvars (vpc_network) for a pre-spec instance.
func EnvVPCCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "vpc <deployment>",
		Short: "print the shared VPC a deployment attaches to (spec.networks name); empty for a dedicated VPC",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			env := args[0]
			if err := validate.EnvName(env); err != nil {
				return err
			}
			// Spec is the source of truth; the committed tfvars can lag a spec edit.
			if lz, present, err := clusterspec.Detected(); present {
				if err != nil {
					return err
				}
				e, ok := lz.Env(env)
				if !ok {
					return fmt.Errorf("no such deployment %q in the spec (run `llz env list`)", env)
				}
				fmt.Println(e.Cluster.Network.VPC)
				return nil
			}
			tfDir, _, _ := instancelayout.Detect()
			p := filepath.Join(tfDir, "cluster", env+".tfvars")
			b, err := os.ReadFile(p)
			if err != nil {
				return fmt.Errorf("read %s (for spec-driven instances run `llz render %s` first): %w", p, env, err)
			}
			fmt.Println(tfvars.Value(string(b), "vpc_network"))
			return nil
		},
	}
}
func RenderCmd() *cobra.Command {
	var tfvarsOnly, check, diff bool
	c := &cobra.Command{
		Use:   "render [env]",
		Short: "reconcile the LandingZone spec into <env>.tfvars (spec-driven instances)",
		Long: "Reads the LandingZone spec (landingzone.yaml + environments/<env>.yaml) and\n" +
			"renders each deployment's cluster definition into the three\n" +
			"terraform-iac-bootstrap/*/<env>.tfvars files the terraform plan/apply consume.\n" +
			"With no [env], renders every environment in the spec. --check validates the\n" +
			"spec without writing; --diff previews what a render WOULD change (also\n" +
			"writes nothing). A no-op contract: callers gate on the presence of a spec\n" +
			"(CI does), so instances that have not adopted it are unaffected.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			env := ""
			if len(args) == 1 {
				env = args[0]
			}
			return Run(cliopts.Global.DryRun, env, tfvarsOnly, check, diff)
		},
	}
	c.Flags().BoolVar(&tfvarsOnly, "tfvars-only", false, "render only the tfvars (skip the committed manifest kustomizations)")
	c.Flags().BoolVar(&check, "check", false, "validate the spec and exit non-zero on any error; write nothing")
	c.Flags().BoolVar(&diff, "diff", false, "preview which files a render would create/change (writes nothing)")
	return c
}
