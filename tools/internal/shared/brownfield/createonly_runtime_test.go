package brownfield

// createonly_runtime_test.go pins the check that stands between a hand-set
// CreateOnly boolean and an irreversible delete.
//
// WHY IT IS NOT ENOUGH THAT A PR-TIME GATE EXISTS.
// `llz ci assert-overlay-appliability` asks a kind apiserver whether each
// CreateOnly claim is true, which protects the NEXT edit to the field map. An
// instance runs whatever table shipped in its binary, so a row that was wrong when
// it shipped reaches a cluster the gate never saw — and this migration deletes a
// live StatefulSet on the strength of that row. UnmappedOverlayPaths, two files
// over, is deliberately evaluated at BOTH times for exactly this reason; the
// strongest claim in the table had only the PR-time half.

import (
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
)

func walRow(t *testing.T) clusterspec.OverlayField {
	t.Helper()
	for _, f := range clusterspec.OverlayFields() {
		if f.CreateOnly && f.Migration == clusterspec.LokiWALPVCMigration {
			return f
		}
	}
	t.Fatalf("no CreateOnly row names migration %s", clusterspec.LokiWALPVCMigration)
	return clusterspec.OverlayField{}
}

func TestAnAcceptedProbeRefusesTheDeleteInsteadOfPerformingIt(t *testing.T) {
	// THE FINDING. If this apiserver takes the change as an ordinary patch, the
	// field is not create-only HERE, and deleting the object would recreate a live
	// workload's controller to land a value a patch would have landed.
	d := testDepsWith(syncedOwner, true, func(...string) (string, bool) {
		return `{"spec":{"volumeClaimTemplates":[{"metadata":{"name":"data"}}]}}`, true
	})
	err := createOnlyStillHolds(d, walRow(t))
	if err == nil {
		t.Fatal("the apiserver accepted the change as an ordinary patch and the migration went ahead " +
			"with the delete anyway — the CreateOnly claim was never checked against the cluster it acts on")
	}
	if !strings.Contains(err.Error(), "ACCEPTED") {
		t.Errorf("the refusal does not say what the apiserver actually did: %v", err)
	}
}

func TestAnUnclassifiableRefusalAlsoRefusesTheDelete(t *testing.T) {
	// FAIL-CLOSED, AND THE ASYMMETRY IS THE ARGUMENT. An RBAC denial, a 5xx or a
	// timeout is not a verdict about the field. Refusing costs a migration that does
	// not run and says why; deleting costs a live workload's controller on a claim
	// nothing confirmed.
	d := testDepsWith(syncedOwner, true, func(...string) (string, bool) {
		return `Error from server (Forbidden): statefulsets.apps "loki-ingester" is forbidden: ` +
			`User "system:serviceaccount:monitoring:converge" cannot patch resource "statefulsets"`, false
	})
	err := createOnlyStillHolds(d, walRow(t))
	if err == nil {
		t.Fatal("a probe that could not be performed was read as permission to delete")
	}
	if !strings.Contains(err.Error(), "not 'go ahead and delete'") {
		t.Errorf("the refusal does not distinguish 'could not tell' from 'the claim is right': %v", err)
	}
	if !strings.Contains(err.Error(), "cannot patch resource") {
		t.Errorf("the refusal drops the apiserver's own words, which are what tell an RBAC fault "+
			"from a real verdict: %v", err)
	}
}

func TestAnImmutabilityRefusalLetsTheMigrationProceed(t *testing.T) {
	// The control. Without it every case above would pass with the check hard-wired
	// to refuse, and the migration would simply never run.
	if err := createOnlyStillHolds(testDeps(), walRow(t)); err != nil {
		t.Fatalf("a genuine immutability refusal — the state every brownfield cluster this "+
			"migration exists for is in — was not allowed to proceed: %v", err)
	}
}

func TestTheProbeRunsBeforeTheDeleteAndNotAfterIt(t *testing.T) {
	// ORDER IS THE WHOLE VALUE. A check that ran after the delete would be a report,
	// not a guard.
	d := testDepsWith(syncedOwner, true, func(...string) (string, bool) {
		return `{"spec":{}}`, true // accepted → the claim does not hold
	})
	w := &recordingWriter{}
	err := recreate(d, w, Migration{ID: clusterspec.LokiWALPVCMigration, Strategy: StrategyOrphanRecreate}, walRow(t))
	if err == nil {
		t.Fatal("recreate deleted the object despite the claim not holding")
	}
	if len(w.deletes()) != 0 {
		t.Errorf("the object was deleted before the claim was checked: %v", w.deletes())
	}
}

func TestARowWhosePatchCannotBeBuiltRefusesTheDelete(t *testing.T) {
	f := walRow(t)
	f.Patch = func(map[string]any) (string, error) { return "", errProbeUnbuildable }
	err := createOnlyStillHolds(testDeps(), f)
	if err == nil {
		t.Fatal("a claim whose own probe cannot be built was acted on anyway")
	}
	if !strings.Contains(err.Error(), "nothing can confirm") {
		t.Errorf("the refusal does not say the claim is unconfirmable: %v", err)
	}
}

func TestARowWhoseAppDeclaresNoRawValuesRefusesTheDelete(t *testing.T) {
	// THROUGH THE SEAM, BECAUSE THE REAL OVERLAY CANNOT REACH THIS ARM. Every shipped
	// row has a _rawValues entry, so with clusterspec.AplAppRawValues() read directly
	// this branch was unreachable and deletable with 146 packages green — while being
	// the thing that stands between a wrong `App` string and DeleteOrphan on a live
	// StatefulSet. The probe deps are never consulted: the point is that the refusal
	// happens BEFORE anything is asked of the cluster.
	prev := migrationRawValues
	migrationRawValues = func() map[string]map[string]any { return map[string]map[string]any{} }
	t.Cleanup(func() { migrationRawValues = prev })

	err := createOnlyStillHolds(testDeps(), walRow(t))
	if err == nil {
		t.Fatal("a migration whose app declares no _rawValues was cleared to delete a live object")
	}
	if !strings.Contains(err.Error(), "declares no _rawValues for app") {
		t.Errorf("the refusal does not name the missing _rawValues: %v", err)
	}
	if !strings.Contains(err.Error(), "nothing can confirm") {
		t.Errorf("the refusal does not say the claim is unconfirmable: %v", err)
	}
}

func TestARowWhoseDeclaredValueIsMissingRefusesTheDelete(t *testing.T) {
	// The sibling arm one line down: the app resolves but the row's Value path names
	// nothing in it. Same consequence, same fail-closed direction, and reachable only
	// because the seam above lets a test supply an overlay that disagrees with the row.
	prev := migrationRawValues
	migrationRawValues = func() map[string]map[string]any {
		return map[string]map[string]any{walRow(t).App: {"something-else": "1Gi"}}
	}
	t.Cleanup(func() { migrationRawValues = prev })

	f := walRow(t)
	err := createOnlyStillHolds(testDeps(), f)
	if err == nil {
		t.Fatal("a migration whose declared value is absent was cleared to delete a live object")
	}
	// THE ROW'S OWN PATH, NOT "nothing can confirm". Every fail-closed arm in this
	// function ends with that phrase, so asserting on it passes whichever arm fired —
	// measured: with this arm neutered the run still refused, from the patch-build arm
	// one line down, and a "nothing can confirm" assertion stayed green. The declared
	// arm is the only one that names the ROW's full path, and the patch-build arm
	// quotes the Patch's own inner path (…persistence.claims) rather than this one.
	want := "the overlay declares no " + clusterspec.OverlayFieldPath(f)
	if !strings.Contains(err.Error(), want) {
		t.Errorf("the refusal is not this arm's — want a message containing %q\ngot: %v", want, err)
	}
}

var errProbeUnbuildable = errUnbuildable{}

type errUnbuildable struct{}

func (errUnbuildable) Error() string { return "the claims list is not a list" }

func TestATransportBlipDefersTheMigrationRatherThanFailingTheRun(t *testing.T) {
	// THE FLAKE SURFACE THIS CHECK INTRODUCED. Before it, a 5xx or a webhook timeout
	// on this path simply meant the delete was attempted. Now it means the delete is
	// refused — and reporting that as an ERROR would fail a whole converge run on a
	// blip. Converge is a poll loop: "could not ask" is a reason to look again.
	for _, msg := range []string{
		`Error from server: etcdserver: request timed out`,
		`Get "https://10.0.0.1:6443/apis/apps/v1/...": dial tcp 10.0.0.1:6443: connect: connection refused`,
		`error: unexpected EOF`,
	} {
		d := testDepsWith(syncedOwner, true, func(...string) (string, bool) { return msg, false })
		err := createOnlyStillHolds(d, walRow(t))
		if err == nil {
			t.Fatalf("a transport fault was read as permission to delete: %s", msg)
		}
		if !IsProbeInconclusive(err) {
			t.Errorf("a transport fault is fatal rather than deferred, so one blip fails the whole "+
				"converge run: %s -> %v", msg, err)
		}
	}
}

func TestAPermissionDenialStaysFatalRatherThanDeferring(t *testing.T) {
	// The control. If everything unclassifiable deferred, a genuinely misconfigured
	// cluster would poll silently to its budget instead of saying what is wrong.
	d := testDepsWith(syncedOwner, true, func(...string) (string, bool) {
		return `Error from server (Forbidden): statefulsets.apps "loki-ingester" is forbidden: ` +
			`User "system:serviceaccount:monitoring:converge" cannot patch resource "statefulsets"`, false
	})
	err := createOnlyStillHolds(d, walRow(t))
	if err == nil {
		t.Fatal("an RBAC denial was read as permission to delete")
	}
	if IsProbeInconclusive(err) {
		t.Error("an RBAC denial was deferred as a transport blip — it is an answer, and a run that " +
			"keeps polling on it never says what is actually wrong")
	}
}

func TestTheProbeAlwaysCarriesTheDryRunFlag(t *testing.T) {
	// THIS ARGV IS ONE TOKEN FROM A REAL WRITE, and unlike the sibling probe in
	// assertplatform it does not pass through a declared cluster-read handle that
	// would refuse it. The sibling's own comment names the hazard: dropping
	// `--dry-run=server` later would mutate a live object from a path that is
	// supposed only to look at one.
	args := migrationProbeArgs(walRow(t), `{"spec":{}}`)
	var found bool
	for _, a := range args {
		if a == "--dry-run=server" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the migration's pre-delete probe would MUTATE the live object: %v", args)
	}
}
