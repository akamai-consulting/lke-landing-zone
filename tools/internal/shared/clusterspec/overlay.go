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
// shared/apl/overlay). Kept minimal — the obj block, the enabled map, and the
// per-app values LLZ asserts — so the blast radius of overlaying onto a file
// apl-operator co-writes stays small.
//
// appvalues.yaml is the newest and the one to be careful with: it is the only
// channel that can set an apl-core app's CHART values on managed, which is
// exactly why it must stay a narrow, gated list rather than a general escape
// hatch. Its contents and the rule for adding to them are in
// overlay_appvalues.go (OverlayAppValuesFile is declared there, beside them).
const (
	OverlayObjFile  = "obj.yaml"
	OverlayAppsFile = "apps.yaml"
)

// aplStaticDisabledApps are the apl-core apps the landing zone keeps OFF for
// every environment (no per-env component drives them) — the enabled:false set in
// apl-values/_shared/values.yaml. They render into the _shared
// apps overlay. Keep in lockstep with that values.yaml block. external-dns is NOT
// here: its schema permits no `enabled` key (it is gated by otomi.hasExternalDNS).
var aplStaticDisabledApps = []string{
	// GITEA IS HERE AND NOT A PER-ENV COMPONENT TOGGLE, and putting it here is
	// what keeps this list in the lockstep its own comment claims — the values.yaml
	// block it mirrors has listed `gitea: { enabled: false }` all along.
	//
	// It arrived here by a wrong turn worth recording. The per-env overlay used to
	// emit it from the DefaultDisabled gitea component, and that was removed as an
	// ownership violation on the grounds that apl-core's in-cluster gitea is the
	// values-repo backend the overlay travels through. It is not: bootstrap-cluster
	// repoints apl-core at the external GitHub apl-<env> branch (patchAplGitConfig)
	// before any of this runs, so the transport is GitHub and gitea is just another
	// app. Removing the emission therefore removed the ONLY thing that turns gitea
	// off on managed — values.yaml is not rendered there — and a new cluster would
	// have kept it running, with the unencrypted gitea-valkey PVC that
	// health/workloads.go documents.
	//
	// Why LLZ has standing to disable it, unlike kyverno or trivy: apl-core 6.x
	// gates apl-gitea-operator on gitea.enabled, and with BYO-Git that operator's
	// clone path is dead weight pointing at a repo the platform does not use. This
	// is a stated platform decision, which is exactly what this list is for — the
	// per-env gate is for apps whose toggle LLZ merely happens to know about.
	"gitea",
	"knative",
	"kserve",
	"kubeflow-pipelines",
	"linode-cfw",
	"rabbitmq",
	"tekton",
}

// The obj overlay is apl-core's `AplObjectStorage` settings CR (env/settings/obj.yaml),
// NOT a bare `obj:` map — LAB-CONFIRMED against apl-core v6.0.0's fixture + schema
// (tests/fixtures/env/settings/obj.yaml; values-schema.yaml $.obj), and re-confirmed
// byte-identical at v6.1.0 (the whole tests/fixtures/env/ tree moved only
// settings/cluster.yaml + settings/otomi.yaml). Config lives under
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
func RenderObjOverlayEnv(prefix, env, objCluster string) string {
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
				"loki":   ObjLokiChunksBucket(prefix, env),
				"harbor": ObjHarborRegistryBucket(prefix, env),
			},
		}}},
	})
}

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
// for every apl-core app a component owns AND LLZ IS ENTITLED TO SPEAK FOR, set
// from that component's toggle (the same truth RenderValues writes into
// values.yaml, as an overlayable fragment).
//
// IT MUST NOT SPEAK FOR apl-core's OWN APPS, so it gates on
// Component.EmitOnManaged like every other renderer (kustomize.go and render.go
// both do). Walking the registry unfiltered writes two classes of nonsense onto
// the machine-owned apl-<env> branch of every managed instance:
//
//   - `gitea: enabled: false`, because the gitea component is DefaultDisabled.
//     On managed, apl-core runs its own in-cluster gitea AS THE VALUES-REPO
//     BACKEND — so that is a write that disables the thing carrying it.
//   - `kyverno`, `policy-reporter` and `trivy` FORCE-ENABLED, from policyEngine
//     and imageScanning. Turning on an app the operator did not ask for is
//     quieter than turning one off and no less wrong: LLZ has no manifest backend
//     for any of them.
//
// A component LLZ does not emit is a component LLZ has no opinion about, so its
// app is simply absent from the overlay and apl-core's own value survives the
// merge — the whole point of the overlay being a fragment rather than a file
// replacement.
//
// THE WHOLE GATE, NOT JUST ManagedSkip. EmitOnManaged also honours
// ManagedConditionalOn, so with an explicit `managedApps: [harbor, kyverno]` the
// observability component's apps (prometheus, loki, grafana, otel, alertmanager)
// leave the overlay too. `llz ci bootstrap-cluster` is what enables an env's
// managedApps in apl-core; the overlay drives the toggles of components LLZ ships
// alongside them, and render.go already drops observability's MANIFESTS under the
// same condition.
//
// A KNOWN, UNFIXED GAP SITS BEHIND THIS: DependsOn is enforced over toggles and
// not over this gate. llzReconciler emits a ServiceMonitor and PrometheusRule when
// observability does not emit, and imageSignature ships a Kyverno ClusterPolicy
// gated only on kyverno being DECLARED in managedApps — both against CRDs that may
// then be absent. Do not paper over the second by force-enabling kyverno here: a
// continuous re-assertion of someone else's app is not a dependency mechanism. It
// belongs to components.go's dependency model, and keeping the overlay
// inconsistent to hide it would leave the real gap with nothing pointing at it.
func RenderAppsOverlayEnv(boot Bootstrap, components map[string]ComponentToggle) string {
	apps := map[string]appToggle{}
	for _, c := range Components {
		if len(c.AplCoreApps) == 0 {
			continue
		}
		// SAY NOTHING about an app whose component this instance does not emit.
		// Absent is not `enabled: false`: the overlay deep-merges onto apl-core's
		// own settings, so omitting a key leaves apl-core's value and writing
		// `false` overrides it.
		if !c.EmitOnManaged(boot, components) {
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
