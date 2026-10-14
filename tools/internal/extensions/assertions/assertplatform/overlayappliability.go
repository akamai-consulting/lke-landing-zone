package assertplatform

// overlayappliability.go implements `llz ci assert-overlay-appliability` — the
// PR-TIME half of the question assert-overlay-applied asks at runtime.
//
// WHAT IT IS FOR. clusterspec.OverlayField.CreateOnly is a HAND-SET BOOLEAN. The
// two guards around it both read that same flag: one requires a CreateOnly field
// to name a Migration, the other requires a mutable field not to. Neither asks an
// API SERVER whether the classification is true. So the next overlay entry whose
// field happens to be fixed at create time gets CreateOnly:false by omission,
// ships green, and reproduces the failure the flag exists to prevent — Argo
// dry-run-applies, the apiserver refuses, no diff is produced, the Application
// reads Synced, and the value (plus every other change to that object) is
// silently discarded. That is the 16-day Loki outage, arriving a second time
// through the gate built to stop it.
//
// This lane derives the answer instead of trusting the annotation. Against a
// cluster holding each mapped object in its PRE-OVERLAY shape, it server-dry-runs
// every declared change and compares what the apiserver said against what the
// field map claims. A mismatch in either direction fails the PR.
//
// WHY IT NEEDS A CLUSTER AND CANNOT BE A STATIC GUARD. Create-time immutability
// is not a property of the overlay, the chart, or anything in this repo — it is a
// property of the apiserver's validation for that Kind. No amount of reading YAML
// can tell you that adding volumeClaimTemplates to an existing StatefulSet is
// Forbidden. Only the apiserver knows, so the gate has to ask one. It runs in the
// kind lane, against generated fixtures, which is why it is safe to run on every
// PR: the objects it probes are ones it created seconds earlier in a throwaway
// cluster.
//
// WHERE THE FIXTURE COMES FROM, AND THE LIMIT THAT SETS. `--emit-fixtures` builds
// a MINIMAL object per mapped target, from the field map's own identity fields
// (kind, namespace, name, and the container names its selectors reference). It is
// deliberately not apl-core's real rendered ingester, and the difference bounds
// what this lane can answer:
//
//	IT ANSWERS      is this field one the apiserver refuses to change on an object
//	                that already exists — i.e. does landing it require a migration.
//	                The fixture is seeded with each scalar row's OverlayField.Prior,
//	                the chart default a pre-overlay object carries, so the probe
//	                tests default→declared: the transition a brownfield cluster
//	                actually performs. An earlier version omitted the field and
//	                tested absent→set, which is a WEAKER question — anything gated
//	                on a transition (a CRD `self == oldSelf` rule, a quantity that
//	                may grow but not shrink, a set-once field) accepts absent→set
//	                and refuses the real change, and would have been graded
//	                APPLIABLE and shipped.
//	IT DOES NOT     did apl-core change the object's shape underneath us. That
//	                needs the real chart at the real version, and the fixture feed
//	                is the seam for it: this verb probes whatever objects are in
//	                the cluster, so a lane that applies rendered apl-core manifests
//	                instead of `--emit-fixtures` gets the same verdicts with no
//	                code change here.
//
// DERIVED FROM THE MAP'S IDENTITY, NEVER FROM ITS ANSWER. The fixture is built
// from which object a row points at; the verdict comes from the apiserver. Those
// are different halves, and only the second one is the thing under test — so this
// does not fall into deriving the expected set from the code being checked.
//
// FAIL-CLOSED, FIVE WAYS, and every one of them is a state in which a naive
// implementation reports success having proven nothing:
//
//	the object is absent          the fixture never applied; every probe would be
//	                              a 404 and every row would "pass" unexamined
//	the field is ALREADY there    the probe becomes a no-op patch, which the
//	                              apiserver accepts, which reads as "mutable" —
//	                              a fixture polluted with the declared value
//	                              silently inverts the whole lane
//	the patch will not build      a row whose probe cannot be constructed is a row
//	                              vouching for nothing
//	the apiserver did not answer  "could not tell" is not "nothing wrong"
//	nothing was examined          vacuity is the failure mode every green check
//	                              that means nothing shares

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/health"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

// ── transport seams ──────────────────────────────────────────────────────────

// appliabilityDryRun server-dry-runs one declared change against the fixture.
//
// ITS OWN SEAM, NOT dryRunPatch's. The two send the same kubectl and mean
// different things: dryRunPatch runs through the `overlay-applied` binding
// against production, this one through `overlay-appliability` against a kind
// fixture. Sharing the var would make a test double installed by one lane's
// tests answer for the other's, which is how two lanes come to be exercising one
// code path and neither notices the other has stopped working.
// `-o json` IS LOad-BEARING, not formatting. An accepted dry run has to be read
// BACK: kubectl exits 0 for a patch that changes nothing — a mistyped key is
// merely a `Warning: unknown field` on stderr and `patched (no change)` on stdout
// — so "accepted" alone cannot tell an appliable field from a probe that landed
// nothing. The returned object is the only evidence of which it was.
//
// `--field-validation=Strict` would be the obvious alternative and does not
// exist: kubectl accepts that flag on apply and create, not on patch. Verified
// against the same v1.34.8 apiserver this lane runs on.
var appliabilityDryRun = func(kind, namespace, name, patch string) (out string, accepted bool) {
	b, err := capability.MustCluster(Extension().MustBinding("overlay-appliability")).Run(
		"-n", namespace, "patch", kind, name, "--dry-run=server", "-o", "json", "-p", patch)
	if err != nil {
		return kubectlprobe.ErrText(err), false
	}
	return string(b), true
}

// appliabilityValidatePayload dry-run-CREATES one self-describing object from a
// row's payload, standalone.
//
// BECAUSE THE WHOLE-SPEC REFUSAL SHORT-CIRCUITS FIELD VALIDATION. On a
// StatefulSet UPDATE the apiserver answers `spec: Forbidden: updates to …` and
// never validates the fields inside, so malformedProbeRefusal — which reads the
// refusal for a `Required value` line — is STRUCTURALLY UNREACHABLE for
// spec.volumeClaimTemplates. Measured: sabotage claimTemplate() to emit a PVC with
// an empty spec and the lane still printed `ok … is CREATE-ONLY, as declared` and
// exited 0. That is the row wired to the orphan delete, so its CREATE-ONLY verdict
// established only "the patch touched sts.spec outside the mutable whitelist" and
// never that the object the MIGRATION WILL CREATE is well-formed.
//
// Creating the entry on its own is where validation is not short-circuited, and it
// is the same doctrine as the rest of this lane: only the apiserver knows. A dry
// run persists nothing, and the capability model classifies `create
// --dry-run=server` as a cluster read for exactly that reason.
var appliabilityDryRunCreate = func(namespace, doc string) (out string, accepted bool) {
	f, err := os.CreateTemp("", "llz-appliability-*.json")
	if err != nil {
		return "could not stage the payload for validation: " + err.Error(), false
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(doc); err != nil {
		f.Close()
		return "could not stage the payload for validation: " + err.Error(), false
	}
	f.Close()
	b, err := capability.MustCluster(Extension().MustBinding("overlay-appliability")).Run(
		"-n", namespace, "create", "--dry-run=server", "-o", "json", "-f", f.Name())
	if err != nil {
		return kubectlprobe.ErrText(err), false
	}
	return string(b), true
}

// payloadObjects returns the self-describing objects a row's patch carries at its
// own Live path — the ones that can be validated standalone. A payload that is not
// a list of apiVersion/kind objects yields none, and the row is unaffected.
func payloadObjects(patch string, f clusterspec.OverlayField) []string {
	var m map[string]any
	if err := json.Unmarshal([]byte(patch), &m); err != nil {
		return nil
	}
	v, found, missed := clusterspec.LiveValue(m, f.Live)
	if missed || !found {
		return nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, e := range list {
		em, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if _, hasKind := em["kind"]; !hasKind {
			continue
		}
		if _, hasAPI := em["apiVersion"]; !hasAPI {
			continue
		}
		if b, err := json.Marshal(em); err == nil {
			out = append(out, string(b))
		}
	}
	return out
}

// appliabilityReadObject fetches the fixture object as JSON.
// IT RETURNS WHAT KUBECTL SAID, and that is not decoration. An unanswerable read
// is where every kubeconfig fault lands — unset, pointing at a file that is not
// there, naming a context that is gone, naming a cluster that is down — and
// without the text all four print the same sentence and name none of them.
// Measured: three of those produced byte-identical output mentioning no
// kubeconfig, no file and no host, while an RBAC denial one arm over was instantly
// readable for the single reason that that arm prints the apiserver's own words.
// This one now does too.
var appliabilityReadObject = func(kind, namespace, name string) (raw []byte, absent, answered bool, detail string) {
	out, verdict, detail := kubectlprobe.ProbeDetail("-n", namespace, "get", kind, name, "-o", "json")
	switch verdict {
	case kubectlprobe.Found:
		return out, false, true, ""
	case kubectlprobe.Absent:
		return nil, true, true, detail
	default:
		return nil, false, false, detail
	}
}

// ── what the apiserver said ──────────────────────────────────────────────────

// observed is the apiserver's verdict on one declared change, before it is
// compared against what the field map claims.
type observed int

const (
	// observedAppliable — the change was accepted, so the field is mutable.
	observedAppliable observed = iota
	// observedCreateOnly — refused, and the refusal is an immutability rejection.
	observedCreateOnly
	// observedRefusedOther — refused for a reason this gate does not classify.
	// Never silently folded into either of the two above: an unclassified
	// rejection means the probe itself may be malformed, and reading it as
	// "create-only" would invent a migration requirement out of a broken patch.
	observedRefusedOther
)

func (o observed) String() string {
	switch o {
	case observedAppliable:
		return "APPLIABLE"
	case observedCreateOnly:
		return "CREATE-ONLY"
	default:
		return "REFUSED (unclassified)"
	}
}

// observeAppliability turns the apiserver's answer into an observed verdict.
// Pure, and it goes through health.IsImmutableFieldRejection so this lane and
// assert-overlay-applied cannot come to disagree about what a Forbidden means.
func observeAppliability(out string, accepted bool) observed {
	switch {
	case accepted:
		return observedAppliable
	// A REFUSAL THAT ALSO REPORTS A MALFORMED PATCH IS NOT A VERDICT ABOUT THE
	// FIELD, where the apiserver reports both in one message — a multi-error
	// validation failure whose lines include a `Required value` alongside an
	// immutability marker. The marker wins a plain substring match, so a
	// structurally broken probe would grade "CREATE-ONLY, as declared" and exit 0.
	//
	// THIS DOES NOT COVER A WHOLE-SPEC REFUSAL, and it is worth being exact because
	// an earlier version of this comment cited a captured message
	// (`volumes[0].claimName: Required value`) that was taken against a differently
	// shaped probe and no longer describes anything the lane sends. On a StatefulSet
	// UPDATE the `spec: Forbidden` answer SHORT-CIRCUITS field validation: the
	// apiserver never validates the fields inside, so no Required-value line is ever
	// emitted and this check is structurally unreachable for
	// spec.volumeClaimTemplates — the row wired to the orphan delete.
	// appliabilityDryRunCreate is what covers that row, by creating the payload
	// standalone where validation does run. This arm remains for the Kinds and
	// paths where the apiserver does answer with both.
	case malformedProbeRefusal(out):
		return observedRefusedOther
	case health.IsImmutableFieldRejection(out):
		return observedCreateOnly
	default:
		return observedRefusedOther
	}
}

// malformedFieldErrors are apimachinery field.Error kinds that can only mean the
// REQUEST was wrong.
//
// DELIBERATELY NOT "invalid value". A genuine narrowed immutability rejection is
// spelled `spec.foo: Invalid value: "x": field is immutable`, so treating that as
// malformedness would reclassify the very rejections this gate exists to detect.
// These three never describe an immutable field: a required key that is missing,
// an enum the request got wrong, and a duplicate the request supplied twice are
// all statements about the patch.
// SPECULATIVE ENTRIES ARE MARKED, for the reason health's immutableMarkers now
// records: "required value" is the only one a captured refusal carries, and the
// other two were dead — deletable with the whole tree green. They are kept because
// an enum violation and a duplicate are both unambiguously statements about the
// REQUEST, and a future Kind may answer that way; they are marked so a later round
// cannot "confirm" them from a fixture written to match them. That shape was added
// to kubectlprobe and to health in this same PR and not applied here, which is how
// two dead entries shipped one file from the test that would have caught them.
var malformedFieldErrors = []string{
	"required value",    // captured: a PVC created without accessModes or storage
	"unsupported value", // SPECULATIVE — an enum violation; no captured refusal uses it
	"duplicate value",   // SPECULATIVE — a duplicated key; no captured refusal uses it
}

// speculativeMalformedErrors names the entries no captured refusal carries, in ONE
// place so the test and the list cannot come to disagree about which are pinned.
var speculativeMalformedErrors = map[string]bool{
	"unsupported value": true,
	"duplicate value":   true,
}

// malformedProbeRefusal reports whether a refusal says, anywhere in it, that the
// probe itself was malformed.
func malformedProbeRefusal(out string) bool {
	low := strings.ToLower(out)
	for _, m := range malformedFieldErrors {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// ── grading it against the field map ─────────────────────────────────────────

type appliabilityVerdict struct {
	Field clusterspec.OverlayField
	// OK is whether the map's claim survived contact with the apiserver.
	OK bool
	// Skipped marks a row graded WITHOUT asking the apiserver. It is reported
	// separately from OK because the two say opposite things about evidence: an OK
	// row was probed and agreed, a Skipped row was never probed at all. Folding the
	// second into the first is what let a hand-set Prior turn a row off under a
	// green tick.
	Skipped bool
	Detail  string
}

// gradeAppliability compares what the apiserver did against what the field map
// declares. Pure — this is the whole decision, and the reason it takes `observed`
// rather than raw kubectl output is so the test suite can drive every arm without
// a cluster.
//
// BOTH DIRECTIONS FAIL, and the second one is not pedantry. A field the map calls
// create-only but the apiserver accepts carries a Migration that will delete and
// recreate a live object to land a value a patch would have landed — a
// destructive repair for a problem that does not exist. `llz ci converge` applies
// Auto migrations unattended, so an over-declared CreateOnly is a StatefulSet
// recreate nobody asked for.
//
// UNATTENDED, NOT "ON A SCHEDULE" — the earlier wording sent the reader to the
// wrong lane. The migration block is ScopePlatform-gated, and the only cron an
// instance carries is `converge --scope=apps`. It runs at BOOTSTRAP and on a
// dispatched platform-scope health run: nobody is watching either, which is what
// makes it unattended, but no timer fires it.
func gradeAppliability(f clusterspec.OverlayField, o observed, detail string) appliabilityVerdict {
	path := clusterspec.OverlayFieldPath(f)
	switch {
	case o == observedRefusedOther:
		// THE APISERVER'S OWN WORDS, because an earlier version told the reader to
		// "read it before concluding anything" and then printed nothing to read. This
		// arm is also where a TRANSPORT fault lands — connection refused, a timeout, a
		// 429, an RBAC denial — and those are indistinguishable from a validation
		// refusal without the text. The old wording asserted the likely cause was a
		// malformed Patch, which for StatefulSet is close to unreachable: its refusal
		// is a whole-spec message, so a malformed spec key trips the IMMUTABILITY
		// marker instead. Say what happened and let the reader judge.
		return appliabilityVerdict{Field: f, OK: false, Detail: fmt.Sprintf(
			"%s: the apiserver did not accept the probe, and the refusal is not one this gate classifies "+
				"as immutability. It may be a transport or permission fault rather than a verdict about "+
				"the field. The apiserver said: %s", path, health.RefusalText(detail))}

	case o == observedCreateOnly && !f.CreateOnly:
		return appliabilityVerdict{Field: f, OK: false, Detail: fmt.Sprintf(
			"%s is declared MUTABLE in clusterspec.OverlayFields(), but the apiserver fixes it at CREATE time. "+
				"On every cluster whose %s %s/%s predates this value, Argo will dry-run-apply it, be refused, "+
				"produce no diff, and report Synced — and the refusal is per OBJECT, so it will discard every "+
				"other change to that object too. Set CreateOnly:true and register a brownfield migration",
			path, f.Kind, f.Namespace, f.Name)}

	case o == observedAppliable && f.CreateOnly:
		return appliabilityVerdict{Field: f, OK: false, Detail: fmt.Sprintf(
			"%s is declared CreateOnly and names migration %q, but the apiserver ACCEPTED the change to an "+
				"existing %s. A migration recreates a live object to land a value an ordinary patch would "+
				"land, and `llz ci converge` applies Auto migrations unattended — so this declaration is a "+
				"destructive repair for a problem that does not exist, applied unattended. Drop CreateOnly and the "+
				"Migration, or correct the row's Patch if it is not sending what you think it is",
			path, f.Migration, f.Kind)}

	case o == observedCreateOnly:
		return appliabilityVerdict{Field: f, OK: true, Detail: fmt.Sprintf(
			"%s is CREATE-ONLY, as declared — migration %s is what lands it on a cluster that predates it",
			path, f.Migration)}

	default:
		return appliabilityVerdict{Field: f, OK: true, Detail: fmt.Sprintf(
			"%s is APPLIABLE in place, as declared — no migration needed", path)}
	}
}

// ── the lane ─────────────────────────────────────────────────────────────────

// overlayRawValuesFor is the seam the lane reads the declared overlay through.
// A var for the same reason overlayFieldsFor is one: without it the "no _rawValues
// for this app" arm is unreachable from a test, and an unreachable fail-closed arm
// is one that can be flipped to OK:true with the suite green — which four of them
// could be.
var overlayRawValuesFor = clusterspec.AplAppRawValues

func assertOverlayAppliability() error {
	// THROUGH THE SEAMS, NOT THE PACKAGE FUNCTIONS. Reading
	// clusterspec.OverlayFields() directly pinned every lane-level test to the five
	// real loki rows, so no test could construct a row that reaches the arms below —
	// a missing app, an undecodable object, an unbuildable patch, a Live path that
	// does not resolve. All four were deletable with the whole suite green.
	fields := overlayFieldsFor()
	raw := overlayRawValuesFor()

	var verdicts []appliabilityVerdict
	examined := 0
	for _, f := range fields {
		rv, ok := raw[f.App]
		if !ok {
			verdicts = append(verdicts, appliabilityVerdict{Field: f, OK: false,
				Detail: "the overlay declares no _rawValues for app " + f.App +
					" — this row of the field map is checking a value nothing asserts"})
			continue
		}
		declared, ok := clusterspec.RawValue(rv, f.Value...)
		if !ok {
			verdicts = append(verdicts, appliabilityVerdict{Field: f, OK: false,
				Detail: "the overlay declares no " + clusterspec.OverlayFieldPath(f) +
					" — this row of the field map is checking a value nothing asserts"})
			continue
		}

		name := appliabilityFixtureName(f)
		rawObj, absent, answered, detail := appliabilityReadObject(f.Kind, f.Namespace, name)
		if !answered {
			verdicts = append(verdicts, appliabilityVerdict{Field: f, OK: false, Detail: fmt.Sprintf(
				"could not read %s %s/%s — 'could not tell' is not 'the declaration is right'. "+
					"kubectl said: %s", f.Kind, f.Namespace, name, health.RefusalText(detail))})
			continue
		}
		if absent {
			// ABSENT IS FATAL HERE, WHICH IS THE OPPOSITE OF THE RUNTIME LANE. There,
			// a missing object means the instance does not run that app. Here the
			// object is one this lane's own fixtures were supposed to create seconds
			// ago, so missing means the fixture step did not run or did not cover this
			// row — and every probe against it would 404 into an unexamined pass.
			verdicts = append(verdicts, appliabilityVerdict{Field: f, OK: false, Detail: fmt.Sprintf(
				"%s %s/%s does not exist. This lane probes fixtures it creates itself, so an absent object "+
					"means `llz ci assert-overlay-appliability --emit-fixtures --out F && kubectl apply -f F` "+
					"did not run, or does not cover this row. kubectl said: %s",
				f.Kind, f.Namespace, name, health.RefusalText(detail))})
			continue
		}
		var live map[string]any
		if err := json.Unmarshal(rawObj, &live); err != nil {
			verdicts = append(verdicts, appliabilityVerdict{Field: f, OK: false,
				Detail: fmt.Sprintf("%s %s/%s did not decode: %v", f.Kind, f.Namespace, name, err)})
			continue
		}

		// THE FIXTURE MUST NOT ALREADY CARRY THE VALUE, and this is the arm that
		// keeps the lane honest rather than merely tidy. A probe sent against an
		// object that already holds the declared value is a no-op patch; the
		// apiserver accepts a no-op happily, that reads as observedAppliable, and
		// every CreateOnly row in the map would be reported as an over-declaration.
		// One polluted fixture inverts the entire lane, so the pollution has to be
		// the failure rather than the input.
		match, delivered, readable := clusterspec.OverlayFieldDelivered(f, declared, live)
		if !readable {
			verdicts = append(verdicts, appliabilityVerdict{Field: f, OK: false, Detail: fmt.Sprintf(
				"%s does not resolve on %s %s/%s — the row points at something the fixture does not have, "+
					"so it covers nothing", strings.Join(f.Live, "."), f.Kind, f.Namespace, name)})
			continue
		}
		if match {
			// TWO CAUSES, AND THEY ARE OPPOSITE. The fixture carries the declared value
			// either because the row DECLARES THE CHART DEFAULT — Prior == declared, so a
			// brownfield object is already correct and there is no transition any cluster
			// has to make — or because the fixture is polluted, which turns the probe into
			// a no-op patch the apiserver accepts and inverts this lane's whole verdict.
			// The row's own Prior tells them apart, so the benign case does not have to be
			// paid for by dropping the guard that catches the dangerous one.
			if f.Prior != nil && clusterspec.OverlayScalarEqual(f.Prior, declared) {
				// A CreateOnly ROW MAY NOT TAKE THIS EXIT. Its claim is that the apiserver
				// refuses the change on an existing object, and that claim is wired to a
				// migration that DELETES a live object to land the value. A row that declares
				// its own Prior equal to its own declared value asks the apiserver nothing —
				// so the destructive half ships on a hand-set string, which is the exact
				// shape of trust this whole lane was built to remove.
				if f.CreateOnly {
					verdicts = append(verdicts, appliabilityVerdict{Field: f, OK: false, Detail: fmt.Sprintf(
						"%s declares Prior %v and the overlay declares the same value, so there is no "+
							"transition to probe — but the row also declares CreateOnly and names migration "+
							"%s, which deletes and recreates a live %s. That claim can never be tested while "+
							"Prior equals the declared value: the lane would grade it green having asked the "+
							"apiserver nothing. Correct Prior to what a pre-overlay object actually carries, "+
							"or drop CreateOnly and the Migration",
						clusterspec.OverlayFieldPath(f), declared, f.Migration, f.Kind)})
					continue
				}
				// SKIPPED, NOT ok. This arm is legitimate — a row declaring the chart default
				// describes a cluster that is already correct — but it is graded WITHOUT the
				// apiserver, and printing it as `ok` beside rows that were actually probed is
				// how a wrong Prior turns a row off under a green tick. Measured: setting one
				// row's Prior to its declared value greened the lane AND left `go test ./...`
				// green across the whole tree, while the report asserted in prose that no
				// cluster could ever need the field. It is now counted and named as evidence
				// nobody gathered.
				verdicts = append(verdicts, appliabilityVerdict{Field: f, Skipped: true, OK: true, Detail: fmt.Sprintf(
					"%s declares %v, which is already what a pre-overlay %s carries per the row's own Prior "+
						"— NO TRANSITION to test, so this row was NOT probed. That is a claim about the "+
						"chart default, not evidence from the apiserver; "+
						"TestEveryScalarRowsPriorIsWhatTheRecordedBrownfieldObjectCarries is what holds "+
						"Prior honest", clusterspec.OverlayFieldPath(f), declared, f.Kind)})
				continue
			}
			verdicts = append(verdicts, appliabilityVerdict{Field: f, OK: false, Detail: fmt.Sprintf(
				"the fixture %s %s/%s ALREADY carries %s = %v, and the row's Prior (%v) is not that value. "+
					"The probe would be a no-op patch, which the apiserver accepts, which this lane would "+
					"read as 'mutable' — so a pre-overlay fixture that is not pre-overlay silently inverts "+
					"the verdict. The fixture is GENERATED FROM Prior, so the thing to correct is the row's "+
					"Prior (or the object the lane was pointed at), not this check",
				f.Kind, f.Namespace, name, clusterspec.OverlayFieldPath(f), delivered, f.Prior)})
			continue
		}

		// AND IT MUST BE IN THE PRE-OVERLAY SHAPE, not merely "not already correct".
		// The arm above only rules out a fixture that already carries the DECLARED value.
		// A fixture carrying NEITHER — the field stripped out entirely — passes it and
		// then probes absent→set instead of Prior→declared. Measured: stripping the
		// container's resources took the report from "4 probed / 1 skipped" to "All 5
		// probed field(s) match", exit 0. The DEGRADED fixture claims BETTER coverage,
		// which is the direction that gets a broken fixture kept. On a CreateOnly row the
		// misgrade prints "Drop CreateOnly and the Migration" — advice to delete the only
		// thing that lands the value on a brownfield cluster.
		if f.Prior != nil {
			onObj := clusterspec.PriorOnObject(f, live)
			if !clusterspec.OverlayScalarEqual(onObj, f.Prior) {
				verdicts = append(verdicts, appliabilityVerdict{Field: f, OK: false, Detail: fmt.Sprintf(
					"the fixture %s %s/%s carries %v at %s, but the row's Prior says a pre-overlay object "+
						"carries %v. The probe would test a transition no cluster performs — and a fixture "+
						"MISSING the field reports as better coverage than one that has it, so this cannot "+
						"be left to a reader to notice",
					f.Kind, f.Namespace, name, onObj, clusterspec.OverlayFieldPath(f), f.Prior)})
				continue
			}
		}

		patch, err := f.Patch(rv)
		if err != nil {
			verdicts = append(verdicts, appliabilityVerdict{Field: f, OK: false,
				Detail: "could not build the appliability probe from the declared values: " + err.Error()})
			continue
		}
		// BEFORE THE PROBE, because a patch that does not target this row's field makes
		// every possible verdict evidence about something else. Cheap, offline, and the
		// only check available on the refused arm — there is no returned object to read
		// back when the apiserver says no.
		if err := patchTargetsLive(patch, f, declared); err != nil {
			verdicts = append(verdicts, appliabilityVerdict{Field: f, OK: false,
				Detail: clusterspec.OverlayFieldPath(f) + ": " + err.Error()})
			continue
		}
		// VALIDATE A SELF-DESCRIBING PAYLOAD BEFORE TRUSTING THE REFUSAL, because the
		// refusal cannot speak for it — see appliabilityDryRunCreate.
		if bad := validatePayload(f, patch, name); bad != "" {
			verdicts = append(verdicts, appliabilityVerdict{Field: f, OK: false, Detail: bad})
			continue
		}
		examined++
		out, accepted := appliabilityDryRun(f.Kind, f.Namespace, name, patch)
		// AN ACCEPTED PROBE HAS TO PROVE IT LANDED. Everything else about this lane
		// treats exit 0 as "the cluster would take it"; kubectl also exits 0 for a
		// patch that changes nothing at all.
		if accepted {
			if err := probeLandedTheValue(out, f, declared); err != nil {
				verdicts = append(verdicts, appliabilityVerdict{Field: f, OK: false,
					Detail: clusterspec.OverlayFieldPath(f) + ": " + err.Error()})
				continue
			}
		}
		verdicts = append(verdicts, gradeAppliability(f, observeAppliability(out, accepted), out))
	}

	return reportAppliability(verdicts, examined)
}

// validatePayload returns a failure detail if any object the row's patch carries
// at its own path is refused when created standalone, or "" if there is nothing to
// validate or everything validated.
func validatePayload(f clusterspec.OverlayField, patch, name string) string {
	for _, obj := range payloadObjects(patch, f) {
		out, accepted := appliabilityDryRunCreate(f.Namespace, obj)
		if accepted {
			continue
		}
		return fmt.Sprintf("%s: the row builds a payload the apiserver will not accept, so its "+
			"CREATE-ONLY verdict cannot be trusted — the whole-spec refusal short-circuits field "+
			"validation, so a malformed entry reads exactly like an immutable one, and the migration "+
			"that runs on this row's word would CREATE this object. The apiserver said: %s",
			clusterspec.OverlayFieldPath(f), health.RefusalText(out))
	}
	return ""
}

func reportAppliability(verdicts []appliabilityVerdict, examined int) error {
	failed, skipped := 0, 0
	fmt.Printf("Overlay appliability — what the apiserver says about each declared field, "+
		"against an object in its pre-overlay shape (%d row(s)):\n", len(verdicts))
	for _, v := range verdicts {
		mark := "ok  "
		switch {
		case !v.OK:
			mark = "FAIL"
			failed++
		case v.Skipped:
			// A THIRD MARK, BECAUSE THERE ARE THREE OUTCOMES. `ok` and `skip` differ in
			// what was ASKED, not in what was found, and a reader scanning a green report
			// has no other way to see that a row was never put to the apiserver.
			mark = "skip"
			skipped++
		}
		fmt.Printf("  %s %s\n", mark, v.Detail)
	}

	// VACUITY IS A FAILURE, NOT A PASS. Zero rows examined means the field map is
	// empty, or every row bailed before its probe — either way this lane proved
	// nothing, and reporting success would launder that into a green check.
	if examined == 0 {
		return fmt.Errorf("no overlay field was actually probed (%d row(s) considered, %d skipped without "+
			"asking the apiserver) — this lane examined nothing, which is not the same as finding "+
			"nothing wrong", len(verdicts), skipped)
	}
	if failed > 0 {
		// COUNTED AGAINST THE ROWS CONSIDERED, NOT THE ROWS PROBED. A row that bailed
		// before its probe — an absent object, a fixture already carrying the value, a
		// patch that would not build — is counted in `failed` but never reached
		// `examined`, so reporting it as a fraction of `examined` could print "3 of 1
		// probed" and would attribute a missing declaration to a map/apiserver
		// disagreement it never got far enough to have.
		return fmt.Errorf("%d of %d overlay field(s) did not come back as clusterspec.OverlayFields() "+
			"declares them (%d were probed against the apiserver; the rest failed before their probe)",
			failed, len(verdicts), examined)
	}
	// THE SKIPPED COUNT IS PART OF THE PASS, not a footnote under it. "All 4 probed
	// field(s) match" was printed for a FIVE-row map and read as full coverage; the
	// difference between the two numbers is the whole question of how much this run
	// actually asked.
	fmt.Printf("\nAll %d probed field(s) match their declared appliability", examined)
	if skipped > 0 {
		fmt.Printf("; %d of %d row(s) were NOT probed (see the skip lines above)", skipped, len(verdicts))
	}
	fmt.Println(".")
	return nil
}

// ── coupling the probe to the row it claims to test ──────────────────────────

// patchTargetsLive is clusterspec.PatchTargetsField, kept as a named call here
// because this lane's report reads better with the row path prefixed and because
// the check's rationale belongs beside the arm it protects.
//
// IT LIVES IN clusterspec NOW BECAUSE TWO HALVES ASK IT. This lane asks at PR
// time; brownfield.createOnlyStillHolds asks the same question of a LIVE object
// immediately before an orphan delete. It had only this half, and a patch aimed at
// an unrelated spec key drew the byte-identical whole-spec refusal and CLEARED the
// delete — the exact false green this function was written to close, reproduced on
// the runtime side because the two sides did not share the check.
func patchTargetsLive(patch string, f clusterspec.OverlayField, declared any) error {
	return clusterspec.PatchTargetsField(patch, f, declared)
}

// probeLandedTheValue reports whether an ACCEPTED dry run actually delivered the
// declared value, by reading the object the apiserver returned.
//
// "ACCEPTED" IS NOT "APPLIED". kubectl exits 0 for a patch that changes nothing:
// a mistyped key is a `Warning: unknown field` on stderr and
// `statefulset.apps/… patched (no change)` on stdout, exit 0. Measured: retyping
// a row's key to `resourcesTYPO` had the lane print all four resource rows as
// "APPLIABLE in place, as declared" and exit 0 — a row vouching for nothing,
// which is the class the header claims to fail closed on and did not cover.
//
// Reading the RESULT closes it: the returned object either carries the declared
// value at the row's Live path or it does not, and a no-op patch returns the
// object unchanged with the prior value still in place.
func probeLandedTheValue(out string, f clusterspec.OverlayField, declared any) error {
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		return fmt.Errorf("the apiserver accepted the probe but its response did not decode as an "+
			"object, so whether the value landed cannot be established: %w", err)
	}
	match, delivered, readable := clusterspec.OverlayFieldDelivered(f, declared, result)
	if !readable {
		return fmt.Errorf("the apiserver accepted the probe, but %s does not resolve on the object it "+
			"returned — the row cannot confirm its own patch landed", strings.Join(f.Live, "."))
	}
	if !match {
		return fmt.Errorf("the apiserver ACCEPTED the probe and the value did NOT land: %s is %s on the "+
			"returned object, not the declared %v. An accepted patch that changes nothing exits 0 (kubectl "+
			"prints \"patched (no change)\"), so this row would otherwise have reported APPLIABLE having "+
			"tested nothing. Check the row's Patch builds the key the chart actually uses",
			strings.Join(f.Live, "."), delivered, declared)
	}
	return nil
}
