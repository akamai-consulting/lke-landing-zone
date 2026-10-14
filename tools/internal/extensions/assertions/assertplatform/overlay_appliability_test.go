package assertplatform

// overlay_appliability_test.go covers the one judgement this lane makes — does
// the apiserver agree with what clusterspec.OverlayFields() claims about a field
// — plus every fail-closed arm and the generator that feeds it.
//
// THE REFUSAL TEXT IS THE REAL ONE. statefulSetRefusal (overlay_applied_test.go)
// is what the apiserver actually returned on the cluster this whole class of
// failure was found on. A refusal invented to match the classifier would test the
// classifier against itself.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
)

func withAppliabilityObject(t *testing.T, raw string, absent, answered bool) {
	t.Helper()
	prev := appliabilityReadObject
	appliabilityReadObject = func(string, string, string) ([]byte, bool, bool, string) {
		return []byte(raw), absent, answered, "stubbed kubectl said nothing"
	}
	t.Cleanup(func() { appliabilityReadObject = prev })
}

// withAppliabilityDryRun answers every probe with a fixed verdict and records the
// patches sent, so a test can assert the lane probed at all.
func withAppliabilityDryRun(t *testing.T, out string, accepted bool) *[]string {
	t.Helper()
	var sent []string
	prev := appliabilityDryRun
	appliabilityDryRun = func(_, _, _, patch string) (string, bool) {
		sent = append(sent, patch)
		return out, accepted
	}
	// THE PAYLOAD VALIDATOR IS STUBBED HERE TOO, because a lane test that does not
	// install it shells out to a real kubectl and fails on whatever the machine's
	// kubeconfig happens to be. Accepting by default keeps these tests about the
	// verdict they are named for; the cases that care about a refused payload
	// install their own.
	prevCreate := appliabilityDryRunCreate
	appliabilityDryRunCreate = func(string, string) (string, bool) { return "{}", true }
	t.Cleanup(func() {
		appliabilityDryRun = prev
		appliabilityDryRunCreate = prevCreate
	})
	return &sent
}

// withGeneratedFixture serves the object EmitFixtures actually produces, which is
// what the kind lane probes. Using the recorded brownfield shape here instead
// would test against an object that already carries limits.cpu: "1" — a real
// property of that recording, and one that makes it a PARTIALLY pre-overlay
// fixture rather than a clean one.
func withGeneratedFixture(t *testing.T) {
	t.Helper()
	out, err := EmitFixtures()
	if err != nil {
		t.Fatalf("EmitFixtures: %v", err)
	}
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatalf("the emitted fixtures are not valid JSON: %v", err)
	}
	byName := map[string][]byte{}
	for _, raw := range list.Items {
		var md struct {
			Metadata struct{ Name, Namespace string } `json:"metadata"`
		}
		if err := json.Unmarshal(raw, &md); err != nil {
			t.Fatalf("a fixture item does not decode: %v", err)
		}
		byName[md.Metadata.Namespace+"/"+md.Metadata.Name] = raw
	}
	prev := appliabilityReadObject
	appliabilityReadObject = func(_, ns, name string) ([]byte, bool, bool, string) {
		raw, ok := byName[ns+"/"+name]
		if !ok {
			return nil, true, true, "stubbed: no such fixture"
		}
		return raw, false, true, ""
	}
	// AND THE PAYLOAD VALIDATOR, which otherwise shells out to a real kubectl and
	// fails the run on whatever kubeconfig the machine happens to have. A test that
	// cares about a refused payload installs its own after this.
	prevCreate := appliabilityDryRunCreate
	appliabilityDryRunCreate = func(string, string) (string, bool) { return "{}", true }
	t.Cleanup(func() {
		appliabilityReadObject = prev
		appliabilityDryRunCreate = prevCreate
	})
}

// appliabilityRun is a run's error plus what it printed, because two of the
// fail-closed arms are only distinguishable by the REPORT: the returned error is
// the vacuity line in both cases.
type appliabilityRun struct {
	err    error
	report string
}

func captureAppliabilityReport(t *testing.T, run func() error) appliabilityRun {
	t.Helper()
	prev := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	// RESTORED ON A PANIC TOO. Without the cleanup a panic inside run() left the
	// process-global os.Stdout pointing at this pipe for the rest of the package.
	t.Cleanup(func() { os.Stdout = prev })
	runErr := run()
	w.Close()
	os.Stdout = prev
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	return appliabilityRun{err: runErr, report: buf.String()}
}

// fixturesByName decodes an emitted List into namespace/name -> object.
func fixturesByName(t *testing.T, out string) map[string]map[string]any {
	t.Helper()
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatalf("the emitted fixtures are not valid JSON: %v", err)
	}
	byName := map[string]map[string]any{}
	for _, it := range list.Items {
		md, _ := it["metadata"].(map[string]any)
		ns, _ := md["namespace"].(string)
		name, _ := md["name"].(string)
		byName[ns+"/"+name] = it
	}
	return byName
}

// ── what the apiserver said ──────────────────────────────────────────────────

func TestTheRealStatefulSetRefusalIsObservedAsCreateOnly(t *testing.T) {
	if got := observeAppliability(statefulSetRefusal, false); got != observedCreateOnly {
		t.Errorf("the apiserver's real immutability refusal was observed as %v, want CREATE-ONLY", got)
	}
}

func TestAnAcceptedProbeIsObservedAsAppliable(t *testing.T) {
	if got := observeAppliability("statefulset.apps/loki-ingester patched", true); got != observedAppliable {
		t.Errorf("an accepted dry run was observed as %v, want APPLIABLE", got)
	}
}

func TestAnUnclassifiedRefusalIsNotObservedAsCreateOnly(t *testing.T) {
	// A malformed patch, a webhook denial, an RBAC error — none of these mean the
	// field is fixed at create time, and reading them as such would invent a
	// migration requirement out of a broken probe.
	got := observeAppliability(`Error from server (BadRequest): json: cannot unmarshal string`, false)
	if got != observedRefusedOther {
		t.Errorf("an unclassified refusal was observed as %v, want REFUSED (unclassified)", got)
	}
}

// ── grading it against the declaration ───────────────────────────────────────

func TestAMutableDeclarationRefusedByTheApiserverFailsAndNamesTheRemedy(t *testing.T) {
	// THE CASE THE LANE EXISTS FOR: someone adds an overlay value, leaves
	// CreateOnly at its zero value, and the apiserver would have refused it on
	// every brownfield cluster.
	f := fieldNamed(t, "loki.ingester.resources.limits.memory")
	if f.CreateOnly {
		t.Fatal("this test needs a field the map declares MUTABLE")
	}
	v := gradeAppliability(f, observedCreateOnly, "")
	if v.OK {
		t.Fatal("a field declared mutable that the apiserver fixes at create time passed — this is the " +
			"16-day Loki outage shipping again through the gate built to stop it")
	}
	for _, want := range []string{"declared MUTABLE", "CreateOnly:true", "brownfield migration", "Synced"} {
		if !strings.Contains(v.Detail, want) {
			t.Errorf("the failure does not mention %q, so it does not tell the author what to do:\n%s",
				want, v.Detail)
		}
	}
}

func TestACreateOnlyDeclarationTheApiserverAcceptsFailsAsANeedlessRecreate(t *testing.T) {
	// THE OPPOSITE ERROR, AND IT IS NOT PEDANTRY. converge applies Auto migrations
	// unattended, so an over-declared CreateOnly is a StatefulSet recreate on a
	// schedule for a value a patch would have landed.
	f := fieldNamed(t, "loki.ingester.persistence.enabled")
	if !f.CreateOnly {
		t.Fatal("this test needs a field the map declares CreateOnly")
	}
	v := gradeAppliability(f, observedAppliable, "")
	if v.OK {
		t.Fatal("a CreateOnly declaration the apiserver would have accepted passed — a destructive " +
			"migration registered for a problem that does not exist")
	}
	if !strings.Contains(v.Detail, "ACCEPTED") || !strings.Contains(v.Detail, "unattended") {
		t.Errorf("the failure does not explain why an over-declaration is harmful:\n%s", v.Detail)
	}
}

func TestTheDeclaredLokiRowsMatchWhatTheApiserverActuallyDid(t *testing.T) {
	// Both agreeing arms, driven with the real refusal and a real acceptance.
	claim := fieldNamed(t, "loki.ingester.persistence.enabled")
	if v := gradeAppliability(claim, observedCreateOnly, ""); !v.OK {
		t.Errorf("the WAL claim row is CreateOnly and the apiserver refused it, which agree: %s", v.Detail)
	}
	mem := fieldNamed(t, "loki.ingester.resources.limits.memory")
	if v := gradeAppliability(mem, observedAppliable, ""); !v.OK {
		t.Errorf("the memory row is mutable and the apiserver accepted it, which agree: %s", v.Detail)
	}
}

func TestAnUnclassifiedRefusalFailsRatherThanBeingGradedEitherWay(t *testing.T) {
	f := fieldNamed(t, "loki.ingester.resources.limits.memory")
	const apiserverSaid = "Error from server (Timeout): the connection to the server was refused"
	v := gradeAppliability(f, observedRefusedOther, apiserverSaid)
	if v.OK {
		t.Fatal("an unclassified refusal passed — nothing was established about this field")
	}
	// THE APISERVER'S OWN WORDS HAVE TO BE IN THE REPORT. An earlier version said
	// "read it before concluding anything" and printed nothing to read, while
	// asserting the likely cause was a malformed Patch — which for StatefulSet is
	// close to unreachable, since a malformed spec key trips the whole-spec
	// immutability marker instead. A transport fault lands here too, and only the
	// text tells the two apart.
	if !strings.Contains(v.Detail, "connection to the server was refused") {
		t.Errorf("the failure discards what the apiserver actually said:\n%s", v.Detail)
	}
	if !strings.Contains(v.Detail, "transport or permission") {
		t.Errorf("the failure does not admit a transport fault lands in this arm:\n%s", v.Detail)
	}
}

// ── fail-closed arms ─────────────────────────────────────────────────────────

func TestAnAbsentFixtureFailsRatherThanPassingUnexamined(t *testing.T) {
	// The opposite of the runtime lane, deliberately: there an absent object means
	// the instance does not run the app; here it means the fixture step did not run,
	// and every probe would 404 into an unexamined pass.
	withAppliabilityObject(t, "", true, true)
	withAppliabilityDryRun(t, "", true)
	out := captureAppliabilityReport(t, func() error { return assertOverlayAppliability() })
	if out.err == nil {
		t.Fatal("an absent fixture passed — the lane probed nothing and said so was fine")
	}
	// THE VERDICT, NOT JUST THE VACUITY. Asserting only "examined nothing" passes
	// even with the absent-object arm deleted, because the vacuity check fires either
	// way — a test that cannot fail when the thing it names is removed is not testing
	// it. The report has to say the object was missing AND how to put it there.
	if !strings.Contains(out.report, "does not exist") ||
		!strings.Contains(out.report, "--emit-fixtures") {
		t.Errorf("the report does not identify an absent fixture or say how to create one:\n%s", out.report)
	}
	if !strings.Contains(out.err.Error(), "examined nothing") {
		t.Errorf("the absent-fixture run should also fail on vacuity, got: %v", out.err)
	}
}

func TestAFixtureThatAlreadyCarriesTheValueFailsInsteadOfInvertingTheLane(t *testing.T) {
	// A no-op patch is ACCEPTED by the apiserver, which reads as "mutable", which
	// would report every CreateOnly row as an over-declaration. One polluted
	// fixture inverts the whole lane, so the pollution has to be the failure.
	withAppliabilityObject(t, deliveredIngester, false, true)
	sent := withAppliabilityDryRun(t, "patched", true)
	out := captureAppliabilityReport(t, func() error { return assertOverlayAppliability() })
	if out.err == nil {
		t.Fatal("a fixture already carrying the declared values passed — every probe was a no-op patch")
	}
	// Named, not merely counted — see the absent-fixture test above for why the
	// vacuity assertion alone would survive deleting the arm it describes.
	if !strings.Contains(out.report, "ALREADY carries") {
		t.Errorf("the report does not name the polluted fixture as the cause:\n%s", out.report)
	}
	// AND NOTHING WAS SENT. The whole hazard is that a no-op patch reaches the
	// apiserver and comes back accepted; the arm has to stop the probe, not grade it.
	if len(*sent) != 0 {
		t.Errorf("%d no-op patch(es) were sent to the apiserver from a fixture that already "+
			"carried the values", len(*sent))
	}
}

// THESE TWO ASSERT ON THE REPORT, NOT ONLY ON THE ERROR, and that is the whole
// difference between them testing their arm and testing nothing. Every row bails
// before its probe in both cases, so `examined` is zero and the returned error is
// the VACUITY line either way — identical for an unreadable apiserver, an
// undecodable object, an absent fixture and an empty field map. Asserting `err !=
// nil` therefore passed with the arm the test is named for deleted outright. The
// arms are only distinguishable by what the run printed, which is what
// captureAppliabilityReport exists for and what these now read.
//
// overlay_appliability_arms_test.go carries the stronger form of both, over a
// synthetic map with a healthy control row so vacuity cannot fire at all.

func TestAnUnreadableApiserverFailsTheAppliabilityLane(t *testing.T) {
	withAppliabilityObject(t, "", false, false)
	withAppliabilityDryRun(t, "", true)
	got := captureAppliabilityReport(t, assertOverlayAppliability)
	if got.err == nil {
		t.Fatal("'could not tell' was read as 'the declaration is right'")
	}
	if !strings.Contains(got.report, "could not read") {
		t.Errorf("the run failed, but not through the unreadable-apiserver arm — with that arm removed "+
			"the vacuity line fails this run just as well.\nreport:\n%s", got.report)
	}
}

func TestAnUndecodableFixtureFails(t *testing.T) {
	withAppliabilityObject(t, "{not json", false, true)
	withAppliabilityDryRun(t, "", true)
	got := captureAppliabilityReport(t, assertOverlayAppliability)
	if got.err == nil {
		t.Fatal("an undecodable object passed")
	}
	if !strings.Contains(got.report, "did not decode") {
		t.Errorf("the run failed, but not through the undecodable-object arm.\nreport:\n%s", got.report)
	}
}

func TestTheGeneratedFixtureProbesEveryRowAndAgreesWithTheMap(t *testing.T) {
	// The end-to-end run, and the one that proves the lane examines rather than
	// short-circuits: the generated fixture carries NONE of the declared values, so
	// every row builds a patch and sends it.
	withGeneratedFixture(t)
	sent := withAppliabilityDryRun(t, statefulSetRefusal, false)

	err := assertOverlayAppliability()
	// Every row now observes CREATE-ONLY. The claim row agrees; the four mutable
	// resource rows do not, and must fail — which is exactly right, because a
	// stubbed apiserver that refuses everything IS a map/reality disagreement.
	if err == nil {
		t.Fatal("an apiserver refusing every probe agreed with a map that declares four rows mutable")
	}
	// EVERY ROW THAT HAS A TRANSITION TO TEST, which is not every row: a row whose
	// declared value IS the chart default (Prior == declared) has nothing for any
	// cluster to apply, so it is reported and not probed. Counting it as a missing
	// probe would push someone to "fix" a row that is behaving correctly.
	wantProbed := 0
	for _, f := range clusterspec.OverlayFields() {
		declared, ok := clusterspec.RawValue(clusterspec.AplAppRawValues()[f.App], f.Value...)
		if ok && f.Prior != nil && fmt.Sprintf("%v", f.Prior) == fmt.Sprintf("%v", declared) {
			continue
		}
		wantProbed++
	}
	if len(*sent) != wantProbed {
		t.Errorf("probed %d row(s), want %d — a row with a transition to test that never sent a patch "+
			"vouched for nothing", len(*sent), wantProbed)
	}
}

func TestTheAgreeingRunPassesAndSaysWhatItProved(t *testing.T) {
	// The apiserver answers each row the way the map declares it: refusal for the
	// CreateOnly claim template, acceptance for the four mutable resource rows.
	withGeneratedFixture(t)
	prev := appliabilityDryRun
	appliabilityDryRun = func(_, _, _, patch string) (string, bool) {
		if strings.Contains(patch, "volumeClaimTemplates") {
			return statefulSetRefusal, false
		}
		// THE RESULTING OBJECT, not a status line — that is the contract `-o json`
		// establishes, and the reason it is the contract: an accepted probe is only
		// evidence if the value can be seen to have landed. deliveredIngester is the
		// converged shape, so it carries every declared value.
		return deliveredIngester, true
	}
	t.Cleanup(func() { appliabilityDryRun = prev })

	if err := assertOverlayAppliability(); err != nil {
		t.Fatalf("a cluster answering exactly as the map declares failed the lane: %v", err)
	}
}

// ── the generated fixtures ───────────────────────────────────────────────────

func TestEveryMappedObjectGetsAFixture(t *testing.T) {
	out, err := EmitFixtures()
	if err != nil {
		t.Fatalf("EmitFixtures: %v", err)
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatalf("the emitted fixtures are not valid JSON: %v", err)
	}
	have := map[string]bool{}
	for _, it := range list.Items {
		md, _ := it["metadata"].(map[string]any)
		ns, _ := md["namespace"].(string)
		name, _ := md["name"].(string)
		have[ns+"/"+name] = true
	}
	for _, f := range clusterspec.OverlayFields() {
		if !have[f.Namespace+"/"+appliabilityFixtureName(f)] {
			t.Errorf("no fixture emitted for %s %s/%s — the lane would probe an absent object",
				f.Kind, f.Namespace, appliabilityFixtureName(f))
		}
	}
}

func TestTheEmittedFixtureCarriesNoneOfTheDeclaredValues(t *testing.T) {
	// THE COUPLING TEST THAT KEEPS THE LANE HONEST. If the generator ever emits an
	// object already carrying a declared value, that row's probe becomes a no-op
	// patch and the lane silently inverts. Checked through the gate's OWN
	// comparison, so the generator and the check cannot drift apart.
	out, err := EmitFixtures()
	if err != nil {
		t.Fatalf("EmitFixtures: %v", err)
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatalf("the emitted fixtures are not valid JSON: %v", err)
	}
	byName := map[string]map[string]any{}
	for _, it := range list.Items {
		md, _ := it["metadata"].(map[string]any)
		ns, _ := md["namespace"].(string)
		name, _ := md["name"].(string)
		byName[ns+"/"+name] = it
	}

	raw := clusterspec.AplAppRawValues()
	checked := 0
	for _, f := range clusterspec.OverlayFields() {
		obj, ok := byName[f.Namespace+"/"+appliabilityFixtureName(f)]
		if !ok {
			continue // covered by TestEveryMappedObjectGetsAFixture
		}
		declared, ok := clusterspec.RawValue(raw[f.App], f.Value...)
		if !ok {
			t.Errorf("%s is mapped but the overlay declares no value for it", clusterspec.OverlayFieldPath(f))
			continue
		}
		checked++
		match, delivered, readable := clusterspec.OverlayFieldDelivered(f, declared, obj)
		if !readable {
			t.Errorf("%s does not resolve on the emitted fixture — the row selects something the "+
				"generator did not build, so it would cover nothing", clusterspec.OverlayFieldPath(f))
			continue
		}
		if match {
			// ALLOWED ONLY WHERE THE ROW SAYS SO. A row whose Prior IS the declared value
			// declares the chart default, so a pre-overlay object legitimately already
			// carries it. Any other match is a polluted fixture.
			if f.Prior != nil && fmt.Sprintf("%v", f.Prior) == fmt.Sprintf("%v", declared) {
				continue
			}
			t.Errorf("the emitted fixture already carries %s = %s — its probe would be a no-op patch, "+
				"which the apiserver accepts, which reads as MUTABLE and inverts the lane",
				clusterspec.OverlayFieldPath(f), delivered)
		}
	}
	if checked == 0 {
		t.Fatal("no field was checked against the emitted fixtures — this test proved nothing")
	}
}

func TestTheFixtureBuildsTheContainerTheRowSelectsOn(t *testing.T) {
	// clusterspec.LiveValue has no only-one-element fallback, so a fixture whose
	// container is named anything else makes every resource row resolve to nothing.
	targets := fixtureTargets(clusterspec.OverlayFields())
	var found bool
	for _, tgt := range targets {
		for _, c := range tgt.Containers {
			if c == clusterspec.LokiIngesterContainer {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("no fixture target carries the container %q the loki rows select on: %+v",
			clusterspec.LokiIngesterContainer, targets)
	}
}

func TestContainerSelectorsReadsOnlyWellFormedNameSelectors(t *testing.T) {
	cases := []struct {
		name            string
		live            []string
		want            []string
		wantUnsupported []string
	}{
		{"a name selector", []string{"spec", "containers[name=ingester]", "resources"}, []string{"ingester"}, nil},
		{"no selector at all", []string{"spec", "volumeClaimTemplates"}, nil, nil},
		// UNSUPPORTED, NOT IGNORED. Skipping these silently left the fixture without
		// the list, so the row resolved to "(absent)" and graded APPLIABLE instead of
		// failing closed.
		{"a selector on another list", []string{"spec", "volumes[name=data]"}, nil, []string{"volumes[name=data]"}},
		{"an initContainers selector", []string{"spec", "initContainers[name=x]"}, nil, []string{"initContainers[name=x]"}},
		{"a non-name selector", []string{"containers[image=x]"}, nil, []string{"containers[image=x]"}},
		{"an unterminated selector", []string{"containers[name=ingester"}, nil, nil},
		{"an empty name", []string{"containers[name=]"}, nil, []string{"containers[name=]"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, unsupported := containerSelectors(tc.live)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("containerSelectors(%v) = %v, want %v", tc.live, got, tc.want)
			}
			if len(tc.wantUnsupported) != len(unsupported) {
				t.Errorf("containerSelectors(%v) unsupported = %v, want %v",
					tc.live, unsupported, tc.wantUnsupported)
			}
		})
	}
}

func TestAnUnknownKindRefusesRatherThanEmittingNothingForIt(t *testing.T) {
	// A row mapping a Deployment must fail here, loudly, rather than leave the lane
	// probing an object nothing created.
	_, err := fixtureObject(fixtureTarget{Kind: "deployment", Namespace: "monitoring", Name: "loki-gateway"})
	if err == nil {
		t.Fatal("an unmapped kind emitted a fixture — the lane would probe an absent object")
	}
	if !strings.Contains(err.Error(), "fixtureObject") {
		t.Errorf("the failure does not say where to fix it: %v", err)
	}
}

func TestAStatefulSetFixtureIsValidEnoughToBeAccepted(t *testing.T) {
	// The required fields for the Kind: a selector, a matching template label set,
	// and at least one container. A fixture the apiserver rejects at CREATE would
	// surface as an absent object on every row.
	obj := statefulSetFixture(fixtureTarget{
		Kind: "statefulset", Namespace: "monitoring", Name: "loki-ingester",
		Containers: []string{"ingester"},
	})
	spec := obj["spec"].(map[string]any)
	sel := spec["selector"].(map[string]any)["matchLabels"].(map[string]any)
	tmpl := spec["template"].(map[string]any)
	tmplLabels := tmpl["metadata"].(map[string]any)["labels"].(map[string]any)
	for k, v := range sel {
		if tmplLabels[k] != v {
			t.Errorf("the pod template does not carry the selector label %s=%v — the apiserver rejects that",
				k, v)
		}
	}
	if spec["serviceName"] == "" || spec["serviceName"] == nil {
		t.Error("a StatefulSet with no serviceName is rejected at create")
	}
	cs := tmpl["spec"].(map[string]any)["containers"].([]any)
	if len(cs) == 0 {
		t.Fatal("a StatefulSet with no containers is rejected at create")
	}
	if cs[0].(map[string]any)["image"] == "" {
		t.Error("a container with no image is rejected at create")
	}
}

func TestAnEmptyFieldMapRefusesToEmitAnEmptyFixtureSet(t *testing.T) {
	// THROUGH THE SEAM, so this exercises the refusal it is named after. It used to
	// assert fixtureTargets(nil) was empty — true, trivially, and nothing to do with
	// whether EmitFixtures refuses. The arm was unreachable and the test covered a
	// different function.
	prev := overlayFieldsFor
	overlayFieldsFor = func() []clusterspec.OverlayField { return nil }
	t.Cleanup(func() { overlayFieldsFor = prev })

	if _, err := EmitFixtures(); err == nil {
		t.Fatal("an empty field map emitted a fixture set — a lane with no fixtures examines nothing")
	}
}

func TestAScalarRowWithNoPriorRefusesRatherThanOmittingTheField(t *testing.T) {
	// THE REGRESSION THAT WOULD RESTORE absent→set. A scalar row with no Prior would
	// have the fixture omit the field, so the probe tests a transition no brownfield
	// cluster performs — and anything gated on a transition rule would be graded
	// APPLIABLE and ship.
	prev := overlayFieldsFor
	overlayFieldsFor = func() []clusterspec.OverlayField {
		f := prev()[0]
		f.Prior = nil
		return []clusterspec.OverlayField{f}
	}
	t.Cleanup(func() { overlayFieldsFor = prev })

	_, err := EmitFixtures()
	if err == nil {
		t.Fatal("a scalar row with no Prior emitted a fixture — the probe would test absent→set")
	}
	if !strings.Contains(err.Error(), "absent→set") {
		t.Errorf("the refusal does not explain what would go wrong: %v", err)
	}
}

func TestTheEmittedFixtureCarriesEachScalarRowsPriorValue(t *testing.T) {
	// The positive half: the seed actually lands where the gate reads, checked
	// through clusterspec.LiveValue so the fixture and the probe cannot disagree
	// about where the value is.
	out, err := EmitFixtures()
	if err != nil {
		t.Fatalf("EmitFixtures: %v", err)
	}
	byName := fixturesByName(t, out)
	checked := 0
	for _, f := range clusterspec.OverlayFields() {
		if f.Prior == nil {
			continue
		}
		obj, ok := byName[f.Namespace+"/"+appliabilityFixtureName(f)]
		if !ok {
			t.Errorf("no fixture for %s/%s", f.Namespace, appliabilityFixtureName(f))
			continue
		}
		got, found, missed := clusterspec.LiveValue(obj, f.Live)
		if missed || !found {
			t.Errorf("%s: the seeded prior does not resolve at %s — the fixture was seeded somewhere "+
				"the gate does not look", clusterspec.OverlayFieldPath(f), strings.Join(f.Live, "."))
			continue
		}
		checked++
		if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", f.Prior) {
			t.Errorf("%s: fixture carries %v, want the declared Prior %v",
				clusterspec.OverlayFieldPath(f), got, f.Prior)
		}
	}
	if checked == 0 {
		t.Fatal("no Prior was checked — this test proved nothing")
	}
}

// ── the two false greens the review found ────────────────────────────────────

func TestAPatchThatDoesNotTargetTheRowsFieldIsRefused(t *testing.T) {
	// MEASURED, NOT HYPOTHESISED. Repointing the WAL-claim row's Patch at
	// {"spec":{"serviceName":"…"}} still graded it CREATE-ONLY and exited 0, because
	// StatefulSet's refusal is a whole-spec message emitted identically for ANY
	// non-whitelisted spec key. Patch and Live were independent, so a green
	// CREATE-ONLY row established only "the patch touched sts.spec outside the
	// mutable whitelist".
	claim := fieldNamed(t, "loki.ingester.persistence.enabled")
	if err := patchTargetsLive(`{"spec":{"serviceName":"an-entirely-unrelated-field"}}`, claim, true); err == nil {
		t.Fatal("a patch aimed at an unrelated spec key was accepted as testing this row's field")
	}
	// And the row's real patch must pass, or the check is just a blocker.
	real, err := claim.Patch(clusterspec.AplAppRawValues()[claim.App])
	if err != nil {
		t.Fatalf("building the row's own patch: %v", err)
	}
	if err := patchTargetsLive(real, claim, true); err != nil {
		t.Errorf("the row's own patch was rejected by its own coupling check: %v", err)
	}
}

func TestATypoedPatchKeyIsRefusedRatherThanGradedAppliable(t *testing.T) {
	// The other measured false green: `resources` retyped to `resourcesTYPO` makes
	// the apiserver exit 0 with `patched (no change)` and a stderr warning, and all
	// four resource rows printed "APPLIABLE in place, as declared".
	mem := fieldNamed(t, "loki.ingester.resources.limits.memory")
	typo := `{"spec":{"template":{"spec":{"containers":[{"name":"ingester",` +
		`"resourcesTYPO":{"limits":{"memory":"3Gi"}}}]}}}}`
	if err := patchTargetsLive(typo, mem, "3Gi"); err == nil {
		t.Fatal("a patch whose key is misspelled was accepted as testing this row's field")
	}
}

func TestAnAcceptedProbeThatLandedNothingIsRefused(t *testing.T) {
	// The second layer, independent of the structural one: a patch that targets the
	// right path but whose value does not survive. brownfieldIngester is what the
	// apiserver would return for a no-op — the prior value still in place.
	mem := fieldNamed(t, "loki.ingester.resources.limits.memory")
	if err := probeLandedTheValue(brownfieldIngester, mem, "3Gi"); err == nil {
		t.Fatal("an accepted probe whose value never landed passed — the row tested nothing")
	}
	// The converged shape carries it, so the same check must pass there.
	if err := probeLandedTheValue(deliveredIngester, mem, "3Gi"); err != nil {
		t.Errorf("a probe that DID land was rejected: %v", err)
	}
}

func TestAnUndecodableDryRunResponseIsRefused(t *testing.T) {
	mem := fieldNamed(t, "loki.ingester.resources.limits.memory")
	if err := probeLandedTheValue("not json", mem, "3Gi"); err == nil {
		t.Fatal("an undecodable dry-run response was read as proof the value landed")
	}
}

func TestAFixtureBuilderThatCannotHonourASelectorRefuses(t *testing.T) {
	// initContainers, volumes, ephemeralContainers: the builder does not build them,
	// and skipping the selector silently left the row resolving to "(absent)" — which
	// probes and grades APPLIABLE instead of failing closed.
	prev := overlayFieldsFor
	overlayFieldsFor = func() []clusterspec.OverlayField {
		f := prev()[0]
		f.Live = []string{"spec", "template", "spec", "initContainers[name=wal-warm]", "resources", "limits", "memory"}
		return []clusterspec.OverlayField{f}
	}
	t.Cleanup(func() { overlayFieldsFor = prev })

	if _, err := EmitFixtures(); err == nil {
		t.Fatal("a row selecting a list the builder cannot build emitted a fixture anyway")
	}
}

func TestALongObjectNameStillYieldsAValidLabelValue(t *testing.T) {
	long := strings.Repeat("a", 200)
	obj := statefulSetFixture(fixtureTarget{Kind: "statefulset", Namespace: "monitoring", Name: long})
	labels := obj["metadata"].(map[string]any)["labels"].(map[string]any)
	for k, v := range labels {
		if s, ok := v.(string); ok && len(s) > maxLabelValue {
			t.Errorf("label %s is %d chars, over Kubernetes' %d limit — the apiserver would reject the "+
				"whole fixture at create, which this lane reports as an absent object", k, len(s), maxLabelValue)
		}
	}
}
