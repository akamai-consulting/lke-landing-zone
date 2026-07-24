package clusterspec

// overlay.go renders the apl-values/{_shared,<env>}/apl-overlay/ tree — the
// spec-owned, secret-free source of truth for the apl-core config the landing
// zone drives into apl-core's NATIVE values (obj.provider.linode object storage +
// apps.<name>.enabled toggles). See docs/designs/apl-overlay-obj-native.md.
//
// The overlay is split, like values.yaml, into a _shared base and per-env
// overrides; the in-cluster apl-overlay reconciler reads both from the primary
// repo, fills the credential placeholders from OpenBao, MERGES them (env wins),
// and overlays the owned files onto the machine-owned apl-<env> branch. Two of
// the functions the reconciler needs — MergeOverlay + FillObjPlaceholders — live
// here (not in cmd/llz) so the shared/env composition and the placeholder
// contract are unit-tested in one place and the reconciler stays thin.
//
// NOTE — bucket model. apl-core's obj.provider.linode uses ONE bucket per app
// (buckets.loki / buckets.harbor), NOT the landing zone's three Loki buckets
// (chunks/ruler/admin). Adopting native obj therefore consolidates Loki to a
// single bucket; that live flip is lab-gated (see the design doc). The names here
// follow the llz-object-storage module's "<label_prefix>-<app>-<env>" convention.

import (
	"bytes"
	"fmt"
	"sort"

	yaml "gopkg.in/yaml.v3"
)

// The accessKeyId placeholder the committed overlay carries in place of the real
// (rotated) obj access-key ID. The apl-overlay reconciler is the ONLY filler — it
// substitutes this with the value read from OpenBao secret/obj/platform before
// overlaying onto apl-<env>. It never resolves on main, so nothing but a
// placeholder is committed there. The ${...} shape matches the repo's placeholder
// idiom; FillObjPlaceholders (not templatefile) is what replaces it.
//
// The secretAccessKey is DELIBERATELY not a placeholder and never touches git:
// the overlay leaves it blank (an empty x-secret apl-core seals nothing for) and
// ESO materializes the real value straight into the `obj-secrets` Secret from
// OpenBao (the openbao ClusterSecretStore → obj-secrets ExternalSecret). See
// platform-apl/components/observability/obj-secrets-externalsecret.yaml.
const ObjAccessKeyIDPlaceholder = "${obj_access_key_id}"

// Owned overlay file basenames (relative to an apl-overlay/ dir). The reconciler
// maps each onto a path in the apl-<env> values tree (aplOverlayTargets, in
// cmd/llz). Kept minimal — the obj block and the enabled map only — so the
// blast radius of overlaying onto a file apl-operator co-writes stays small.
const (
	OverlayObjFile  = "obj.yaml"
	OverlayAppsFile = "apps.yaml"
)

// aplStaticDisabledApps are the apl-core apps the landing zone keeps OFF for
// every environment (no per-env component drives them) — the enabled:false set in
// instance-template/apl-values/_shared/values.yaml. They render into the _shared
// apps overlay. Keep in lockstep with that values.yaml block. external-dns is NOT
// here: its schema permits no `enabled` key (it is gated by otomi.hasExternalDNS).
var aplStaticDisabledApps = []string{
	"knative",
	"kserve",
	"kubeflow-pipelines",
	"linode-cfw",
	"rabbitmq",
	"tekton",
}

// The obj overlay is apl-core's `AplObjectStorage` settings CR (env/settings/obj.yaml),
// NOT a bare `obj:` map — LAB-CONFIRMED against apl-core v6.0.0's fixture + schema
// (tests/fixtures/env/settings/obj.yaml; values-schema.yaml $.obj). Config lives under
// `spec` (kind/metadata/spec), all-but-buckets omitempty so the _shared and per-env
// fragments each emit ONLY their own keys and deep-merge cleanly.
//
// secretAccessKey is NOT rendered: it is an x-secret in apl-core's schema (kept out of
// env/settings, additionalProperties governs the rest), and apl-core reads it from the
// `obj-secrets` Secret via ESO (loki-raw.gotmpl / harbor-raw.gotmpl:
// property provider_linode_secretAccessKey). LLZ populates that Secret from OpenBao
// (obj-secrets ExternalSecret). accessKeyId, by contrast, apl-core INLINES from these
// settings, so the reconciler fills it — see ObjAccessKeyIDPlaceholder + linode/apl-core#3459.
const objKind = "AplObjectStorage"

type objOverlayDoc struct {
	Kind     string  `yaml:"kind"`
	Metadata objMeta `yaml:"metadata"`
	Spec     objSpec `yaml:"spec"`
}

type objMeta struct {
	Name string `yaml:"name"`
}

type objSpec struct {
	ShowWizard *bool       `yaml:"showWizard,omitempty"`
	Provider   objProvider `yaml:"provider"`
}

type objProvider struct {
	Type   string    `yaml:"type,omitempty"`
	Linode objLinode `yaml:"linode"`
}

type objLinode struct {
	Region      string            `yaml:"region,omitempty"`
	AccessKeyID string            `yaml:"accessKeyId,omitempty"`
	Buckets     map[string]string `yaml:"buckets,omitempty"`
}

// RenderObjOverlayShared is the instance-wide obj.yaml base: the AplObjectStorage CR
// identity, showWizard off, provider linode, and the accessKeyId placeholder the
// reconciler fills. No region/buckets (those are per-env). secretAccessKey is
// deliberately absent (apl-core sources it from obj-secrets via ESO).
func RenderObjOverlayShared() string {
	off := false
	return marshalYAML(objOverlayDoc{
		Kind:     objKind,
		Metadata: objMeta{Name: "obj"},
		Spec: objSpec{
			ShowWizard: &off,
			Provider: objProvider{
				Type:   "linode",
				Linode: objLinode{AccessKeyID: ObjAccessKeyIDPlaceholder},
			},
		},
	})
}

// RenderObjOverlayEnv is a deployment's per-env obj.yaml override: the object-storage
// region (the OBJ cluster id) and the loki/harbor bucket names, derived from the spec
// per the llz-object-storage module's naming. It deep-merges onto the _shared CR (same
// kind/metadata; spec.provider.linode gains region+buckets). Empty when the env
// declares no object-storage cluster (nothing to point at).
func RenderObjOverlayEnv(env, objCluster string) string {
	if objCluster == "" {
		return ""
	}
	return marshalYAML(objOverlayDoc{
		Kind:     objKind,
		Metadata: objMeta{Name: "obj"},
		Spec: objSpec{Provider: objProvider{Linode: objLinode{
			Region: objCluster,
			Buckets: map[string]string{
				// apl-core native obj uses ONE bucket per app. Point Loki at the EXISTING
				// primary Loki bucket (the chunks bucket the object-storage module already
				// provisions), which Loki multiplexes chunks/ruler/admin within — so this
				// works with no new bucket. A dedicated single platform-loki-<env> bucket
				// is the cleaner future target (lab-gated; see the design doc). Harbor uses
				// its existing registry bucket. Both on the platform-<app>-<env> convention.
				"loki":   objLabelPrefix + "-loki-chunks-" + env,
				"harbor": objLabelPrefix + "-harbor-registry-" + env,
			},
		}}},
	})
}

// objLabelPrefix mirrors the llz-object-storage module's label_prefix default —
// keep in lockstep with the module if that default changes.
const objLabelPrefix = "platform"

// appsOverlayDoc marshals the apps.<name>.enabled toggle fragment.
type appsOverlayDoc struct {
	Apps map[string]appToggle `yaml:"apps"`
}

type appToggle struct {
	Enabled bool `yaml:"enabled"`
}

// RenderAppsOverlayShared is the instance-wide apps.yaml base: the statically
// disabled apps (aplStaticDisabledApps). Per-env component toggles override/extend
// it via RenderAppsOverlayEnv + the reconciler's merge.
func RenderAppsOverlayShared() string {
	apps := make(map[string]appToggle, len(aplStaticDisabledApps))
	for _, a := range aplStaticDisabledApps {
		apps[a] = appToggle{Enabled: false}
	}
	return marshalYAML(appsOverlayDoc{Apps: apps})
}

// RenderAppsOverlayEnv is a deployment's per-env apps.yaml: apps.<name>.enabled
// for every apl-core app a component owns, set from that component's toggle (the
// same truth RenderValues writes into values.yaml, as an overlayable fragment).
func RenderAppsOverlayEnv(components map[string]ComponentToggle) string {
	apps := map[string]appToggle{}
	for _, c := range Components {
		if len(c.AplCoreApps) == 0 {
			continue
		}
		on := ComponentEnabled(components, c.Name)
		for _, app := range c.AplCoreApps {
			apps[app] = appToggle{Enabled: on}
		}
	}
	return marshalYAML(appsOverlayDoc{Apps: apps})
}

// MergeOverlay deep-merges an env overlay fragment onto a _shared base (env wins
// on a scalar conflict; maps merge recursively). Both are YAML documents; the
// result is re-emitted canonically. Used by the reconciler to compose the two
// overlay layers before it fills + overlays them. A nil/empty layer is treated as
// the empty map.
func MergeOverlay(shared, env []byte) ([]byte, error) {
	base, err := unmarshalMap(shared)
	if err != nil {
		return nil, fmt.Errorf("parse _shared overlay: %w", err)
	}
	over, err := unmarshalMap(env)
	if err != nil {
		return nil, fmt.Errorf("parse env overlay: %w", err)
	}
	return marshalMap(mergeMaps(base, over)), nil
}

// FillObjPlaceholders substitutes the committed accessKeyId placeholder with the
// live value read from OpenBao. Operates on bytes (a rendered/merged overlay) so
// the reconciler need not re-parse — the placeholder is a unique token. An empty
// input is left as the placeholder (nothing to fill), so a missing OpenBao read
// never writes an empty accessKeyId. The secretAccessKey is intentionally NOT
// handled here — it never transits git; ESO writes it into obj-secrets directly.
func FillObjPlaceholders(overlay []byte, accessKeyID string) []byte {
	if accessKeyID == "" {
		return overlay
	}
	return bytes.ReplaceAll(overlay, []byte(ObjAccessKeyIDPlaceholder), []byte(accessKeyID))
}

// AppToggles parses a merged apps overlay (apps.<name>.enabled) into LLZ's desired
// {app: enabled} state. That map is LLZ's SOURCE of truth; the reconciler fans it
// out to apl-core's per-app AplApp CRs at env/apps/<name>.yaml — NOT a single
// env/settings/apps.yaml (apl-core has no such file). Apps with no `enabled` scalar
// are skipped (nothing to assert).
func AppToggles(mergedApps []byte) (map[string]bool, error) {
	m, err := unmarshalMap(mergedApps)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	apps, _ := m["apps"].(map[string]any)
	for name, v := range apps {
		if am, ok := v.(map[string]any); ok {
			if en, ok := am["enabled"].(bool); ok {
				out[name] = en
			}
		}
	}
	return out, nil
}

// SetAppEnabled key-level-merges spec.enabled onto apl-operator's existing AplApp CR
// (env/apps/<name>.yaml), preserving every other field it owns (resources,
// autoscaling, _rawValues — lab-confirmed: apl-operator re-populates its defaults and
// keeps whatever `enabled` we set). Returns changed=false when spec.enabled ALREADY
// equals want — the SEMANTIC skip (compare the value, never the bytes) that stops the
// reconciler churning against apl-operator's re-formatted (4-space, re-sorted) file.
func SetAppEnabled(current []byte, want bool) (updated []byte, changed bool, err error) {
	m, err := unmarshalMap(current)
	if err != nil {
		return nil, false, err
	}
	spec, ok := m["spec"].(map[string]any)
	if !ok {
		spec = map[string]any{}
		m["spec"] = spec
	}
	if cur, ok := spec["enabled"].(bool); ok && cur == want {
		return current, false, nil // already correct — no push, no re-marshal
	}
	spec["enabled"] = want
	return marshalMap(m), true, nil
}

// mergeMaps recursively merges over onto base (over wins). Nested maps merge;
// every other value (scalar, sequence) is replaced wholesale by over's.
func mergeMaps(base, over map[string]any) map[string]any {
	if base == nil {
		base = map[string]any{}
	}
	for k, ov := range over {
		if bv, ok := base[k]; ok {
			if bm, ok1 := bv.(map[string]any); ok1 {
				if om, ok2 := ov.(map[string]any); ok2 {
					base[k] = mergeMaps(bm, om)
					continue
				}
			}
		}
		base[k] = ov
	}
	return base
}

// unmarshalMap decodes a YAML document into a string-keyed map, or an empty map
// for empty input. yaml.v3 decodes nested maps as map[string]interface{} when the
// top level is map[string]any, which mergeMaps relies on.
func unmarshalMap(b []byte) (map[string]any, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// marshalYAML encodes v at 2-space indent (matching the hand-authored values.yaml).
func marshalYAML(v any) string {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	_ = enc.Encode(v)
	_ = enc.Close()
	return buf.String()
}

// marshalMap encodes a merged map with map keys sorted (deterministic output for
// the reconciler's tree-sha no-op detection). yaml.v3 already sorts map[string]any
// keys, but we sort explicitly so the contract does not depend on that.
func marshalMap(m map[string]any) []byte {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	_ = enc.Encode(sortedNode(m))
	_ = enc.Close()
	return buf.Bytes()
}

// sortedNode builds a yaml.Node mapping with keys in sorted order (recursively),
// so a merged overlay marshals deterministically regardless of Go map iteration.
func sortedNode(v any) *yaml.Node {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		for _, k := range keys {
			kn := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}
			n.Content = append(n.Content, kn, sortedNode(t[k]))
		}
		return n
	default:
		n := &yaml.Node{}
		_ = n.Encode(v)
		return n
	}
}
