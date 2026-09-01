package clusterspec

// overlay_fields.go maps what the apl-overlay DECLARES onto what a live object
// would have to look like for the declaration to have landed.
//
// WHY IT EXISTS. appvalues.yaml's header states the rule this file serves: a key
// that names nothing in the chart is silently ignored, so every entry must be
// backed by a gate that reads the CONSUMER. That rule has been honoured one app
// at a time — `llz ci assert-loki` reads the running ingester — and it does not
// scale to an overlay that grows an entry per release. This is the generic half:
// for the paths named here, `llz ci assert-overlay-applied` can ask the cluster
// whether the declared value is what the object actually has, and — when it is
// not — whether the change is even APPLIABLE.
//
// THE FAILURE THE `CreateOnly` FLAG IS FOR. A field the API server fixes at
// CREATE time cannot be changed on an object that already exists. Argo CD
// computes its diff by dry-run-applying the desired state (ServerSideDiff=true,
// see kustomize.go), so when that apply is rejected NO DIFF IS PRODUCED and the
// Application reads Synced — the failure to apply is what makes the status green.
// Worse, the rejection is per OBJECT: one unappliable field blocks every other
// change to the same object, including perfectly mutable ones. Loki's 3Gi memory
// limit sat undelivered for that reason alone, sharing a StatefulSet with a WAL
// claim template that could not be added in place.
//
// On a fresh cluster none of this happens: the object is CREATED in its final
// shape, so there is no transition to reject. The trap exists only where an
// overlay is introduced over infrastructure that predates it — the normal
// brownfield adoption path, and the one configuration no e2e lane runs.
//
// THE TABLE IS DELIBERATELY SHORT AND EXPLICITLY INCOMPLETE. It is written
// against one apl-core version and the chart is not ours. A declared path with no
// row here is reported as UNCHECKED by the gate — never silently passed — and
// TestEveryDeclaredOverlayPathIsMappedOrExempt refuses at PR time a declared path
// that is neither mapped here nor exempted in OverlayUnmapped with a reason. A table
// that quietly vouches for what it did not examine would be another green check
// that means nothing, which is the failure this whole file is about.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// OverlayMatch is how a declared value is compared against what the live object
// carries.
type OverlayMatch int

const (
	// MatchScalar compares the declared leaf to the delivered leaf as strings.
	MatchScalar OverlayMatch = iota
	// MatchNonEmptyList reads a declared `true` as "this list must have entries".
	// It is what a chart toggle that GATES a structure looks like from the
	// cluster side: `persistence.enabled` renders volumeClaimTemplates, and the
	// only honest question to ask an object is whether they are there.
	MatchNonEmptyList
)

// OverlayField is one declared overlay path and the live field it must reach.
type OverlayField struct {
	// App is the apl-core app whose _rawValues carries Value.
	App string
	// Value is the path within that app's _rawValues, e.g.
	// {"ingester","resources","limits","memory"}.
	Value []string
	// Kind/Namespace/Name identify the live object the value has to land on, as
	// kubectl names it.
	Kind, Namespace, Name string
	// OwnerApp is the Argo CD Application that declares this object — the one that
	// has to RECREATE it after a migration deletes it.
	//
	// IT IS A PRECONDITION, NOT DOCUMENTATION. A migration that deletes an object
	// whose owning Application cannot sync leaves the workload with no controller
	// and nothing to put it back, and because an absent object reads as
	// nothing-to-do, no later run retries it. Checking the owner first is the
	// difference between a repair and a one-way door.
	OwnerApp string
	// APIGroupVersion/Resource identify the SAME object to the Kubernetes API, for
	// the in-cluster REST client the reconciler samples through — which has no
	// kubectl to resolve a kind for it. Two spellings of one object because two
	// clients need it; TestEveryOverlayFieldSpellsItsObjectBothWays holds them to
	// naming the same thing.
	APIGroupVersion, Resource string
	// Live is the path within the live object. A segment may carry a selector —
	// `containers[name=ingester]` — for a list keyed by name.
	Live  []string
	Match OverlayMatch
	// CreateOnly marks a field the API server fixes at create time, so a change
	// to it cannot be applied to an object that already exists. It must name a
	// Migration — a row that says "this cannot be applied in place" and does not
	// say what to do about it has told an operator only half of what it knows —
	// and TestEveryCreateOnlyOverlayFieldNamesItsMigration holds it to that.
	CreateOnly bool
	// Migration is the brownfield migration id that makes CreateOnly appliable —
	// the thing an operator has to run on a cluster that predates the overlay.
	Migration string
	// Patch builds the object-shaped patch that TESTS appliability, from the
	// app's whole _rawValues (not just the leaf: a claim template is assembled
	// from a sibling path). Server-dry-run only — it is never applied for real.
	Patch func(rawValues map[string]any) (string, error)
	// Why is what this field is for, printed with a failure so the report carries
	// its own rationale.
	Why string
}

// LokiIngesterContainer is the chart's name for the WAL-holding container, and
// the selector the memory-limit row matches on.
//
// A RENAME MUST SURFACE AS A MISS, NEVER AS A PASS. If the chart renames the
// container, the selector below finds nothing and the gate reports that it could
// not locate the field — which is a red lane naming the probe, not a green one
// naming nothing. That is the deliberate trade: assertobs's durability probe
// matches the ingester three ways because it must not go blind on a rename, and
// this one must not go QUIET on one.
const LokiIngesterContainer = "ingester"

// OverlayFields is the mapping table. Ordered so a report reads app by app.
func OverlayFields() []OverlayField {
	return []OverlayField{
		lokiIngesterResource("limits", "memory",
			"the ingester OOMs replaying its WAL at the chart default; this limit bounds the replay spike"),
		lokiIngesterResource("limits", "cpu",
			"500m made replay needlessly slow; 1 CPU roughly halves the not-ready window"),
		lokiIngesterResource("requests", "memory",
			"the requests stay modest (burstable) so the limit is what bounds the spike, not the schedule"),
		lokiIngesterResource("requests", "cpu",
			"as above — the request is the schedule, the limit is the bound"),
		{
			App:             "loki",
			Value:           []string{"ingester", "persistence", "enabled"},
			Kind:            "statefulset",
			OwnerApp:        "monitoring-loki",
			APIGroupVersion: "apps/v1",
			Resource:        "statefulsets",
			Namespace:       "monitoring",
			Name:            "loki-ingester",
			Live:            []string{"spec", "volumeClaimTemplates"},
			Match:           MatchNonEmptyList,
			CreateOnly:      true,
			Migration:       LokiWALPVCMigration,
			Why: "the chart's default ingester volume is an emptyDir, so an ingester that OOMs " +
				"mid-replay replays the identical WAL forever and the loop cannot self-heal",
			Patch: func(rv map[string]any) (string, error) {
				claims, err := rawSlice(rv, "ingester", "persistence", "claims")
				if err != nil {
					return "", err
				}
				templates := make([]any, 0, len(claims))
				for _, c := range claims {
					m, ok := c.(map[string]any)
					if !ok {
						return "", fmt.Errorf("ingester.persistence.claims holds a %T, not a mapping", c)
					}
					templates = append(templates, claimTemplate(m))
				}
				return jsonPatch(map[string]any{"spec": map[string]any{"volumeClaimTemplates": templates}})
			},
		},
	}
}

// lokiIngesterResource builds one container-resource row.
//
// FOUR ROWS FROM ONE FUNCTION, and every one of them is MUTABLE — which is the
// point. They live under spec.template and the API server takes them happily; the
// 3Gi limit went undelivered for weeks anyway, because it shares a StatefulSet
// with the claim template below and Argo's diff is computed per object. Rows that
// can only ever fail for a reason belonging to a DIFFERENT row are what make the
// contagion visible instead of inferable.
func lokiIngesterResource(section, resource, why string) OverlayField {
	return OverlayField{
		App:             "loki",
		Value:           []string{"ingester", "resources", section, resource},
		Kind:            "statefulset",
		OwnerApp:        "monitoring-loki",
		APIGroupVersion: "apps/v1",
		Resource:        "statefulsets",
		Namespace:       "monitoring",
		Name:            "loki-ingester",
		Live: []string{"spec", "template", "spec",
			"containers[name=" + LokiIngesterContainer + "]", "resources", section, resource},
		Match: MatchScalar,
		Why:   why,
		Patch: func(rv map[string]any) (string, error) {
			v, err := rawString(rv, "ingester", "resources", section, resource)
			if err != nil {
				return "", err
			}
			return jsonPatch(map[string]any{"spec": map[string]any{"template": map[string]any{
				"spec": map[string]any{"containers": []any{map[string]any{
					"name":      LokiIngesterContainer,
					"resources": map[string]any{section: map[string]any{resource: v}},
				}}},
			}}})
		},
	}
}

// ArgoNamespace is where the owning Applications live. One constant so the
// migration's owner check and any future reader ask the same question of the same
// place.
const ArgoNamespace = "argocd"

// LokiWALPVCMigration is the brownfield migration that makes the WAL claim
// template appliable on a cluster whose loki-ingester predates it. Named as a
// constant because two sides read it: the table above declares it as the remedy,
// and the migration itself is registered under the same id.
const LokiWALPVCMigration = "049-loki-wal-pvc"

// claimTemplate turns one chart claim entry into the PersistentVolumeClaim the
// StatefulSet would carry, which is what the appliability probe has to send.
//
// The chart's shape ({name,size,storageClass,accessModes}) is NOT the API's
// shape, and sending the chart's would have the API server reject it for being
// malformed — a rejection that looks exactly like the immutability rejection this
// probe exists to detect. Translating here keeps the two distinguishable.
func claimTemplate(c map[string]any) map[string]any {
	pvc := map[string]any{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata":   map[string]any{"name": c["name"]},
		"spec":       map[string]any{},
	}
	spec := pvc["spec"].(map[string]any)
	if modes, ok := c["accessModes"]; ok {
		spec["accessModes"] = modes
	}
	if size, ok := c["size"]; ok {
		spec["resources"] = map[string]any{"requests": map[string]any{"storage": size}}
	}
	if sc, ok := c["storageClass"]; ok {
		spec["storageClassName"] = sc
	}
	return pvc
}

// ── walking a declared overlay, and a live object ────────────────────────────

// RawValue resolves a path inside one app's _rawValues.
func RawValue(rv map[string]any, path ...string) (any, bool) {
	var cur any = rv
	for _, seg := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func rawString(rv map[string]any, path ...string) (string, error) {
	v, ok := RawValue(rv, path...)
	if !ok {
		return "", fmt.Errorf("the overlay declares no %s", strings.Join(path, "."))
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s is a %T, not a string", strings.Join(path, "."), v)
	}
	return s, nil
}

func rawSlice(rv map[string]any, path ...string) ([]any, error) {
	v, ok := RawValue(rv, path...)
	if !ok {
		return nil, fmt.Errorf("the overlay declares no %s", strings.Join(path, "."))
	}
	s, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%s is a %T, not a sequence", strings.Join(path, "."), v)
	}
	return s, nil
}

func jsonPatch(m map[string]any) (string, error) {
	b, err := json.Marshal(m)
	return string(b), err
}

// LiveValue walks a decoded live object along a path whose segments may carry a
// `[name=x]` selector.
//
// TWO WAYS TO NOT FIND SOMETHING, AND THEY MEAN OPPOSITE THINGS. A missing KEY
// is a value the object does not carry — an undelivered limit, an absent
// volumeClaimTemplates list — which is the finding itself. A missing SELECTOR
// TARGET is the probe pointed at something that is not there: the chart renamed
// the container, and this row now covers nothing. The first is a fact about the
// cluster; the second is a fact about this table, and reporting them the same way
// would let a rename be read as a value to go and deliver.
//
// missedSelector is that distinction, returned rather than inferred, because a
// caller cannot recover it from a bare false.
func LiveValue(obj map[string]any, path []string) (value any, found, missedSelector bool) {
	var cur any = obj
	for _, seg := range path {
		key, sel, hasSel := strings.Cut(seg, "[")
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false, false
		}
		cur, ok = m[key]
		if !ok {
			return nil, false, false
		}
		if !hasSel {
			continue
		}
		want, ok := strings.CutSuffix(sel, "]")
		if !ok {
			// A malformed selector in the TABLE, not a fact about the cluster. Reported
			// as a probe fault for the same reason a rename is.
			return nil, false, true
		}
		field, wantVal, ok := strings.Cut(want, "=")
		if !ok {
			return nil, false, true
		}
		list, ok := cur.([]any)
		if !ok {
			return nil, false, true
		}
		cur, ok = selectByField(list, field, wantVal)
		if !ok {
			return nil, false, true
		}
	}
	return cur, true, false
}

// selectByField picks the list element whose `field` equals want.
//
// NO FALLBACK TO "the only element". A single-container pod would make one
// tempting, and it would turn a chart rename — the case the selector exists to
// notice — into a silent match against whatever happened to be there. Missing is
// missing, and the gate reports it as a field it could not read.
func selectByField(list []any, field, want string) (any, bool) {
	for _, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if s, _ := m[field].(string); s == want {
			return m, true
		}
	}
	return nil, false
}

// ── the declared surface, for the guard that keeps this table honest ─────────

// AplAppRawValues is the per-app `_rawValues` this overlay asserts — the same map
// RenderAppValuesOverlayShared writes out.
//
// EXPORTED FOR THE GATE AND THE GUARD, so both walk what is actually rendered
// rather than a transcription of it. A copy is returned for the reason
// aplAppRawValues is a function and not a var: no caller may mutate the shared
// maps out from under another.
func AplAppRawValues() map[string]map[string]any { return aplAppRawValues() }

// DeclaredOverlayPaths lists every LEAF path the overlay declares, as
// "<app>.<dotted path>". A leaf is a scalar or a sequence: a sequence is asserted
// as a unit (mergeAsserted compares it that way), so descending into one would
// invent paths nothing declares.
//
// Sorted, because it is compared against a table in a test whose failure message
// should be stable.
func DeclaredOverlayPaths() []string {
	var out []string
	for app, rv := range aplAppRawValues() {
		out = append(out, leafPaths(app, rv)...)
	}
	sort.Strings(out)
	return out
}

func leafPaths(prefix string, m map[string]any) []string {
	var out []string
	for k, v := range m {
		p := prefix + "." + k
		if sub, ok := v.(map[string]any); ok && len(sub) > 0 {
			out = append(out, leafPaths(p, sub)...)
			continue
		}
		out = append(out, p)
	}
	return out
}

// APIPath is the object's Kubernetes API path, for a client that speaks REST
// rather than kubectl. Core-group objects live under /api/v1; everything else
// under /apis/<group>/<version>.
func (f OverlayField) APIPath() string {
	base := "/apis/" + f.APIGroupVersion
	if !strings.Contains(f.APIGroupVersion, "/") {
		base = "/api/" + f.APIGroupVersion
	}
	return base + "/namespaces/" + f.Namespace + "/" + f.Resource + "/" + f.Name
}

// OverlayFieldPath renders a field's declared path in DeclaredOverlayPaths' form,
// so the two can be compared without either side re-spelling the other.
func OverlayFieldPath(f OverlayField) string {
	return f.App + "." + strings.Join(f.Value, ".")
}

// OverlayFieldDelivered compares one declared value against what the live object
// carries. Pure.
//
// IT LIVES HERE BECAUSE TWO SIDES ASK IT AND THEY MUST NOT DISAGREE.
// `llz ci assert-overlay-applied` asks to decide whether a value landed; a
// brownfield migration asks the same question as its PRECONDITION — "is this
// field still undelivered, so is there anything to migrate?". One reading the
// object as converged while the other recreates a StatefulSet over it is the
// disagreement a second copy of this walk would eventually produce.
//
// It returns the delivered rendering as well as the verdict, because a report
// line that says only "does not match" sends the reader back to kubectl. What was
// declared and what is actually there is the whole finding.
func OverlayFieldDelivered(f OverlayField, declared any, live map[string]any) (match bool, delivered string, readable bool) {
	v, found, missedSelector := LiveValue(live, f.Live)
	// A PROBE AIMED AT NOTHING IS NOT A FINDING ABOUT THE CLUSTER. The selector
	// missing means the chart renamed what this row points at, so the row now covers
	// nothing — that has to read as a table to fix, never as a value to go and
	// deliver.
	if missedSelector {
		return false, "", false
	}
	switch f.Match {
	case MatchNonEmptyList:
		// AN ABSENT LIST IS THE ANSWER, NOT A FAILURE TO ASK. `spec.volumeClaimTemplates`
		// missing entirely means zero claim templates, which is precisely the state a
		// declared `persistence.enabled: true` contradicts — the brownfield StatefulSet
		// this gate was written for has no such key at all.
		list, isList := v.([]any)
		if found && v != nil && !isList {
			// The path resolved to something that is not a list at all. That is a row
			// aimed at the wrong field, not a value — say so rather than counting it.
			return false, fmt.Sprintf("%v", v), false
		}
		// A PRESENCE ROW READS A BOOL, AND A SWALLOWED ASSERTION INVERTS IT. `want, _
		// := declared.(bool)` on a non-bool yields false, which silently turns the
		// question into "this list must be EMPTY" — so an object with no
		// volumeClaimTemplates would report DELIVERED and the migration that fixes it
		// would report DONE. The overlay is YAML and a value can arrive as a string
		// (`enabled: "true"`) from an edit nobody reviewed as type-significant, so
		// this is a live shape, not a hypothetical. Unreadable, like every other
		// condition this function cannot answer.
		want, isBool := declared.(bool)
		if !isBool {
			return false, fmt.Sprintf("declared %v (%T, not a bool)", declared, declared), false
		}
		return (len(list) > 0) == want, fmt.Sprintf("%d entries", len(list)), true
	default:
		// AN ABSENT KEY IS ALSO AN ANSWER: the object does not carry the value, which
		// is exactly what "undelivered" looks like when the chart writes nothing at
		// all rather than writing a default. It goes on to the appliability probe like
		// any other mismatch.
		if !found {
			return false, "(absent)", true
		}
		got := fmt.Sprintf("%v", v)
		return got == fmt.Sprintf("%v", declared), got, true
	}
}

// ── the paths deliberately left unmapped ─────────────────────────────────────

// OverlayUnmapped names every declared overlay path that has NO row in
// OverlayFields, with the reason someone decided it needs none.
//
// A BOOL WOULD NOT HAVE BEEN ENOUGH, for the reason commands_census_test.go's
// exemption table gives: an exemption without an argument is indistinguishable
// from an oversight, and this table exists precisely to make the difference
// visible. TestEveryDeclaredOverlayPathIsMappedOrExempt refuses a path that is in
// neither table, so adding an overlay entry is a decision someone has to write
// down rather than a value that quietly reaches no gate.
//
// The reasons are not interchangeable. Two of these say "its consumer screams",
// which is a real gate; one says "checked as part of another row"; and the harbor
// three say the honest thing appvalues.yaml already says about them — that they
// are REPORTED on, not gated on, and why.
func OverlayUnmapped() map[string]string {
	const argoHealthOverride = "renders into the argocd-cm ConfigMap, whose failure mode is loud and immediate " +
		"rather than silent: without these overrides platform-bootstrap wedges at a negative sync wave and the " +
		"whole cluster bootstrap stops. wave-health-guard holds the PR-time half (AllowedKinds.overrideKey is " +
		"coupled to the rendered overlay by TestEveryOverrideBackedKindIsBackedByTheRenderedOverlay), and the " +
		"bootstrap itself is the runtime half. A live field check here would add a third reading of a value that " +
		"cannot go quietly missing"
	const harborMetrics = "REPORTED, NOT GATED, and appvalues.yaml says so in its own header: Harbor is " +
		"ManagedConditionalOn, so a gating row would turn permanently red on every instance that does not run " +
		"it — and a gate nobody can turn green gets switched off. alert-eval's weekly run is what says whether " +
		"the metrics arrived. Mapping this properly needs the scrape set to become component-aware first"

	return map[string]string{
		"argocd.configs.cm.resource.customizations.health.cert-manager.io_ClusterIssuer":          argoHealthOverride,
		"argocd.configs.cm.resource.customizations.health.external-secrets.io_ClusterSecretStore": argoHealthOverride,
		"argocd.configs.cm.resource.customizations.health.external-secrets.io_ExternalSecret":     argoHealthOverride,
		"argocd.configs.cm.resource.customizations.health.external-secrets.io_PushSecret":         argoHealthOverride,
		"argocd.configs.cm.resource.customizations.health.networking.k8s.io_NetworkPolicy":        argoHealthOverride,

		"harbor.metrics.enabled":                                    harborMetrics,
		"harbor.metrics.serviceMonitor.enabled":                     harborMetrics,
		"harbor.metrics.serviceMonitor.additionalLabels.prometheus": harborMetrics,

		"loki.ingester.persistence.claims": "checked as part of loki.ingester.persistence.enabled, not beside it: " +
			"the claim entries ARE the payload of that row's appliability probe (claimTemplate turns each into the " +
			"PersistentVolumeClaim the StatefulSet would carry), so a row of its own would send the same patch twice " +
			"and report one finding as two",
	}
}

// UnmappedOverlayPaths returns every declared overlay path that has neither a row
// in OverlayFields nor a reason in OverlayUnmapped — a value asserted onto a real
// channel that nothing reads back and nobody decided about.
//
// TWO CALLERS, ONE ANSWER. TestEveryDeclaredOverlayPathIsMappedOrExempt requires
// it to be empty at PR time, and `llz ci assert-overlay-applied` fails on it at
// runtime. The runtime half is not redundant: an instance runs the gate from a
// released binary whose table is whatever shipped, and "the guard would have
// caught it" is not a thing a cluster can check.
func UnmappedOverlayPaths() []string {
	mapped := map[string]bool{}
	for _, f := range OverlayFields() {
		mapped[OverlayFieldPath(f)] = true
	}
	exempt := OverlayUnmapped()
	var out []string
	for _, p := range DeclaredOverlayPaths() {
		if !mapped[p] && exempt[p] == "" {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}
