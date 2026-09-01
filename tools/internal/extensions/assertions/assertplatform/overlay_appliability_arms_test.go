package assertplatform

// overlay_appliability_arms_test.go drives assertOverlayAppliability over a
// SYNTHETIC field map, and exists because the lane's real one made most of its
// own fail-closed arms untestable.
//
// THE HOLE THIS CLOSES. Every lane-level test used to read
// clusterspec.OverlayFields() directly, so every one of them was pinned to the
// five real loki rows — all scalar-or-list rows on one StatefulSet, all with a
// declared _rawValues entry, all resolving. No test could construct a row that
// reaches "the overlay declares no _rawValues for app X", "did not decode",
// "could not build the probe", or "does not resolve on the fixture". Measured:
// all four could be flipped to OK:true with the whole suite green.
//
// AND THE WIRING, WHICH IS THE SHARPER HALF. patchTargetsLive and
// probeLandedTheValue were each well covered AS FUNCTIONS and never covered as
// CALLS: deleting either call site — or both — left 146 packages green and
// reproduced both of the false greens they were written to close. A function
// nothing is proven to call is a function that can be unplugged in silence, so
// the tests below assert on the LANE's verdict, never on the helper's.
//
// EVERY CASE CARRIES A HEALTHY CONTROL ROW. Without one the run's only error is
// the vacuity line, which every arm produces — so the test would pass for a
// reason that has nothing to do with the arm it is named for. That is exactly how
// two of the pre-existing arm tests came to be passing on vacuity. With a control
// row, `examined` is 1, vacuity cannot fire, and the failure is the arm's own.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
)

// ── a synthetic map, and the cluster that answers for it ─────────────────────

const (
	armNamespace = "arm-ns"
	armApp       = "armapp"
)

// armRow is a minimal, self-consistent scalar row: pre-overlay 1Gi, declared 3Gi,
// a Patch that writes exactly that at exactly its own Live path.
func armRow(name string, mut ...func(*clusterspec.OverlayField)) clusterspec.OverlayField {
	f := clusterspec.OverlayField{
		App:             armApp,
		Value:           []string{name},
		Kind:            "widget",
		Namespace:       armNamespace,
		Name:            name,
		OwnerApp:        "arm-owner",
		APIGroupVersion: "arm/v1",
		Resource:        "widgets",
		Live:            []string{"spec", "size"},
		Match:           clusterspec.MatchScalar,
		Prior:           "1Gi",
		Why:             "a synthetic row, so the lane's fail-closed arms are reachable",
		Patch: func(rv map[string]any) (string, error) {
			v, ok := clusterspec.RawValue(rv, name)
			if !ok {
				return "", fmt.Errorf("the overlay declares no %s", name)
			}
			b, err := json.Marshal(map[string]any{"spec": map[string]any{"size": v}})
			return string(b), err
		},
	}
	for _, m := range mut {
		m(&f)
	}
	return f
}

// armCluster is what the apiserver holds and says, per object name.
type armCluster struct {
	// objects is the fixture each row reads, as JSON. A name absent from the map
	// reads as an absent object.
	objects map[string]string
	// dryRun answers a probe. Defaults to "accepted, and the value landed".
	dryRun func(name, patch string) (string, bool)
}

func withArmMap(t *testing.T, fields []clusterspec.OverlayField, raw map[string]any, c armCluster) {
	t.Helper()
	pf, pr := overlayFieldsFor, overlayRawValuesFor
	pRead, pDry := appliabilityReadObject, appliabilityDryRun

	// THE CLUSTER IS KEYED BY THE FIXTURE'S NAME, NOT THE ROW'S, because the lane
	// probes appliabilityFixtureName(f) — the fixture is deliberately not allowed to
	// carry the row's production identity. Cases below name their objects by row for
	// readability, so the translation happens once, here, through the same function
	// the lane and the emitter use.
	byFixture := map[string]string{}
	rowOf := map[string]string{}
	for _, f := range fields {
		rowOf[appliabilityFixtureName(f)] = f.Name
		if obj, ok := c.objects[f.Name]; ok {
			byFixture[appliabilityFixtureName(f)] = obj
		}
	}

	overlayFieldsFor = func() []clusterspec.OverlayField { return fields }
	overlayRawValuesFor = func() map[string]map[string]any {
		return map[string]map[string]any{armApp: raw}
	}
	appliabilityReadObject = func(_, _, name string) ([]byte, bool, bool, string) {
		obj, ok := byFixture[name]
		if !ok {
			return nil, true, true, `widgets "` + name + `" not found`
		}
		return []byte(obj), false, true, ""
	}
	prevCreate := appliabilityDryRunCreate
	appliabilityDryRunCreate = func(string, string) (string, bool) { return "{}", true }
	appliabilityDryRun = func(_, _, name, patch string) (string, bool) {
		if c.dryRun != nil {
			return c.dryRun(rowOf[name], patch)
		}
		// The default apiserver: accepts, and returns the object with the patch applied
		// — which is what makes probeLandedTheValue's success path realistic.
		var obj, p map[string]any
		_ = json.Unmarshal([]byte(byFixture[name]), &obj)
		_ = json.Unmarshal([]byte(patch), &p)
		mergeInto(obj, p)
		b, _ := json.Marshal(obj)
		return string(b), true
	}
	t.Cleanup(func() {
		overlayFieldsFor, overlayRawValuesFor = pf, pr
		appliabilityReadObject, appliabilityDryRun = pRead, pDry
		appliabilityDryRunCreate = prevCreate
	})
}

// mergeInto is a strategic-merge stand-in good enough for the synthetic rows: a
// map merges key by key, anything else replaces.
func mergeInto(dst, src map[string]any) {
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				mergeInto(dm, sm)
				continue
			}
		}
		dst[k] = v
	}
}

// healthyRow is the control every case carries: a row that probes cleanly, so
// `examined` is non-zero and the vacuity line cannot be what fails the run.
func healthyRow() (clusterspec.OverlayField, string) {
	return armRow("healthy"), `{"spec":{"size":"1Gi"}}`
}

// runArms drives the lane and returns the error plus the printed report.
func runArms(t *testing.T) appliabilityRun {
	t.Helper()
	return captureAppliabilityReport(t, assertOverlayAppliability)
}

// requireArmFailure asserts the run failed, that it did NOT fail merely because
// nothing was examined, and that the report names the arm.
func requireArmFailure(t *testing.T, got appliabilityRun, want string) {
	t.Helper()
	if got.err == nil {
		t.Fatalf("the lane passed.\nreport:\n%s", got.report)
	}
	if strings.Contains(got.err.Error(), "examined nothing") {
		t.Fatalf("the lane failed only because nothing was probed, so this case proves nothing about "+
			"the arm it names — the control row should have kept `examined` non-zero.\nerr: %v\nreport:\n%s",
			got.err, got.report)
	}
	if !strings.Contains(got.report, want) {
		t.Errorf("the report does not name this arm.\nwant a line containing: %s\nreport:\n%s", want, got.report)
	}
}

// ── the four arms that were unreachable ──────────────────────────────────────

func TestARowWhoseAppDeclaresNoRawValuesFails(t *testing.T) {
	h, hObj := healthyRow()
	orphan := armRow("orphan", func(f *clusterspec.OverlayField) { f.App = "an-app-the-overlay-does-not-declare" })
	withArmMap(t, []clusterspec.OverlayField{h, orphan},
		map[string]any{"healthy": "3Gi"},
		armCluster{objects: map[string]string{"healthy": hObj, "orphan": `{"spec":{"size":"1Gi"}}`}})
	requireArmFailure(t, runArms(t), "declares no _rawValues for app")
}

func TestARowWhoseDeclaredValueIsMissingFails(t *testing.T) {
	h, hObj := healthyRow()
	withArmMap(t, []clusterspec.OverlayField{h, armRow("undeclared")},
		map[string]any{"healthy": "3Gi"}, // nothing for "undeclared"
		armCluster{objects: map[string]string{"healthy": hObj, "undeclared": `{"spec":{"size":"1Gi"}}`}})
	requireArmFailure(t, runArms(t), "the overlay declares no")
}

func TestAnAbsentObjectFailsEvenBesideAProbedRow(t *testing.T) {
	// THE ARM'S OWN VERDICT, NOT THE VACUITY FLOOR UNDER IT. `examined == 0` fires
	// whenever every row bails, so a case whose only row is absent fails for a reason
	// that has nothing to do with this arm. Measured: flipping the absent verdict to
	// OK:true left all 146 packages green, because
	// TestAnAbsentFixtureFailsRatherThanPassingUnexamined still failed on vacuity and
	// its `does not exist` substring matched either way.
	//
	// AND IT STOPS BEING HYPOTHETICAL THE MOMENT THE MAP NAMES A SECOND OBJECT —
	// which FixtureNamespaces() and --print-namespaces exist to support. One fixture
	// missing from a multi-object apply then leaves `examined` non-zero and `failed`
	// at zero, and the lane prints `ok <obj> does not exist` and exits 0: an absent
	// fixture reported as evidence. The control row here reproduces exactly that
	// shape, so the absent row's own OK:false is what has to fail the run.
	h, hObj := healthyRow()
	withArmMap(t, []clusterspec.OverlayField{h, armRow("gone")},
		map[string]any{"healthy": "3Gi", "gone": "3Gi"},
		// "gone" is deliberately absent from the cluster; "healthy" probes normally.
		armCluster{objects: map[string]string{"healthy": hObj}})
	requireArmFailure(t, runArms(t), "does not exist")
}

func TestAnUndecodableFixtureObjectFailsTheAppliabilityArm(t *testing.T) {
	h, hObj := healthyRow()
	withArmMap(t, []clusterspec.OverlayField{h, armRow("garbled")},
		map[string]any{"healthy": "3Gi", "garbled": "3Gi"},
		armCluster{objects: map[string]string{"healthy": hObj, "garbled": `{"spec":{"size":`}})
	requireArmFailure(t, runArms(t), "did not decode")
}

func TestARowWhosePatchWillNotBuildFails(t *testing.T) {
	h, hObj := healthyRow()
	broken := armRow("broken", func(f *clusterspec.OverlayField) {
		f.Patch = func(map[string]any) (string, error) { return "", fmt.Errorf("the claims list is not a list") }
	})
	withArmMap(t, []clusterspec.OverlayField{h, broken},
		map[string]any{"healthy": "3Gi", "broken": "3Gi"},
		armCluster{objects: map[string]string{"healthy": hObj, "broken": `{"spec":{"size":"1Gi"}}`}})
	requireArmFailure(t, runArms(t), "could not build the appliability probe")
}

func TestARowWhoseLivePathDoesNotResolveFails(t *testing.T) {
	h, hObj := healthyRow()
	adrift := armRow("adrift", func(f *clusterspec.OverlayField) {
		f.Live = []string{"spec", "containers[name=gone]", "size"}
	})
	// The LIST is there and the SELECTOR misses — a chart rename, which has to read
	// as a row pointing at nothing rather than as a value to go and deliver.
	withArmMap(t, []clusterspec.OverlayField{h, adrift},
		map[string]any{"healthy": "3Gi", "adrift": "3Gi"},
		armCluster{objects: map[string]string{
			"healthy": hObj,
			"adrift":  `{"spec":{"containers":[{"name":"renamed","size":"1Gi"}]}}`,
		}})
	requireArmFailure(t, runArms(t), "does not resolve on")
}

func TestAnUnreadableApiserverFailsAndSaysWhatKubectlSaid(t *testing.T) {
	h, hObj := healthyRow()
	prevRead := appliabilityReadObject
	withArmMap(t, []clusterspec.OverlayField{h, armRow("unreachable")},
		map[string]any{"healthy": "3Gi", "unreachable": "3Gi"},
		armCluster{objects: map[string]string{"healthy": hObj}})
	// One row cannot be read at all — the shape every kubeconfig fault takes.
	inner := appliabilityReadObject
	appliabilityReadObject = func(k, ns, name string) ([]byte, bool, bool, string) {
		if strings.HasPrefix(name, "unreachable") {
			return nil, false, false, "The connection to the server 10.0.0.1:6443 was refused"
		}
		return inner(k, ns, name)
	}
	t.Cleanup(func() { appliabilityReadObject = prevRead })

	got := runArms(t)
	requireArmFailure(t, got, "could not read")
	// THE WHOLE POINT OF THE ARM. Three kubeconfig faults used to print the same
	// sentence and name none of them; the fix is that kubectl's own words survive.
	if !strings.Contains(got.report, "connection to the server") {
		t.Errorf("the report drops what kubectl actually said, which is the one thing that tells an "+
			"unset KUBECONFIG from a dead cluster.\nreport:\n%s", got.report)
	}
}

// ── the wiring: the two coupling calls, asserted through the lane ────────────

func TestTheLaneRefusesAPatchThatDoesNotTargetTheRowsField(t *testing.T) {
	// DELETING THE patchTargetsLive CALL MUST FAIL THIS. It is asserted through
	// assertOverlayAppliability rather than through the helper, because the helper
	// was already well tested and the CALL was what could be removed in silence.
	h, hObj := healthyRow()
	astray := armRow("astray", func(f *clusterspec.OverlayField) {
		f.Patch = func(map[string]any) (string, error) {
			return `{"spec":{"anEntirelyUnrelatedField":"x"}}`, nil
		}
	})
	withArmMap(t, []clusterspec.OverlayField{h, astray},
		map[string]any{"healthy": "3Gi", "astray": "3Gi"},
		armCluster{
			objects: map[string]string{"healthy": hObj, "astray": `{"spec":{"size":"1Gi"}}`},
			// The apiserver refuses it as immutability — which is exactly how this used to
			// grade "CREATE-ONLY, as declared" on evidence about a different field.
			dryRun: func(name, _ string) (string, bool) {
				if name == "astray" {
					return statefulSetRefusal, false
				}
				return `{"spec":{"size":"3Gi"}}`, true
			},
		})
	requireArmFailure(t, runArms(t), "does not write anything at")
}

func TestTheLaneRefusesAPatchThatAlsoWritesSomethingElse(t *testing.T) {
	// CONTAINMENT IS NOT EXCLUSIVITY. Measured on a real apiserver: the row's own
	// path PLUS one unrelated spec key is refused for the unrelated key, with a
	// byte-identical whole-spec message, and graded "CREATE-ONLY, as declared".
	h, hObj := healthyRow()
	extra := armRow("extra", func(f *clusterspec.OverlayField) {
		f.Patch = func(map[string]any) (string, error) {
			return `{"spec":{"size":"3Gi","podManagementPolicy":"Parallel"}}`, nil
		}
	})
	withArmMap(t, []clusterspec.OverlayField{h, extra},
		map[string]any{"healthy": "3Gi", "extra": "3Gi"},
		armCluster{
			objects: map[string]string{"healthy": hObj, "extra": `{"spec":{"size":"1Gi"}}`},
			dryRun: func(name, _ string) (string, bool) {
				if name == "extra" {
					return statefulSetRefusal, false
				}
				return `{"spec":{"size":"3Gi"}}`, true
			},
		})
	requireArmFailure(t, runArms(t), "also writes")
}

func TestTheLaneRefusesAPatchThatSendsTheWrongValue(t *testing.T) {
	// THE REFUSED ARM HAS NO OBJECT TO READ BACK, so this is the only place a wrong
	// payload can be caught for a CreateOnly row — the row whose claim is wired to a
	// migration that deletes a live object.
	h, hObj := healthyRow()
	wrong := armRow("wrong", func(f *clusterspec.OverlayField) {
		f.Patch = func(map[string]any) (string, error) {
			return `{"spec":{"size":"something-nobody-declared"}}`, nil
		}
	})
	withArmMap(t, []clusterspec.OverlayField{h, wrong},
		map[string]any{"healthy": "3Gi", "wrong": "3Gi"},
		armCluster{
			objects: map[string]string{"healthy": hObj, "wrong": `{"spec":{"size":"1Gi"}}`},
			dryRun: func(name, _ string) (string, bool) {
				if name == "wrong" {
					return statefulSetRefusal, false
				}
				return `{"spec":{"size":"3Gi"}}`, true
			},
		})
	requireArmFailure(t, runArms(t), "not the declared")
}

func TestTheLaneRefusesAnAcceptedProbeThatLandedNothing(t *testing.T) {
	// DELETING THE probeLandedTheValue CALL MUST FAIL THIS. kubectl exits 0 for a
	// patch that changes nothing — `patched (no change)`, with the unknown field only
	// a stderr warning — so "accepted" alone cannot tell an appliable field from a
	// probe that landed nothing.
	h, hObj := healthyRow()
	withArmMap(t, []clusterspec.OverlayField{h, armRow("noop")},
		map[string]any{"healthy": "3Gi", "noop": "3Gi"},
		armCluster{
			objects: map[string]string{"healthy": hObj, "noop": `{"spec":{"size":"1Gi"}}`},
			dryRun: func(name, _ string) (string, bool) {
				if name == "noop" {
					// Accepted, and the object comes back UNCHANGED.
					return `{"spec":{"size":"1Gi"}}`, true
				}
				return `{"spec":{"size":"3Gi"}}`, true
			},
		})
	requireArmFailure(t, runArms(t), "did NOT land")
}

func TestARefusalThatAlsoReportsAMalformedPatchIsNotGradedCreateOnly(t *testing.T) {
	// The apiserver reports BOTH in one message, and the immutability marker used to
	// win a plain substring match — so a structurally broken probe graded
	// "CREATE-ONLY, as declared" and exited 0.
	h, hObj := healthyRow()
	malformed := armRow("malformed", func(f *clusterspec.OverlayField) {
		f.CreateOnly = true
		f.Migration = "999-a-migration"
	})
	withArmMap(t, []clusterspec.OverlayField{h, malformed},
		map[string]any{"healthy": "3Gi", "malformed": "3Gi"},
		armCluster{
			objects: map[string]string{"healthy": hObj, "malformed": `{"spec":{"size":"1Gi"}}`},
			dryRun: func(name, _ string) (string, bool) {
				if name == "malformed" {
					return `The Widget "malformed" is invalid: ` +
						`* spec.template.spec.volumes[0].name: Required value` + "\n" +
						`* spec: Forbidden: updates to widget spec for fields other than 'replicas' are forbidden`, false
				}
				return `{"spec":{"size":"3Gi"}}`, true
			},
		})
	got := runArms(t)
	requireArmFailure(t, got, "not one this gate classifies")
	if strings.Contains(got.report, "CREATE-ONLY, as declared") {
		t.Errorf("a refusal that also says the patch was malformed was graded CREATE-ONLY — the "+
			"immutability marker won a substring match over the rest of the message.\nreport:\n%s", got.report)
	}
}

// ── the escape hatch ─────────────────────────────────────────────────────────

func TestARowWhosePriorEqualsItsDeclaredValueIsReportedAsSkippedNotOk(t *testing.T) {
	h, hObj := healthyRow()
	noTransition := armRow("notransition", func(f *clusterspec.OverlayField) { f.Prior = "3Gi" })
	withArmMap(t, []clusterspec.OverlayField{h, noTransition},
		map[string]any{"healthy": "3Gi", "notransition": "3Gi"},
		armCluster{objects: map[string]string{"healthy": hObj, "notransition": `{"spec":{"size":"3Gi"}}`}})

	got := runArms(t)
	if got.err != nil {
		t.Fatalf("a mutable row declaring the chart default is legitimate and must not fail: %v\n%s",
			got.err, got.report)
	}
	// THE POINT: it must not read as a probed, agreeing row — asserted on THE ROW'S
	// OWN LINE, not on the report as a whole.
	//
	// A whole-report `strings.Contains(report, "skip")` passed with the marker
	// flipped back to "ok", because the success line this same fix added prints
	// "(see the skip lines above)" and satisfied the assertion by itself. The
	// visible half of the fix was ungated by the test written to gate it — the exact
	// shape of "a passing test may pass for an unrelated reason" this file is full of
	// warnings about.
	line := reportLineFor(t, got.report, "armapp.notransition")
	if !strings.HasPrefix(strings.TrimSpace(line), "skip") {
		t.Errorf("a row that was never put to the apiserver is not marked `skip` on its own line, so "+
			"a reader scanning a green report cannot tell it from one that agreed.\nline: %q", line)
	}
	if !strings.Contains(line, "NOT probed") {
		t.Errorf("the skip line does not say the row was not probed.\nline: %q", line)
	}
	// And the pass line has to carry the count, or "All 1 probed field(s) match" reads
	// as full coverage of a two-row map.
	if !strings.Contains(got.report, "were NOT probed") {
		t.Errorf("the success line does not say how many rows went unprobed, which is the difference "+
			"between coverage and the appearance of it.\nreport:\n%s", got.report)
	}
}

func TestACreateOnlyRowMayNotEscapeThroughItsOwnPrior(t *testing.T) {
	// THE FALSE GREEN THIS CLOSES. A CreateOnly row's claim is wired to a migration
	// that DELETES a live object. Setting Prior equal to the declared value made the
	// lane grade it `ok` having asked the apiserver nothing — so the destructive half
	// shipped on a hand-set string, which is the exact trust this lane removes
	// everywhere else.
	h, hObj := healthyRow()
	escaped := armRow("escaped", func(f *clusterspec.OverlayField) {
		f.Prior = "3Gi"
		f.CreateOnly = true
		f.Migration = "999-a-migration"
	})
	withArmMap(t, []clusterspec.OverlayField{h, escaped},
		map[string]any{"healthy": "3Gi", "escaped": "3Gi"},
		armCluster{objects: map[string]string{"healthy": hObj, "escaped": `{"spec":{"size":"3Gi"}}`}})
	requireArmFailure(t, runArms(t), "can never be tested while Prior equals the declared value")
}

// ── quantities ───────────────────────────────────────────────────────────────

func TestANonCanonicalDeclaredQuantityIsNotReportedAsUndelivered(t *testing.T) {
	// The apiserver canonicalises: patch 3072Mi, read back 3Gi. A `%v` compare called
	// that a value that did not land, and blamed the row's Patch for the apiserver's
	// own normalisation — while the same helper, in the migration precondition, made
	// "is this field still undelivered" answer yes forever.
	row := armRow("canon")
	withArmMap(t, []clusterspec.OverlayField{row},
		map[string]any{"canon": "3072Mi"},
		armCluster{
			objects: map[string]string{"canon": `{"spec":{"size":"1Gi"}}`},
			dryRun: func(string, string) (string, bool) {
				return `{"spec":{"size":"3Gi"}}`, true // what the apiserver really returns
			},
		})
	got := runArms(t)
	if got.err != nil {
		t.Fatalf("a correctly delivered value spelled non-canonically was reported as a failure: %v\n%s",
			got.err, got.report)
	}
}

// ── what "and nothing else" does and does not mean ───────────────────────────

func TestAMergeKeyOrCoRequiredSiblingInsideTheRowsOwnElementIsNotAnExtraWrite(t *testing.T) {
	// A FALSE RED HERE BLOCKS A LEGITIMATE ROW, and the exclusivity check's first
	// version did. Two shapes, both plausible and neither able to produce a spurious
	// CREATE-ONLY verdict:
	//
	//   a Service port list merges on `port`, not on the `name` the row selects on,
	//   so the only correct strategic-merge patch carries `port` as well;
	//   the apiserver refuses `requests > limits`, so a row raising requests past
	//   the fixture's limit can only be expressed as a patch carrying both.
	//
	// Rejecting either pushes the author toward a patch that is wrong in a way this
	// gate cannot see.
	f := armRow("merge", func(f *clusterspec.OverlayField) {
		f.Live = []string{"spec", "ports[name=http-metrics]", "targetPort"}
		f.Patch = func(map[string]any) (string, error) {
			return `{"spec":{"ports":[{"name":"http-metrics","port":3100,"targetPort":"3100"}]}}`, nil
		}
	})
	if err := patchTargetsLive(`{"spec":{"ports":[{"name":"http-metrics","port":3100,"targetPort":"3100"}]}}`,
		f, "3100"); err != nil {
		t.Errorf("a patch carrying the list's own merge key was rejected as writing something the row "+
			"does not test: %v", err)
	}
}

func TestAKeyBesideTheRowsFieldAtAMapLevelIsStillAnExtraWrite(t *testing.T) {
	// The control for the test above: the measured false green must stay refused.
	// spec.podManagementPolicy sits beside spec.volumeClaimTemplates at a MAP level
	// the walk passes through, not inside a list element the row selects, and on its
	// own it produces the whole-spec refusal that grades CREATE-ONLY.
	f := armRow("beside", func(f *clusterspec.OverlayField) { f.Live = []string{"spec", "size"} })
	err := patchTargetsLive(`{"spec":{"size":"3Gi","podManagementPolicy":"Parallel"}}`, f, "3Gi")
	if err == nil {
		t.Fatal("a patch carrying an unrelated sibling key was accepted as testing only this row")
	}
	if !strings.Contains(err.Error(), "podManagementPolicy") {
		t.Errorf("the refusal does not name the extra key: %v", err)
	}
}

func TestAPatchCannotSmuggleASecondValueThroughADuplicateSelectedElement(t *testing.T) {
	// LiveValue selects the FIRST match and stops, so the value check reads element
	// zero. An exclusivity check that treated every match as payload saw nothing
	// extra — and the row was certified "path, value, and nothing else" for a patch
	// carrying a second, different value for its own field.
	f := armRow("dup", func(f *clusterspec.OverlayField) {
		f.Live = []string{"spec", "containers[name=ingester]", "size"}
	})
	patch := `{"spec":{"containers":[{"name":"ingester","size":"3Gi"},{"name":"ingester","size":"64Mi"}]}}`
	if err := patchTargetsLive(patch, f, "3Gi"); err == nil {
		t.Fatal("a patch writing this row's own field twice, with two different values, was " +
			"certified as writing the declared value and nothing else")
	}
}

// reportLineFor returns the report line naming a row, so an assertion is about
// that row rather than about the report happening to contain a word somewhere.
func reportLineFor(t *testing.T, report, path string) string {
	t.Helper()
	for _, ln := range strings.Split(report, "\n") {
		if strings.Contains(ln, path+" ") || strings.Contains(ln, path+":") {
			return ln
		}
	}
	t.Fatalf("no report line names %s.\nreport:\n%s", path, report)
	return ""
}

func TestAPayloadTheApiserverWillNotAcceptFailsTheRowItBacks(t *testing.T) {
	// THE WHOLE-SPEC REFUSAL CANNOT SPEAK FOR THE PAYLOAD. On a StatefulSet update
	// `spec: Forbidden` short-circuits field validation, so a malformed claim
	// template reads exactly like an immutable one — and that is the row whose word
	// an orphan delete runs on. Measured live: sabotaging claimTemplate to emit an
	// empty-spec PVC still printed "CREATE-ONLY, as declared" and exited 0.
	h, hObj := healthyRow()
	payload := armRow("payload", func(f *clusterspec.OverlayField) {
		f.Live = []string{"spec", "claims"}
		f.Match = clusterspec.MatchNonEmptyList
		f.CreateOnly = true
		f.Migration = "999-a-migration"
		f.Prior = nil
		f.Patch = func(map[string]any) (string, error) {
			return `{"spec":{"claims":[{"apiVersion":"v1","kind":"PersistentVolumeClaim",` +
				`"metadata":{"name":"data"},"spec":{}}]}}`, nil
		}
	})
	withArmMap(t, []clusterspec.OverlayField{h, payload},
		map[string]any{"healthy": "3Gi", "payload": true},
		armCluster{
			objects: map[string]string{"healthy": hObj, "payload": `{"spec":{}}`},
			dryRun: func(name, _ string) (string, bool) {
				if name == "payload" {
					return statefulSetRefusal, false // the whole-spec answer that hides the payload
				}
				return `{"spec":{"size":"3Gi"}}`, true
			},
		})
	// Installed AFTER withArmMap, whose cleanup restores this seam too.
	appliabilityDryRunCreate = func(_, doc string) (string, bool) {
		if strings.Contains(doc, "PersistentVolumeClaim") {
			return "The PersistentVolumeClaim \"data\" is invalid:\n" +
				"* spec.accessModes: Required value: at least 1 access mode is required\n" +
				"* spec.resources[storage]: Required value", false
		}
		return "{}", true
	}
	got := runArms(t)
	requireArmFailure(t, got, "will not accept")
	if strings.Contains(got.report, "CREATE-ONLY, as declared") {
		t.Errorf("a row whose payload the apiserver refuses was still graded CREATE-ONLY — its "+
			"verdict rests on a whole-spec refusal that says nothing about the payload.\nreport:\n%s",
			got.report)
	}
}

func TestEachMalformedFieldErrorIsCarriedByACapturedRefusalOrMarkedSpeculative(t *testing.T) {
	// THE SAME META-TEST THIS PR ADDED TO kubectlprobe AND health, applied here at
	// last. "unsupported value" and "duplicate value" were both dead — deletable with
	// the whole tree green — one file from a test whose own header says it "would
	// have caught the dead entries at authoring time rather than in review".
	captured := map[string]string{
		"PVC missing its required fields": "The PersistentVolumeClaim \"data\" is invalid:\n" +
			"* spec.accessModes: Required value: at least 1 access mode is required\n" +
			"* spec.resources[storage]: Required value",
	}
	for _, m := range malformedFieldErrors {
		var carried bool
		for _, msg := range captured {
			if strings.Contains(strings.ToLower(msg), m) {
				carried = true
			}
		}
		if !carried && !speculativeMalformedErrors[m] {
			t.Errorf("malformedFieldErrors entry %q is carried by no captured refusal and is not "+
				"marked speculative — an unpinned marker is one a later edit deletes silently", m)
		}
		if carried && speculativeMalformedErrors[m] {
			t.Errorf("%q is marked speculative but a captured refusal now carries it — drop it from "+
				"the speculative set so the entry is genuinely pinned", m)
		}
	}
	// And the positive anchor: the captured refusal must actually be classified.
	for name, msg := range captured {
		if !malformedProbeRefusal(msg) {
			t.Errorf("%s is not recognised as a malformed probe: %s", name, msg)
		}
	}
}
