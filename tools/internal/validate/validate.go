// Package validate holds the small, pure input validators shared between the
// `llz` CLI (cmd/llz) and the declarative LandingZone spec (internal/clusterspec).
// They were originally inlined in cmd/llz (validateEnvName, validateOBJCluster,
// the --ha-role/--ha-group check); centralizing them here lets the spec validator
// reuse the exact same rules without an import cycle (clusterspec → validate, and
// cmd/llz → validate). Keep this package side-effect free.
package validate

import (
	"fmt"
	"net"
	"regexp"
	"strings"
)

// HA role values for a deployment's OpenBao topology.
const (
	RoleStandalone = "standalone"
	RoleActive     = "active"
	RoleStandby    = "standby"
)

// Forge flavors an instance may target. These are the canonical wire strings
// for spec.instance.forge; internal/forge re-exports them as its Flavor type
// and holds the per-flavor behavior. GHEC (Enterprise Cloud) and GHES
// (Enterprise Server) are distinct flavors: GHEC is github.com's API with an
// enterprise tenant, GHES is a self-hosted appliance with its own API base and
// OIDC issuer. See docs/designs/forge-abstraction.md.
const (
	ForgeGitHub                 = "github"
	ForgeGitHubEnterprise       = "github-enterprise"        // Enterprise Cloud (GHEC)
	ForgeGitHubEnterpriseServer = "github-enterprise-server" // Enterprise Server (GHES)
	ForgeGitLab                 = "gitlab"
)

// EnvNameRe is the deployment-name contract: lowercase start, then
// [a-z0-9-], 2–31 chars total. Shared by `llz env add` and `llz build`.
var EnvNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}$`)

// objClusterRe matches a Linode OBJ cluster id: a region plus a datacenter
// ordinal, e.g. us-ord-1 or the newer-generation us-ord-10.
var objClusterRe = regexp.MustCompile(`^[a-z]{2}-[a-z]+-\d+$`)

// EnvName returns an error if env is not a legal deployment name.
func EnvName(env string) error {
	if !EnvNameRe.MatchString(env) {
		return fmt.Errorf("invalid deployment name %q (want %s — the same contract as `llz env add`)", env, EnvNameRe.String())
	}
	return nil
}

// OBJClusterID catches a value that isn't shaped like a Linode OBJ cluster id
// (a CIDR, a bare region with no datacenter ordinal, an unfilled placeholder)
// early, before it reaches the object-storage apply. It does NOT constrain the
// ordinal: both legacy (-1) and newer-generation (e.g. -10) clusters are valid —
// the exact set is account/region-specific (`linode-cli object-storage
// clusters-list`). Empty is allowed (the caller decides whether unset is OK).
func OBJClusterID(v string) error {
	if v == "" {
		return nil
	}
	if !objClusterRe.MatchString(v) {
		return fmt.Errorf("obj_cluster %q is not a Linode OBJ cluster id (expected e.g. us-ord-1 or us-ord-10); "+
			"list them with `linode-cli object-storage clusters-list`", v)
	}
	return nil
}

// IPFamily selects which address family a CIDRList must contain. The ACL is two
// separate spec fields (apiServerAllowCIDRs.ipv4 / .ipv6) fed by two separate
// flags, so a v6 prefix handed to the v4 flag is not a harmless mix — it is
// written into the wrong field and fails at apply, or seeds the wrong list.
type IPFamily int

const (
	AnyFamily IPFamily = iota
	IPv4
	IPv6
)

func (f IPFamily) String() string {
	switch f {
	case IPv4:
		return "IPv4"
	case IPv6:
		return "IPv6"
	default:
		return "IP"
	}
}

// CIDRList validates a comma-separated CIDR list destined for
// cluster.apiServerAllowCIDRs: every entry must parse, belong to want's address
// family, and none may match every address. field names the caller's flag/field
// in the message. An empty list is allowed — github.com-hosted runners open
// their egress IP at runtime (`llz ci runner-acl open`), so an empty seed is a
// legitimate choice.
//
// An open-world prefix is not a permissive setting in a control-plane ACL, it is
// a disabled one: LKE-E accepts it, the apply succeeds, and the API server is
// then reachable from the whole internet with nothing to indicate it. `llz env
// add`'s flag help has said "never 0.0.0.0/0" since the flag existed; this is
// the check behind it.
//
// The test is on the parsed PREFIX LENGTH, not the spelling. A zero-length mask
// matches everything however it is written — "0.0.0.0/0", "::/0", "0::/0", and
// "203.0.113.7/0", which net.ParseCIDR happily normalizes to 0.0.0.0/0 while
// looking, to a human and to a string comparison, like a specific host.
func CIDRList(field, csv string, want IPFamily) error {
	for _, raw := range strings.Split(csv, ",") {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		_, network, err := net.ParseCIDR(entry)
		if err != nil {
			hint := ""
			if ip := net.ParseIP(entry); ip != nil {
				full := "/32" // IPv4
				if ip.To4() == nil {
					full = "/128" // IPv6 — a /32 there is 79 octillion addresses
				}
				hint = fmt.Sprintf(" — a bare address needs a prefix length, e.g. %s%s", entry, full)
			}
			return fmt.Errorf("%s: %q is not a CIDR%s", field, entry, hint)
		}
		if got := familyOf(network.IP); want != AnyFamily && got != want {
			return fmt.Errorf("%s: %q is %s — it belongs in the %s list (the ACL keeps the two families in separate fields)",
				field, entry, got, got)
		}
		if IsOpenWorldCIDR(entry) {
			return fmt.Errorf("%s: %q matches every address (%s) and opens the Kubernetes control plane to "+
				"the entire internet — list your operator/CI egress prefixes instead, or leave it empty for "+
				"github.com-hosted runners (they open their own egress IP at runtime via `llz ci runner-acl open`)",
				field, entry, network.String())
		}
	}
	return nil
}

// familyOf classifies a parsed address. net.IP holds v4 as a 16-byte v4-in-v6
// form, so To4() is the test, not len().
func familyOf(ip net.IP) IPFamily {
	if ip.To4() != nil {
		return IPv4
	}
	return IPv6
}

// IsOpenWorldCIDR reports whether entry is a CIDR whose mask matches every
// address. Shared so the flag check (CIDRList) and the doctor scan of the
// RENDERED tfvars — which sees values that arrived by any route, including a
// hand edit that never passed a flag — apply one rule to one definition.
func IsOpenWorldCIDR(entry string) bool {
	_, network, err := net.ParseCIDR(strings.TrimSpace(entry))
	if err != nil {
		return false
	}
	ones, _ := network.Mask.Size()
	return ones == 0
}

// Forge returns an error if f is not a recognized git-forge flavor. Recognized
// is not the same as supported end-to-end — internal/forge.Supported is the
// gate for that; this only rejects a value that names no forge at all.
func Forge(f string) error {
	switch f {
	case ForgeGitHub, ForgeGitHubEnterprise, ForgeGitHubEnterpriseServer, ForgeGitLab:
		return nil
	default:
		return fmt.Errorf("forge %q invalid (want %s|%s|%s|%s)", f,
			ForgeGitHub, ForgeGitHubEnterprise, ForgeGitHubEnterpriseServer, ForgeGitLab)
	}
}

// HATopology checks a single deployment's OpenBao HA role/group combination:
// standalone (the default) takes no group; active/standby each require the
// shared pair id. fieldRole/fieldGroup name the fields in the caller's error
// messages (e.g. "--ha-role"/"--ha-group" for the CLI, "ha.role"/"ha.group"
// for the spec) so one rule serves both surfaces.
func HATopology(role, group, fieldRole, fieldGroup string) error {
	switch role {
	case "", RoleStandalone:
		if group != "" {
			return fmt.Errorf("%s set but %s is standalone — drop one", fieldGroup, fieldRole)
		}
	case RoleActive, RoleStandby:
		if group == "" {
			return fmt.Errorf("%s %s requires %s (the pair id shared with its peer)", fieldRole, role, fieldGroup)
		}
	default:
		return fmt.Errorf("%s %q invalid (want %s|%s|%s)", fieldRole, role, RoleActive, RoleStandby, RoleStandalone)
	}
	return nil
}
