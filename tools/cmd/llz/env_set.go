package main

// env_set.go is the spec WRITE side — the counterpart to the read commands
// (`llz env show`, `llz components`). `llz env set` mutates fields in
// environments/<env>.yaml, `llz env edit` opens it in $EDITOR, and `llz network
// add` declares a shared VPC in landingzone.yaml. All three edit the declarative
// source in place and then re-render, so the edit→render loop can't be forgotten.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
	"github.com/spf13/cobra"
	yaml "gopkg.in/yaml.v3"
)

// envSpecFile returns the path to environments/<env>.yaml, erroring if it (or the
// instance's spec) is absent.
func envSpecFile(env string) (string, error) {
	if err := validateEnvName(env); err != nil {
		return "", err
	}
	tfDir, _, _ := instanceLayout()
	p := filepath.Join(filepath.Dir(tfDir), clusterspec.EnvironmentsDir, env+".yaml")
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("no spec for %q (%s missing) — run `llz env add %s` first", env, p, env)
	}
	return p, nil
}

func envSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set <deployment> <path=value>...",
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
			assigns, err := parseAssignments(args[1:])
			if err != nil {
				return err
			}
			envFile, err := envSpecFile(env)
			if err != nil {
				return err
			}
			if err := editYAMLFile(envFile, func(doc *yaml.Node) error {
				for _, a := range assigns {
					if err := setSpecPath(doc, a[0], a[1]); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				return err
			}
			for _, a := range assigns {
				fmt.Printf("  %s spec.%s = %s\n", green("set"), a[0], a[1])
			}
			fmt.Printf("\nReconciling (`llz render %s`):\n", env)
			return runRender(gopts, env, false, false, false)
		},
	}
}

func envEditCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "edit <deployment>",
		Short: "open environments/<env>.yaml in $EDITOR, then re-render on exit",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			env := args[0]
			envFile, err := envSpecFile(env)
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
			fmt.Printf("Reconciling (`llz render %s`):\n", env)
			return runRender(gopts, env, false, false, false)
		},
	}
}

func networkCmd() *cobra.Command {
	c := &cobra.Command{Use: "network", Short: "manage shared VPCs (spec.networks)"}
	c.AddCommand(networkAddCmd(), networkListCmd())
	return c
}

func networkAddCmd() *cobra.Command {
	var region string
	c := &cobra.Command{
		Use:   "add <name> --region <region>",
		Short: "declare a shared, region-scoped VPC in landingzone.yaml's spec.networks",
		Long: "Adds a named shared VPC to spec.networks so 2+ same-region clusters can\n" +
			"co-locate in it (attach an env with `llz env set <env> cluster.network.vpc=<name>`).\n" +
			"A Linode VPC is region-scoped, so the network is too. Re-renders after editing.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			if region == "" {
				return fmt.Errorf("--region is required (a Linode region, e.g. us-ord)")
			}
			tfDir, _, _ := instanceLayout()
			lzPath := filepath.Join(filepath.Dir(tfDir), clusterspec.LandingZoneFile)
			if _, err := os.Stat(lzPath); err != nil {
				return fmt.Errorf("no %s — run `llz env add <env>` first to create the spec", clusterspec.LandingZoneFile)
			}
			if err := editYAMLFile(lzPath, func(doc *yaml.Node) error {
				return setSpecPath(doc, "networks."+name+".region", region)
			}); err != nil {
				return err
			}
			fmt.Printf("  %s shared VPC %q (region %s) → spec.networks\n", green("added"), name, region)
			fmt.Printf("  attach an env:  llz env set <env> cluster.network.vpc=%s cluster.network.subnetCIDR=10.0.0.0/14\n", name)
			fmt.Println("\nReconciling (`llz render`):")
			return runRender(gopts, "", false, false, false)
		},
	}
	c.Flags().StringVar(&region, "region", "", "Linode region for the VPC (e.g. us-ord)")
	return c
}

// (firstNonEmpty lives in tokens.go)

func networkListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "list the shared VPCs declared in spec.networks (name → region)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			lz, present, err := loadSpec()
			if !present {
				return fmt.Errorf("no LandingZone spec found — run `llz env add <env>` first")
			}
			if err != nil {
				return err
			}
			if len(lz.Spec.Networks) == 0 {
				fmt.Println("no shared VPCs declared (every env uses a dedicated VPC) — add one with `llz network add`")
				return nil
			}
			names := make([]string, 0, len(lz.Spec.Networks))
			for n := range lz.Spec.Networks {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				fmt.Printf("%s\t%s\n", n, lz.Spec.Networks[n].Region)
			}
			return nil
		},
	}
}
