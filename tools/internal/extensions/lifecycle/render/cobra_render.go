package render

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cliopts"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/instancelayout"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/tfvars"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/validate"
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
	var tfvarsOnly, check, diff, ifSpec bool
	c := &cobra.Command{
		Use:   "render [env]",
		Short: "reconcile the LandingZone spec into <env>.tfvars (spec-driven instances)",
		Long: "Reads the LandingZone spec (landingzone.yaml + environments/<env>.yaml) and\n" +
			"renders each deployment's cluster definition into the three\n" +
			"terraform-iac-bootstrap/*/<env>.tfvars files the terraform plan/apply consume.\n" +
			"With no [env], renders every environment in the spec. --check validates the\n" +
			"spec without writing; --diff previews what a render WOULD change (also\n" +
			"writes nothing). --if-spec makes a pre-spec instance a no-op instead of an\n" +
			"error, so a caller does not have to test for the spec file itself.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			env := ""
			if len(args) == 1 {
				env = args[0]
			}
			// THE NO-OP CONTRACT, MOVED OFF THE CALLER. It used to read "callers
			// gate on the presence of a spec (CI does)", and CI did — by repeating
			//     if [ -f landingzone.yaml ]; then llz render …; fi
			// at every call site. That is a three-line shell conditional per site
			// restating a rule this binary already knows, in the one place the repo
			// measures and refuses to grow (untestable-loc's workflow-inline-bash).
			// Same rule, one copy, and it resolves the spec the way Run does rather
			// than by testing for a filename relative to whatever cwd a step had.
			if ifSpec && !SpecPresent() {
				fmt.Fprintln(os.Stderr, "llz render: no LandingZone spec — nothing to render (pre-spec instance)")
				return nil
			}
			return Run(cliopts.Global.DryRun, env, tfvarsOnly, check, diff)
		},
	}
	c.Flags().BoolVar(&tfvarsOnly, "tfvars-only", false, "render only the tfvars (skip the committed manifest kustomizations)")
	c.Flags().BoolVar(&check, "check", false, "validate the spec and exit non-zero on any error; write nothing")
	c.Flags().BoolVar(&diff, "diff", false, "preview which files a render would create/change (writes nothing)")
	c.Flags().BoolVar(&ifSpec, "if-spec", false, "no-op (exit 0) when the instance has no LandingZone spec, instead of failing")
	return c
}
