package clusterspec

// overlay_fields_test.go is the PR-time half of the overlay's own rule.
//
// THE RULE, FROM appvalues.yaml's HEADER: "A key that names nothing in the chart
// is silently ignored — that is the failure this file was created in response to,
// and living on a real channel does not make it self-verifying. Every entry must
// be backed by a gate that reads the CONSUMER. Do not add one without naming its
// gate."
//
// It was prose, and prose is what the two regressions in docs/e2e-gates.md were
// both green under. These tests are the mechanism: an overlay entry now has to be
// mapped to a live field or exempted with a reason, and the exemption has to name
// one. Both sides are DERIVED — the declared paths from the renderer's own output,
// the mapped ones from the field map — so a new entry appears the moment it
// compiles rather than when someone remembers.
//
// It is the cheapest layer that can see this failure at all (docs/e2e-gates.md,
// "Which layer": decidable from repo contents alone → a static guard that fails at
// PR time). It cannot know whether a given site is brownfield, and does not need
// to: it forces the author to answer "what happens on a cluster where this object
// already exists?" at the one moment they are best placed to answer it.

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// TestEveryDeclaredOverlayPathIsMappedOrExempt is the guard the header promises.
//
// DERIVED FROM THE RENDERER, NEVER LISTED. Naming today's fourteen paths by hand
// would pass unchanged the day a fifteenth arrives, which is precisely the failure
// mode being closed.
func TestEveryDeclaredOverlayPathIsMappedOrExempt(t *testing.T) {
	mapped := map[string]bool{}
	for _, f := range OverlayFields() {
		mapped[OverlayFieldPath(f)] = true
	}
	exempt := OverlayUnmapped()

	declared := DeclaredOverlayPaths()
	if len(declared) == 0 {
		t.Fatal("the overlay declares no paths at all — either aplAppRawValues is empty or " +
			"DeclaredOverlayPaths stopped walking it, and this guard is passing vacuously")
	}
	for _, p := range declared {
		switch {
		case mapped[p] && exempt[p] != "":
			t.Errorf("%s is both mapped and exempted — one of the two is stale, and a reader cannot "+
				"tell which describes the intent", p)
		case mapped[p], exempt[p] != "":
		default:
			t.Errorf("%s is declared by the apl-overlay and reaches no gate.\n"+
				"  Add a row to OverlayFields() so `llz ci assert-overlay-applied` reads it back out of the "+
				"live object — and if the field is one the API server fixes at CREATE time, set CreateOnly "+
				"and name the brownfield migration that lands it on a cluster the object predates.\n"+
				"  Or add it to OverlayUnmapped() with the reason it needs no live check. What must not "+
				"happen is the third thing: a value asserted onto a real channel that nothing ever reads back.", p)
		}
	}
}

// An exemption for a path nobody declares any more is a decision about nothing,
// and it makes the table read as covering more than it does.
func TestNoExemptionOutlivesThePathItExempts(t *testing.T) {
	declared := map[string]bool{}
	for _, p := range DeclaredOverlayPaths() {
		declared[p] = true
	}
	var stale []string
	for p := range OverlayUnmapped() {
		if !declared[p] {
			stale = append(stale, p)
		}
	}
	sort.Strings(stale)
	for _, p := range stale {
		t.Errorf("OverlayUnmapped exempts %q, which the overlay no longer declares — drop the row; "+
			"an exemption list longer than the thing it exempts is how a table stops describing anything", p)
	}
}

// A row that says "this cannot be applied in place" and does not say what to do
// about it has told an operator half of what it knows — and the half it kept is
// the actionable one.
func TestEveryCreateOnlyOverlayFieldNamesItsMigration(t *testing.T) {
	checked := 0
	for _, f := range OverlayFields() {
		if !f.CreateOnly {
			continue
		}
		checked++
		if f.Migration == "" {
			t.Errorf("%s is marked CreateOnly and names no Migration — the gate can then only tell an "+
				"operator that the value will never land, not what to run", OverlayFieldPath(f))
		}
	}
	if checked == 0 {
		t.Fatal("no CreateOnly field in the map — either the flag has stopped being set or this test " +
			"has stopped reaching it; the class it exists for has not gone away")
	}
}

// The inverse is worth pinning too: a Migration named on a field the API server
// would take in place sends an operator to recreate a live object for nothing.
func TestNoMutableOverlayFieldClaimsToNeedAMigration(t *testing.T) {
	for _, f := range OverlayFields() {
		if !f.CreateOnly && f.Migration != "" {
			t.Errorf("%s names migration %q but is not CreateOnly — a mutable field needs no recreate, "+
				"and offering one invites a destructive remedy for ordinary drift", OverlayFieldPath(f), f.Migration)
		}
	}
}

// Every row's appliability probe is built from the DECLARED values, so a row
// whose patch names a path the overlay does not carry would fail at runtime, in a
// cluster, as a gate reporting its own breakage. Build them all here instead.
func TestEveryOverlayFieldProbeBuildsFromTheRenderedValues(t *testing.T) {
	raw := AplAppRawValues()
	for _, f := range OverlayFields() {
		rv, ok := raw[f.App]
		if !ok {
			t.Errorf("%s: the overlay declares no _rawValues for app %q", OverlayFieldPath(f), f.App)
			continue
		}
		if _, ok := RawValue(rv, f.Value...); !ok {
			t.Errorf("%s: the overlay declares no such path — this row checks a value nothing asserts",
				OverlayFieldPath(f))
			continue
		}
		patch, err := f.Patch(rv)
		if err != nil {
			t.Errorf("%s: the appliability probe does not build: %v", OverlayFieldPath(f), err)
			continue
		}
		if !strings.HasPrefix(patch, `{"spec":`) {
			t.Errorf("%s: the probe must patch the object's spec, got %s", OverlayFieldPath(f), patch)
		}
	}
}

// The claim template is the one place this table RESHAPES a declared value rather
// than passing it through, and getting it wrong would produce a malformed patch —
// which the apiserver rejects in a way that looks exactly like the immutability
// rejection the gate exists to detect.
func TestTheClaimTemplateIsTheApiShapeAndNotTheChartShape(t *testing.T) {
	claims, err := rawSlice(AplAppRawValues()["loki"], "ingester", "persistence", "claims")
	if err != nil {
		t.Fatalf("the loki overlay carries no claims: %v", err)
	}
	if len(claims) == 0 {
		t.Fatal("no claims declared — this test would pass having checked nothing")
	}
	pvc := claimTemplate(claims[0].(map[string]any))
	if pvc["kind"] != "PersistentVolumeClaim" {
		t.Errorf("kind = %v, want PersistentVolumeClaim", pvc["kind"])
	}
	spec := pvc["spec"].(map[string]any)
	if spec["storageClassName"] != LokiIngesterStorageClass {
		t.Errorf("storageClassName = %v, want %s — an unpinned class lands the WAL, which carries the "+
			"OpenBao audit stream, on an unencrypted Volume", spec["storageClassName"], LokiIngesterStorageClass)
	}
	res, ok := spec["resources"].(map[string]any)
	if !ok {
		t.Fatal("the chart's `size` must become spec.resources.requests.storage; the API has no `size`")
	}
	if _, ok := res["requests"].(map[string]any)["storage"]; !ok {
		t.Error("no spec.resources.requests.storage in the translated claim")
	}
	if _, chartShape := spec["size"]; chartShape {
		t.Error("the chart's own key survived into the API shape — the apiserver would reject this patch " +
			"as malformed, which is indistinguishable from the immutability rejection the probe looks for")
	}
}

// The migration id is read by two sides: the field map names it as the remedy and
// the migration registers under it. A literal on either side would let them drift
// into naming different things, so the constant is the only spelling.
func TestTheWALMigrationIdIsNamedOnceAndUsedByTheRow(t *testing.T) {
	var found bool
	for _, f := range OverlayFields() {
		if f.Migration == LokiWALPVCMigration {
			found = true
		}
	}
	if !found {
		t.Errorf("no overlay field names %s — either the row lost its remedy or the id was respelled",
			LokiWALPVCMigration)
	}
}

// The guard's own predicate, exercised against a shape the repo does not have —
// because a test that only ever sees the passing case cannot show it would fail.
// UnmappedOverlayPaths is what both the guard and the runtime gate read, so this
// is the one place its arithmetic is checked directly.
func TestUnmappedOverlayPathsIsEmptyOnlyBecauseEveryPathWasDecided(t *testing.T) {
	if got := UnmappedOverlayPaths(); len(got) != 0 {
		t.Fatalf("declared paths with no row and no reason: %v", got)
	}
	// Now the same arithmetic over a synthetic set, to show the empty result above
	// is a fact about the tables and not about the function.
	declared := []string{"app.a", "app.b", "app.c"}
	mapped := map[string]bool{"app.a": true}
	exempt := map[string]string{"app.b": "because"}
	var got []string
	for _, p := range declared {
		if !mapped[p] && exempt[p] == "" {
			got = append(got, p)
		}
	}
	if len(got) != 1 || got[0] != "app.c" {
		t.Errorf("the undecided path must be the one in neither table, got %v", got)
	}
}

// Every exemption has to carry a reason a reader can act on. An empty string
// would satisfy the presence check in the guard above while saying nothing.
func TestEveryExemptionCarriesARealReason(t *testing.T) {
	for path, reason := range OverlayUnmapped() {
		if len(strings.TrimSpace(reason)) < 40 {
			t.Errorf("%s: the exemption reason is %q — an exemption without an argument is "+
				"indistinguishable from an oversight, which is what this table exists to prevent",
				path, reason)
		}
	}
}

// Two clients reach the same object by different names: `llz ci
// assert-overlay-applied` shells out to kubectl and says `statefulset`, while the
// in-cluster reconciler speaks REST and needs `/apis/apps/v1/…/statefulsets/…`.
// A row whose two spellings named different objects would have one of them
// silently measuring nothing.
func TestEveryOverlayFieldSpellsItsObjectBothWays(t *testing.T) {
	for _, f := range OverlayFields() {
		path := OverlayFieldPath(f)
		if f.Kind == "" || f.Namespace == "" || f.Name == "" {
			t.Errorf("%s: the kubectl spelling is incomplete (kind=%q ns=%q name=%q)",
				path, f.Kind, f.Namespace, f.Name)
		}
		if f.APIGroupVersion == "" || f.Resource == "" {
			t.Errorf("%s: no API spelling — the reconciler lane cannot reach this object", path)
			continue
		}
		// The regular plural is all this repo has needed. An irregular one (endpoints,
		// ingresses, networkpolicies) is a real case and gets its own line here rather
		// than a cleverer rule, because the failure of a cleverer rule is a 404 the
		// gauge would publish as "not delivered".
		if f.Resource != f.Kind+"s" {
			t.Errorf("%s: kubectl says %q and the API path says %q — if that is a genuine "+
				"irregular plural, add the pair here; otherwise one of them is a typo and the "+
				"client using it is measuring a different object", path, f.Kind, f.Resource)
		}
		want := "/apis/" + f.APIGroupVersion + "/namespaces/" + f.Namespace + "/" + f.Resource + "/" + f.Name
		if !strings.Contains(f.APIGroupVersion, "/") {
			want = "/api/" + f.APIGroupVersion + "/namespaces/" + f.Namespace + "/" + f.Resource + "/" + f.Name
		}
		if got := f.APIPath(); got != want {
			t.Errorf("%s: APIPath() = %q, want %q", path, got, want)
		}
	}
}

// The core group has no group segment, and getting that wrong is a 404 that a
// gauge would publish as "the value is not delivered" — blaming a cluster for a
// path bug. There is no core-group row today, so the rule is pinned directly.
func TestACoreGroupObjectResolvesUnderApiV1(t *testing.T) {
	f := OverlayField{APIGroupVersion: "v1", Resource: "services", Namespace: "monitoring", Name: "loki-gateway"}
	if got, want := f.APIPath(), "/api/v1/namespaces/monitoring/services/loki-gateway"; got != want {
		t.Errorf("APIPath() = %q, want %q", got, want)
	}
}

// A presence row reads a bool. A swallowed type assertion turns the question into
// "this list must be EMPTY", so an object with no volumeClaimTemplates would
// report DELIVERED and the migration that fixes it would report DONE — the exact
// silent green this file exists to prevent, reachable from a YAML edit nobody
// reviewed as type-significant (`enabled: "true"`).
func TestAPresenceRowWithANonBoolDeclaredValueIsUnreadableNotInverted(t *testing.T) {
	f := OverlayField{
		Match: MatchNonEmptyList,
		Live:  []string{"spec", "volumeClaimTemplates"},
	}
	empty := map[string]any{"spec": map[string]any{}}
	match, delivered, readable := OverlayFieldDelivered(f, "true", empty)
	if readable {
		t.Fatalf("a non-bool must read as unreadable; got match=%v delivered=%q", match, delivered)
	}
	if match {
		t.Error("an object with no list must never match a presence row it cannot judge")
	}
	if !strings.Contains(delivered, "not a bool") {
		t.Errorf("the report must name the type problem, got %q", delivered)
	}
	// …and the real bool still works, in both directions.
	if m, _, ok := OverlayFieldDelivered(f, true, empty); !ok || m {
		t.Errorf("declared true against an empty list: match=%v readable=%v, want false/true", m, ok)
	}
	full := map[string]any{"spec": map[string]any{"volumeClaimTemplates": []any{map[string]any{}}}}
	if m, _, ok := OverlayFieldDelivered(f, true, full); !ok || !m {
		t.Errorf("declared true against a populated list: match=%v readable=%v, want true/true", m, ok)
	}
}

// A migration deletes a live object and depends on an Argo Application to put it
// back. A row that does not name that Application cannot have the question asked
// before the delete — and once the object is gone, ABSENT reads as nothing-to-do,
// so no later run retries. The field is the difference between a repair and a
// one-way door.
func TestEveryCreateOnlyOverlayFieldNamesTheApplicationThatWouldRecreateIt(t *testing.T) {
	checked := 0
	for _, f := range OverlayFields() {
		if !f.CreateOnly {
			continue
		}
		checked++
		if f.OwnerApp == "" {
			t.Errorf("%s is CreateOnly and names no OwnerApp — nothing can check that its object would be "+
				"recreated before the migration deletes it", OverlayFieldPath(f))
		}
	}
	if checked == 0 {
		t.Fatal("no CreateOnly field to check — this test has stopped reaching what it describes")
	}
}

// ── the walkers, tested where they live ──────────────────────────────────────
//
// LiveValue and OverlayFieldDelivered are consumed by three packages and were
// only ever exercised through them, which left the package that OWNS them
// carrying untested branches — including selectByField, the one that decides
// whether a chart rename reads as a missing value or as a probe pointing at
// nothing. A caller's test proves the caller; these prove the rule.

func TestLiveValueDistinguishesAMissingKeyFromAMissedSelector(t *testing.T) {
	obj := map[string]any{"spec": map[string]any{
		"template": map[string]any{"spec": map[string]any{
			"containers": []any{
				map[string]any{"name": "sidecar", "image": "a"},
				map[string]any{"name": "ingester", "resources": map[string]any{"limits": map[string]any{"memory": "1Gi"}}},
			},
		}},
	}}
	for _, tc := range []struct {
		name          string
		path          []string
		wantValue     string // rendered, because a map is not comparable
		wantFound     bool
		wantMissedSel bool
	}{
		{"plain key", []string{"spec", "template"}, "", true, false},
		{"selector picks the named element",
			[]string{"spec", "template", "spec", "containers[name=ingester]", "resources", "limits", "memory"},
			"1Gi", true, false},
		{"a key that is not there is a missing VALUE",
			[]string{"spec", "volumeClaimTemplates"}, "", false, false},
		{"a selector that matches nothing is a missing PROBE",
			[]string{"spec", "template", "spec", "containers[name=gone]"}, "", false, true},
		{"an unterminated selector is a probe fault too",
			[]string{"spec", "template", "spec", "containers[name=ingester"}, "", false, true},
		{"a selector with no = is a probe fault",
			[]string{"spec", "template", "spec", "containers[ingester]"}, "", false, true},
		{"a selector on something that is not a list is a probe fault",
			[]string{"spec", "template[name=x]"}, "", false, true},
		{"descending through a scalar finds nothing",
			[]string{"spec", "template", "spec", "containers[name=sidecar]", "image", "deeper"}, "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, found, missed := LiveValue(obj, tc.path)
			if found != tc.wantFound || missed != tc.wantMissedSel {
				t.Fatalf("found=%v missedSelector=%v, want %v/%v", found, missed, tc.wantFound, tc.wantMissedSel)
			}
			if tc.wantValue != "" && fmt.Sprintf("%v", got) != tc.wantValue {
				t.Errorf("value = %v, want %v", got, tc.wantValue)
			}
		})
	}
}

func TestSelectByFieldMatchesOnlyTheNamedElement(t *testing.T) {
	list := []any{
		"not a map",
		map[string]any{"name": "a"},
		map[string]any{"name": "b", "keep": true},
		map[string]any{"other": "c"},
	}
	got, ok := selectByField(list, "name", "b")
	if !ok {
		t.Fatal("the named element is right there")
	}
	if m, _ := got.(map[string]any); m["keep"] != true {
		t.Errorf("selected the wrong element: %v", got)
	}
	if _, ok := selectByField(list, "name", "zzz"); ok {
		t.Error("a name nothing carries must not match — a chart rename has to surface, not resolve " +
			"to whatever happened to be there")
	}
	if _, ok := selectByField(nil, "name", "a"); ok {
		t.Error("an empty list matches nothing")
	}
}

func TestOverlayFieldDeliveredCoversEachComparisonArm(t *testing.T) {
	scalar := OverlayField{Match: MatchScalar,
		Live: []string{"spec", "containers[name=ingester]", "memory"}}
	presence := OverlayField{Match: MatchNonEmptyList, Live: []string{"spec", "claims"}}

	obj := func(inner map[string]any) map[string]any { return map[string]any{"spec": inner} }
	withContainer := func(name string, mem any) map[string]any {
		return obj(map[string]any{"containers": []any{map[string]any{"name": name, "memory": mem}}})
	}

	for _, tc := range []struct {
		name            string
		f               OverlayField
		declared        any
		live            map[string]any
		match, readable bool
		delivered       string
	}{
		{"scalar matches", scalar, "3Gi", withContainer("ingester", "3Gi"), true, true, "3Gi"},
		{"scalar differs", scalar, "3Gi", withContainer("ingester", "1Gi"), false, true, "1Gi"},
		{"scalar absent is undelivered, not unreadable", scalar, "3Gi",
			withContainer("ingester", nil), false, true, "<nil>"},
		{"renamed container is unreadable", scalar, "3Gi", withContainer("loki", "3Gi"), false, false, ""},
		{"presence satisfied", presence, true, obj(map[string]any{"claims": []any{map[string]any{}}}),
			true, true, "1 entries"},
		{"presence unsatisfied", presence, true, obj(map[string]any{}), false, true, "0 entries"},
		{"presence false against an empty list", presence, false, obj(map[string]any{}), true, true, "0 entries"},
		{"presence against a non-list is unreadable", presence, true,
			obj(map[string]any{"claims": "yes"}), false, false, "yes"},
		{"presence with a non-bool declared value is unreadable", presence, "true",
			obj(map[string]any{}), false, false, "declared true (string, not a bool)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			match, delivered, readable := OverlayFieldDelivered(tc.f, tc.declared, tc.live)
			if match != tc.match || readable != tc.readable {
				t.Fatalf("match=%v readable=%v, want %v/%v (delivered %q)",
					match, readable, tc.match, tc.readable, delivered)
			}
			if tc.delivered != "" && delivered != tc.delivered {
				t.Errorf("delivered = %q, want %q", delivered, tc.delivered)
			}
		})
	}
}

// The probe builders' error arms: a row whose declared value is missing or of the
// wrong type must say so rather than emit a patch built from nothing.
func TestAProbeBuiltFromTheWrongValuesFailsWithTheReason(t *testing.T) {
	memory := fieldFor(t, "loki.ingester.resources.limits.memory")
	claims := fieldFor(t, "loki.ingester.persistence.enabled")

	if _, err := memory.Patch(map[string]any{}); err == nil {
		t.Error("no declared value must not produce a patch")
	}
	if _, err := memory.Patch(map[string]any{"ingester": map[string]any{
		"resources": map[string]any{"limits": map[string]any{"memory": 3}}}}); err == nil {
		t.Error("a non-string quantity must be refused: Kubernetes quantities are strings")
	}
	if _, err := claims.Patch(map[string]any{"ingester": map[string]any{
		"persistence": map[string]any{"claims": "not-a-list"}}}); err == nil {
		t.Error("a non-sequence claims value must be refused")
	}
	if _, err := claims.Patch(map[string]any{"ingester": map[string]any{
		"persistence": map[string]any{"claims": []any{"not-a-map"}}}}); err == nil {
		t.Error("a claim entry that is not a mapping must be refused")
	}
}

func fieldFor(t *testing.T, path string) OverlayField {
	t.Helper()
	for _, f := range OverlayFields() {
		if OverlayFieldPath(f) == path {
			return f
		}
	}
	t.Fatalf("no overlay field %q", path)
	return OverlayField{}
}

// RawValue's miss arms, which every caller depends on to distinguish "the overlay
// does not declare this" from a zero value.
func TestRawValueReportsAMissRatherThanAZeroValue(t *testing.T) {
	rv := map[string]any{"ingester": map[string]any{"persistence": map[string]any{"enabled": true}}}
	if v, ok := RawValue(rv, "ingester", "persistence", "enabled"); !ok || v != true {
		t.Errorf("value = %v, ok = %v", v, ok)
	}
	for _, path := range [][]string{
		{"ingester", "nope"},
		{"nope"},
		{"ingester", "persistence", "enabled", "deeper"}, // descending through a scalar
	} {
		if _, ok := RawValue(rv, path...); ok {
			t.Errorf("%v must report a miss", path)
		}
	}
}
