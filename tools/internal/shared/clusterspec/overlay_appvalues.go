package clusterspec

// overlay_appvalues.go renders apl-values/_shared/apl-overlay/appvalues.yaml —
// the third overlay file, and the ONLY channel through which LLZ can set an
// apl-core app's chart values on the managed App Platform.
//
// WHY IT EXISTS. Until this file, LLZ had exactly two things it could say about
// an apl-core app: which object-storage bucket it uses (obj.yaml) and whether it
// is enabled (apps.yaml). Everything else lived in the apl-core values base the
// scaffold used to ship — a file that `llz render` STOPPED EMITTING when LLZ moved
// to the managed platform (values.go; render_test asserts a managed render never
// writes one). It kept shipping, kept being edited, and reached no cluster, and it
// has since been deleted. Two settings that mattered went with it:
//
//   - The argocd resource.customizations.health overrides — the PR #142
//     four-wedge bootstrap protection. wave-health-guard has been enforcing at PR
//     time that they exist in a file no cluster reads.
//   - Loki's ingester resources. An instance carried a 3Gi WAL-replay override,
//     believed it applied, and ran a 1Gi ingester in an OOM crashloop for 16 days
//     with log ingestion down.
//
// The transport was already here. The apl-overlay reconciler key-level-merges
// onto apl-operator's own AplApp CR at env/apps/<name>.yaml, and that CR carries
// `_rawValues` alongside `enabled` (lab-confirmed — see SetAppSpec). This file
// just gives LLZ a source to merge FROM.
//
// WHY _shared AND NOT THE SPEC. Everything here is a platform decision with a
// stated reason, exactly like aplStaticDisabledApps: no instance should be
// choosing whether Argo CD's NetworkPolicy health check can wedge its own
// bootstrap. If a per-env override is ever genuinely needed, the per-env layer
// merges on top with no change to this shape.
//
// THE HAZARD THIS FILE CANNOT CLOSE. A key that names nothing in the chart is
// silently ignored — that is the whole failure above, and moving it to a live
// channel does not make it self-verifying. Every entry below must therefore be
// backed by a gate that reads the CONSUMER, not this source. Loki's is
// `llz ci assert-loki` (lokiIngesterDurability, which reads the running
// ingester's limits and WAL volume); argocd's is wave-health-guard plus the
// bootstrap itself. Do not add an entry here without naming its gate.

import (
	"bytes"

	yaml "gopkg.in/yaml.v3"
)

// OverlayAppValuesFile is the appvalues.yaml basename under an apl-overlay/ dir.
// Declared beside the other two rather than with them so the reader meeting it in
// aplOverlayTargets lands on this file's header.
const OverlayAppValuesFile = "appvalues.yaml"

// LokiWALReplayMemoryLimit is the ingester memory limit this overlay asserts, and
// the floor `llz ci assert-loki` holds the RUNNING ingester to. One constant, two
// consumers, because the failure it encodes is precisely a source and a consumer
// that disagreed about whether a value had been applied.
//
// 3Gi, not 4Gi, and the reasoning is load-bearing: a healthy Loki that flushes to
// object storage never approaches it (a fresh-cluster e2e runs well under), while
// on a ~6Gi-allocatable node a 4Gi-limit pod co-scheduled with argocd-controller
// (~0.9Gi) and prometheus (~0.8Gi) can push the node to MemoryPressure and evict
// its neighbours. 3Gi keeps replay headroom while leaving a genuine runaway to
// OOM-restart Loki alone (self-heal) instead of taking the node with it.
const LokiWALReplayMemoryLimit = "3Gi"

// LokiWALReplayCeiling bounds the memory WAL replay may consume before Loki
// flushes to object storage and continues, and it is the SECOND half of the
// limit above — neither is load-bearing without the other.
//
// THE FAILURE IT ENCODES, measured on a live cluster. Loki's own default for
// `ingester.wal.replay_memory_ceiling` is 4GB. Set a 3Gi container limit and
// leave the ceiling unset and the two disagree in the one direction that cannot
// recover: replay grows toward a 4GB ceiling inside a 3.2GB cgroup, so the
// ceiling is unreachable, the flush that would drain the WAL never fires, and
// the kernel OOMKills the process ~11 seconds in. Every retry replays the same
// WAL and dies at the same place. Observed at 205 restarts over 2d7h with log
// ingestion down, on an ingester whose limit was CORRECT and whose gate was green.
//
// The limit above cannot be raised to escape this — 4Gi was considered and
// rejected there for node-eviction reasons that still hold. The ceiling is the
// knob that makes a 3Gi limit survivable instead of a trap.
//
// AND THE PVC MADE IT PERMANENT. persistence below moved the WAL off emptyDir so
// it survives pod recreation — which is right, and which also removed the escape
// hatch the emptyDir era relied on ("delete the pod, lose un-flushed chunks").
// After that change a replay that cannot fit has no way out at all. That is why
// this constant lands with the PVC rather than after it.
//
// ~48% of the limit, not 90%: the ceiling bounds the replay buffer, not the
// process. The ingester also holds its index, its gRPC buffers and the Go heap's
// own slack inside the same cgroup, and a ceiling just under the limit OOMs on
// that overhead while reporting a ceiling that "fits".
const LokiWALReplayCeiling = "1536MB"

// LokiIngesterStorageClass pins the ingester's WAL volume to the encrypted,
// retain-policy class the llz-cluster-foundation chart ships. Loki is NOT covered
// by cluster.defaultStorageClass on managed (apl-core leaves it unset there and
// falls back to linode-block-storage), so an unpinned class lands the WAL — which
// holds un-flushed log lines, including the OpenBao audit stream — on an
// unencrypted Volume.
const LokiIngesterStorageClass = "block-storage-retain"

// LokiWALClaimName is the chart's name for the ingester's WAL claim, and the name
// the pod mounts at /var/loki.
//
// DUPLICATED FROM health.LokiWALVolumeName ON PURPOSE, and the duplication is
// load-bearing rather than lazy. Importing health here would pull
// clusterspec → health → linode, and internal/shared/apl imports clusterspec — so
// the APL layer would gain a dependency on a concrete cloud, which
// TestAPLLayerDoesNotDependOnAConcreteCloud forbids (ADR 0013). The two constants
// are held equal by TestTheAssertedClaimNameIsTheOneTheProbeLooksFor in assertobs,
// which is the one package that legitimately imports both.
const LokiWALClaimName = "data"

// appValuesOverlayDoc marshals the apps.<name>._rawValues fragment. Same
// apps.<name>.<key> shape as appsOverlayDoc so the two files deep-merge with each
// other and with apl-core's own values without a special case.
type appValuesOverlayDoc struct {
	Apps map[string]appRawValues `yaml:"apps"`
}

type appRawValues struct {
	RawValues map[string]any `yaml:"_rawValues,omitempty"`
}

// RenderAppValuesOverlayShared is the instance-wide appvalues.yaml base. It takes
// no spec input for the same reason RenderAppsOverlayShared takes none: every
// entry is a platform decision, so the output is deterministic and the committed
// template copy is held byte-identical to it by
// TestTemplateSharedOverlayMatchesRenderers.
func RenderAppValuesOverlayShared() string {
	apps := map[string]appRawValues{}
	for name, rv := range aplAppRawValues() {
		apps[name] = appRawValues{RawValues: rv}
	}
	return marshalYAML(appValuesOverlayDoc{Apps: apps})
}

// aplAppRawValues is the per-app `_rawValues` LLZ asserts on every environment.
// Built by a function rather than declared as a package var so no caller can
// mutate the shared maps out from under another (the reconciler merges into
// apl-operator's document, and a var would hand it the originals).
func aplAppRawValues() map[string]map[string]any {
	return map[string]map[string]any{
		"argocd": {"configs": map[string]any{"cm": argoHealthCustomizations()}},
		// TWO KEYS, TWO DESTINATIONS, and conflating them is the whole reason the
		// ceiling was missing. In the grafana/loki chart the TOP-LEVEL `ingester`
		// renders the StatefulSet (replicas, persistence, resources) and never
		// appears in Loki's config.yaml, while `loki.ingester` is templated INTO
		// that config file. A replay ceiling written under the first key is
		// accepted by Helm, read by nothing, and leaves the default in force —
		// the exact silent no-op this file's header warns about.
		"loki":   {"ingester": lokiIngesterValues(), "loki": lokiRuntimeConfigValues()},
		"harbor": {"metrics": harborMetricsValues()},
	}
}

// lokiIngesterValues is the DISTRIBUTED-topology ingester config.
//
// TOPOLOGY, AND WHY THE KEY IS `ingester`. apl-core runs the grafana/loki chart in
// its distributed deployment mode: a `loki-ingester` StatefulSet started with
// `-target=ingester`, alongside separate querier/distributor/compactor/gateway
// workloads. The chart applies `singleBinary.*` ONLY in single-binary mode, so
// the override this replaces — written when LLZ self-installed apl-core in
// single-binary mode — named a key the running chart never reads. `ingester.*` is
// the distributed-mode key.
//
// CONFIRM THIS AGAINST THE RUNNING CHART, do not take it on faith from this
// comment — that is exactly the mistake being fixed:
//
//	kubectl -n monitoring get statefulset,deploy -l app.kubernetes.io/name=loki
//	kubectl -n monitoring get sts loki-ingester \
//	  -o jsonpath='{.spec.template.spec.containers[0].resources}'
//
// `llz ci assert-loki` asks the second question on every run and fails naming
// what it actually found, so a wrong key here surfaces as a red lane rather than
// as a silent no-op. That gate, not this comment, is what makes the entry safe.
//
// WHY PERSISTENCE IS NOT OPTIONAL. The chart's default ingester volume is an
// `emptyDir`, which survives CONTAINER restarts within a pod. So an ingester that
// OOMs mid-WAL-replay replays the identical WAL on every retry and dies
// identically — the loop cannot self-heal, and the only way out is deleting the
// pod, which discards un-flushed chunks. Observed live: 104,337 BackOff events
// over 16 days. A real PVC is what makes the WAL survivable rather than a trap.
func lokiIngesterValues() map[string]any {
	return map[string]any{
		// Requests stay modest (burstable) — the limit is what bounds the replay
		// spike. 500m CPU also made replay needlessly slow; 1 CPU roughly halves
		// the not-ready window.
		"resources": map[string]any{
			"requests": map[string]any{"cpu": "100m", "memory": "512Mi"},
			"limits":   map[string]any{"cpu": "1", "memory": LokiWALReplayMemoryLimit},
		},
		// CLAIMS, NOT size/storageClass AT THIS LEVEL — and getting this wrong once
		// already is the reason the comment is this long. `ingester.persistence`
		// takes {enabled, inMemory, claims[], enableStatefulSetAutoDeletePVC,
		// whenDeleted, whenScaled}; the size and the class live on each entry of
		// `claims`. A `persistence.size` / `persistence.storageClass` pair one level
		// up is accepted silently by Helm and read by nothing, which would have left
		// the PVC at the chart default (10Gi, NO storageClassName) — the default
		// provisioner, an UNENCRYPTED volume, holding a WAL that carries the OpenBao
		// audit stream. Verified against grafana/loki chart 6.55.0's values.yaml.
		//
		// The claim is named `data` because that is the chart's own name for it and
		// what the ingester mounts at /var/loki; health.LokiWALVolumeName is the same
		// string on the consumer side.
		"persistence": map[string]any{
			"enabled": true,
			"claims": []any{
				map[string]any{
					"name":         LokiWALClaimName,
					"size":         "5Gi",
					"storageClass": LokiIngesterStorageClass,
					"accessModes":  []any{"ReadWriteOnce"},
				},
			},
		},
	}
}

// lokiRuntimeConfigValues is the `loki.*` half: values templated into Loki's
// rendered config.yaml rather than into a Kubernetes object.
//
// ITS GATE is `llz ci assert-loki` (health.LokiIngesterDurability), which reads
// the ceiling back off the config the ingester actually loaded and fails when it
// is absent or does not fit inside the delivered memory limit. Absent is a
// FAILURE there rather than a default, because absent is precisely the state
// that OOMKills: it means Loki's own 4GB default is in force above a 3Gi limit.
//
// ONLY `wal` IS SET. `loki.ingester` is rendered with `{{- with }}` + `toYaml`
// over the whole map, and apl-core already supplies `chunk_encoding: snappy`
// there. Helm deep-merges maps, so adding `wal` keeps that sibling; REPLACING
// the map would silently drop it. Do not add keys here without checking what
// apl-core already puts in the same block.
func lokiRuntimeConfigValues() map[string]any {
	return map[string]any{
		"ingester": map[string]any{
			"wal": map[string]any{
				"replay_memory_ceiling": LokiWALReplayCeiling,
			},
		},
	}
}

// harborMetricsValues turns Harbor's Prometheus metrics on.
//
// IT CAME BACK FROM THE RETIRED VALUES FILE, and it is here because leaving it
// out would have been a quiet regression in the same release that added its
// alerts. Harbor's metrics are OFF by default in the goharbor chart, so
// harbor-core/registry/jobservice expose no :8001 /metrics and no harbor-exporter
// is deployed — Harbor was the one support-plane service with zero metrics
// coverage (issue #183). This block was carried in the apl-core values base,
// which reached no cluster, so on managed it has never actually applied; the
// difference now is that appvalues.yaml is a channel that does.
//
// That matters this release specifically: llz-scheduled-checks.yml newly runs
// alert-eval over `^(Loki|Harbor|Grafana|OTel|SupportPlane)`, so without this the
// three Harbor rules would report DEAD?/NOMATCH forever — a new signal whose first
// act is to tell everyone it cannot see anything.
//
// ITS GATE IS THAT alert-eval STEP, and it is WEAKER than the rule at the top of
// this file asks for — said plainly rather than glossed. The strong gate would be
// assert-scrape-targets' defaultScrapeMonitors, and this entry was put there for
// one commit before being withdrawn: Harbor is ManagedConditionalOn "harbor", so
// listing it turned a GATING lane permanently red on every instance that does not
// run Harbor. A gate nobody can turn green gets the lane switched off, which is
// the failure this whole PR is about, one level up.
//
// So the honest position is: this key is reported on, not gated on, until the
// scrape set can be made component-aware. If it names nothing, alert-eval says so
// on the weekly run and nothing fails.
//
// additionalLabels is not optional: apl-core's Prometheus matches
// serviceMonitorSelector {prometheus: system}, so a ServiceMonitor without that
// label is silently never scraped. The chart emits its own ServiceMonitor (it
// knows its service labels and ports); LLZ only adds the label. The scrape path
// itself (Prometheus in `monitoring` → harbor :8001) is opened by the
// llz-cluster-foundation harbor-allow-metrics NetworkPolicy, and the mesh is
// PERMISSIVE so the plaintext scrape needs no PeerAuthentication exception.
func harborMetricsValues() map[string]any {
	return map[string]any{
		"enabled": true,
		"serviceMonitor": map[string]any{
			"enabled":          true,
			"additionalLabels": map[string]any{"prometheus": "system"},
		},
	}
}

// argoHealthCustomizations is the PR #142 bootstrap-wedge protection, moved here
// from the retired apl-core values base unchanged except for the two messages
// that named that file as their source.
//
// Argo CD gates sync-wave progression on PER-RESOURCE health. A resource at wave
// N that sits Progressing forever, or goes Degraded, blocks or fails every wave
// above N of the platform-bootstrap sync — and OpenBao is at wave 0, so the whole
// cluster bootstrap wedges. Two of the four 2026-07-04 wedges were this class and
// each cost a ~50-minute e2e run to find.
//
// wave-health-guard pins each key below to the kinds that depend on it
// (guards/wavehealth/health.go, AllowedKinds.overrideKey), so deleting one
// re-fails the guard at PR time.
func argoHealthCustomizations() map[string]any {
	return map[string]any{
		// LKE's CNI does not populate NetworkPolicy status.conditions (KEP-2943),
		// so Argo's built-in check reports Progressing forever. That is what stuck
		// platform-bootstrap at wave -18 on
		// "waiting for healthy state of NetworkPolicy/argo-resync-nudger-allow-egress",
		// leaving the ESO NPs (-10), the DNS ClusterIssuers (-5) and OpenBao (0)
		// never applied. An applied NetworkPolicy is as healthy as it gets here.
		"resource.customizations.health.networking.k8s.io_NetworkPolicy": `local hs = {}
hs.status = "Healthy"
hs.message = "NetworkPolicy applied (no status on this CNI; health forced — see apl-values/_shared/apl-overlay/appvalues.yaml)"
return hs
`,
		// The llz-letsencrypt-* ClusterIssuers ship with a deferrable ACME email
		// (REPLACE_PER_ENV until spec.dns.acmeEmail is set — a documented supported
		// state). Let's Encrypt rejects registration, cert-manager sets Ready=False,
		// and Argo grades that Degraded, failing the wave -5 sync task on every
		// retry. (The argocd.argoproj.io/ignore-healthcheck annotation demonstrably
		// does NOT exempt a resource from operation-level wave gating — only from
		// the app health rollup.) cert-manager keeps retrying on its own and
		// `llz ci health` reports issuer readiness independently.
		"resource.customizations.health.cert-manager.io_ClusterIssuer": `local hs = {}
hs.status = "Healthy"
hs.message = "ClusterIssuer applied (readiness not sync-gating; llz ci health reports it — see apl-values/_shared/apl-overlay/appvalues.yaml)"
if obj.status ~= nil and obj.status.conditions ~= nil then
  for _, c in ipairs(obj.status.conditions) do
    if c.type == "Ready" then
      hs.message = "Ready=" .. c.status .. ": " .. (c.message or "")
    end
  end
end
return hs
`,
		// The three ESO overrides are LENIENT, not forced: not-Ready becomes
		// Progressing rather than Healthy. They still wait, which is correct — they
		// converge once OpenBao is bootstrapped, and none sits at a wave that gates
		// OpenBao itself. During first boot the `openbao` ClusterSecretStore
		// legitimately cannot be Ready until OpenBao is unsealed and the `eso`
		// Kubernetes-auth role is configured.
		"resource.customizations.health.external-secrets.io_ClusterSecretStore": `local hs = {}
if obj.status ~= nil and obj.status.conditions ~= nil then
  for _, c in ipairs(obj.status.conditions) do
    if c.type == "Ready" then
      if c.status == "True" then
        hs.status = "Healthy"
      else
        -- OpenBao not yet reachable/unsealed or the eso role not yet configured:
        -- still converging, do not fail the sync.
        hs.status = "Progressing"
      end
      hs.message = c.message
      return hs
    end
  end
end
hs.status = "Progressing"
hs.message = "Waiting for ClusterSecretStore Ready condition"
return hs
`,
		"resource.customizations.health.external-secrets.io_ExternalSecret": `local hs = {}
if obj.status ~= nil and obj.status.conditions ~= nil then
  for _, c in ipairs(obj.status.conditions) do
    if c.type == "Ready" then
      if c.status == "True" then
        hs.status = "Healthy"
      else
        -- ESO reconciles on refreshInterval; a not-yet-synced secret (e.g. an
        -- OpenBao path not seeded yet) self-heals. Progressing, never Degraded.
        hs.status = "Progressing"
      end
      hs.message = c.message
      return hs
    end
  end
end
hs.status = "Progressing"
hs.message = "Waiting for ExternalSecret to sync"
return hs
`,
		// The generated-secrets PushSecrets (grafana/admin, otel/ingress) cannot
		// write until OpenBao is unsealed and the eso-pusher role exists; ESO
		// retries on refreshInterval. Same rationale as ExternalSecret above.
		"resource.customizations.health.external-secrets.io_PushSecret": `local hs = {}
if obj.status ~= nil and obj.status.conditions ~= nil then
  for _, c in ipairs(obj.status.conditions) do
    if c.type == "Ready" then
      if c.status == "True" then
        hs.status = "Healthy"
      else
        hs.status = "Progressing"
      end
      hs.message = c.message
      return hs
    end
  end
end
hs.status = "Progressing"
hs.message = "Waiting for PushSecret to sync"
return hs
`,
	}
}

// AppRawValues parses a merged appvalues overlay into {app: _rawValues}. Apps
// with no `_rawValues` map are skipped — nothing to assert, and an empty map
// written onto apl-operator's CR would be a change with no meaning.
func AppRawValues(mergedAppValues []byte) (map[string]map[string]any, error) {
	m, err := unmarshalMap(mergedAppValues)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]any{}
	apps, _ := m["apps"].(map[string]any)
	for name, v := range apps {
		am, ok := v.(map[string]any)
		if !ok {
			continue
		}
		rv, ok := am["_rawValues"].(map[string]any)
		if !ok || len(rv) == 0 {
			continue
		}
		out[name] = rv
	}
	return out, nil
}

// AppOverlay is everything LLZ asserts about one apl-core app. Both fields are
// optional and independently absent: an app may carry only a toggle (most do),
// only values (argocd, which is a core app with no `enabled` key at all), or
// both. Absent means "no opinion" — never a default.
type AppOverlay struct {
	Enabled   *bool          // nil → leave apl-core's own spec.enabled alone
	RawValues map[string]any // nil/empty → leave apl-core's own spec._rawValues alone
}

// AppOverlays composes the two merged sources into one desired state per app, so
// the reconciler writes each env/apps/<name>.yaml ONCE. Two independent writers
// on the same target would race on the `files` map and the second would silently
// drop the first's edit.
func AppOverlays(mergedApps, mergedAppValues []byte) (map[string]AppOverlay, error) {
	toggles, err := AppToggles(mergedApps)
	if err != nil {
		return nil, err
	}
	values, err := AppRawValues(mergedAppValues)
	if err != nil {
		return nil, err
	}
	out := make(map[string]AppOverlay, len(toggles)+len(values))
	for app, on := range toggles {
		enabled := on
		out[app] = AppOverlay{Enabled: &enabled}
	}
	for app, rv := range values {
		cur := out[app]
		cur.RawValues = rv
		out[app] = cur
	}
	return out, nil
}

// SetAppSpec key-level-merges LLZ's desired state onto apl-operator's existing
// AplApp CR (env/apps/<name>.yaml), preserving every other field it owns —
// resources, autoscaling, and any _rawValues key LLZ does not name (lab-confirmed:
// apl-operator re-populates its defaults and keeps what we set).
//
// changed IS THE WHOLE CONTRACT, and it is SEMANTIC, never byte-wise. apl-operator
// rewrites this file on its own schedule with different indentation and key order.
// A byte comparison would report a diff on every pass, and the reconciler would
// push on every pass, fighting apl-operator forever. So: compare the values LLZ
// asserts against the values already there, and return the current bytes untouched
// when they already agree.
//
// It follows that LLZ only ever asserts a SUBSET. A _rawValues key apl-operator
// adds that LLZ does not name is not a difference — it is apl-core's business.
func SetAppSpec(current []byte, want AppOverlay) (updated []byte, changed bool, err error) {
	m, err := unmarshalMap(current)
	if err != nil {
		return nil, false, err
	}
	spec, ok := m["spec"].(map[string]any)
	if !ok {
		spec = map[string]any{}
		m["spec"] = spec
	}
	if want.Enabled != nil {
		if cur, ok := spec["enabled"].(bool); !ok || cur != *want.Enabled {
			spec["enabled"] = *want.Enabled
			changed = true
		}
	}
	if len(want.RawValues) > 0 {
		base, _ := spec["_rawValues"].(map[string]any)
		merged, diff := mergeAsserted(base, want.RawValues)
		if diff {
			spec["_rawValues"] = merged
			changed = true
		}
	}
	if !changed {
		return current, false, nil // already correct — no push, no re-marshal
	}
	return marshalMap(m), true, nil
}

// SetAppEnabled is SetAppSpec for callers that assert only the toggle. Kept as
// its own name because "set enabled" is what the apps.yaml overlay means, and
// collapsing it into the two-field call at every site would make the toggle path
// harder to read than it is.
func SetAppEnabled(current []byte, want bool) (updated []byte, changed bool, err error) {
	return SetAppSpec(current, AppOverlay{Enabled: &want})
}

// mergeAsserted merges over onto base and reports whether anything ACTUALLY
// differed. It is mergeMaps plus the change signal, and the signal is why it is
// separate: mergeMaps cannot distinguish "wrote the same value again" from "wrote
// a new one", and on this path that distinction is the difference between a quiet
// reconciler and one that pushes a commit every pass forever.
//
// Nested maps recurse; every other value (scalar, sequence) is replaced wholesale
// and compared with reflect.DeepEqual — a sequence is asserted as a unit because
// merging two lists element-wise has no defensible meaning here.
func mergeAsserted(base, over map[string]any) (map[string]any, bool) {
	if base == nil {
		base = map[string]any{}
	}
	changed := false
	for k, ov := range over {
		bv, present := base[k]
		if present {
			if bm, ok1 := bv.(map[string]any); ok1 {
				if om, ok2 := ov.(map[string]any); ok2 {
					merged, sub := mergeAsserted(bm, om)
					base[k] = merged
					changed = changed || sub
					continue
				}
			}
			// SEQUENCES OF NAMED MAPS MERGE BY NAME, everything else compares whole.
			//
			// A wholesale comparison is right for an anonymous list (merging two
			// argument lists element-wise has no defensible meaning), and WRONG for
			// `ingester.persistence.claims`, whose entries are keyed by `name` and
			// which apl-operator re-emits with the chart's own defaults filled in
			// (volumeAttributesClassName, accessModes it normalised, keys LLZ never
			// mentions). Compared whole, LLZ's four-key claim would never equal
			// apl-operator's six-key one, and the reconciler would push a commit on
			// every pass forever — the exact churn loop this function's header
			// forbids, reintroduced by the value TYPE rather than by the logic.
			//
			// So a list whose elements are maps carrying `name` is merged per entry,
			// subset-wise, exactly like the maps above.
			if bs, os, ok := namedMapSeqs(bv, ov); ok {
				merged, sub := mergeNamedSeq(bs, os)
				base[k] = merged
				changed = changed || sub
				continue
			}
			if yamlEqual(bv, ov) {
				continue
			}
		}
		base[k] = ov
		changed = true
	}
	return base, changed
}

// yamlEqual reports whether two values encode to the same YAML. It compares the
// ENCODING rather than the Go values because the two sides arrive by different
// routes — the asserted side from Go literals in this file, the current side from
// apl-operator's parsed document — and a comparison that disagreed with what gets
// written would push a commit on every reconcile pass forever.
//
// A type change IS a difference and stays one: `cpu: "1"` (string, what a
// Kubernetes quantity must be) and `cpu: 1` (int) encode differently, so the
// asserted string wins. That is the intended outcome, not a false positive.
func yamlEqual(a, b any) bool {
	ab, aerr := yaml.Marshal(a)
	bb, berr := yaml.Marshal(b)
	if aerr != nil || berr != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}

// namedMapSeqs reports whether both values are sequences whose elements are maps
// carrying a `name`, which is the shape a keyed merge is defensible for. It is
// deliberately narrow: anything else falls through to the wholesale comparison,
// because "merge two lists" has no general answer.
//
// The convention is Kubernetes' own — strategic-merge patch keys list elements by
// `name` for exactly this reason — and the chart's claims[] follows it.
func namedMapSeqs(bv, ov any) (bs, os []any, ok bool) {
	bs, ok1 := bv.([]any)
	os, ok2 := ov.([]any)
	if !ok1 || !ok2 || len(os) == 0 {
		return nil, nil, false
	}
	for _, seq := range [][]any{bs, os} {
		for _, e := range seq {
			m, isMap := e.(map[string]any)
			if !isMap {
				return nil, nil, false
			}
			if _, hasName := m["name"].(string); !hasName {
				return nil, nil, false
			}
		}
	}
	return bs, os, true
}

// mergeNamedSeq merges over's entries onto base's by `name`, preserving base
// entries LLZ says nothing about and appending ones it names that are absent.
// Order follows base, so apl-operator's ordering survives and does not read as a
// change.
func mergeNamedSeq(base, over []any) ([]any, bool) {
	changed := false
	idx := map[string]int{}
	out := make([]any, len(base))
	for i, e := range base {
		m := e.(map[string]any)
		out[i] = m
		idx[m["name"].(string)] = i
	}
	for _, e := range over {
		om := e.(map[string]any)
		name := om["name"].(string)
		i, found := idx[name]
		if !found {
			out = append(out, om)
			changed = true
			continue
		}
		merged, sub := mergeAsserted(out[i].(map[string]any), om)
		out[i] = merged
		changed = changed || sub
	}
	return out, changed
}
