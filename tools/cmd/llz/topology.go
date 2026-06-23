package main

// topology.go reads the OpenBao HA topology declared in the cluster tfvars
// (ha_role + ha_group, see terraform-iac-bootstrap/cluster/variables.tf) and
// resolves the role/peer questions the CI workflows used to answer by hardcoding
// "primary"/"secondary":
//
//   • ha_role = active     — provisions Harbor robots, owns the base-named
//                            AppRole GH-secret, receives the standby's CA.
//   • ha_role = standby    — seeds Harbor creds from the active, owns the
//                            _STANDBY-suffixed secret, ships its CA to the active.
//   • ha_role = standalone — a single self-contained OpenBao (no peer).
//
// `llz env role <name>` and `llz env peer <name>` expose this to the workflows;
// `validateTopology` enforces the active/standby pairing. Pure helpers (take a
// tfDir / a []deployment) so they unit-test against a temp dir.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/validate"
	"github.com/spf13/cobra"
)

const (
	roleActive     = "active"
	roleStandby    = "standby"
	roleStandalone = "standalone"
)

// deployment is one cluster's declared HA identity.
type deployment struct {
	name    string
	haRole  string // active | standby | standalone
	haGroup string // pair id; "" for standalone
}

// hclStringField matches `field = "value"` in a tfvars line (the read-only twin
// of scaffold.go's setHCLField).
func hclStringField(body, field string) (string, bool) {
	re := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(field) + `\s*=\s*"([^"]*)"`)
	if m := re.FindStringSubmatch(body); m != nil {
		return m[1], true
	}
	return "", false
}

// readTopology returns every deployment with its declared ha_role/ha_group (role
// defaults to standalone when absent). It reads the LandingZone spec when one is
// present — the source of truth, so `llz env role`/`peer` stay correct even when
// the committed tfvars lag a spec edit — and falls back to <tfDir>/cluster/*.tfvars
// otherwise (reusing listDeployments' name discovery so the set is identical).
func readTopology(tfDir string) ([]deployment, error) {
	if lz, present, err := loadSpec(); present {
		if err != nil {
			return nil, err
		}
		return topologyFromSpec(lz), nil
	}
	names, err := listDeployments(tfDir)
	if err != nil {
		return nil, err
	}
	deps := make([]deployment, 0, len(names))
	for _, name := range names {
		body, err := os.ReadFile(filepath.Join(tfDir, "cluster", name+".tfvars"))
		if err != nil {
			return nil, err
		}
		role, ok := hclStringField(string(body), "ha_role")
		if !ok || role == "" {
			role = roleStandalone
		}
		group, _ := hclStringField(string(body), "ha_group")
		deps = append(deps, deployment{name: name, haRole: role, haGroup: group})
	}
	return deps, nil
}

// topologyFromSpec builds the HA topology from the assembled spec (the merged
// per-env ha.role/ha.group), sorted by name to match the tfvars path's ordering.
func topologyFromSpec(lz *clusterspec.LandingZone) []deployment {
	deps := make([]deployment, 0, len(lz.Spec.Environments))
	for name, e := range lz.Spec.Environments {
		role := e.Cluster.HA.Role
		if role == "" {
			role = roleStandalone
		}
		deps = append(deps, deployment{name: name, haRole: role, haGroup: e.Cluster.HA.Group})
	}
	sort.Slice(deps, func(i, j int) bool { return deps[i].name < deps[j].name })
	return deps
}

func findDeployment(deps []deployment, name string) (deployment, bool) {
	for _, d := range deps {
		if d.name == name {
			return d, true
		}
	}
	return deployment{}, false
}

// haMembers returns the names of deployments that belong to an HA pair
// (role != standalone), sorted.
func haMembers(deps []deployment) []string {
	out := []string{}
	for _, d := range deps {
		if d.haRole != roleStandalone {
			out = append(out, d.name)
		}
	}
	sort.Strings(out)
	return out
}

// byRole returns the names of deployments with the given ha_role, sorted.
func byRole(deps []deployment, role string) []string {
	out := []string{}
	for _, d := range deps {
		if d.haRole == role {
			out = append(out, d.name)
		}
	}
	sort.Strings(out)
	return out
}

// peerOf returns the other member of name's HA group. ok is false for a
// standalone deployment (no peer) or an unknown name.
func peerOf(deps []deployment, name string) (string, bool) {
	self, found := findDeployment(deps, name)
	if !found || self.haRole == roleStandalone || self.haGroup == "" {
		return "", false
	}
	for _, d := range deps {
		if d.name != name && d.haGroup == self.haGroup {
			return d.name, true
		}
	}
	return "", false
}

// validateTopology enforces the pairing contract: every non-empty ha_group has
// exactly one active and one standby; standalone carries no group.
func validateTopology(deps []deployment) error {
	groups := map[string][]deployment{}
	for _, d := range deps {
		switch d.haRole {
		case roleStandalone:
			if d.haGroup != "" {
				return fmt.Errorf("deployment %q is standalone but sets ha_group=%q", d.name, d.haGroup)
			}
		case roleActive, roleStandby:
			if d.haGroup == "" {
				return fmt.Errorf("deployment %q is %s but has no ha_group", d.name, d.haRole)
			}
			groups[d.haGroup] = append(groups[d.haGroup], d)
		default:
			return fmt.Errorf("deployment %q has invalid ha_role %q (want active|standby|standalone)", d.name, d.haRole)
		}
	}
	for g, members := range groups {
		var actives, standbys []string
		for _, d := range members {
			if d.haRole == roleActive {
				actives = append(actives, d.name)
			} else {
				standbys = append(standbys, d.name)
			}
		}
		if len(actives) != 1 || len(standbys) != 1 {
			return fmt.Errorf("ha_group %q must have exactly one active and one standby; got active=%v standby=%v",
				g, actives, standbys)
		}
	}
	return nil
}

// validateHAFlags checks the `llz env add` --ha-role/--ha-group combination
// before any files are written. The rule lives in internal/validate so the
// LandingZone spec validator enforces the same active/standby pairing (with
// spec-field names in its messages).
func validateHAFlags(role, group string) error {
	return validate.HATopology(role, group, "--ha-role", "--ha-group")
}

func envRoleCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "role <deployment>",
		Short: "print a deployment's OpenBao HA role (active|standby|standalone)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			tfDir, _, _ := instanceLayout()
			deps, err := readTopology(tfDir)
			if err != nil {
				return err
			}
			d, ok := findDeployment(deps, args[0])
			if !ok {
				return fmt.Errorf("no such deployment %q (run `llz env list`)", args[0])
			}
			fmt.Println(d.haRole)
			return nil
		},
	}
}

func envPeerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "peer <deployment>",
		Short: "print a deployment's HA peer (the other member of its ha_group); errors for standalone",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			tfDir, _, _ := instanceLayout()
			deps, err := readTopology(tfDir)
			if err != nil {
				return err
			}
			if _, ok := findDeployment(deps, args[0]); !ok {
				return fmt.Errorf("no such deployment %q (run `llz env list`)", args[0])
			}
			peer, ok := peerOf(deps, args[0])
			if !ok {
				return fmt.Errorf("deployment %q is standalone — it has no HA peer", args[0])
			}
			fmt.Println(peer)
			return nil
		},
	}
}

// haFilter narrows a deployment-name list per the `llz env list` flags.
func haFilter(deps []deployment, haOnly bool, role string) []string {
	switch {
	case role != "":
		return byRole(deps, role)
	case haOnly:
		return haMembers(deps)
	default:
		names := make([]string, 0, len(deps))
		for _, d := range deps {
			names = append(names, d.name)
		}
		sort.Strings(names)
		return names
	}
}
