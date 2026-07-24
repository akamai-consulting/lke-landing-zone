package main

// scaffold_spec.go is the spec-first half of `llz env add`: it authors the
// declarative LandingZone spec (landingzone.yaml + environments/<env>.yaml) that
// `llz render` then reconciles into the tfvars + apl-values overlay. The first
// `env add` in an instance creates landingzone.yaml from .copier-answers.yml (the
// instance identity) seeded with shared spec.defaults; each `env add` then writes
// one environments/<env>.yaml ClusterDefinition from the flags. The cluster
// identity the spec doesn't carry by flag (clusterLabel, bootstrap.name) is
// derived from the instance name so every env gets a unique, editable default.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/tfroots"
)

// shortRepoName returns the <name> half of an <owner>/<name> instance_repo (or
// the whole string if there's no slash).
func shortRepoName(repo string) string {
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		return repo[i+1:]
	}
	return repo
}

// tfvarsExampleValue reads a root's terraform.tfvars.example from the embedded
// tfroots package (it no longer ships in the instance) and returns key's value with
// surrounding quotes stripped (empty if the root/key is absent). The seeded keys
// (k8s_version, node_type, node_count) carry no copier token, so reading the raw
// embed is sufficient.
func tfvarsExampleValue(root, key string) string {
	b, err := tfroots.TfvarsExample(root)
	if err != nil {
		return ""
	}
	return strings.Trim(tfvarsValue(string(b), key), `"`)
}

// ensureLandingZone creates landingzone.yaml at specRoot from .copier-answers.yml
// (the instance identity) seeded with spec.defaults from the cluster
// terraform.tfvars.example, unless it already exists. It returns the instance name
// (the <name> half of instance_repo, used to derive per-env cluster identity) and
// whether it created the file.
func ensureLandingZone(specRoot string) (instanceName string, created bool, err error) {
	a, _ := readAnswers(specRoot)
	if a == nil {
		a = &answers{}
	}
	instanceName = shortRepoName(a.InstanceRepo)
	if instanceName == "" {
		instanceName = filepath.Base(mustAbs(specRoot))
	}

	lzPath := filepath.Join(specRoot, clusterspec.LandingZoneFile)
	if _, statErr := os.Stat(lzPath); statErr == nil {
		return instanceName, false, nil
	}

	// Identity comes from .copier-answers.yml in a real instance; fall back to the
	// upstream defaults so the template-root path (CI scaffold checks, which have
	// no rendered answers file) still authors a spec that validates. Warn on the
	// fallback so a fork doesn't silently inherit the wrong upstream.
	if a.UpstreamOrg == "" || a.InstanceRepo == "" {
		fmt.Fprintf(os.Stderr, "warning: no .copier-answers.yml identity — defaulting spec.instance to akamai-consulting / %s; edit %s if that's wrong.\n",
			instanceName, clusterspec.LandingZoneFile)
	}
	upstreamOrg := orElse(a.UpstreamOrg, "akamai-consulting")
	repo := orElse(a.InstanceRepo, instanceName+"/"+instanceName)
	version := orElse(orElse(a.Version, a.Commit), "main")
	k8s := orElse(tfvarsExampleValue("cluster", "k8s_version"), "v1.33.6+lke7")
	nodeType := orElse(tfvarsExampleValue("cluster", "node_type"), "g8-dedicated-8-4")
	nodeCount := orElse(tfvarsExampleValue("cluster", "node_count"), "5")
	// The default OpenBao team, chosen at `llz new` (copier openbao_team question);
	// the built-in platform team when unset. Written as an explicit spec.teams so
	// a NEW instance gets a non-root write path out of the box (there is no
	// load-time default — existing instances opt in via the retrofit runbook).
	team := orElse(a.OpenbaoTeam, clusterspec.DefaultTeamName)

	lz := fmt.Sprintf(`# LandingZone spec — created by `+"`llz env add`"+`. The single source for this
# instance: edit it (+ one environments/<env>.yaml per deployment), then
# `+"`llz render`"+` reconciles it into the tfvars + apl-values overlay.
# See docs/landing-zone-spec.md.
apiVersion: %s
kind: %s
metadata:
  name: %s
spec:
  instance:
    upstreamOrg: %s
    repo: %s
    forge: github
    templateVersion: %s
  # Team-scoped OpenBao WRITE access (non-root), chosen at `+"`llz new`"+`. Each entry
  # becomes a native apl-core team (namespace + Keycloak group/role team-<name>) and
  # a <name>-writer policy; operators use `+"`llz openbao login --team <name>`"+`, then
  # `+"`llz openbao set`"+`. Add more teams here. See docs/runbooks/openbao-team-login.md.
  teams:
    - name: %s
      openbaoSubtree: secret/%s
  # Instance-wide DNS/cert config rendered into every env's overlay. Uncomment +
  # set to fill the cert-manager DNS-01 issuer's ACME email from the spec (else
  # it stays a REPLACE_PER_ENV placeholder you fill by hand).
  # dns:
  #   acmeEmail: ops@example.com
  # Alertmanager receivers (default none: alerts aggregate but notify nobody).
  # To notify Slack: uncomment, re-render, then seed the webhook into OpenBao
  # (llz openbao set alerts/webhooks slack_url=...) — see docs/alerting.md.
  # alerting:
  #   receivers: [slack]
  # Shared defaults inherited by every environment (a per-env value overrides
  # field-by-field). Add spec.networks here to co-locate clusters in one VPC.
  defaults:
    cluster:
      k8sVersion: %s
      nodePool: { type: %s, count: %s }
      # LLZ runs exclusively on Linode's MANAGED App Platform (apl_enabled): Linode
      # installs+manages apl-core and provisions the lke<id>.akamai-apl.net domain +
      # DNS + wildcard cert. Do NOT set cluster.bootstrap.domainSuffix (Linode owns
      # the domain). Declare optional apl-core apps you enable in the Console via
      # cluster.bootstrap.managedApps (e.g. [harbor, loki]). See docs/adr/0005.
      bootstrap: { managedAppPlatform: true }
`, clusterspec.APIVersion, clusterspec.Kind, instanceName,
		upstreamOrg, repo, version, team, team, k8s, nodeType, nodeCount)

	if err := os.WriteFile(lzPath, []byte(lz), 0o644); err != nil {
		return instanceName, false, err
	}
	return instanceName, true, nil
}

// writeEnvDefinition authors environments/<env>.yaml from the flags. Required
// identity the flags don't carry (clusterLabel, bootstrap.name) is derived from
// the instance name; unset optional fields are omitted so they inherit
// spec.defaults. k8sVersion/nodePool are written only when overridden by a flag.
func writeEnvDefinition(path, name string, o envAddOpts, instanceName string) error {
	label := instanceName + "-" + name
	role := orElse(o.haRole, "standalone")

	var b strings.Builder
	fmt.Fprintf(&b, `apiVersion: %s
kind: %s
metadata:
  name: %s
spec:
  cluster:
    clusterLabel: %s
    region: %s
`, clusterspec.APIVersion, clusterspec.KindClusterDefinition, name, label, o.region)

	if o.k8sVersion != "" {
		fmt.Fprintf(&b, "    k8sVersion: %s\n", o.k8sVersion)
	}
	if o.nodeType != "" || o.nodeCount != "" {
		b.WriteString("    nodePool: {")
		var parts []string
		if o.nodeType != "" {
			parts = append(parts, " type: "+o.nodeType)
		}
		if o.nodeCount != "" {
			parts = append(parts, " count: "+o.nodeCount)
		}
		b.WriteString(strings.Join(parts, ",") + " }\n")
	}
	if o.runnerIPv4CIDRs != "" || o.runnerIPv6CIDRs != "" {
		b.WriteString("    apiServerAllowCIDRs:\n")
		fmt.Fprintf(&b, "      ipv4: %s\n", yamlList(o.runnerIPv4CIDRs))
		fmt.Fprintf(&b, "      ipv6: %s\n", yamlList(o.runnerIPv6CIDRs))
	}
	if o.network != "" || o.subnetCIDR != "" {
		b.WriteString("    network: {")
		var parts []string
		if o.network != "" {
			parts = append(parts, " vpc: "+o.network)
		}
		if o.subnetCIDR != "" {
			parts = append(parts, " subnetCIDR: "+o.subnetCIDR)
		}
		b.WriteString(strings.Join(parts, ",") + " }\n")
	}
	b.WriteString("    ha:\n")
	fmt.Fprintf(&b, "      role: %s\n", role)
	if o.haGroup != "" {
		fmt.Fprintf(&b, "      group: %s\n", o.haGroup)
	}
	if o.promotionRank > 0 {
		fmt.Fprintf(&b, "    promotionRank: %d\n", o.promotionRank)
	}
	b.WriteString("    bootstrap:\n")
	fmt.Fprintf(&b, "      name: %s\n", label)
	// domainSuffix is NOT authored: managedAppPlatform (inherited from spec.defaults)
	// means Linode owns the lke<id>.akamai-apl.net domain; validateEnv rejects a
	// non-empty domainSuffix. See docs/adr/0005-managed-app-platform.md.
	if o.aplChartVersion != "" {
		fmt.Fprintf(&b, "      aplChartVersion: %s\n", o.aplChartVersion)
	}
	if o.aplValuesRepoURL != "" {
		b.WriteString("      aplValues:\n")
		fmt.Fprintf(&b, "        repoURL: %s\n", o.aplValuesRepoURL)
	}
	fmt.Fprintf(&b, "    objectStorage:\n      cluster: %s\n", o.objCluster)
	b.WriteString("  # components omitted → all default-enabled except dns. Add a components:\n")
	b.WriteString("  # block to toggle or size them (see docs/landing-zone-spec.md).\n")

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// haGroupMissingRole returns the HA role (active|standby) still missing from
// group after the current env was authored, or "" when the pair is complete or no
// spec loads. Used by `llz env add` to defer the render of a half-authored pair.
func haGroupMissingRole(group string) string {
	lz, present, err := loadSpec()
	if !present || err != nil {
		return ""
	}
	var actives, standbys int
	for _, e := range lz.Spec.Environments {
		if e.Cluster.HA.Group != group {
			continue
		}
		switch e.Cluster.HA.Role {
		case "active":
			actives++
		case "standby":
			standbys++
		}
	}
	switch {
	case actives == 0:
		return "active"
	case standbys == 0:
		return "standby"
	default:
		return ""
	}
}

// yamlList renders a comma-separated CIDR string as a YAML flow sequence.
func yamlList(csv string) string {
	var out []string
	for _, p := range strings.Split(csv, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, quote(p))
		}
	}
	return "[" + strings.Join(out, ", ") + "]"
}

func orElse(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}

func mustAbs(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}
