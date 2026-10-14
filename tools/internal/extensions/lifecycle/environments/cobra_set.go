package environments

// cobra_set.go — the CLI surface for set.
//
// Split from set.go so an extension directory shows its commands at a glance:
// every file named cobra_*.go is flag wiring and help text, and nothing else.

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/yamledit"
	"github.com/spf13/cobra"
	yaml "gopkg.in/yaml.v3"
)

func SetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set <topo.Deployment> <path=value>...",
		Short: "set fields in environments/<env>.yaml + re-render (e.g. cluster.nodePool.count=8)",
		Long: "Sets one or more spec.<path>=<value> fields in environments/<env>.yaml,\n" +
			"preserving comments, then validates + re-renders. Paths are relative to the\n" +
			"env's spec (cluster.* / components.*); values are typed automatically (bool /\n" +
			"int / string). Examples:\n" +
			"  llz env set prod cluster.nodePool.count=8\n" +
			"  llz env set prod components.harbor.enabled=false components.observability.retention=30d",
		Args: cobra.MinimumNArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			env := args[0]
			assigns, err := yamledit.ParseAssignments(args[1:])
			if err != nil {
				return err
			}
			for _, a := range assigns {
				if !yamledit.IsPerEnvPath(a[0]) {
					return fmt.Errorf("%q is an instance-level field — set it with `llz spec set %s=%s` (per-env paths are cluster.* / components.*)", a[0], a[0], a[1])
				}
			}
			envFile, err := SpecFile(env)
			if err != nil {
				return err
			}
			if err := yamledit.EditSpecFileVia(specEditor("set"), envFile, func(doc *yaml.Node) error {
				for _, a := range assigns {
					if err := yamledit.SetSpecPath(doc, a[0], a[1]); err != nil {
						return err
					}
				}
				return nil
			}, func(b []byte) error { _, e := clusterspec.DecodeClusterDefinition(b); return e }); err != nil {
				return err
			}
			for _, a := range assigns {
				fmt.Printf("  %s spec.%s = %s\n", color.Green("set"), a[0], a[1])
			}
			fmt.Printf("\n%s\n", color.Bold(fmt.Sprintf("Reconciling (`llz render %s`):", env)))
			return caps.Render(env)
		},
	}
}
func EditCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "edit <topo.Deployment>",
		Short: "open environments/<env>.yaml in $EDITOR, then re-render on exit",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			env := args[0]
			envFile, err := SpecFile(env)
			if err != nil {
				return err
			}
			editor := firstNonEmpty(os.Getenv("VISUAL"), os.Getenv("EDITOR"), "vi")
			parts := strings.Fields(editor)
			c := exec.Command(parts[0], append(parts[1:], envFile)...) //nolint:gosec // operator's own $EDITOR
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			if err := c.Run(); err != nil {
				return fmt.Errorf("editor %q: %w", editor, err)
			}
			// Keep the operator's edit (don't roll back manual work) but catch a
			// parse error before render so the message is clear.
			if b, rerr := os.ReadFile(envFile); rerr == nil {
				if _, derr := clusterspec.DecodeClusterDefinition(b); derr != nil {
					return fmt.Errorf("%s won't parse after your edit: %s\n  fix it, then `llz render %s`", envFile, yamledit.CleanFieldErr(derr), env)
				}
			}
			fmt.Printf("%s\n", color.Bold(fmt.Sprintf("Reconciling (`llz render %s`):", env)))
			return caps.Render(env)
		},
	}
}
func SpecCmd() *cobra.Command {
	c := &cobra.Command{Use: "spec", Short: "edit/validate the instance-level spec (landingzone.yaml)"}
	c.AddCommand(specSetCmd(), specValidateCmd())
	return c
}
func NetworkCmd() *cobra.Command {
	c := &cobra.Command{Use: "network", Short: "manage shared VPCs (spec.networks)"}
	c.AddCommand(networkAddCmd(), networkListCmd())
	return c
}
