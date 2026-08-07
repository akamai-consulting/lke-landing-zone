package envtopology

// env_set.go is the spec WRITE side — the counterpart to the read commands
// (`llz env show`, `llz components`). `llz env set` mutates fields in
// environments/<env>.yaml, `llz env edit` opens it in $EDITOR, and `llz network
// add` declares a shared VPC in landingzone.yaml. All three edit the declarative
// source in place and then re-render, so the edit→render loop can't be forgotten.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/instancelayout"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/yamledit"
	"github.com/spf13/cobra"
	yaml "gopkg.in/yaml.v3"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
)

// SpecFile returns the path to environments/<env>.yaml, erroring if it (or the
// instance's spec) is absent.
func SpecFile(env string) (string, error) {
	if err := validateEnvName(env); err != nil {
		return "", err
	}
	tfDir, _, _ := instancelayout.Detect()
	p := filepath.Join(filepath.Dir(tfDir), clusterspec.EnvironmentsDir, env+".yaml")
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("no spec for %q (%s missing) — run `llz env add %s` first", env, p, env)
	}
	return p, nil
}

func specSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set <path=value>...",
		Short: "set instance-level fields in landingzone.yaml + re-render (e.g. dns.acmeEmail=ops@x.com)",
		Long: "Sets spec.<path>=<value> in landingzone.yaml (the instance-wide config: dns,\n" +
			"defaults, platform, instance identity), preserving comments, then validates +\n" +
			"re-renders every env. Per-env fields (cluster.* / components.*) go on\n" +
			"`llz env set <env>`; shared VPCs on `llz network add`. Examples:\n" +
			"  llz spec set dns.acmeEmail=ops@example.com\n" +
			"  llz spec set defaults.cluster.nodePool.count=5 defaults.platform.externalIDP=true",
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			assigns, err := yamledit.ParseAssignments(args)
			if err != nil {
				return err
			}
			for _, a := range assigns {
				if yamledit.IsPerEnvPath(a[0]) {
					return fmt.Errorf("%q is a per-env field — set it with `llz env set <env> %s=%s`", a[0], a[0], a[1])
				}
			}
			tfDir, _, _ := instancelayout.Detect()
			lzPath := filepath.Join(filepath.Dir(tfDir), clusterspec.LandingZoneFile)
			if _, err := os.Stat(lzPath); err != nil {
				return fmt.Errorf("no %s — run `llz env add <env>` first to create the spec", clusterspec.LandingZoneFile)
			}
			if err := yamledit.EditSpecFile(lzPath, func(doc *yaml.Node) error {
				for _, a := range assigns {
					if err := yamledit.SetSpecPath(doc, a[0], a[1]); err != nil {
						return err
					}
				}
				return nil
			}, func(b []byte) error { _, e := clusterspec.Decode(b); return e }); err != nil {
				return err
			}
			for _, a := range assigns {
				fmt.Printf("  %s spec.%s = %s\n", color.Green("set"), a[0], a[1])
			}
			fmt.Println("\n" + color.Bold("Reconciling (`llz render`):"))
			return caps.Render("")
		},
	}
}

func specValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "validate the LandingZone spec (alias for `llz render --check`)",
		Args:  cobra.NoArgs,
		RunE:  func(_ *cobra.Command, _ []string) error { return caps.Render("") },
	}
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
			tfDir, _, _ := instancelayout.Detect()
			lzPath := filepath.Join(filepath.Dir(tfDir), clusterspec.LandingZoneFile)
			if _, err := os.Stat(lzPath); err != nil {
				return fmt.Errorf("no %s — run `llz env add <env>` first to create the spec", clusterspec.LandingZoneFile)
			}
			if err := yamledit.EditSpecFile(lzPath, func(doc *yaml.Node) error {
				return yamledit.SetSpecPath(doc, "networks."+name+".region", region)
			}, func(b []byte) error { _, e := clusterspec.Decode(b); return e }); err != nil {
				return err
			}
			fmt.Printf("  %s shared VPC %q (region %s) → spec.networks\n", color.Green("added"), name, region)
			fmt.Printf("  %s  %s\n", color.Dim("attach an env:"), color.Cyan(fmt.Sprintf("llz env set <env> cluster.network.vpc=%s cluster.network.subnetCIDR=10.0.0.0/14", name)))
			fmt.Println("\n" + color.Bold("Reconciling (`llz render`):"))
			return caps.Render("")
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
			lz, present, err := caps.LoadSpec()
			if !present {
				return fmt.Errorf("no LandingZone spec found — run `llz env add <env>` first")
			}
			if err != nil {
				return err
			}
			if len(lz.Spec.Networks) == 0 {
				fmt.Println(color.Dim("no shared VPCs declared (every env uses a dedicated VPC) — add one with `llz network add`"))
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
