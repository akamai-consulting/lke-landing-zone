package assertplatform

// overlayapplied.go implements `llz ci assert-overlay-applied` — did what the
// apl-overlay DECLARES actually reach the cluster, and if not, can it?
//
// THE QUESTION IT ASKS THAT NOTHING ELSE DOES. Three checks were green through
// the whole 16-day Loki outage: the overlay file held the right value, the
// rendered Helm values held the right value, and Argo CD said Synced. Each is a
// statement about a producer. The only check with the power to catch it reads the
// value back out of the CLUSTER — and then asks the second question, which is the
// one that made the failure invisible: not "is the object right" but "COULD it be
// right", because Argo computes its diff by dry-run-applying the desired state,
// so an apply the API server refuses produces no diff, and no diff reads as
// Synced.
//
// SO A DIFFERENCE IS NEVER THE END OF THE ANSWER. When declared and delivered
// disagree, this gate sends the same change to the apiserver as a SERVER DRY RUN
// and reports which of two very different failures it is:
//
//	UNAPPLIABLE          the field is fixed at create time; Argo will never land
//	                     it, and it silently blocks every other change to that
//	                     object. Needs a brownfield migration, named in the row.
//	APPLIABLE, NOT APPLIED  the cluster would take it; something upstream has not
//	                     delivered it. Ordinary drift, or the contagion of an
//	                     unappliable sibling on the same object.
//
// A DRY RUN IS A READ, and the capability model says so
// (capability.Permits: `patch --dry-run=server` passes a cluster-read handle).
// Nothing here writes; the lane is safe to point at production, which matters
// because production is the only place this class of failure exists.
//
// FAIL-CLOSED, THREE WAYS. An unreadable apiserver fails. A field this gate
// cannot locate in a live object fails — a chart rename must surface as a red
// lane naming the probe, not as a check that quietly examined nothing. And if no
// field was checked at all, the lane fails on vacuity rather than reporting a
// success it did not earn.
//
// WHAT IT DOES NOT COVER, said plainly: only the paths in
// clusterspec.OverlayFields(). Every other declared path is printed as UNCHECKED
// with its count. Keeping that number in the report is deliberate — a gate whose
// coverage is invisible is read as total.

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/health"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

// ── transport seams ──────────────────────────────────────────────────────────

// readLiveObject fetches one object as JSON. answered=false is "the apiserver did
// not tell us", which is distinct from absent and graded differently.
var readLiveObject = func(kind, namespace, name string) (raw []byte, absent, answered bool) {
	out, verdict := kubectlprobe.Probe("-n", namespace, "get", kind, name, "-o", "json")
	switch verdict {
	case kubectlprobe.Found:
		return out, false, true
	case kubectlprobe.Absent:
		return nil, true, true
	default:
		return nil, false, false
	}
}

// dryRunPatch server-dry-runs a strategic-merge patch and returns the apiserver's
// combined output plus whether it was accepted.
//
// STRATEGIC, NOT JSON-MERGE. The containers list is keyed by name
// (patchMergeKey), so a strategic patch names one container and leaves the rest
// alone; a JSON merge patch would REPLACE the whole list with a one-element
// object missing every required field, and the API server would reject it for
// being malformed — a rejection indistinguishable, from here, from the
// immutability rejection this probe exists to detect.
// THROUGH THE DECLARED CLUSTER-READ HANDLE, so the justification in this
// extension's declaration is enforced rather than merely asserted. That comment
// says `capability.Permits` classifies `patch --dry-run=server` as a read — which
// was true of the classifier and irrelevant to this call, because it went through
// the general exec seam and was never shown to Permits. Dropping the
// `--dry-run=server` later would then have mutated a live object out of a lane
// declared read-only; now it is refused at the handle.
var dryRunPatch = func(kind, namespace, name, patch string) (out string, ok bool) {
	b, err := capability.MustCluster(Extension().MustBinding("overlay-applied")).Run(
		"-n", namespace, "patch", kind, name, "--dry-run=server", "-p", patch)
	if err != nil {
		return kubectlprobe.ErrText(err), false
	}
	return string(b), true
}

// ownerExists reports whether the Argo Application that declares an object is
// present, and whether the cluster answered at all.
//
// THE SECOND RETURN IS NOT OPTIONAL HERE, and reading it as "absent" was a way
// for this gate to go green on the worst state it can be in. kubectlprobe.Exists
// collapses unanswerable into false — its own doc says a caller whose absent
// branch SKIPS work must use ExistsOK — and this caller's absent branch does
// exactly that: object gone plus one failed Application read made every row
// stateObjectAbsent, which with a single mapped object means examined == 0, which
// is the vacuity pass. A deleted-and-never-recreated loki-ingester would have
// reported clean.
var ownerExists = func(f clusterspec.OverlayField) (exists, answered bool) {
	if f.OwnerApp == "" {
		return false, true
	}
	return kubectlprobe.ExistsOK("-n", clusterspec.ArgoNamespace, "get", "application.argoproj.io", f.OwnerApp)
}

// ── the verdicts ─────────────────────────────────────────────────────────────

type overlayState int

const (
	stateDelivered overlayState = iota
	stateUnappliable
	stateNotApplied
	stateRefused
	stateUnreadable
	stateObjectAbsent
)

// Fatal reports whether a state fails the lane. An absent object does not: an app
// that is not deployed on this instance has nothing to have delivered, and the
// vacuity guard is what stops a fleet of absent objects reading as a pass.
func (s overlayState) Fatal() bool { return s != stateDelivered && s != stateObjectAbsent }

func (s overlayState) String() string {
	switch s {
	case stateDelivered:
		return "DELIVERED"
	case stateUnappliable:
		return "UNAPPLIABLE"
	case stateNotApplied:
		return "APPLIABLE, NOT APPLIED"
	case stateRefused:
		return "REFUSED"
	case stateUnreadable:
		return "UNREADABLE"
	default:
		return "OBJECT ABSENT"
	}
}

type overlayVerdict struct {
	Field  clusterspec.OverlayField
	State  overlayState
	Detail string
}

// classifyRefusal turns the apiserver's answer to the dry run into a verdict.
// Pure, and the single place the immutability distinction is made — through
// health.IsImmutableFieldRejection, so this gate and the brownfield migration
// precondition cannot disagree about what a Forbidden means.
func classifyRefusal(f clusterspec.OverlayField, out string, accepted bool) overlayVerdict {
	switch {
	case accepted:
		return overlayVerdict{Field: f, State: stateNotApplied, Detail: "the cluster would accept this change — " +
			"it has simply not been delivered. Either the sync has not reached it, or another field on this same " +
			"object is unappliable and Argo's per-object diff is discarding this one with it"}
	case health.IsImmutableFieldRejection(out):
		detail := "the API server fixes this field at CREATE time, so Argo can never apply it to an object that " +
			"already exists — and because the diff is computed per object, this rejection silently discards every " +
			"other change to it. The apiserver said: " + health.FirstLine(out)
		if f.Migration != "" {
			detail += "\n      Remedy: llz ci brownfield-migrate --id " + f.Migration + " --yes  " +
				"(llz ci brownfield-migrations shows where it stands first)"
		}
		return overlayVerdict{Field: f, State: stateUnappliable, Detail: detail}
	default:
		return overlayVerdict{Field: f, State: stateRefused, Detail: "the cluster refused the change for a reason " +
			"this gate does not classify as immutability — read it before assuming either: " + health.FirstLine(out)}
	}
}

// ── the lane ─────────────────────────────────────────────────────────────────

func assertOverlayApplied() error {
	fields := clusterspec.OverlayFields()
	raw := clusterspec.AplAppRawValues()

	var verdicts []overlayVerdict
	examined := 0
	for _, f := range fields {
		rv, ok := raw[f.App]
		if !ok {
			// The table names an app the overlay does not carry. That is a coupling
			// break, not a cluster finding, and it must be loud: the row it belongs to
			// has been vouching for nothing.
			verdicts = append(verdicts, overlayVerdict{Field: f, State: stateRefused,
				Detail: "the overlay declares no _rawValues for app " + f.App + " — this row of the field map " +
					"is checking a value nothing asserts"})
			continue
		}
		declared, ok := clusterspec.RawValue(rv, f.Value...)
		if !ok {
			verdicts = append(verdicts, overlayVerdict{Field: f, State: stateRefused,
				Detail: "the overlay declares no " + clusterspec.OverlayFieldPath(f) + " — this row of the field " +
					"map is checking a value nothing asserts"})
			continue
		}

		rawObj, absent, answered := readLiveObject(f.Kind, f.Namespace, f.Name)
		if !answered {
			verdicts = append(verdicts, overlayVerdict{Field: f, State: stateUnreadable,
				Detail: fmt.Sprintf("could not read %s %s/%s — this is 'could not tell', not 'nothing wrong'",
					f.Kind, f.Namespace, f.Name)})
			continue
		}
		if absent {
			// ABSENT HAS TWO CAUSES AND THEY ARE OPPOSITE. An instance that does not run
			// the app has no object and nothing is wrong; a migration that deleted the
			// object and never saw it recreated ALSO has no object, and that is the
			// worst state this gate can be in — the workload running with no controller.
			// The owning Application tells them apart: if it exists, something is
			// supposed to be maintaining this object.
			owned, answered := ownerExists(f)
			if !answered {
				verdicts = append(verdicts, overlayVerdict{Field: f, State: stateUnreadable,
					Detail: fmt.Sprintf("%s %s/%s does not exist, and whether its Application (%s) does "+
						"could not be determined — 'could not tell' must not resolve to 'this instance does "+
						"not run it', because the other reading is a recreate that never completed",
						f.Kind, f.Namespace, f.Name, f.OwnerApp)})
				continue
			}
			if owned {
				verdicts = append(verdicts, overlayVerdict{Field: f, State: stateRefused,
					Detail: fmt.Sprintf("%s %s/%s does not exist, but its Application (%s) does — an object "+
						"its owner declares is missing. If a brownfield migration deleted it, the recreate "+
						"never completed and the workload is running with no controller",
						f.Kind, f.Namespace, f.Name, f.OwnerApp)})
				continue
			}
			verdicts = append(verdicts, overlayVerdict{Field: f, State: stateObjectAbsent,
				Detail: fmt.Sprintf("%s %s/%s does not exist here, and neither does its Application (%s) — "+
					"this instance does not run it", f.Kind, f.Namespace, f.Name, f.OwnerApp)})
			continue
		}
		var live map[string]any
		if err := json.Unmarshal(rawObj, &live); err != nil {
			verdicts = append(verdicts, overlayVerdict{Field: f, State: stateUnreadable,
				Detail: fmt.Sprintf("%s %s/%s did not decode: %v", f.Kind, f.Namespace, f.Name, err)})
			continue
		}

		examined++
		match, delivered, readable := clusterspec.OverlayFieldDelivered(f, declared, live)
		if !readable {
			verdicts = append(verdicts, overlayVerdict{Field: f, State: stateUnreadable,
				Detail: fmt.Sprintf("%s does not resolve on the live %s %s/%s — the chart has moved or renamed "+
					"what this row points at, and until the row is corrected the gate covers nothing here",
					strings.Join(f.Live, "."), f.Kind, f.Namespace, f.Name)})
			continue
		}
		if match {
			verdicts = append(verdicts, overlayVerdict{Field: f, State: stateDelivered,
				Detail: fmt.Sprintf("declared %v, delivered %s", declared, delivered)})
			continue
		}

		patch, err := f.Patch(rv)
		if err != nil {
			verdicts = append(verdicts, overlayVerdict{Field: f, State: stateRefused,
				Detail: "could not build the appliability probe from the declared values: " + err.Error()})
			continue
		}
		out, accepted := dryRunPatch(f.Kind, f.Namespace, f.Name, patch)
		v := classifyRefusal(f, out, accepted)
		v.Detail = fmt.Sprintf("declared %v, delivered %s. ", declared, delivered) + v.Detail
		verdicts = append(verdicts, v)
	}

	return reportOverlay(verdicts, uncheckedPaths(fields), clusterspec.UnmappedOverlayPaths(), examined)
}

// uncheckedPaths is every declared overlay leaf with no row in the field map.
// Pure, and printed on every run: a gate whose coverage is invisible gets read as
// total coverage, which is how a partial check becomes a false assurance.
func uncheckedPaths(fields []clusterspec.OverlayField) []string {
	mapped := map[string]bool{}
	for _, f := range fields {
		mapped[clusterspec.OverlayFieldPath(f)] = true
	}
	var out []string
	for _, p := range clusterspec.DeclaredOverlayPaths() {
		if !mapped[p] {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func reportOverlay(verdicts []overlayVerdict, unchecked, undecided []string, examined int) error {
	fatal, absent := 0, map[string]bool{}
	for _, v := range verdicts {
		if v.State == stateObjectAbsent {
			absent[v.Field.Kind+"/"+v.Field.Namespace+"/"+v.Field.Name] = true
		}
		marker := "✓"
		if v.State.Fatal() {
			marker = "✗"
			fatal++
		}
		fmt.Printf("  %s %-40s %-22s %s\n", marker, clusterspec.OverlayFieldPath(v.Field), v.State, v.Detail)
		if v.Field.Why != "" && v.State.Fatal() {
			fmt.Printf("      why the overlay asserts it: %s\n", v.Field.Why)
		}
	}
	if len(unchecked) > 0 {
		exempt := clusterspec.OverlayUnmapped()
		fmt.Printf("\n%d declared overlay path(s) are UNCHECKED — no row in the field map maps them to a live "+
			"field, so this gate says nothing about them:\n", len(unchecked))
		for _, p := range unchecked {
			fmt.Printf("  ? %s\n      %s\n", p, health.FirstLine(exempt[p]))
		}
	}
	// An unchecked path someone DECIDED about is a coverage note. One nobody
	// decided about is a value asserted onto a real channel that nothing reads back
	// — the exact shape appvalues.yaml's header forbids — and the PR-time guard that
	// should have caught it does not run on an instance's released binary.
	if len(undecided) > 0 {
		fmt.Fprintf(os.Stderr, "::error::%d declared overlay path(s) reach no gate and carry no reason for "+
			"it: %s. Add a row to clusterspec.OverlayFields() or a reason to OverlayUnmapped()\n",
			len(undecided), strings.Join(undecided, ", "))
		return fmt.Errorf("%d overlay path(s) neither checked nor exempted", len(undecided))
	}
	// A FATAL VERDICT OUTRANKS THE VACUITY BRANCHES. A row that names a path the
	// overlay does not declare is REFUSED before any object is read, so it never
	// increments `examined` — and the absent-object pass below would then return
	// nil with that finding printed and discarded. Checked first, so no branch can
	// swallow a verdict it did not consider.
	if refused := countState(verdicts, stateRefused); refused > 0 && examined == 0 {
		fmt.Fprintf(os.Stderr, "::error::%d overlay row(s) could not be evaluated at all — the field map "+
			"and the overlay disagree, so this gate is checking values nothing asserts\n", refused)
		return fmt.Errorf("%d overlay row(s) are not evaluable", refused)
	}
	if examined == 0 {
		// NOTHING TO CHECK IS NOT THE SAME AS COULD NOT CHECK, and the difference
		// decides whether a gating lane can ever be green. Every mapped row today
		// points at one StatefulSet, so on an instance that does not run the
		// observability component this branch is the whole run — and failing it would
		// be the "gate nobody can turn green" the harbor exemption argues against, on
		// a lane the suite runs for everyone.
		//
		// So: every object absent and none unreadable is a PASS with a loud line. An
		// unreadable object is still a failure, because that is the arm where the gate
		// genuinely cannot speak.
		if len(absent) > 0 && !anyUnreadable(verdicts) {
			fmt.Printf("\nNothing to check on this cluster: none of the %d object(s) the overlay field map "+
				"names exist here (%s). That is expected where the instance does not run them.\n",
				len(absent), strings.Join(sortedKeys(absent), ", "))
			fmt.Fprintf(os.Stderr, "::warning::assert-overlay-applied examined no field — every mapped object "+
				"is absent from this cluster. If this instance DOES run them, that is a recreate which never "+
				"completed, not a component that is off\n")
			return nil
		}
		fmt.Fprintln(os.Stderr, "::error::no overlay field was examined — the mapped objects could not be "+
			"read, so this run is not evidence that the overlay landed")
		return fmt.Errorf("no overlay field examined")
	}
	if fatal > 0 {
		fmt.Fprintf(os.Stderr, "::error::%d of %d examined overlay field(s) are not what the overlay declares\n",
			fatal, examined)
		return fmt.Errorf("%d overlay field(s) undelivered", fatal)
	}
	// AN ABSENT OBJECT IS NOT A DELIVERED ONE, and the summary line is where that
	// distinction gets lost. An app this instance does not run legitimately has no
	// object, so it must not fail the lane — but "All N delivered" over a fleet of
	// absences is the vacuous green this gate exists to end, and today only the
	// accident that every row points at ONE object makes the examined==0 guard
	// catch it. Say the number instead of relying on that.
	if len(absent) > 0 {
		names := sortedKeys(absent)
		fmt.Printf("\n%d mapped overlay field(s) delivered. NOTHING was checked on %d absent object(s): %s\n",
			examined, len(absent), strings.Join(names, ", "))
		fmt.Fprintf(os.Stderr, "::warning::%d object(s) named by the overlay field map do not exist on this "+
			"cluster (%s) — expected if this instance does not run them, and the sign of a recreate that never "+
			"completed if it does\n", len(absent), strings.Join(names, ", "))
		return nil
	}
	fmt.Printf("\nAll %d mapped overlay field(s) are delivered.\n", examined)
	return nil
}

// anyUnreadable reports whether the gate met an object it could not read, as
// opposed to one that is simply not here. The two look identical in a count of
// what was examined and mean opposite things about whether this run is evidence.
func anyUnreadable(verdicts []overlayVerdict) bool { return countState(verdicts, stateUnreadable) > 0 }

func countState(verdicts []overlayVerdict, s overlayState) int {
	n := 0
	for _, v := range verdicts {
		if v.State == s {
			n++
		}
	}
	return n
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
