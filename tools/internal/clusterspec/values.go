package clusterspec

import (
	"bytes"
	"fmt"

	yaml "gopkg.in/yaml.v3"
)

// values.go renders the apl-core backend: a deployment's component toggles flip
// apps.<key>.enabled in the committed apl-values/<env>/values.yaml. apl-core is a
// Helm umbrella whose bundled apps (prometheus, loki, harbor, kyverno, …) are
// switched inside ONE values.yaml — so unlike the manifest backend, this is not a
// resource selection but a targeted edit of existing keys.
//
// values.yaml is hand-authored with load-bearing comments and ${...} placeholders
// the cluster-bootstrap Terraform renders via templatefile(); we therefore parse +
// re-emit with yaml.v3's Node API, which preserves comments AND each scalar's
// quoting style (so "${coredns_cluster_ip}" stays quoted and ${cluster_name} stays
// plain). Only the apps.<key>.enabled scalars the registry maps are touched;
// everything else round-trips unchanged in meaning.

// RenderValues returns base (an apl-core values.yaml) with apps.<key>.enabled set
// from the component toggles: each component's AplCoreApps are enabled iff the
// component is. Apps not present in base, or with no enabled key, are left alone.
func RenderValues(base []byte, components map[string]ComponentToggle) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(base, &doc); err != nil {
		return nil, fmt.Errorf("parse values.yaml: %w", err)
	}
	if len(doc.Content) == 0 {
		return nil, fmt.Errorf("values.yaml is empty")
	}
	apps := mapValue(doc.Content[0], "apps")
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

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2) // match the hand-authored values.yaml
	if err := enc.Encode(&doc); err != nil {
		return nil, fmt.Errorf("encode values.yaml: %w", err)
	}
	_ = enc.Close()
	return buf.Bytes(), nil
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
