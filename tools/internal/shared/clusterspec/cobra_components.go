package clusterspec

// components_cmd.go adds the spec-introspection commands: `llz components` lists
// the component registry (what's toggleable, its backends + sizing knobs) so a
// user doesn't have to read the Go source or docs to discover it; `llz env show`
// prints a deployment's EFFECTIVE config — the per-env ClusterDefinition after
// spec.defaults is merged in — so "what does this env actually resolve to?" has a
// direct answer instead of mentally folding defaults into the env file.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
)

func ComponentsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "components",
		Short: "list the spec.components registry (default state, backends, sizing knobs)",
		Long: "Lists every component spec.components can toggle: its default state, which\n" +
			"backend(s) deliver it (apl-core values vs the llz Argo manifests), the sizing\n" +
			"knobs it accepts, and any components it requires. Toggle them per env in\n" +
			"environments/<env>.yaml; see `llz env show <env>` for an env's resolved set.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "COMPONENT\tDEFAULT\tBACKENDS\tSIZING\tREQUIRES")
			for _, c := range Components {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
					c.Name, ComponentDefault(c),
					OrDash(strings.Join(c.Backends(), ",")),
					OrDash(strings.Join(ComponentKnobs(c.Name), ",")),
					OrDash(strings.Join(c.DependsOn, ",")))
			}
			return tw.Flush()
		},
	}
}

func ComponentDefault(c Component) string {
	switch {
	case c.Mandatory:
		return "on (required)"
	case c.DefaultDisabled:
		return "off"
	default:
		return "on"
	}
}

func EnvShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <deployment>",
		Short: "print a deployment's effective config (spec.defaults merged in) + component set",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			env := args[0]
			lz, present, err := Detected()
			if !present {
				return fmt.Errorf("no LandingZone spec found — `llz env show` needs a spec (run `llz env add %s` first)", env)
			}
			if err != nil {
				return err
			}
			e, ok := lz.Env(env)
			if !ok {
				return fmt.Errorf("no such deployment %q in the spec (run `llz env list`)", env)
			}
			PrintEnvShow(env, e)
			return nil
		},
	}
}

func PrintEnvShow(env string, e Environment) {
	c := e.Cluster
	fmt.Printf("%s\n\n", color.Bold(fmt.Sprintf("Deployment %q — effective config (spec.defaults merged in):", env)))

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	row := func(k, v string) {
		if v != "" {
			fmt.Fprintf(tw, "  %s\t%s\n", k, v)
		}
	}
	row("clusterLabel", c.ClusterLabel)
	row("region", c.Region)
	row("k8sVersion", c.K8sVersion)
	if c.NodePool.Type != "" || c.NodePool.Count > 0 {
		row("nodePool", fmt.Sprintf("%s × %d", c.NodePool.Type, c.NodePool.Count))
	}
	if cp := ControlPlaneSummary(c.ControlPlane); cp != "" {
		row("controlPlane", cp)
	}
	ha := c.HA.Role
	if ha == "" {
		ha = "standalone"
	}
	if c.HA.Group != "" {
		ha += " (group " + c.HA.Group + ")"
	}
	row("ha", ha)
	if c.Network.VPC != "" {
		row("network", fmt.Sprintf("%s  %s", c.Network.VPC, OrDash(c.Network.SubnetCIDR)))
	} else {
		row("network", "dedicated VPC"+OptSubnet(c.Network.SubnetCIDR))
	}
	row("objectStorage", c.ObjectStorage.Cluster)
	row("domainSuffix", c.Bootstrap.DomainSuffix)
	row("cluster.name", c.Bootstrap.Name)
	if c.PromotionRank > 0 {
		row("promotionRank", strconv.Itoa(c.PromotionRank))
	}
	_ = tw.Flush()

	fmt.Printf("\n%s\n", color.Bold("components:"))
	ctw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, comp := range Components {
		state := "off"
		if ComponentEnabled(e.Components, comp.Name) {
			state = "on"
		}
		fmt.Fprintf(ctw, "  %s\t%s\t%s\n", comp.Name, state, OrDash(SizingSummary(e.Components[comp.Name])))
	}
	_ = ctw.Flush()
}

func ControlPlaneSummary(cp ControlPlane) string {
	var parts []string
	if cp.HighAvailability != nil && *cp.HighAvailability {
		parts = append(parts, "HA")
	}
	if cp.AuditLogsEnabled != nil && *cp.AuditLogsEnabled {
		parts = append(parts, "audit logs")
	}
	return strings.Join(parts, ", ")
}

func SizingSummary(t ComponentToggle) string {
	var parts []string
	add := func(k, v string) {
		if v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	add("retention", t.Retention)
	add("storage", t.Storage)
	if t.Replicas != nil {
		parts = append(parts, "replicas="+strconv.Itoa(*t.Replicas))
	}
	add("registryStorage", t.RegistryStorage)
	return strings.Join(parts, " ")
}

func OptSubnet(cidr string) string {
	if cidr == "" {
		return ""
	}
	return " (" + cidr + ")"
}

func OrDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
