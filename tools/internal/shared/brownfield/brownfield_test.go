package brownfield

// brownfield_test.go covers the precondition (the only thing standing between a
// migration and a needless recreate of live infrastructure), the coupling to the
// field map, the refusal arms, and the unattended path converge drives.

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
)

// The two shapes of the object this migration exists for.
const (
	preMigrationSTS = `{"apiVersion":"apps/v1","kind":"StatefulSet",
      "metadata":{"name":"loki-ingester","namespace":"monitoring"},
      "spec":{"template":{"spec":{"containers":[{"name":"ingester",
        "resources":{"limits":{"cpu":"1","memory":"1Gi"}}}],
        "volumes":[{"name":"data","emptyDir":{}}]}}}}`

	postMigrationSTS = `{"apiVersion":"apps/v1","kind":"StatefulSet",
      "metadata":{"name":"loki-ingester","namespace":"monitoring"},
      "spec":{"template":{"spec":{"containers":[{"name":"ingester",
        "resources":{"limits":{"cpu":"1","memory":"3Gi"}}}]}},
        "volumeClaimTemplates":[{"metadata":{"name":"data"}}]}}`
)

// withObject swaps the read seam. Answers are given in sequence so a test can
// show the object changing under the migration — the recreate wait depends on it.
func withObject(t *testing.T, answers ...func() (string, bool, bool)) {
	t.Helper()
	prev := readObject
	i := 0
	readObject = func(Deps, string, string, string) ([]byte, bool, bool) {
		a := answers[min(i, len(answers)-1)]
		i++
		raw, absent, answered := a()
		return []byte(raw), absent, answered
	}
	t.Cleanup(func() { readObject = prev })
}

func found(raw string) func() (string, bool, bool) {
	return func() (string, bool, bool) { return raw, false, true }
}
func absentObject() func() (string, bool, bool) {
	return func() (string, bool, bool) { return "", true, true }
}
func unanswered() func() (string, bool, bool) {
	return func() (string, bool, bool) { return "", false, false }
}

// syncedOwner is the Application that declares the object, in the state that
// permits a recreate: Synced, no spec error. Health is deliberately Progressing —
// the app IS progressing, precisely because the value has not landed, and a guard
// that required Healthy would refuse to repair the only cluster that needs it.
// The syncPolicy is the REAL one off gsap-apl prod — `automated {prune: true,
// selfHeal: true}` — because selfHeal is what puts a deleted object back, and a
// fixture that omitted it would be testing a permission the cluster may not give.
// The values carry what the overlay declares, since the guard also refuses to
// delete an object whose Application does not yet want the new shape.
const syncedOwner = `{"metadata":{"name":"monitoring-loki"},
  "spec":{"project":"default",
    "syncPolicy":{"automated":{"allowEmpty":false,"prune":true,"selfHeal":true},
                  "syncOptions":["ServerSideApply=true"]},
    "source":{"helm":{"values":"ingester:\n  persistence:\n    enabled: true\n    claims:\n    - name: data\n      size: 5Gi\n  resources:\n    limits:\n      cpu: \"1\"\n      memory: 3Gi\n    requests:\n      cpu: 100m\n      memory: 512Mi\n"}}},
  "status":{"sync":{"status":"Synced"},"health":{"status":"Progressing"},"conditions":[]}}`

// statefulSetRefusal is what a real apiserver returned for the WAL-claim change
// on the cluster this whole migration was written for. Every test below describes
// a BROWNFIELD cluster — one where the migration is genuinely needed — so this is
// the honest answer to the appliability probe createOnlyStillHolds sends before
// deleting anything. A runner that answered "accepted" instead would be
// describing a cluster on which the migration must NOT run.
const statefulSetRefusal = `The StatefulSet "loki-ingester" is invalid: spec: Forbidden: updates to ` +
	`statefulset spec for fields other than 'replicas', 'ordinals', 'template', 'updateStrategy', ` +
	`'persistentVolumeClaimRetentionPolicy' and 'minReadySeconds' are forbidden`

// testDeps is a Deps whose clock does not sleep, so the recreate wait runs at
// test speed. Its runner answers the OWNER read — every path that deletes checks
// first — and the pre-delete appliability probe, and returns empty for anything
// else.
func testDeps() Deps { return testDepsWithOwner(syncedOwner, true) }

func testDepsWithOwner(owner string, answered bool) Deps {
	return testDepsWith(owner, answered, func(...string) (string, bool) {
		return statefulSetRefusal, false
	})
}

// testDepsWith lets a case answer the pre-delete probe differently — the arm that
// decides whether an irreversible delete happens at all.
func testDepsWith(owner string, answered bool, probe func(...string) (string, bool)) Deps {
	now := time.Now()
	return Deps{
		Kubectl: func(args ...string) (string, bool) {
			for _, a := range args {
				if a == "application.argoproj.io" {
					return owner, answered
				}
			}
			for _, a := range args {
				if a == "--dry-run=server" {
					return probe(args...)
				}
			}
			return "", true
		},
		Now:   func() time.Time { return now },
		Sleep: func(d time.Duration) { now = now.Add(d) },
	}
}

// recordingWriter captures the mutation instead of performing it.
type recordingWriter struct {
	capability.Writer
	calls    []string
	managers []string
	err      error
	applyErr error
}

// deletes returns only the orphan deletes, so a test that counts the repair is
// not counting the record written beside it.
func (w *recordingWriter) deletes() []string {
	var out []string
	for _, c := range w.calls {
		if strings.HasPrefix(c, "DeleteOrphan") {
			out = append(out, c)
		}
	}
	return out
}

func (w *recordingWriter) DeleteOrphan(ns, kind, name string) ([]byte, error) {
	w.calls = append(w.calls, strings.Join([]string{"DeleteOrphan", ns, kind, name}, " "))
	return nil, w.err
}

func (w *recordingWriter) ApplyStdin(manifest, fieldManager string) ([]byte, error) {
	w.calls = append(w.calls, "ApplyStdin "+strings.ReplaceAll(strings.TrimSpace(manifest), "\n", " "))
	w.managers = append(w.managers, fieldManager)
	return nil, w.applyErr
}

func (w *recordingWriter) PatchMerge(ns, kind, name, patch string) ([]byte, error) {
	w.calls = append(w.calls, strings.Join([]string{"PatchMerge", ns, kind, name, patch}, " "))
	return nil, nil
}

// ── the precondition ─────────────────────────────────────────────────────────

func TestABrownfieldObjectReadsPending(t *testing.T) {
	withObject(t, found(preMigrationSTS))
	sts := MigrationStatuses(testDeps())
	if len(sts) != 1 {
		t.Fatalf("one CreateOnly field is mapped today; got %d statuses", len(sts))
	}
	if sts[0].State != MigrationPending {
		t.Errorf("state = %s, want PENDING: %s", sts[0].State, sts[0].Detail)
	}
	if !strings.Contains(sts[0].Detail, "0 entries") {
		t.Errorf("the detail must say what the object actually has, got %q", sts[0].Detail)
	}
}

// The greenfield case, and the reason this can be reported eagerly on every
// bootstrap: a cluster built after the change has nothing to do.
func TestAnObjectCreatedInTheDeclaredShapeReadsDone(t *testing.T) {
	withObject(t, found(postMigrationSTS))
	if got := MigrationStatuses(testDeps())[0].State; got != MigrationDone {
		t.Errorf("state = %s, want DONE", got)
	}
}

func TestAnAbsentObjectIsNotAPendingMigration(t *testing.T) {
	withObject(t, absentObject())
	if got := MigrationStatuses(testDeps())[0].State; got != MigrationNotHere {
		t.Errorf("state = %s, want NOT HERE — an app that is not deployed has nothing to migrate", got)
	}
}

// UNKNOWN is never DONE and never PENDING: one would hide a real migration, the
// other would recreate live infrastructure on the strength of a failed read.
func TestAnUnreadableClusterIsUnknownRatherThanEitherVerdict(t *testing.T) {
	withObject(t, unanswered())
	if got := MigrationStatuses(testDeps())[0].State; got != MigrationUnknown {
		t.Errorf("state = %s, want UNKNOWN", got)
	}
}

// ── execution ────────────────────────────────────────────────────────────────

func TestRunMigrationRefusesWithoutYes(t *testing.T) {
	withObject(t, found(preMigrationSTS))
	w := &recordingWriter{Writer: capability.Denied()}
	err := RunMigration(testDeps(), w, clusterspec.LokiWALPVCMigration, false, false, false)
	if err == nil {
		t.Fatal("a recreate of live infrastructure must not happen without --yes")
	}
	if len(w.deletes()) != 0 {
		t.Errorf("nothing may be written on the unconfirmed path, got %v", w.calls)
	}
}

func TestRunMigrationOrphanDeletesThenWaitsForTheRecreate(t *testing.T) {
	// Pending, pending (Argo has not put it back yet), then delivered.
	withObject(t, found(preMigrationSTS), absentObject(), found(postMigrationSTS))
	w := &recordingWriter{Writer: capability.Denied()}
	if err := RunMigration(testDeps(), w, clusterspec.LokiWALPVCMigration, true, false, false); err != nil {
		t.Fatalf("the migration should complete once the object comes back carrying the field: %v", err)
	}
	if d := w.deletes(); len(d) != 1 || !strings.Contains(d[0], "DeleteOrphan monitoring statefulset loki-ingester") {
		t.Errorf("the recreate must go through the named orphan delete, got %v", w.calls)
	}
}

// The object is gone and Argo has not put it back. That is the one genuinely bad
// outcome, and it has to be reported as work an operator is owed rather than as a
// migration that finished.
func TestAnObjectThatNeverComesBackIsAFailureNamingIt(t *testing.T) {
	withObject(t, found(preMigrationSTS), absentObject())
	w := &recordingWriter{Writer: capability.Denied()}
	err := RunMigration(testDeps(), w, clusterspec.LokiWALPVCMigration, true, false, false)
	if err == nil {
		t.Fatal("a StatefulSet that is never recreated must fail loudly")
	}
	for _, want := range []string{"has not come back carrying", "Argo owns the recreate", "pods keep serving"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure must say what happened and what is owed; %q missing from: %v", want, err)
		}
	}
}

func TestRunMigrationDoesNothingWhenTheFieldIsAlreadyDelivered(t *testing.T) {
	withObject(t, found(postMigrationSTS))
	w := &recordingWriter{Writer: capability.Denied()}
	if err := RunMigration(testDeps(), w, clusterspec.LokiWALPVCMigration, true, false, false); err != nil {
		t.Fatalf("an already-applied migration is a no-op, not an error: %v", err)
	}
	if len(w.deletes()) != 0 {
		t.Errorf("nothing may be recreated when the field is already there, got %v", w.calls)
	}
}

func TestRunMigrationRefusesOnAnUnreadableCluster(t *testing.T) {
	withObject(t, unanswered())
	w := &recordingWriter{Writer: capability.Denied()}
	if err := RunMigration(testDeps(), w, clusterspec.LokiWALPVCMigration, true, false, false); err == nil {
		t.Fatal("a cluster that did not answer must not be migrated on the strength of a failed read")
	}
	if len(w.deletes()) != 0 {
		t.Errorf("nothing may be written after an unreadable precondition, got %v", w.calls)
	}
}

func TestAnUnknownMigrationIdIsAnError(t *testing.T) {
	withObject(t, found(preMigrationSTS))
	if err := RunMigration(testDeps(), &recordingWriter{Writer: capability.Denied()}, "nope", true, false, false); err == nil {
		t.Fatal("an id nothing registers must not silently do nothing")
	}
}

// ── the coupling to the field map ────────────────────────────────────────────

// Every CreateOnly field names a migration (clusterspec pins that); this is the
// other direction — that the id it names is one something here can actually run,
// and that nothing is registered which no field asks for.
func TestTheRegistryAndTheFieldMapNameTheSameMigrations(t *testing.T) {
	declared := map[string]bool{}
	for _, f := range clusterspec.OverlayFields() {
		if f.CreateOnly {
			declared[f.Migration] = true
		}
	}
	if len(declared) == 0 {
		t.Fatal("no CreateOnly field declares a migration — this coupling test is passing vacuously")
	}
	registered := map[string]bool{}
	for _, m := range Migrations() {
		registered[m.ID] = true
		if !declared[m.ID] {
			t.Errorf("migration %q is registered and no overlay field names it — it would be evaluated "+
				"against nothing", m.ID)
		}
	}
	for id := range declared {
		if !registered[id] {
			t.Errorf("an overlay field names migration %q as its remedy and nothing registers it — the gate "+
				"would tell an operator to run a command that does not exist", id)
		}
	}
}

// A migration that says what to do and not what is left over hands an operator a
// half-finished cluster and no way to know it.
func TestEveryMigrationSaysWhatItLeavesBehind(t *testing.T) {
	for _, m := range Migrations() {
		if m.Strategy != StrategyOrphanRecreate {
			continue
		}
		if !strings.Contains(m.Then, "pods") {
			t.Errorf("%s uses orphan-recreate, which leaves the pods on the OLD spec; Then must say so, "+
				"got %q", m.ID, m.Then)
		}
		if m.Why == "" {
			t.Errorf("%s does not say what stays broken while it is pending", m.ID)
		}
	}
}

// ── the unattended path ──────────────────────────────────────────────────────

// captureStdout runs fn with stdout redirected, returning what it printed. The
// report IS this package's product on the observe path, so asserting on it is
// asserting on the deliverable rather than on an internal.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	prev := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var b bytes.Buffer
		_, _ = io.Copy(&b, r)
		done <- b.String()
	}()
	fn()
	_ = w.Close()
	os.Stdout = prev
	return <-done
}

func TestApplyPendingRecreatesAnAutoMigration(t *testing.T) {
	withObject(t, found(preMigrationSTS))
	w := &recordingWriter{Writer: capability.Denied()}
	var r ApplyResult
	out := captureStdout(t, func() { r = ApplyPending(testDeps(), w, nil) })
	applied, deferred := r.Applied, r.Deferred
	if len(r.Errs) != 0 {
		t.Fatalf("unexpected errors: %v", r.Errs)
	}
	if len(applied) != 1 || applied[0] != clusterspec.LokiWALPVCMigration {
		t.Errorf("applied = %v, want the WAL migration", applied)
	}
	if len(deferred) != 0 {
		t.Errorf("deferred = %v, want none", deferred)
	}
	if d := w.deletes(); len(d) != 1 || !strings.Contains(d[0], "DeleteOrphan monitoring statefulset loki-ingester") {
		t.Errorf("the recreate must go through the named orphan delete, got %v", w.calls)
	}
	// The pod roll is the operator's, so the line that says the object is gone has
	// to say so in the same breath — this output is the only place anyone is told.
	if !strings.Contains(out, "STILL TO DO AFTERWARDS") || !strings.Contains(out, "pods") {
		t.Errorf("the applied line must name what it leaves behind:\n%s", out)
	}
}

// A migration nobody cleared for unattended use must be reported and left alone.
// This is the safety valve the next strategy will need, so it is tested before
// there is a caller for it.
func TestApplyPendingDefersAMigrationThatIsNotAuto(t *testing.T) {
	withObject(t, found(preMigrationSTS))
	prev := migrations
	migrations = func() []Migration {
		out := prev()
		for i := range out {
			out[i].Auto = false
		}
		return out
	}
	t.Cleanup(func() { migrations = prev })

	w := &recordingWriter{Writer: capability.Denied()}
	var r ApplyResult
	captureStdout(t, func() { r = ApplyPending(testDeps(), w, nil) })
	applied, deferred := r.Applied, r.Deferred
	if len(applied) != 0 || len(w.calls) != 0 {
		t.Errorf("a non-Auto migration must not be applied: applied=%v writes=%v", applied, w.calls)
	}
	if len(deferred) != 1 {
		t.Errorf("deferred = %v, want the one migration", deferred)
	}
}

func TestApplyPendingReportsAFailedDeleteRatherThanClaimingItApplied(t *testing.T) {
	withObject(t, found(preMigrationSTS))
	w := &recordingWriter{Writer: capability.Denied(), err: errors.New("apiserver said no")}
	var r ApplyResult
	captureStdout(t, func() { r = ApplyPending(testDeps(), w, nil) })
	applied, errs := r.Applied, r.Errs
	if len(applied) != 0 {
		t.Errorf("a failed delete is not an applied migration, got %v", applied)
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "apiserver said no") {
		t.Errorf("the apiserver's own message must survive, got %v", errs)
	}
}

func TestApplyPendingTouchesNothingWhenEveryFieldIsDelivered(t *testing.T) {
	withObject(t, found(postMigrationSTS))
	w := &recordingWriter{Writer: capability.Denied()}
	var r ApplyResult
	captureStdout(t, func() { r = ApplyPending(testDeps(), w, nil) })
	applied, deferred := r.Applied, r.Deferred
	if len(applied) != 0 || len(deferred) != 0 || len(w.calls) != 0 {
		t.Errorf("a converged cluster is a no-op: applied=%v deferred=%v writes=%v", applied, deferred, w.calls)
	}
}

// ── the report ───────────────────────────────────────────────────────────────

func TestTheReportNamesWhatIsBrokenAndWhoLandsIt(t *testing.T) {
	withObject(t, found(preMigrationSTS))
	var pending int
	out := captureStdout(t, func() { pending = ReportMigrations(testDeps()) })
	if pending != 1 {
		t.Fatalf("pending = %d, want 1", pending)
	}
	for _, want := range []string{"PENDING", "what stays broken", "landed by", "llz ci converge"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report must carry %q:\n%s", want, out)
		}
	}
}

func TestAConvergedClusterReportsNothingPending(t *testing.T) {
	withObject(t, found(postMigrationSTS))
	var pending int
	out := captureStdout(t, func() { pending = ReportMigrations(testDeps()) })
	if pending != 0 {
		t.Errorf("pending = %d, want 0", pending)
	}
	if !strings.Contains(out, "DONE") {
		t.Errorf("a done migration is still reported, so a reader knows it was checked:\n%s", out)
	}
}

func TestTheBestEffortReportWarnsButNeverFails(t *testing.T) {
	withObject(t, found(preMigrationSTS))
	out := captureStdout(t, func() { ReportMigrationsBestEffort(testDeps()) })
	if !strings.Contains(out, "::warning::") {
		t.Errorf("a pending migration must surface as a warning annotation:\n%s", out)
	}
}

// A strategy nothing implements must REFUSE, not fall through to the orphan
// delete. Strategy was declared, printed in the plan, and then never read — both
// callers issued the StatefulSet treatment unconditionally, so a second strategy
// (or a zero value on a hand-built Migration) would have got it too.
func TestAnUnimplementedStrategyIsRefusedRatherThanOrphanDeleted(t *testing.T) {
	withObject(t, found(preMigrationSTS))
	prev := migrations
	migrations = func() []Migration {
		out := prev()
		for i := range out {
			out[i].Strategy = Strategy("recreate-namespace")
		}
		return out
	}
	t.Cleanup(func() { migrations = prev })

	w := &recordingWriter{Writer: capability.Denied()}
	var r ApplyResult
	captureStdout(t, func() { r = ApplyPending(testDeps(), w, nil) })
	if len(w.deletes()) != 0 {
		t.Errorf("an unknown strategy must not be guessed at, got %v", w.calls)
	}
	if len(r.Applied) != 0 {
		t.Errorf("nothing was repaired, so nothing may be reported as applied: %v", r.Applied)
	}
	if len(r.Errs) != 1 || !strings.Contains(r.Errs[0].Error(), "nothing here implements") {
		t.Errorf("the refusal must name the unimplemented strategy, got %v", r.Errs)
	}

	// The operator path refuses in the same way rather than in its own way.
	err := RunMigration(testDeps(), w, clusterspec.LokiWALPVCMigration, true, false, false)
	if err == nil || !strings.Contains(err.Error(), "nothing here implements") {
		t.Errorf("RunMigration = %v, want the same refusal", err)
	}
	if len(w.deletes()) != 0 {
		t.Errorf("still nothing may be deleted, got %v", w.calls)
	}
}

// A cluster that did not answer is reported as inconclusive, never as "nothing to
// do" — the caller latches a once-per-run flag on this and would otherwise
// disable the repair for a whole run on one failed read.
func TestAnUnreadableClusterIsReportedInconclusiveNotEmpty(t *testing.T) {
	withObject(t, unanswered())
	w := &recordingWriter{Writer: capability.Denied()}
	var r ApplyResult
	captureStdout(t, func() { r = ApplyPending(testDeps(), w, nil) })
	if len(r.Applied) != 0 || len(w.calls) != 0 {
		t.Errorf("nothing may be written after an unreadable read: applied=%v writes=%v", r.Applied, w.calls)
	}
	if len(r.Inconclusive) != 1 {
		t.Errorf("Inconclusive = %v, want the one migration whose state is unknown", r.Inconclusive)
	}
}

// ONCE PER RUN MEANS ONCE PER MIGRATION, and the caller's set is what makes it
// true. A single boolean latch cannot express it: with two migrations, one
// unreadable and one pending, either the unreadable one disables the pending one
// for the whole run or the pending one is deleted on every poll.
func TestAnAttemptedMigrationIsNotRetriedInTheSameRun(t *testing.T) {
	withObject(t, found(preMigrationSTS))
	w := &recordingWriter{Writer: capability.Denied()}
	attempted := map[string]bool{}

	captureStdout(t, func() { ApplyPending(testDeps(), w, attempted) })
	if len(w.deletes()) != 1 {
		t.Fatalf("first pass: deletes = %v, want one", w.calls)
	}
	if !attempted[clusterspec.LokiWALPVCMigration] {
		t.Fatalf("the migration was attempted and is not recorded: %v", attempted)
	}
	// Same cluster state, same run: the object still reads PENDING (Argo has not
	// recreated it yet), and it must NOT be deleted again.
	captureStdout(t, func() { ApplyPending(testDeps(), w, attempted) })
	if len(w.deletes()) != 1 {
		t.Errorf("second pass in the same run re-attempted the repair: %v", w.calls)
	}
}

// A FAILED REPAIR IS RECORDED TOO. Asking the apiserver to refuse the same delete
// again inside one run adds a write and no information.
func TestAFailedAttemptIsAlsoRecorded(t *testing.T) {
	withObject(t, found(preMigrationSTS))
	w := &recordingWriter{Writer: capability.Denied(), err: errors.New("apiserver said no")}
	attempted := map[string]bool{}
	captureStdout(t, func() { ApplyPending(testDeps(), w, attempted) })
	if !attempted[clusterspec.LokiWALPVCMigration] {
		t.Error("a failed attempt must still count as attempted")
	}
	captureStdout(t, func() { ApplyPending(testDeps(), w, attempted) })
	if len(w.deletes()) != 1 {
		t.Errorf("the failed repair was retried in the same run: %v", w.calls)
	}
}

// The runner contract, pinned because it broke once: a successful read is STDOUT
// ONLY, and the engine feeds it to a JSON decoder. A combined-output runner puts
// kubectl's stderr in the same buffer, and one deprecation warning then reads as
// "the cluster did not answer".
func TestAWarningOnStderrDoesNotMakeTheObjectUnreadable(t *testing.T) {
	prev := readObject
	readObject = func(Deps, string, string, string) ([]byte, bool, bool) {
		// What a stdout-only runner returns. If this were combined output it would
		// carry a "Warning: v1 ... is deprecated" line and fail to decode.
		return []byte(preMigrationSTS), false, true
	}
	t.Cleanup(func() { readObject = prev })
	if got := MigrationStatuses(testDeps())[0].State; got != MigrationPending {
		t.Errorf("state = %s, want PENDING — a clean JSON body must decode", got)
	}

	readObject = func(Deps, string, string, string) ([]byte, bool, bool) {
		return []byte("Warning: apps/v1 something is deprecated\n" + preMigrationSTS), false, true
	}
	if got := MigrationStatuses(testDeps())[0].State; got != MigrationUnknown {
		t.Errorf("state = %s — a body this engine cannot decode must read UNKNOWN, never DONE; the fix is "+
			"the runner (stdout only), not a lenient decoder", got)
	}
}

// ── the owner check: the guard that makes the delete reversible ──────────────

// Once the object is gone, ABSENT reads as nothing-to-do and no later run
// retries. So an Application that cannot put it back must stop the delete
// happening at all — on every path, including the one an operator drives.
func TestNoDeleteWhenTheOwningApplicationCannotRecreate(t *testing.T) {
	const comparisonError = `{"metadata":{"name":"monitoring-loki"},"spec":{"project":"default"},
      "status":{"sync":{"status":"Synced"},"conditions":[
        {"type":"ComparisonError","message":"failed to generate manifest"}]}}`
	const outOfSync = `{"metadata":{"name":"monitoring-loki"},"spec":{"project":"default"},
      "status":{"sync":{"status":"OutOfSync"},"conditions":[]}}`

	for _, tc := range []struct {
		name, owner, want string
		answered          bool
	}{
		{"cannot compute its target state", comparisonError, "cannot compute its target state", true},
		{"not currently applying", outOfSync, "not Synced", true},
		{"unreadable", "", "could not read Application", false},
		{"unparseable", "{not json", "did not parse", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withObject(t, found(preMigrationSTS))
			d := testDepsWithOwner(tc.owner, tc.answered)
			w := &recordingWriter{Writer: capability.Denied()}

			// The unattended path defers and says why.
			var r ApplyResult
			out := captureStdout(t, func() { r = ApplyPending(d, w, nil) })
			if len(w.deletes()) != 0 {
				t.Errorf("deleted an object its owner could not recreate: %v", w.calls)
			}
			if len(r.Applied) != 0 {
				t.Errorf("applied = %v, want none", r.Applied)
			}
			if len(r.Inconclusive) != 1 {
				t.Errorf("a deferred migration must be inconclusive, not silence: %+v", r)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("the deferral must say why; %q missing from:\n%s", tc.want, out)
			}

			// …and so does the operator path, rather than deleting because a human asked.
			err := RunMigration(d, w, clusterspec.LokiWALPVCMigration, true, false, false)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("RunMigration = %v, want a refusal naming %q", err, tc.want)
			}
			if len(w.deletes()) != 0 {
				t.Errorf("still nothing may be deleted: %v", w.calls)
			}
		})
	}
}

// The owner check must not be so strict that it refuses the cluster it exists
// for: the Application IS Progressing while the value is undelivered.
func TestAProgressingButSyncedOwnerIsAllowedToRecreate(t *testing.T) {
	withObject(t, found(preMigrationSTS))
	w := &recordingWriter{Writer: capability.Denied()}
	var r ApplyResult
	captureStdout(t, func() { r = ApplyPending(testDeps(), w, nil) })
	if len(r.Applied) != 1 || len(w.deletes()) != 1 {
		t.Errorf("a Synced/Progressing owner must permit the repair: applied=%v writes=%v", r.Applied, w.calls)
	}
}

// SELFHEAL IS WHAT PUTS THE OBJECT BACK. `automated` syncs on a desired-state
// change; a cluster-side deletion is drift, and only selfHeal corrects drift.
// apl-core owns this Application's syncPolicy, so this is checked rather than
// assumed from what it happens to be today.
func TestNoDeleteWhenTheOwnerWouldNotPutTheObjectBack(t *testing.T) {
	noSelfHeal := strings.Replace(syncedOwner, `"selfHeal":true`, `"selfHeal":false`, 1)
	withObject(t, found(preMigrationSTS))
	w := &recordingWriter{Writer: capability.Denied()}
	var r ApplyResult
	out := captureStdout(t, func() { r = ApplyPending(testDepsWithOwner(noSelfHeal, true), w, nil) })
	if len(w.deletes()) != 0 {
		t.Errorf("deleted an object nothing would recreate: %v", w.calls)
	}
	if len(r.Inconclusive) != 1 || !strings.Contains(out, "does not self-heal") {
		t.Errorf("the deferral must name the missing selfHeal:\n%s", out)
	}
}

// AND THE APPLICATION HAS TO ALREADY WANT THE NEW SHAPE. Without this the repair
// has no cross-run bound: if apl-core has not rendered the overlay yet, deleting
// recreates the SAME object, the field is still missing, and the next converge run
// deletes it again — forever, with no signal that the repair cannot work.
func TestNoDeleteWhenTheOwnersDesiredStateDoesNotCarryTheValue(t *testing.T) {
	for _, tc := range []struct{ name, values, want string }{
		// No document at all is "this guard cannot see what the app wants", which must
		// not share an answer with "the app does not want it".
		{"no values document", `"values":""`, "carries no Helm values this check can read"},
		{"rendered, but not this path", `"values":"querier:\n  replicas: 2\n"`, "does not yet declare"},
		{"a different value", `"values":"ingester:\n  persistence:\n    enabled: false\n"`, "declares"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner := syncedOwner[:strings.Index(syncedOwner, `"values":`)] + tc.values + `}}},
  "status":{"sync":{"status":"Synced"},"health":{"status":"Progressing"},"conditions":[]}}`
			withObject(t, found(preMigrationSTS))
			w := &recordingWriter{Writer: capability.Denied()}
			var r ApplyResult
			out := captureStdout(t, func() { r = ApplyPending(testDepsWithOwner(owner, true), w, nil) })
			if len(w.deletes()) != 0 {
				t.Errorf("recreating would reproduce the same shape; nothing may be deleted: %v", w.calls)
			}
			if len(r.Inconclusive) != 1 || !strings.Contains(out, tc.want) {
				t.Errorf("the deferral must say the Application does not want this yet:\n%s", out)
			}
		})
	}
}

// The caller owns the attempted map, and a nil one records nothing — Go cannot
// grow a nil map through a parameter. Pinned because the doc comment said the
// opposite, on a function that deletes live objects: a loop that passed nil each
// pass would re-delete on every poll.
func TestANilAttemptedMapRecordsNothingAndIsSafeOnlyForOneShotCallers(t *testing.T) {
	withObject(t, found(preMigrationSTS))
	w := &recordingWriter{Writer: capability.Denied()}
	captureStdout(t, func() { ApplyPending(testDeps(), w, nil) })
	captureStdout(t, func() { ApplyPending(testDeps(), w, nil) })
	if len(w.deletes()) != 2 {
		t.Fatalf("two nil-map passes over a still-pending object should each act (that is the hazard the "+
			"doc now names); got %v", w.calls)
	}
	// …and the same two passes with a caller-owned map act exactly once.
	w2 := &recordingWriter{Writer: capability.Denied()}
	owned := map[string]bool{}
	captureStdout(t, func() { ApplyPending(testDeps(), w2, owned) })
	captureStdout(t, func() { ApplyPending(testDeps(), w2, owned) })
	if len(w2.deletes()) != 1 {
		t.Errorf("a caller-owned map must bound the repair to once per run, got %v", w2.calls)
	}
}

// AN APPLICATION CARRIES ITS VALUES IN FOUR PLACES and reading one is how this
// guard goes quietly inert: a multi-source or valuesObject Application would
// yield nothing, every migration would defer behind a warning, and converge would
// stay green having repaired nothing. apl-core owns this shape and can change it
// in a chart release without telling anyone.
func TestTheOwnerCheckReadsEveryShapeAnApplicationCarriesValuesIn(t *testing.T) {
	const valuesObject = `{"metadata":{"name":"monitoring-loki"},
      "spec":{"project":"default","syncPolicy":{"automated":{"selfHeal":true}},
        "source":{"helm":{"valuesObject":{"ingester":{"persistence":{"enabled":true}}}}}},
      "status":{"sync":{"status":"Synced"},"conditions":[]}}`
	const multiSource = `{"metadata":{"name":"monitoring-loki"},
      "spec":{"project":"default","syncPolicy":{"automated":{"selfHeal":true}},
        "sources":[{"helm":{"values":"querier:\n  replicas: 2\n"}},
                   {"helm":{"values":"ingester:\n  persistence:\n    enabled: true\n"}}]},
      "status":{"sync":{"status":"Synced"},"conditions":[]}}`

	for _, tc := range []struct{ name, owner string }{
		{"valuesObject", valuesObject},
		{"sources[]", multiSource},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withObject(t, found(preMigrationSTS))
			w := &recordingWriter{Writer: capability.Denied()}
			var r ApplyResult
			out := captureStdout(t, func() { r = ApplyPending(testDepsWithOwner(tc.owner, true), w, nil) })
			if len(r.Applied) != 1 || len(w.deletes()) != 1 {
				t.Errorf("the Application declares the value in this shape and the repair must proceed: "+
					"applied=%v writes=%v\n%s", r.Applied, w.calls, out)
			}
		})
	}
}

// LATER SOURCES LAYER OVER EARLIER ONES, so the value Argo will actually render
// is the LAST one carrying the path. Taking the first would approve a delete
// whose recreate then delivers something else — the across-runs bound inverted
// into a StatefulSet re-deleted on every converge run.
func TestAMultiSourceOwnerIsJudgedOnTheValueThatWins(t *testing.T) {
	const overriddenToFalse = `{"metadata":{"name":"monitoring-loki"},
      "spec":{"project":"default","syncPolicy":{"automated":{"selfHeal":true}},
        "sources":[{"helm":{"values":"ingester:\n  persistence:\n    enabled: true\n"}},
                   {"helm":{"values":"ingester:\n  persistence:\n    enabled: false\n"}}]},
      "status":{"sync":{"status":"Synced"},"conditions":[]}}`
	withObject(t, found(preMigrationSTS))
	w := &recordingWriter{Writer: capability.Denied()}
	var r ApplyResult
	out := captureStdout(t, func() { r = ApplyPending(testDepsWithOwner(overriddenToFalse, true), w, nil) })
	if len(w.deletes()) != 0 {
		t.Errorf("the winning value is false, so recreating delivers false — nothing may be deleted: %v",
			w.calls)
	}
	if len(r.Inconclusive) != 1 || !strings.Contains(out, "declares") {
		t.Errorf("the deferral must name the value the Application would actually render:\n%s", out)
	}
}

// A DRY RUN IS NOT A MISSING --yes. Folding them together produced "refusing …
// without --yes" from an invocation that passed --yes.
func TestADryRunPrintsThePlanAndSucceeds(t *testing.T) {
	withObject(t, found(preMigrationSTS))
	w := &recordingWriter{Writer: capability.Denied()}
	var err error
	out := captureStdout(t, func() {
		err = RunMigration(testDeps(), w, clusterspec.LokiWALPVCMigration, true, true, false)
	})
	if err != nil {
		t.Errorf("a dry run asked to see the plan and got it; that is a success, got %v", err)
	}
	if len(w.deletes()) != 0 {
		t.Errorf("--dry-run wrote: %v", w.calls)
	}
	for _, want := range []string{"it will:", "--dry-run: nothing was written"} {
		if !strings.Contains(out, want) {
			t.Errorf("the dry run must print the plan and say it wrote nothing; %q missing from:\n%s", want, out)
		}
	}
}

// objectWithSelector stamps a creationTimestamp and the selector the pod lookup
// keys on, so the fixture is the shape the guard actually reads.
func objectWithSelector(obj string, age time.Duration) string {
	withMeta := strings.Replace(obj, `"namespace":"monitoring"`,
		`"namespace":"monitoring","creationTimestamp":"`+time.Now().Add(-age).UTC().Format(time.RFC3339)+`"`, 1)
	return strings.Replace(withMeta, `"spec":{"template"`,
		`"spec":{"selector":{"matchLabels":{"app.kubernetes.io/component":"ingester"}},"template"`, 1)
}

// testDepsWithPods answers the pod-age lookup as well as the owner read.
func testDepsWithPods(owner string, podAge time.Duration) Deps {
	d := testDepsWithOwner(owner, true)
	inner := d.Kubectl
	d.Kubectl = func(args ...string) (string, bool) {
		for _, a := range args {
			if a == "pods" {
				return time.Now().Add(-podAge).UTC().Format(time.RFC3339) + "\n", true
			}
		}
		return inner(args...)
	}
	return d
}

// …and an object that has been standing since before the overlay existed is
// exactly what this migration is for.
func TestAnObjectAsOldAsItsOwnWorkloadIsStillMigrated(t *testing.T) {
	// Created with its pods 37 days ago: nothing has recreated this, which is the
	// case the migration exists for.
	withObject(t, found(objectWithSelector(preMigrationSTS, 37*24*time.Hour)))
	w := &recordingWriter{Writer: capability.Denied()}
	var r ApplyResult
	captureStdout(t, func() { r = ApplyPending(testDepsWithPods(syncedOwner, 37*24*time.Hour), w, nil) })
	if len(r.Applied) != 1 || len(w.deletes()) != 1 {
		t.Errorf("an object nothing has recreated is the case this exists for: applied=%v writes=%v",
			r.Applied, w.calls)
	}
}

// An object with no readable creationTimestamp must not be blocked: refusing on
// the strength of a field we could not read would stop every migration on any
// object whose metadata shape surprises us.
func TestAnUnreadableCreationTimestampDoesNotBlockTheMigration(t *testing.T) {
	// This guard only ever adds a reason to STOP, so an unreadable object must not
	// block the repair — the guards that permit the delete are the ones that fail
	// closed. The fixture carries no creationTimestamp and no selector.
	withObject(t, found(preMigrationSTS))
	w := &recordingWriter{Writer: capability.Denied()}
	var r ApplyResult
	captureStdout(t, func() { r = ApplyPending(testDeps(), w, nil) })
	if len(r.Applied) != 1 {
		t.Errorf("no timestamp must read as 'not recently recreated', got %+v", r)
	}
}

// Within one source, valuesObject is the structured form and takes precedence, so
// it must be the one judged — the inverse ordering would re-create the repeated
// delete this guard exists to prevent.
func TestValuesObjectWinsOverTheValuesStringInOneSource(t *testing.T) {
	const both = `{"metadata":{"name":"monitoring-loki"},
      "spec":{"project":"default","syncPolicy":{"automated":{"selfHeal":true}},
        "source":{"helm":{"values":"ingester:\n  persistence:\n    enabled: true\n",
                          "valuesObject":{"ingester":{"persistence":{"enabled":false}}}}}},
      "status":{"sync":{"status":"Synced"},"conditions":[]}}`
	withObject(t, found(preMigrationSTS))
	w := &recordingWriter{Writer: capability.Denied()}
	captureStdout(t, func() { ApplyPending(testDepsWithOwner(both, true), w, nil) })
	if len(w.deletes()) != 0 {
		t.Errorf("valuesObject says false, so the recreate would deliver false — nothing may be deleted: %v",
			w.calls)
	}
}

// The retry is not suppressed, only the noise: a skipped migration is re-read
// every poll (the thing blocking it may clear), but the annotation appears once —
// sixty identical warnings in a run is how a warning stops being read.
func TestASkippedMigrationWarnsOncePerRunAndKeepsBeingRetried(t *testing.T) {
	withObject(t, found(preMigrationSTS))
	blocked := strings.Replace(syncedOwner, `"status":{"sync":{"status":"Synced"}`,
		`"status":{"sync":{"status":"OutOfSync"}`, 1)
	d := testDepsWithOwner(blocked, true)
	w := &recordingWriter{Writer: capability.Denied()}
	run := map[string]bool{}

	first := captureStdout(t, func() { ApplyPending(d, w, run) })
	second := captureStdout(t, func() { ApplyPending(d, w, run) })
	if !strings.Contains(first, "::warning::") {
		t.Errorf("the first skip must say why:\n%s", first)
	}
	if strings.Contains(second, "::warning::") {
		t.Errorf("the same warning repeated on the next poll:\n%s", second)
	}
	// …and the migration is still being evaluated, not latched off: once the owner
	// is Synced again the repair proceeds within the same run.
	captureStdout(t, func() { ApplyPending(testDeps(), w, run) })
	if len(w.deletes()) != 1 {
		t.Errorf("a skip must not disable the repair for the run; writes=%v", w.calls)
	}
}

// THE DOCUMENTED ESCAPE HATCH HAS TO EXIST. The already-recreated deferral tells
// an operator to force it; before --force, RunMigration refused every UNKNOWN
// before it looked at any flag, so the sentence pointed at nothing.
func TestForceOverridesTheAdvisoryRefusalAndNothingElse(t *testing.T) {
	d := testDepsWithRecord(clusterspec.LokiWALPVCMigration, time.Now().Add(-3*24*time.Hour))

	// Without --force: refused, and the refusal names the flag.
	withObject(t, found(preMigrationSTS))
	w := &recordingWriter{Writer: capability.Denied()}
	err := RunMigration(d, w, clusterspec.LokiWALPVCMigration, true, false, false)
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("the refusal must name the way past it, got %v", err)
	}
	if len(w.deletes()) != 0 {
		t.Errorf("nothing may be written on the refused path: %v", w.calls)
	}

	// With --force: it proceeds, and the cluster then behaves — Argo recreates the
	// object carrying the value, which is what the wait is for.
	withObject(t, found(preMigrationSTS), found(postMigrationSTS))
	w2 := &recordingWriter{Writer: capability.Denied()}
	if err := RunMigration(d, w2, clusterspec.LokiWALPVCMigration, true, false, true); err != nil {
		t.Fatalf("--force must proceed: %v", err)
	}
	if len(w2.deletes()) != 1 {
		t.Errorf("--force did not reach the recreate: %v", w2.calls)
	}
}

// …and it must NOT override a read that failed. An operator can know something
// this code does not about whether a second attempt is worthwhile; they cannot
// know what an unanswered apiserver would have said.
func TestForceDoesNotOverrideAClusterThatDidNotAnswer(t *testing.T) {
	withObject(t, unanswered())
	w := &recordingWriter{Writer: capability.Denied()}
	err := RunMigration(testDeps(), w, clusterspec.LokiWALPVCMigration, true, false, true)
	if err == nil {
		t.Fatal("--force must not act on a cluster that did not answer")
	}
	if strings.Contains(err.Error(), "--force") {
		t.Errorf("and it must not advertise itself as the way past this one: %v", err)
	}
	if len(w.deletes()) != 0 {
		t.Errorf("nothing may be written: %v", w.calls)
	}
}

// Nor a precondition about whether the object would come back — --force is about
// the operator's judgement, not about overruling Argo.
func TestForceDoesNotOverrideTheOwnerCheck(t *testing.T) {
	withObject(t, found(preMigrationSTS))
	noSelfHeal := strings.Replace(syncedOwner, `"selfHeal":true`, `"selfHeal":false`, 1)
	w := &recordingWriter{Writer: capability.Denied()}
	err := RunMigration(testDepsWithOwner(noSelfHeal, true), w, clusterspec.LokiWALPVCMigration, true, false, true)
	if err == nil || !strings.Contains(err.Error(), "does not self-heal") {
		t.Fatalf("--force must not delete an object nothing would put back, got %v", err)
	}
	if len(w.deletes()) != 0 {
		t.Errorf("nothing may be written: %v", w.calls)
	}
}

// THE RECORD IS WRITTEN BEFORE THE DELETE, and that ordering is the fix for three
// successive versions of this guard. Recorded after, every failure path that
// forgets to write it puts the unbounded repeat back — and each version forgot a
// different one.
func TestTheAttemptIsRecordedBeforeTheDelete(t *testing.T) {
	withObject(t, found(preMigrationSTS))
	w := &recordingWriter{Writer: capability.Denied()}
	captureStdout(t, func() { ApplyPending(testDeps(), w, nil) })
	if len(w.calls) != 2 {
		t.Fatalf("want the record then the delete, got %v", w.calls)
	}
	if !strings.HasPrefix(w.calls[0], "ApplyStdin") || !strings.Contains(w.calls[0], AttemptsConfigMap) {
		t.Errorf("the first write must be the record: %v", w.calls)
	}
	if !strings.HasPrefix(w.calls[1], "DeleteOrphan") {
		t.Errorf("the delete must come second: %v", w.calls)
	}
}

// A FAILURE TO RECORD ABORTS THE MIGRATION — the opposite of the best-effort
// posture elsewhere here, and deliberate: without the record this repair has no
// cross-run bound at all, and an unbounded orphan-delete of a live StatefulSet is
// worse than a repair that did not happen.
func TestNoDeleteWhenTheAttemptCannotBeRecorded(t *testing.T) {
	withObject(t, found(preMigrationSTS))
	w := &recordingWriter{Writer: capability.Denied(), applyErr: errors.New("apiserver said no")}
	var r ApplyResult
	captureStdout(t, func() { r = ApplyPending(testDeps(), w, nil) })
	for _, c := range w.calls {
		if strings.HasPrefix(c, "DeleteOrphan") {
			t.Errorf("deleted without a record of having done so: %v", w.calls)
		}
	}
	if len(r.Errs) != 1 || !strings.Contains(r.Errs[0].Error(), "refusing to delete without it") {
		t.Errorf("the failure must say why it refused, got %v", r.Errs)
	}
}

// And a recorded attempt stops the next run repeating it — whatever the ages of
// the object and its pods, which is what the previous three attempts at this
// depended on.
func TestARecordedAttemptStopsTheRepeat(t *testing.T) {
	withObject(t, found(preMigrationSTS))
	d := testDepsWithRecord(clusterspec.LokiWALPVCMigration, time.Now().Add(-3*24*time.Hour))
	w := &recordingWriter{Writer: capability.Denied()}
	var r ApplyResult
	out := captureStdout(t, func() { r = ApplyPending(d, w, nil) })
	if len(w.deletes()) != 0 {
		t.Errorf("a migration already applied to this object must not repeat: %v", w.calls)
	}
	if len(r.Inconclusive) != 1 || !strings.Contains(out, "already applied to") {
		t.Errorf("the deferral must name the previous attempt:\n%s", out)
	}
}

// A record for a DIFFERENT migration must not block this one.
func TestARecordForAnotherMigrationDoesNotBlockThisOne(t *testing.T) {
	withObject(t, found(preMigrationSTS))
	d := testDepsWithRecord("099-something-else", time.Now().Add(-3*24*time.Hour))
	w := &recordingWriter{Writer: capability.Denied()}
	var r ApplyResult
	captureStdout(t, func() { r = ApplyPending(d, w, nil) })
	if len(r.Applied) != 1 {
		t.Errorf("another migration's record must not block this one: %+v", r)
	}
}

// testDepsWithRecord answers the attempt-record read for one migration id.
func testDepsWithRecord(id string, at time.Time) Deps {
	d := testDeps()
	inner := d.Kubectl
	d.Kubectl = func(args ...string) (string, bool) {
		for _, a := range args {
			if a == "configmap" {
				for _, x := range args {
					if strings.Contains(x, id) {
						return at.UTC().Format(time.RFC3339), true
					}
				}
				return "", true
			}
		}
		return inner(args...)
	}
	return d
}

// A dry run answers "what would happen", and "it would refuse, for this reason"
// is an answer. Erroring made `llz --dry-run ci brownfield-migrate` exit non-zero
// with no plan at all.
func TestADryRunReportsARefusalInsteadOfErroring(t *testing.T) {
	withObject(t, found(preMigrationSTS))
	w := &recordingWriter{Writer: capability.Denied()}
	var err error
	out := captureStdout(t, func() {
		err = RunMigration(testDepsWithRecord(clusterspec.LokiWALPVCMigration, time.Now().Add(-time.Hour)), w,
			clusterspec.LokiWALPVCMigration, true, true, false)
	})
	if err != nil {
		t.Errorf("a dry run that explains the refusal is a success, got %v", err)
	}
	if !strings.Contains(out, "would NOT run here") || !strings.Contains(out, "nothing was written") {
		t.Errorf("the dry run must say what it would do and that it wrote nothing:\n%s", out)
	}
	if len(w.deletes()) != 0 {
		t.Errorf("--dry-run wrote: %v", w.calls)
	}
}

// A shape this guard cannot read is not an absent value. An Application with BOTH
// inline values and valueFiles has documents to read, so the "no documents at
// all" branch never fires — and "the overlay has not reached apl-core's render
// yet" would then be asserted about a file this code never opened.
func TestValueFilesAlongsideInlineValuesIsReportedAsUnreadableNotAbsent(t *testing.T) {
	const both = `{"metadata":{"name":"monitoring-loki"},
      "spec":{"project":"default","syncPolicy":{"automated":{"selfHeal":true}},
        "source":{"helm":{"values":"querier:\n  replicas: 2\n",
                          "valueFiles":["$values/env/loki.yaml"]}}},
      "status":{"sync":{"status":"Synced"},"conditions":[]}}`
	withObject(t, found(preMigrationSTS))
	w := &recordingWriter{Writer: capability.Denied()}
	var r ApplyResult
	out := captureStdout(t, func() { r = ApplyPending(testDepsWithOwner(both, true), w, nil) })
	if len(w.deletes()) != 0 {
		t.Errorf("nothing may be deleted on a conclusion this guard cannot draw: %v", w.calls)
	}
	if len(r.Inconclusive) != 1 || !strings.Contains(out, "cannot be concluded") {
		t.Errorf("the deferral must say the conclusion is unavailable, not that the value is absent:\n%s", out)
	}
}

// Helm's escape: `a\.b` is ONE key. Splitting on every dot turns it into a nested
// structure matching nothing, so a declared value would read as absent.
func TestHelmParameterPathsRespectTheEscape(t *testing.T) {
	got := splitHelmPath(`ingester.resources\.limits.memory`)
	want := []string{"ingester", "resources.limits", "memory"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("splitHelmPath = %v, want %v", got, want)
	}
}

// And a scalar must not be silently replaced by a map that a later parameter
// implies — Helm rejects the pair, and dropping the scalar would make a declared
// value read as undeclared.
func TestAParameterCollisionKeepsTheScalarRatherThanDroppingIt(t *testing.T) {
	doc := parametersDoc([]struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}{{Name: "a", Value: "1"}, {Name: "a.b", Value: "2"}})
	if doc["a"] != "1" {
		t.Errorf("the scalar was dropped by the colliding path: %v", doc)
	}
}

// THE RECORD SAYS "WE DELETED THIS OBJECT", so a delete that did not happen must
// not leave one. Without this a transient refusal — a 5xx, a webhook, an RBAC
// blip — permanently disables the repair behind a message claiming it was already
// applied.
func TestAFailedDeleteClearsTheRecordItJustWrote(t *testing.T) {
	withObject(t, found(preMigrationSTS))
	w := &recordingWriter{Writer: capability.Denied(), err: errors.New("apiserver said no")}
	captureStdout(t, func() { ApplyPending(testDeps(), w, nil) })
	var wrote, cleared bool
	for _, c := range w.calls {
		if strings.HasPrefix(c, "ApplyStdin") {
			wrote = true
		}
		if strings.HasPrefix(c, "PatchMerge") && strings.Contains(c, "null") {
			cleared = true
		}
	}
	if !wrote {
		t.Fatal("the record must be written before the delete is attempted")
	}
	if !cleared {
		t.Errorf("a refused delete must clear the record it wrote, got %v", w.calls)
	}
}

// One field manager per migration: server-side apply prunes what a manager no
// longer sends, so a shared one would delete the other migrations' keys while
// recording its own.
func TestEachMigrationRecordsUnderItsOwnFieldManager(t *testing.T) {
	withObject(t, found(preMigrationSTS))
	w := &recordingWriter{Writer: capability.Denied()}
	captureStdout(t, func() { ApplyPending(testDeps(), w, nil) })
	if len(w.managers) != 1 || w.managers[0] != "llz-brownfield-"+clusterspec.LokiWALPVCMigration {
		t.Errorf("field managers = %v, want one per migration id", w.managers)
	}
}
