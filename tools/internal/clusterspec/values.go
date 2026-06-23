package clusterspec

import (
	"bytes"
	"fmt"
	"strconv"

	yaml "gopkg.in/yaml.v3"
)

// values.go renders the apl-core backend: a deployment's component toggles flip
// apps.<key>.enabled in the committed apl-values/<env>/values.yaml, and the
// spec-owned identity + platform settings (cluster.name/domainSuffix,
// dns.domainFilters, otomi.has*) plus per-component sizing are written in. For
// identity this RESOLVES the ${cluster_name}/${cluster_domain} placeholders from
// the spec before Terraform runs — so for a spec instance landingzone.yaml is the
// single source and the cluster-bootstrap templatefile() (which still fills those
// for non-spec instances) finds nothing left to substitute. apl-core is a Helm
// umbrella whose bundled apps (prometheus, loki, harbor, kyverno, …) are switched
// inside ONE values.yaml — so unlike the manifest backend, this is not a resource
// selection but a targeted edit of existing keys.
//
// values.yaml is hand-authored with load-bearing comments and ${...} placeholders
// the cluster-bootstrap Terraform renders via templatefile() (the secrets + infra
// outputs: loki/harbor buckets, coredns IP, repo creds, dns token — plus identity
// for non-spec instances); we therefore parse + re-emit with yaml.v3's Node API,
// which preserves comments AND each scalar's quoting style (so "${coredns_cluster_ip}"
// stays quoted). Only the keys the spec owns are touched; everything else
// round-trips unchanged in meaning.

// ValuesIdentity carries the spec-derived settings RenderValues writes directly
// into a deployment's values.yaml, resolving the identity placeholders from the
// spec before Terraform's templatefile() runs. Build it with
// (*LandingZone).ValuesIdentity.
type ValuesIdentity struct {
	ClusterName  string // cluster.name (was ${cluster_name})
	DomainSuffix string // cluster.domainSuffix + dns.domainFilters[0] (was ${cluster_domain})
	ExternalDNS  bool   // otomi.hasExternalDNS
	ExternalIDP  bool   // otomi.hasExternalIDP
}

// ValuesIdentity resolves the values.yaml identity + platform settings for env
// from the assembled spec (env identity is already merged with spec.defaults; the
// platform flags are instance-wide).
func (lz *LandingZone) ValuesIdentity(env string) ValuesIdentity {
	e, _ := lz.Env(env)
	return ValuesIdentity{
		ClusterName:  e.Cluster.Bootstrap.Name,
		DomainSuffix: e.Cluster.Bootstrap.DomainSuffix,
		ExternalDNS:  lz.Spec.Defaults.Platform.HasExternalDNS(),
		ExternalIDP:  lz.Spec.Defaults.Platform.HasExternalIDP(),
	}
}

// RenderValues returns base (an apl-core values.yaml) with apps.<key>.enabled set
// from the component toggles (each component's AplCoreApps are enabled iff the
// component is) and the spec-owned identity + platform keys set from id. Apps not
// present in base, or with no enabled key, are left alone; a spec-owned key absent
// from base is skipped (never invented).
func RenderValues(base []byte, components map[string]ComponentToggle, id ValuesIdentity) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(base, &doc); err != nil {
		return nil, fmt.Errorf("parse values.yaml: %w", err)
	}
	if len(doc.Content) == 0 {
		return nil, fmt.Errorf("values.yaml is empty")
	}
	root := doc.Content[0]
	apps := mapValue(root, "apps")
	if apps == nil {
		return nil, fmt.Errorf("values.yaml has no apps: section")
	}

	// Desired enabled state per apl-core app key (later components win, but the
	// registry is disjoint on AplCoreApps so order is immaterial).
	want := map[string]bool{}
	for _, c := range Components {
		for _, app := range c.AplCoreApps {
			want[app] = ComponentEnabled(components, c.Name)
		}
	}
	for app, enabled := range want {
		appNode := mapValue(apps, app)
		if appNode == nil {
			continue // app not in this values.yaml — leave the template as-is
		}
		en := mapValue(appNode, "enabled")
		if en == nil {
			continue // no explicit enabled key (e.g. external-dns) — don't invent one
		}
		en.Tag = "!!bool"
		en.Value = boolString(enabled)
	}

	// Spec-owned identity + platform. Each is set only when the key already exists
	// (so a stripped-down values.yaml isn't grown new keys) and, for the string
	// identity, only when the spec provides a value.
	if cluster := mapValue(root, "cluster"); cluster != nil {
		setStr(mapValue(cluster, "name"), id.ClusterName)
		setStr(mapValue(cluster, "domainSuffix"), id.DomainSuffix)
	}
	if dns := mapValue(root, "dns"); dns != nil {
		if filters := mapValue(dns, "domainFilters"); filters != nil &&
			filters.Kind == yaml.SequenceNode && len(filters.Content) > 0 {
			setStr(filters.Content[0], id.DomainSuffix)
		}
	}
	if otomi := mapValue(root, "otomi"); otomi != nil {
		setBool(mapValue(otomi, "hasExternalDNS"), id.ExternalDNS)
		setBool(mapValue(otomi, "hasExternalIDP"), id.ExternalIDP)
	}

	// Per-component sizing (config in the spec, mechanism in the base). Each knob
	// overwrites an existing scalar in the base; unset knobs leave the base default.
	if o, ok := components["observability"]; ok {
		setStr(dig(apps, "prometheus", "retention"), o.Retention)
		setStr(dig(apps, "prometheus", "storageSize"), o.Storage)
		if o.Replicas != nil {
			setInt(dig(apps, "prometheus", "replicas"), *o.Replicas)
		}
	}
	if h, ok := components["harbor"]; ok {
		setStr(dig(apps, "harbor", "_rawValues", "persistence", "persistentVolumeClaim", "registry", "size"), h.RegistryStorage)
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2) // match the hand-authored values.yaml
	if err := enc.Encode(&doc); err != nil {
		return nil, fmt.Errorf("encode values.yaml: %w", err)
	}
	_ = enc.Close()
	return buf.Bytes(), nil
}

// setStr overwrites a scalar node with a plain string literal. No-op when the node
// is absent (key not in base) or the value is empty (nothing to render).
func setStr(n *yaml.Node, val string) {
	if n == nil || val == "" {
		return
	}
	n.Kind = yaml.ScalarNode
	n.Tag = "!!str"
	n.Style = 0 // plain — drop any ${...}-placeholder quoting
	n.Value = val
}

// setBool overwrites a scalar node with a bool literal. No-op when absent.
func setBool(n *yaml.Node, val bool) {
	if n == nil {
		return
	}
	n.Kind = yaml.ScalarNode
	n.Tag = "!!bool"
	n.Style = 0
	n.Value = boolString(val)
}

// setInt overwrites a scalar node with an int literal. No-op when absent.
func setInt(n *yaml.Node, val int) {
	if n == nil {
		return
	}
	n.Kind = yaml.ScalarNode
	n.Tag = "!!int"
	n.Style = 0
	n.Value = strconv.Itoa(val)
}

// dig walks a chain of mapping keys, returning the value node or nil if any level
// is missing (so a slimmed-down base never grows new structure).
func dig(n *yaml.Node, keys ...string) *yaml.Node {
	for _, k := range keys {
		n = mapValue(n, k)
		if n == nil {
			return nil
		}
	}
	return n
}

// mapValue returns the value node for key in a mapping node, or nil.
func mapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
