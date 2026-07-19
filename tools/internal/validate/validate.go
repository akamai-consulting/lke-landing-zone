// Package validate holds the small, pure input validators shared between the
// `llz` CLI (cmd/llz) and the declarative LandingZone spec (internal/clusterspec).
// They were originally inlined in cmd/llz (validateEnvName, validateOBJCluster,
// the --ha-role/--ha-group check); centralizing them here lets the spec validator
// reuse the exact same rules without an import cycle (clusterspec → validate, and
// cmd/llz → validate). Keep this package side-effect free.
package validate

import (
	"fmt"
	"regexp"
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
