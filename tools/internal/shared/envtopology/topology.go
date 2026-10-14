package envtopology

// topology.go reads the OpenBao HA topology declared in the cluster tfvars
// (ha_role + ha_group, see terraform-iac-bootstrap/cluster/variables.tf) and
// resolves the role/peer questions the CI workflows used to answer by hardcoding
// "primary"/"secondary":
//
//   • ha_role = active     — provisions Harbor robots, receives the standby's CA.
//   • ha_role = standby    — seeds Harbor creds from the active, ships its CA
//                            to the active.
//   • ha_role = standalone — a single self-contained OpenBao (no peer).
//
// `llz env role <name>` and `llz env peer <name>` expose this to the workflows;
// `ValidateTopology` enforces the active/standby pairing. Pure helpers (take a
// tfDir / a []Deployment) so they unit-test against a temp dir.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/validate"
)

const (
	RoleActive     = "active"
	RoleStandby    = "standby"
	roleStandalone = "standalone"
)

// Deployment is one cluster's declared HA identity.
// EXPORTED because FindDeployment/PeerOf are, and a caller handed an opaque value
// it cannot read is a caller that has to re-derive the answer.
type Deployment struct {
	Name    string
	HARole  string // active | standby | standalone
	HAGroup string // pair id; "" for standalone
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

// ReadTopology returns every Deployment with its declared ha_role/ha_group (role
// defaults to standalone when absent). It reads the LandingZone spec when one is
// present — the source of truth, so `llz env role`/`peer` stay correct even when
// the committed tfvars lag a spec edit — and falls back to <tfDir>/cluster/*.tfvars
// otherwise (reusing ListDeployments' name discovery so the set is identical).
// It deliberately does NOT enforce the whole-set pairing contract. `llz env add`
// writes an HA pair one half at a time — scaffold.go defers the render with
// "HA group %q still needs its %s peer" precisely because that half-formed state
// is expected — and `llz env resolve` runs for EVERY Deployment at the head of
// the OpenBao bootstrap job. Validating the whole set here would make one
// incomplete group fail role/peer/resolve for every cluster in the repo,
// including unrelated standalone ones. PeerOf enforces what it needs, where the
// answer would otherwise be a guess.
func ReadTopology(tfDir string) ([]Deployment, error) {
	// clusterspec.Detected() DIRECTLY, not through a seam. package main installed
	// this field as exactly `clusterspec.Detected()` and nothing else ever did --
	// a one-implementation seam that made the model package look like it needed
	// wiring it did not need, and was half of why this file could not leave.
	if lz, present, err := clusterspec.Detected(); present {
		if err != nil {
			return nil, err
		}
		return topologyFromSpec(lz), nil
	}
	names, err := ListDeployments(tfDir)
	if err != nil {
		return nil, err
	}
	deps := make([]Deployment, 0, len(names))
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
		deps = append(deps, Deployment{Name: name, HARole: role, HAGroup: group})
	}
	return deps, nil
}

// topologyFromSpec builds the HA topology from the assembled spec (the merged
// per-env ha.role/ha.group), sorted by name to match the tfvars path's ordering.
func topologyFromSpec(lz *clusterspec.LandingZone) []Deployment {
	deps := make([]Deployment, 0, len(lz.Spec.Environments))
	for name, e := range lz.Spec.Environments {
		role := e.Cluster.HA.Role
		if role == "" {
			role = roleStandalone
		}
		deps = append(deps, Deployment{Name: name, HARole: role, HAGroup: e.Cluster.HA.Group})
	}
	sort.Slice(deps, func(i, j int) bool { return deps[i].Name < deps[j].Name })
	return deps
}

func FindDeployment(deps []Deployment, name string) (Deployment, bool) {
	for _, d := range deps {
		if d.Name == name {
			return d, true
		}
	}
	return Deployment{}, false
}

// HAMembers returns the names of deployments that belong to an HA pair
// (role != standalone), sorted.
func HAMembers(deps []Deployment) []string {
	out := []string{}
	for _, d := range deps {
		if d.HARole != roleStandalone {
			out = append(out, d.Name)
		}
	}
	sort.Strings(out)
	return out
}

// byRole returns the names of deployments with the given ha_role, sorted.
func byRole(deps []Deployment, role string) []string {
	out := []string{}
	for _, d := range deps {
		if d.HARole == role {
			out = append(out, d.Name)
		}
	}
	sort.Strings(out)
	return out
}

// PeerOf returns the other member of name's HA group. ok is false for a
// standalone Deployment, an unknown name, or a group whose other half has not
// been added yet.
//
// An AMBIGUOUS group is an error, not an arbitrary answer. This used to return
// the first other member, so a group holding two standbys silently resolved to
// whichever came first — and `llz env peer` is what tells CI which cluster to
// seed Harbor creds from and exchange CAs with, so guessing seeds from the wrong
// cluster. ValidateHAFlags cannot catch this: it runs at `llz env add` and sees
// one env, never the cross-Deployment pairing.
func PeerOf(deps []Deployment, name string) (string, bool, error) {
	self, found := FindDeployment(deps, name)
	if !found || self.HARole == roleStandalone || self.HAGroup == "" {
		return "", false, nil
	}
	var peers []string
	for _, d := range deps {
		if d.Name != name && d.HAGroup == self.HAGroup {
			peers = append(peers, d.Name)
		}
	}
	if len(peers) > 1 {
		sort.Strings(peers)
		return "", false, fmt.Errorf("ha_group %q holds more than one peer for %q (%s) — exactly one "+
			"active and one standby are required; refusing to guess which cluster to pair with",
			self.HAGroup, name, strings.Join(peers, ", "))
	}
	if len(peers) == 0 {
		return "", false, nil
	}
	return peers[0], true, nil
}

// ValidateTopology enforces the pairing contract: every non-empty ha_group has
// exactly one active and one standby; standalone carries no group.
func ValidateTopology(deps []Deployment) error {
	groups := map[string][]Deployment{}
	for _, d := range deps {
		switch d.HARole {
		case roleStandalone:
			if d.HAGroup != "" {
				return fmt.Errorf("Deployment %q is standalone but sets ha_group=%q", d.Name, d.HAGroup)
			}
		case RoleActive, RoleStandby:
			if d.HAGroup == "" {
				return fmt.Errorf("Deployment %q is %s but has no ha_group", d.Name, d.HARole)
			}
			groups[d.HAGroup] = append(groups[d.HAGroup], d)
		default:
			return fmt.Errorf("Deployment %q has invalid ha_role %q (want active|standby|standalone)", d.Name, d.HARole)
		}
	}
	for g, members := range groups {
		var actives, standbys []string
		for _, d := range members {
			if d.HARole == RoleActive {
				actives = append(actives, d.Name)
			} else {
				standbys = append(standbys, d.Name)
			}
		}
		if len(actives) != 1 || len(standbys) != 1 {
			return fmt.Errorf("ha_group %q must have exactly one active and one standby; got active=%v standby=%v",
				g, actives, standbys)
		}
	}
	return nil
}

// ValidateHAFlags checks the `llz env add` --ha-role/--ha-group combination
// before any files are written. The rule lives in internal/validate so the
// LandingZone spec validator enforces the same active/standby pairing (with
// spec-field names in its messages).
func ValidateHAFlags(role, group string) error {
	return validate.HATopology(role, group, "--ha-role", "--ha-group")
}

// HAFilter narrows a Deployment-name list per the `llz env list` flags.
func HAFilter(deps []Deployment, haOnly bool, role string) []string {
	switch {
	case role != "":
		return byRole(deps, role)
	case haOnly:
		return HAMembers(deps)
	default:
		names := make([]string, 0, len(deps))
		for _, d := range deps {
			names = append(names, d.Name)
		}
		sort.Strings(names)
		return names
	}
}

// ListDeployments returns the sorted Deployment names from BOTH sources: the
// committed <tfDir>/cluster/*.tfvars (one per Deployment that owns a Linode
// cluster) AND the LandingZone spec's environments (the environments/<env>.yaml set)
// when a landingzone.yaml is present. The union (dedup by name) means a
// spec-driven Deployment whose transient tfvars are rendered at build time still
// shows up in the CI matrix. The template's own
// terraform.tfvars[.example] and any non-conforming basename are skipped — the
// latter with a stderr warning, so a stray file can never inject a poisoned value
// into a CI matrix. Pure (takes tfDir; the spec is read from the sibling
// instance root) so it is unit-testable against a temp dir.
func ListDeployments(tfDir string) ([]string, error) {
	set := map[string]struct{}{}

	matches, err := filepath.Glob(filepath.Join(tfDir, "cluster", "*.tfvars"))
	if err != nil {
		return nil, err
	}
	for _, p := range matches {
		name := strings.TrimSuffix(filepath.Base(p), ".tfvars")
		// `terraform.tfvars` (a non-suffixed local override) and
		// `terraform.tfvars.example` (the template) are never deployments.
		if name == "terraform" || name == "terraform.example" {
			continue
		}
		if err := validate.EnvName(name); err != nil {
			fmt.Fprintf(os.Stderr, "warning: skipping %s — %v\n", p, err)
			continue
		}
		set[name] = struct{}{}
	}

	// Union the LandingZone spec's environments. The spec lives at the instance
	// root (the parent of terraform-iac-bootstrap), so it is found in both the
	// instance and template-checkout layouts.
	specRoot := filepath.Dir(tfDir)
	if clusterspec.InstancePresent(specRoot) {
		if lz, lerr := clusterspec.LoadInstance(specRoot); lerr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not read LandingZone spec at %s — %v\n", specRoot, lerr)
		} else {
			for _, name := range lz.EnvNames() {
				if err := validate.EnvName(name); err != nil {
					fmt.Fprintf(os.Stderr, "warning: skipping spec env %q — %v\n", name, err)
					continue
				}
				set[name] = struct{}{}
			}
		}
	}

	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}
