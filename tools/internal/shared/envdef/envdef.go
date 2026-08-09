package envdef

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
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/answers"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/tfroots"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/tfvars"
)

// ShortRepoName returns the <name> half of an <owner>/<name> instance_repo (or
// the whole string if there's no slash).
func ShortRepoName(repo string) string {
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
	return strings.Trim(tfvars.Value(string(b), key), `"`)
}

// EnsureLandingZone creates landingzone.yaml at specRoot from .copier-answers.yml
// (the instance identity) seeded with spec.defaults from the cluster
// terraform.tfvars.example, unless it already exists. It returns the instance name
// (the <name> half of instance_repo, used to derive per-env cluster identity) and
// whether it created the file.
func EnsureLandingZone(specRoot string) (instanceName string, created bool, err error) {
	a, _ := answers.Read(specRoot)
	if a == nil {
		a = &answers.File{}
	}
	instanceName = ShortRepoName(a.InstanceRepo)
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
	upstreamOrg := OrElse(a.UpstreamOrg, "akamai-consulting")
	repo := OrElse(a.InstanceRepo, instanceName+"/"+instanceName)
	// No templateVersion here on purpose: the pin is copier's, and a scaffolded copy
	// of it in the spec only went stale (see Instance.TemplateVersion).
	k8s := OrElse(tfvarsExampleValue("cluster", "k8s_version"), "v1.33.6+lke7")
	nodeType := OrElse(tfvarsExampleValue("cluster", "node_type"), "g8-dedicated-8-4")
	nodeCount := OrElse(tfvarsExampleValue("cluster", "node_count"), "5")
	// The default OpenBao team, chosen at `llz new` (copier openbao_team question);
	// the built-in platform team when unset. Written as an explicit spec.teams so
	// a NEW instance gets a non-root write path out of the box (there is no
	// load-time default — existing instances opt in via the retrofit runbook).
	team := OrElse(a.OpenbaoTeam, clusterspec.DefaultTeamName)

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
    # Namespaces this instance's Object Storage bucket + key labels. Linode bucket
    # labels are GLOBAL PER REGION across accounts, so two instances sharing this
    # collide on "[400] ... already exists" at apply. Derived from the instance
    # name; written out so the effective value is visible. Changing it after an
    # apply orphans the existing buckets.
    objLabelPrefix: %s
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
      # cluster.bootstrap.managedApps (e.g. [harbor, loki]). See docs/adr/0005-managed-app-platform.md.
      bootstrap: { managedAppPlatform: true }
`, clusterspec.APIVersion, clusterspec.Kind, instanceName,
		upstreamOrg, repo, clusterspec.SanitizeObjLabelPrefix(instanceName),
		team, team, k8s, nodeType, nodeCount)

	if err := os.WriteFile(lzPath, []byte(lz), 0o644); err != nil {
		return instanceName, false, err
	}
	return instanceName, true, nil
}

// WriteEnvDefinition authors environments/<env>.yaml from the flags. Required
// identity the flags don't carry (clusterLabel, bootstrap.name) is derived from
// the instance name; unset optional fields are omitted so they inherit
// spec.defaults. k8sVersion/nodePool are written only when overridden by a flag.
// fileWriter is the one write this package makes. capability.RepoWriter satisfies
// it structurally, so a caller holding write-repo composes without this package
// importing the capability layer.
type fileWriter interface {
	MkdirAll(string, fs.FileMode) error
	WriteFile(string, []byte, fs.FileMode) error
}

// osWriter is the unfenced writer, for callers outside the capability model
// (internal/verbs declares no bindings by design).
type osWriter struct{}

func (osWriter) MkdirAll(p string, m fs.FileMode) error { return os.MkdirAll(p, m) }
func (osWriter) WriteFile(p string, b []byte, m fs.FileMode) error {
	return os.WriteFile(p, b, m)
}

// WriteEnvDefinition writes environments/<env>.yaml unfenced. See osWriter.
func WriteEnvDefinition(path, name string, o Opts, instanceName string) error {
	return WriteEnvDefinitionVia(osWriter{}, path, name, o, instanceName)
}

// WriteEnvDefinitionVia is WriteEnvDefinition through a caller's own writer.
//
// It exists because `llz env add` reached this function while its binding
// declared read-repo ALONE — a write laundered through a shared package, which
// is why the extension's own tests could believe only `definition` wrote.
func WriteEnvDefinitionVia(w fileWriter, path, name string, o Opts, instanceName string) error {
	label := instanceName + "-" + name
	role := OrElse(o.HARole, "standalone")

	var b strings.Builder
	fmt.Fprintf(&b, `apiVersion: %s
kind: %s
metadata:
  name: %s
spec:
  cluster:
    clusterLabel: %s
    region: %s
`, clusterspec.APIVersion, clusterspec.KindClusterDefinition, name, label, o.Region)

	if o.K8sVersion != "" {
		fmt.Fprintf(&b, "    k8sVersion: %s\n", o.K8sVersion)
	}
	if o.NodeType != "" || o.NodeCount != "" {
		b.WriteString("    nodePool: {")
		var parts []string
		if o.NodeType != "" {
			parts = append(parts, " type: "+o.NodeType)
		}
		if o.NodeCount != "" {
			parts = append(parts, " count: "+o.NodeCount)
		}
		b.WriteString(strings.Join(parts, ",") + " }\n")
	}
	if o.RunnerIPv4CIDRs != "" || o.RunnerIPv6CIDRs != "" {
		b.WriteString("    apiServerAllowCIDRs:\n")
		fmt.Fprintf(&b, "      ipv4: %s\n", yamlList(o.RunnerIPv4CIDRs))
		fmt.Fprintf(&b, "      ipv6: %s\n", yamlList(o.RunnerIPv6CIDRs))
	}
	if o.Network != "" || o.SubnetCIDR != "" {
		b.WriteString("    network: {")
		var parts []string
		if o.Network != "" {
			parts = append(parts, " vpc: "+o.Network)
		}
		if o.SubnetCIDR != "" {
			parts = append(parts, " subnetCIDR: "+o.SubnetCIDR)
		}
		b.WriteString(strings.Join(parts, ",") + " }\n")
	}
	b.WriteString("    ha:\n")
	fmt.Fprintf(&b, "      role: %s\n", role)
	if o.HAGroup != "" {
		fmt.Fprintf(&b, "      group: %s\n", o.HAGroup)
	}
	if o.PromotionRank > 0 {
		fmt.Fprintf(&b, "    promotionRank: %d\n", o.PromotionRank)
	}
	b.WriteString("    bootstrap:\n")
	fmt.Fprintf(&b, "      name: %s\n", label)
	// domainSuffix is NOT authored: managedAppPlatform (inherited from spec.defaults)
	// means Linode owns the lke<id>.akamai-apl.net domain; validateEnv rejects a
	// non-empty domainSuffix. See docs/adr/0005-managed-app-platform.md.
	if o.AplChartVersion != "" {
		fmt.Fprintf(&b, "      aplChartVersion: %s\n", o.AplChartVersion)
	}
	if o.AplValuesRepoURL != "" {
		b.WriteString("      aplValues:\n")
		fmt.Fprintf(&b, "        repoURL: %s\n", o.AplValuesRepoURL)
	}
	fmt.Fprintf(&b, "    objectStorage:\n      cluster: %s\n", o.ObjCluster)
	b.WriteString("  # components omitted → all default-enabled except dns. Add a components:\n")
	b.WriteString("  # block to toggle or size them (see docs/landing-zone-spec.md).\n")

	if err := w.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return w.WriteFile(path, []byte(b.String()), 0o644)
}

// HAGroupMissingRole returns the HA role (active|standby) still missing from
// group after the current env was authored, or "" when the pair is complete or no
// spec loads. Used by `llz env add` to defer the render of a half-authored pair.
func HAGroupMissingRole(group string) string {
	lz, present, err := clusterspec.Detected()
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

func OrElse(v, fallback string) string {
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
