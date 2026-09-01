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
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// OverlayMatch is how a declared value is compared against what the live object
// carries.
type OverlayMatch int

const (
	// MatchScalar compares the declared leaf to the delivered leaf through
	// OverlayScalarEqual — by VALUE for a quantity, as text for anything else.
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
	// Prior is what a PRE-OVERLAY object carries at Live — the chart's own default,
	// as observed on a cluster that predates this value.
	//
	// IT EXISTS BECAUSE "ABSENT" IS NOT THE TRANSITION A BROWNFIELD CLUSTER MAKES.
	// The appliability gate builds a fixture and asks the apiserver whether the
	// declared change can be applied to it. If the fixture simply OMITS the field,
	// the probe tests absent→set; a real cluster performs default→declared. Those
	// are the same question only for a field whose immutability is unconditional.
	// They differ for anything gated on a transition — a CRD schema's
	// `self == oldSelf` rule, a quantity that may grow but not shrink, a field
	// settable once and then fixed — where absent→set is ACCEPTED and
	// default→declared is refused. A row like that would be graded APPLIABLE and
	// ship, which is the false green this whole gate exists to prevent.
	//
	// REQUIRED FOR MatchScalar, and the emitter refuses a row without it rather
	// than quietly falling back to omission. MatchNonEmptyList needs none: a
	// presence toggle's pre-overlay state genuinely IS the absent list (the
	// brownfield loki-ingester has no volumeClaimTemplates key at all), so there
	// nothing is the honest fixture rather than a gap in one.
	Prior any
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
		// THE PRIOR VALUES ARE OBSERVED, NOT GUESSED. They are what
		// testdata/live/loki-ingester.brownfield.json recorded off the cluster this
		// gate was written for — apl-core's own chart defaults, the shape every
		// instance that predates the overlay is actually running.
		lokiIngesterResource("limits", "memory", "1Gi",
			"the ingester OOMs replaying its WAL at the chart default; this limit bounds the replay spike"),
		lokiIngesterResource("limits", "cpu", "500m",
			"500m made replay needlessly slow; 1 CPU roughly halves the not-ready window"),
		lokiIngesterResource("requests", "memory", "512Mi",
			"the requests stay modest (burstable) so the limit is what bounds the spike, not the schedule"),
		lokiIngesterResource("requests", "cpu", "250m",
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
func lokiIngesterResource(section, resource, prior, why string) OverlayField {
	return OverlayField{
		App:             "loki",
		Value:           []string{"ingester", "resources", section, resource},
		Prior:           prior,
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
		return OverlayScalarEqual(v, declared), got, true
	}
}

// ── comparing two scalars the way the apiserver would ────────────────────────

// OverlayScalarEqual reports whether a delivered scalar is the declared one.
//
// IT IS NOT A STRING COMPARE, AND THAT IS THE WHOLE POINT. The apiserver
// CANONICALISES quantities on the way in: patch `3072Mi` and read back `3Gi`,
// patch `1000m` and read back `1`. Measured against the same v1.34.8 apiserver
// the appliability lane runs on. A `%v` compare therefore reports a correctly
// delivered value as undelivered the moment anyone writes a non-canonical
// spelling in appvalues.yaml — and this function has three readers who each turn
// that into a different kind of damage:
//
//	assert-overlay-applied        a permanently red lane on a converged cluster
//	the appliability probe        "ACCEPTED the probe and the value did NOT land",
//	                              which blames the row's Patch for the apiserver's
//	                              own normalisation
//	the migration precondition    "is this field still undelivered" answers YES
//	                              forever, so `llz ci converge` recreates a live
//	                              StatefulSet on every platform-scope run with no
//	                              terminating condition
//
// The third is why this is not cosmetic. Quantities compare by VALUE; everything
// else still compares as text, because for a non-quantity the exact spelling is
// what the object carries and a looser comparison would start hiding real drift.
//
// A SUFFIX IS WHAT MAKES IT A QUANTITY, and without that rule the promise above is
// not kept. An earlier version took the quantity path for anything that parsed as
// a number, so "2.10" == "2.1", "1.0" == "1" and "0755" == "755" — a chart
// version, an image tag, a file mode or a label value compared NUMERICALLY and
// reported as delivered against a different live string. No declared value today
// is shaped that way, so it was a latent false green rather than a live one.
// Requiring at least one side to carry a UNIT OR AN EXPONENT keeps every case this
// exists for (3072Mi vs 3Gi, 1000m vs 1, 1Gi vs 1073741824, 3e9 vs 3000000000) and
// returns everything else to the text compare the doc promises. Both markers are
// things no chart version, image tag or file mode carries; a bare decimal is not.
func OverlayScalarEqual(delivered, declared any) bool {
	ds, dq, dok := quantityRat(delivered)
	cs, cq, cok := quantityRat(declared)
	if dok && cok && (dq || cq) {
		return ds.Cmp(cs) == 0
	}
	return scalarText(delivered) == scalarText(declared)
}

// scalarText is how a scalar is spelled, for BOTH halves of OverlayScalarEqual.
//
// THE TWO HALVES DISAGREED ABOUT NUMBERS. quantityRat normalises a float64 with
// FormatFloat('f', -1, 64) while the text fallback used %v, which switches to
// exponent form around 1e7 — so a live JSON number of 3000000000 rendered
// "3e+09" against a declared "3000000000" and could never match. Both readings
// have to come from one place, for the same reason OverlayFieldDelivered does:
// this feeds assert-overlay-applied, the appliability probe, and the migration
// precondition, and the last of those answering "still undelivered" forever is a
// live StatefulSet recreated on every platform-scope run with no terminating
// condition.
func scalarText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// quantitySuffixes are Kubernetes' resource.Quantity multipliers, binary and
// decimal. Transcribed from the apiserver's own set rather than invented: a
// suffix this table does not know falls back to a text compare, which is the
// safe direction — it can report a false MISMATCH (a loud red someone fixes) but
// never a false match.
var quantitySuffixes = map[string][2]int64{
	// suffix: {base, exponent}
	"":   {1, 0},
	"n":  {1000, -3},
	"u":  {1000, -2},
	"m":  {1000, -1},
	"k":  {1000, 1},
	"M":  {1000, 2},
	"G":  {1000, 3},
	"T":  {1000, 4},
	"P":  {1000, 5},
	"E":  {1000, 6},
	"Ki": {1024, 1},
	"Mi": {1024, 2},
	"Gi": {1024, 3},
	"Ti": {1024, 4},
	"Pi": {1024, 5},
	"Ei": {1024, 6},
}

// quantityPartsRe splits a quantity into its number and its suffix. validate.go's
// quantityRe answers "is this shaped like a quantity" for the spec validator; this
// one has to CAPTURE the two halves, so it is a second expression over the same
// grammar rather than a second opinion about it.
//
// THE GRAMMAR IS THE APISERVER'S, NOT A TIDY SUBSET OF IT. resource.Quantity
// accepts a sign, and a decimal exponent, and PRESERVES the spelling it was given
// — so a live object can carry `3e9` where the overlay declares `3000000000` and
// mean the identical thing. An earlier version refused both forms, fell back to a
// text compare, and reported a correctly delivered value as UNDELIVERED, which in
// the migration precondition is a recreate with no terminating condition. It also
// omitted `n` and `u`, which are the apiserver's own canonical output for anything
// sub-milli (`1.5m` comes back as `1500u`).
//
// NO TrimSpace. resource.Quantity rejects surrounding whitespace outright, so
// accepting it here would vouch for a declared value the apiserver will refuse as
// malformed — a false MATCH, which the earlier comment claimed was impossible.
var quantityPartsRe = regexp.MustCompile(`^([+-]?[0-9]+(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?)([A-Za-z]*)$`)

// quantityRat parses a Kubernetes quantity into an exact rational, or reports
// that the value is not one.
//
// EXACT, NOT FLOAT. 1Gi is 1073741824 and 1000m is 1; comparing those through
// float64 would work today and stop working for a large enough Ti value, which is
// the kind of bug that surfaces once, in production, on somebody's storage row.
// The second return says the text carried a QUANTITY MARKER — a unit suffix or an
// exponent — which is what lets the caller keep a bare decimal on the text path.
func quantityRat(v any) (r *big.Rat, marked, ok bool) {
	var s string
	switch t := v.(type) {
	case string:
		s = t
	case int:
		s = strconv.Itoa(t)
	case int64:
		s = strconv.FormatInt(t, 10)
	case float64:
		s = strconv.FormatFloat(t, 'f', -1, 64)
	default:
		// bool, nil, a map, a slice — not a quantity, and never coerced into one.
		return nil, false, false
	}
	m := quantityPartsRe.FindStringSubmatch(s)
	if m == nil {
		return nil, false, false
	}
	mult, known := quantitySuffixes[m[2]]
	if !known {
		// A suffix this table does not recognise falls back to a text compare, which
		// can report a false MISMATCH (a loud red someone fixes) but never a false
		// match. Note `K` lands here deliberately: Kubernetes spells kilo `k`.
		return nil, false, false
	}
	// SetString handles the exponent form, so the two halves of the grammar stay in
	// one place rather than being re-implemented here.
	r, parsed := new(big.Rat).SetString(m[1])
	if !parsed {
		return nil, false, false
	}
	scale := new(big.Rat).SetInt64(1)
	base := new(big.Rat).SetInt64(mult[0])
	exp := mult[1]
	neg := exp < 0
	if neg {
		exp = -exp
	}
	for i := int64(0); i < exp; i++ {
		scale.Mul(scale, base)
	}
	marked = m[2] != "" || strings.ContainsAny(m[1], "eE")
	if neg {
		return r.Quo(r, scale), marked, true
	}
	return r.Mul(r, scale), marked, true
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

// ── coupling a row's probe to the row it claims to test ──────────────────────

// patchTargetsLive reports whether a row's Patch actually writes at the row's own
// Live path.
//
// IT CHECKS THREE THINGS, and each closes a measured false green: that the patch
// writes AT the row's path, that it writes the DECLARED VALUE there, and that it
// writes NOTHING ELSE.
//
// WITHOUT IT, Patch AND Live ARE INDEPENDENT AND A GREEN ROW PROVES NOTHING.
// StatefulSet's refusal is a WHOLE-SPEC message — "updates to statefulset spec
// for fields other than 'replicas', 'ordinals', 'template', … are forbidden" —
// emitted byte-identically for ANY non-whitelisted spec key. Measured: repointing
// the WAL-claim row's Patch at {"spec":{"serviceName":"…"}} still graded
// `loki.ingester.persistence.enabled is CREATE-ONLY, as declared` and exited 0.
// So a CREATE-ONLY verdict on its own establishes "the patch touched sts.spec
// outside the mutable whitelist", not "this field is create-only". The two are
// the same claim only when the patch is known to be about this field, and this is
// what knows it.
//
// STRUCTURAL, AND DELIBERATELY SO. It reads the patch the row builds rather than
// asking the cluster, because the refused path has no object to read back — the
// apiserver returns an error, not a result. So this is the only check available
// on the arm where the false green lives.
func PatchTargetsField(patch string, f OverlayField, declared any) error {
	var m map[string]any
	if err := json.Unmarshal([]byte(patch), &m); err != nil {
		return fmt.Errorf("the row's Patch is not valid JSON: %w", err)
	}
	if _, found, missed := LiveValue(m, f.Live); missed || !found {
		return fmt.Errorf("the row's Patch does not write anything at %s, the path this row claims to "+
			"test. Whatever the apiserver says about that patch is evidence about a DIFFERENT field — "+
			"and for a StatefulSet the immutability refusal is a whole-spec message, so an unrelated "+
			"spec key produces the identical text and would grade CREATE-ONLY", strings.Join(f.Live, "."))
	}
	// THE RIGHT PATH IS NOT YET THE RIGHT PROBE. A patch can write at the row's own
	// Live path and send something other than the declared value — and on the
	// REFUSED arm nothing downstream can notice, because a refusal returns an error
	// and not an object for probeLandedTheValue to read. So the value is checked
	// here, offline, where it is checkable on both arms.
	if match, sent, readable := OverlayFieldDelivered(f, declared, m); !readable {
		return fmt.Errorf("the row's Patch writes something at %s that this row cannot read back "+
			"(%s) — a probe whose own payload is unreadable is evidence about nothing",
			strings.Join(f.Live, "."), sent)
	} else if !match {
		return fmt.Errorf("the row's Patch writes %s at %s, not the declared %v. The apiserver's answer "+
			"would be about a value nobody declared, and on the refused arm there is no returned object "+
			"to notice it with", sent, strings.Join(f.Live, "."), declared)
	}
	// AND IT MUST WRITE NOTHING ELSE. Containment is not exclusivity: StatefulSet's
	// refusal is one whole-spec message emitted byte-identically for ANY
	// non-whitelisted spec key, so a patch carrying the row's own field PLUS one
	// unrelated key is refused for the unrelated key and grades "CREATE-ONLY, as
	// declared". Measured: adding `"podManagementPolicy": "Parallel"` beside the WAL
	// claim template produced exactly that, and exit 0. A selector key needed to
	// ADDRESS the row's field (a container's `name`) is not another write and is
	// excluded by extraPatchWrites itself.
	if extra := extraPatchWrites(m, 0, f.Live, "", "", false); len(extra) > 0 {
		sort.Strings(extra)
		return fmt.Errorf("the row's Patch also writes %s, which this row does not claim to test. A "+
			"StatefulSet's immutability refusal is a WHOLE-SPEC message, identical for any "+
			"non-whitelisted spec key — so an unrelated key alone produces a CREATE-ONLY verdict and "+
			"the row would vouch for it. Send only %s", strings.Join(extra, ", "), strings.Join(f.Live, "."))
	}
	return nil
}

// extraPatchWrites lists every leaf the patch writes that is NOT the row's own
// field. It walks the patch and the row's Live path together: `i` is how far into
// Live the walk has got, and `allow` is the selector key that addressed the
// current level, which is part of the address rather than a second write.
//
// Reaching the end of Live means this subtree IS the payload, and it is not
// descended into: for MatchNonEmptyList the payload is a whole list of claim
// templates, and every field inside one of those is the row's own business.
//
// WHAT COUNTS AS "ELSE" IS NARROWER THAN IT LOOKS, and getting that wrong is a
// false RED that blocks a legitimate row months from now. Once the walk has
// entered the list ELEMENT the row selects, other keys in that element are not a
// second write — they are how the element is addressed and how it is made valid:
//
//	the API's merge key    a Service port list merges on `port`, not on `name`, so
//	                       the only correct strategic-merge patch for a
//	                       `ports[name=http-metrics].targetPort` row carries `port`
//	                       too. Rejecting it would push the author to the patch
//	                       that mis-targets the list.
//	a co-required sibling  the apiserver refuses `requests > limits`, so a row
//	                       raising requests past the fixture's limit can only be
//	                       expressed as a patch carrying both. Rejecting that makes
//	                       the row unfixable: every patch is refused by one side or
//	                       the other.
//
// Neither can produce a spurious CREATE-ONLY verdict, which is the thing this
// check exists to prevent: they live inside a list the row already targets, and
// for a StatefulSet everything under spec.template is in the mutable whitelist.
// A key at a MAP level the walk passes through is a different matter — that is the
// measured false green (`spec.podManagementPolicy` beside
// `spec.volumeClaimTemplates`), and it stays refused.
func extraPatchWrites(node any, i int, live []string, cur, allow string, inElement bool) []string {
	if i == len(live) {
		return nil
	}
	m, ok := node.(map[string]any)
	if !ok {
		// The patch's shape diverges from Live before reaching it. LiveValue already
		// refused this patch above; returning the position keeps the message honest if
		// that check is ever reordered.
		return []string{cur}
	}
	key, sel, hasSel := strings.Cut(live[i], "[")
	var out []string
	for k, v := range m {
		at := strings.TrimPrefix(cur+"."+k, ".")
		if k != key {
			if allow != "" && k == allow {
				continue // the selector key that addresses this row's field
			}
			if inElement {
				// ONLY THE ELEMENT'S OWN TOP-LEVEL KEYS, not everything beneath them. The
				// exemption is for a merge key and a co-required sibling — both scalars sitting
				// directly on the element. Carrying it down to arbitrary depth switched the
				// check off for the whole subtree: measured, the real loki row's patch plus
				// `image: evil`, plus a nested sibling, plus a null-delete, plus
				// `"$patch":"replace"` were all accepted. That was safe only because everything
				// under a StatefulSet's spec.template really is mutable — a Kind-specific
				// argument generalised to every future row.
				if _, deeper := v.(map[string]any); !deeper {
					continue
				}
			}
			out = append(out, patchLeaves(v, at)...)
			continue
		}
		if !hasSel {
			out = append(out, extraPatchWrites(v, i+1, live, at, "", inElement)...)
			continue
		}
		field, wantVal, ok := strings.Cut(strings.TrimSuffix(sel, "]"), "=")
		list, isList := v.([]any)
		if !ok || !isList {
			out = append(out, patchLeaves(v, at)...)
			continue
		}
		// ONLY THE FIRST MATCH IS THE PAYLOAD, because LiveValue selects
		// the first match and stops. Treating every match as payload let a patch carry
		// the row's own field TWICE with two different values: the value check read the
		// first, and this saw nothing extra. Later matches are extra writes.
		matched := false
		for n, e := range list {
			el := fmt.Sprintf("%s[%d]", at, n)
			em, isMap := e.(map[string]any)
			if s, _ := em[field].(string); isMap && s == wantVal && !matched {
				matched = true
				out = append(out, extraPatchWrites(em, i+1, live, el, field, true)...)
				continue
			}
			// An element the row does not select — or a second element claiming to be it
			// — is a write to something other than this row's field.
			out = append(out, patchLeaves(e, el)...)
		}
	}
	return out
}

// patchLeaves names every leaf under a subtree the row does not claim.
func patchLeaves(node any, cur string) []string {
	switch t := node.(type) {
	case map[string]any:
		var out []string
		for k, v := range t {
			out = append(out, patchLeaves(v, strings.TrimPrefix(cur+"."+k, "."))...)
		}
		if len(out) == 0 {
			return []string{cur} // an empty object still writes at cur
		}
		return out
	default:
		return []string{cur}
	}
}

// PriorOnObject renders what a live object carries at a row's Live path, for the
// gate that checks a fixture is in its PRE-OVERLAY shape. "(absent)" when the path
// resolves to nothing — itself a shape a fixture can wrongly be in, and the one
// that reports as better coverage than the correct shape.
func PriorOnObject(f OverlayField, live map[string]any) any {
	v, found, missed := LiveValue(live, f.Live)
	if missed || !found {
		return "(absent)"
	}
	return v
}
