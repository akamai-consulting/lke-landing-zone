package converge

// health_brownfield_test.go — converge applies a pending brownfield migration,
// and every arm where it must NOT.
//
// This is the one self-heal in the loop that DELETES a live object, so the tests
// that matter are the negative ones: once per run, platform scope only, never on a
// converged object, never on a cluster that did not answer, and never at all with
// --brownfield-migrate=false.

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
)

// The brownfield StatefulSet: created before the overlay declared a WAL claim.
const convergePendingSTS = `{"apiVersion":"apps/v1","kind":"StatefulSet",
  "metadata":{"name":"loki-ingester","namespace":"monitoring"},
  "spec":{"template":{"spec":{"containers":[{"name":"ingester",
    "resources":{"limits":{"cpu":"1","memory":"1Gi"}}}]}}}}`

// …and the same object after Argo has recreated it.
const convergeMigratedSTS = `{"apiVersion":"apps/v1","kind":"StatefulSet",
  "metadata":{"name":"loki-ingester","namespace":"monitoring"},
  "spec":{"template":{"spec":{"containers":[{"name":"ingester","resources":{
    "limits":{"cpu":"1","memory":"3Gi"},"requests":{"cpu":"100m","memory":"512Mi"}}}]}},
    "volumeClaimTemplates":[{"metadata":{"name":"data"}}]}}`

// recordingMigrationWriter counts the one mutation this path may make.
type recordingMigrationWriter struct {
	capability.Writer
	deletes  []string
	applied  []string
	failWith error
	applyErr error
}

func (w *recordingMigrationWriter) ApplyStdin(manifest, fieldManager string) ([]byte, error) {
	w.applied = append(w.applied, manifest)
	return nil, w.applyErr
}

func (w *recordingMigrationWriter) PatchMerge(ns, kind, name, patch string) ([]byte, error) {
	return nil, nil
}

func (w *recordingMigrationWriter) DeleteOrphan(ns, kind, name string) ([]byte, error) {
	w.deletes = append(w.deletes, ns+"/"+kind+"/"+name)
	return nil, w.failWith
}

// withMigrationCluster stubs the reads the migration makes — the object, and the
// owning Application it checks before deleting anything — and swaps in a writer
// that records instead of deleting.
func withMigrationCluster(t *testing.T, obj string) *recordingMigrationWriter {
	return withMigrationClusterFailing(t, obj, nil)
}

// The fake cluster RESPONDS TO THE DELETE, because converge now waits for the
// recreate before it will report convergence — and a cluster that never puts the
// object back would (correctly) make the loop poll until its budget expires.
// After a successful orphan delete the object comes back in the migrated shape,
// which is what Argo does.
func withMigrationClusterFailing(t *testing.T, obj string, deleteErr error) *recordingMigrationWriter {
	t.Helper()
	w := &recordingMigrationWriter{Writer: capability.Denied(), failWith: deleteErr}
	withDepsExec(t, func(_ string, args ...string) ([]byte, error) {
		for _, a := range args {
			if a == "application.argoproj.io" {
				return []byte(convergeSyncedOwner), nil
			}
		}
		if deleteErr == nil && len(w.deletes) > 0 {
			return []byte(convergeMigratedSTS), nil
		}
		return []byte(obj), nil
	})
	prev := deps.Writer
	deps.Writer = w
	t.Cleanup(func() { deps.Writer = prev })
	return w
}

// withMigrationClusterThatNeverRecreates models the bad outcome: the delete
// succeeds and Argo does not put the object back. Used only where that is the
// property under test, and always with a bounded budget so the loop terminates.
func withMigrationClusterThatNeverRecreates(t *testing.T) *recordingMigrationWriter {
	t.Helper()
	w := &recordingMigrationWriter{Writer: capability.Denied()}
	withDepsExec(t, func(_ string, args ...string) ([]byte, error) {
		for _, a := range args {
			if a == "application.argoproj.io" {
				return []byte(convergeSyncedOwner), nil
			}
		}
		if len(w.deletes) > 0 {
			return nil, errors.New(`Error from server (NotFound): statefulsets.apps "loki-ingester" not found`)
		}
		return []byte(convergePendingSTS), nil
	})
	prev := deps.Writer
	deps.Writer = w
	t.Cleanup(func() { deps.Writer = prev })
	return w
}

// The Application that declares the StatefulSet, in the state that permits a
// recreate. Progressing on purpose: it is progressing BECAUSE the value has not
// landed, and a guard that required Healthy would refuse the cluster it exists for.
// The syncPolicy is the real one off gsap-apl prod, selfHeal included — that is
// what puts a deleted object back — and the values carry what the overlay
// declares, since the guard refuses to delete an object whose Application does not
// yet want the new shape.
const convergeSyncedOwner = `{"metadata":{"name":"monitoring-loki"},
  "spec":{"project":"default",
    "syncPolicy":{"automated":{"prune":true,"selfHeal":true}},
    "source":{"helm":{"values":"ingester:\n  persistence:\n    enabled: true\n  resources:\n    limits:\n      cpu: \"1\"\n      memory: 3Gi\n    requests:\n      cpu: 100m\n      memory: 512Mi\n"}}},
  "status":{"sync":{"status":"Synced"},"health":{"status":"Progressing"},"conditions":[]}}`

// withConvergePollFunc scripts the health scan by ITERATION rather than by a fixed
// list, so a test can observe the cluster's state at the moment of each poll.
func withConvergePollFunc(t *testing.T, fn func(n int) healthResult) {
	t.Helper()
	n := 0
	prev := convergePoll
	convergePoll = func(*convergeState) healthResult {
		r := fn(n)
		n++
		return r
	}
	t.Cleanup(func() { convergePoll = prev })
}

func TestConvergeLandsAPendingBrownfieldMigrationOncePerRun(t *testing.T) {
	w := withMigrationCluster(t, convergePendingSTS)
	// Three polls: the loop must apply on the first and never again, however many
	// times it comes round. A second delete of an object Argo is still recreating
	// is the failure mode this guard exists for.
	withConvergePoll(t, healthResult{code: 2}, healthResult{code: 2}, healthResult{code: 0})
	if err := runConverge(3600, 0, 0, ScopePlatform, true); err != nil {
		t.Fatalf("runConverge = %v, want nil", err)
	}
	if len(w.deletes) != 1 {
		t.Fatalf("orphan deletes = %v, want exactly one (once per run)", w.deletes)
	}
	if w.deletes[0] != "monitoring/statefulset/loki-ingester" {
		t.Errorf("deleted %q, want the object the field map names", w.deletes[0])
	}
}

func TestConvergeDoesNotTouchAnObjectThatAlreadyCarriesTheValue(t *testing.T) {
	w := withMigrationCluster(t, convergeMigratedSTS)
	withConvergePoll(t, healthResult{code: 0})
	if err := runConverge(3600, 0, 0, ScopePlatform, true); err != nil {
		t.Fatalf("runConverge = %v, want nil", err)
	}
	if len(w.deletes) != 0 {
		t.Errorf("a converged object must not be recreated, got %v", w.deletes)
	}
}

// The opt-out has to be real: an operator who wants a window says so and converge
// observes without repairing.
func TestBrownfieldMigrateFalseObservesWithoutRecreating(t *testing.T) {
	w := withMigrationCluster(t, convergePendingSTS)
	withConvergePoll(t, healthResult{code: 0})
	if err := runConverge(3600, 0, 0, ScopePlatform, false); err != nil {
		t.Fatalf("runConverge = %v, want nil", err)
	}
	if len(w.deletes) != 0 {
		t.Errorf("--brownfield-migrate=false must write nothing, got %v", w.deletes)
	}
}

// An apps-scope run gates an app team's content on behalf of an app team.
// Recreating a platform object from there inverts the boundary the scope draws —
// the same rule the redis realign and the annotation strip already follow.
func TestAnAppsScopeRunNeverRecreatesAPlatformObject(t *testing.T) {
	w := withMigrationCluster(t, convergePendingSTS)
	withConvergePoll(t, healthResult{appCode: 0})
	if err := runConverge(3600, 0, 0, ScopeApps, true); err != nil {
		t.Fatalf("runConverge = %v, want nil", err)
	}
	if len(w.deletes) != 0 {
		t.Errorf("the apps scope must not mutate platform objects, got %v", w.deletes)
	}
}

// A cluster that did not answer is not a cluster with an undelivered value.
func TestConvergeDoesNotRecreateOnAnUnreadableCluster(t *testing.T) {
	w := &recordingMigrationWriter{Writer: capability.Denied()}
	withDepsExec(t, func(string, ...string) ([]byte, error) {
		return nil, errors.New("the server was unable to return a response")
	})
	prev := deps.Writer
	deps.Writer = w
	t.Cleanup(func() { deps.Writer = prev })
	withConvergePoll(t, healthResult{code: 0})
	if err := runConverge(3600, 0, 0, ScopePlatform, true); err != nil {
		t.Fatalf("runConverge = %v, want nil", err)
	}
	if len(w.deletes) != 0 {
		t.Errorf("nothing may be deleted on the strength of a failed read, got %v", w.deletes)
	}
}

// The remedy an operator is told to run and the one converge runs are the same
// migration, named by the same constant — a second spelling on either side would
// let converge repair something the gate is not reporting, or the reverse.
func TestConvergeRunsTheMigrationTheFieldMapNames(t *testing.T) {
	var named bool
	for _, f := range clusterspec.OverlayFields() {
		if f.CreateOnly && f.Migration == clusterspec.LokiWALPVCMigration {
			named = true
		}
	}
	if !named {
		t.Fatalf("no CreateOnly overlay field names %s — converge would repair nothing",
			clusterspec.LokiWALPVCMigration)
	}
	w := withMigrationCluster(t, convergePendingSTS)
	withConvergePoll(t, healthResult{code: 0}, healthResult{code: 0})
	if err := runConverge(3600, 0, 0, ScopePlatform, true); err != nil {
		t.Fatal(err)
	}
	if len(w.deletes) != 1 || !strings.Contains(w.deletes[0], "loki-ingester") {
		t.Errorf("converge did not act on the object the field map names, got %v", w.deletes)
	}
}

// THE VERDICT IN HAND PREDATES THE DELETE, and consuming it returns success from
// the very iteration that deleted a StatefulSet.
//
// This is the arm that made the two older self-heals safe by accident: they only
// fire on a poll that was NOT Done, so their stale verdict is never the one that
// ends the run. A pending migration has no such luck — a cluster whose ONLY fault
// is an undelivered create-time field reads Done by construction (the pods are
// Running, the app says Synced), so converge would delete loki-ingester and
// report convergence in the same breath, never polling for the recreate.
func TestConvergeRePollsAfterApplyingRatherThanConsumingTheStaleVerdict(t *testing.T) {
	w := withMigrationCluster(t, convergePendingSTS)
	// The first poll is DONE — everything healthy, migration still pending. If the
	// loop consumed it, it would return after one scan; it must take another.
	calls := withConvergePoll(t, healthResult{code: 0}, healthResult{code: 0})
	if err := runConverge(3600, 0, 0, ScopePlatform, true); err != nil {
		t.Fatalf("runConverge = %v, want nil", err)
	}
	if len(w.deletes) != 1 {
		t.Fatalf("orphan deletes = %v, want one", w.deletes)
	}
	if *calls != 2 {
		t.Errorf("health scans = %d, want 2 — the verdict that ends the run must come from a poll "+
			"taken AFTER the delete, not from the one that preceded it", *calls)
	}
}

// A SPENT BUDGET STARTS NO NEW WORK. `--budget 0` is this file's report-only
// snapshot, and a snapshot that deletes a StatefulSet is not one — more
// generally, beginning a repair the run has no time left to observe leaves the
// object recreated by nobody and reported by nothing.
func TestAnAlreadySpentBudgetRecreatesNothing(t *testing.T) {
	w := withMigrationCluster(t, convergePendingSTS)
	withConvergePoll(t, healthResult{code: 0})
	if err := runConverge(0, 0, 0, ScopePlatform, true); err != nil {
		t.Fatalf("runConverge = %v, want nil (the poll said converged)", err)
	}
	if len(w.deletes) != 0 {
		t.Errorf("--budget 0 is a snapshot; it must write nothing, got %v", w.deletes)
	}
}

// A cluster that did not answer must not latch the once-per-run flag: converge
// drops probe retries to 1, so a single blip would otherwise disable the repair
// for the whole run.
func TestAnInconclusiveReadDoesNotBurnTheOncePerRunAttempt(t *testing.T) {
	w := &recordingMigrationWriter{Writer: capability.Denied()}
	prevW := deps.Writer
	deps.Writer = w
	t.Cleanup(func() { deps.Writer = prevW })

	// The object read fails once, then succeeds. The owner read always answers —
	// what is being pinned is that a transient on the OBJECT does not spend the
	// run's one attempt.
	objectReads := 0
	prevExec := deps.Exec
	deps.Exec = func(_ string, args ...string) ([]byte, error) {
		for _, a := range args {
			if a == "application.argoproj.io" {
				return []byte(convergeSyncedOwner), nil
			}
		}
		objectReads++
		if objectReads == 1 {
			return nil, errors.New("the server was unable to return a response")
		}
		// Argo recreates it once the delete has happened, so the run can finish.
		if len(w.deletes) > 0 {
			return []byte(convergeMigratedSTS), nil
		}
		return []byte(convergePendingSTS), nil
	}
	t.Cleanup(func() { deps.Exec = prevExec })

	withConvergePoll(t, healthResult{code: 2}, healthResult{code: 2}, healthResult{code: 0})
	if err := runConverge(3600, 0, 0, ScopePlatform, true); err != nil {
		t.Fatalf("runConverge = %v, want nil", err)
	}
	if len(w.deletes) != 1 {
		t.Errorf("the migration must still land on a later poll that could read the cluster, got %v", w.deletes)
	}
}

// THE RECREATE DEPENDS ON ARGO, so it must not be started on a poll that has just
// established Argo cannot sync. Both of these are faults the loop repairs on the
// same iteration; deleting into either leaves the workload with no controller for
// the rest of the run and repairs nothing.
func TestNoRecreateWhileArgoIsKnownUnableToSync(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  healthResult
	}{
		{"redis auth split", healthResult{code: 2, redisAuthSplit: true}},
		{"annotation wedge", healthResult{code: 2, annotationWedge: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := withMigrationCluster(t, convergePendingSTS)
			// WHICH POLL the delete happens on is the property, so the budget must NOT
			// be zero — an earlier version of this test used `--budget 0`, which skips
			// the brownfield block entirely and stayed green with the guard deleted.
			// The poll stub records how many deletes had happened when it was called.
			var deletesAtPoll []int
			withConvergePollFunc(t, func(n int) healthResult {
				deletesAtPoll = append(deletesAtPoll, len(w.deletes))
				if n == 0 {
					return tc.res // wedged: must not migrate
				}
				return healthResult{code: 0} // clean: may migrate, then the run ends
			})
			if err := runConverge(3600, 0, 0, ScopePlatform, true); err != nil {
				t.Fatalf("runConverge = %v, want nil", err)
			}
			if len(deletesAtPoll) < 2 {
				t.Fatalf("the loop took %d poll(s); this test needs the wedged one and the clean one",
					len(deletesAtPoll))
			}
			if deletesAtPoll[1] != 0 {
				t.Errorf("%d delete(s) had already happened by the second poll — the wedged poll recreated "+
					"an object while Argo could not sync", deletesAtPoll[1])
			}
			// …and the deferral is not a cancellation: the clean poll lands it.
			if len(w.deletes) != 1 {
				t.Errorf("the migration must still land once the blocker clears, got %v", w.deletes)
			}
		})
	}
}

// A REPAIR THAT FAILED IS NOT A CONVERGED CLUSTER. The health scan sees the
// object exactly as it was — the delete was refused, so nothing changed — and an
// ::error:: annotation does not fail a GitHub Actions step. Converge's exit code
// is the only thing that does.
func TestAFailedRecreateStopsTheRunReportingConvergence(t *testing.T) {
	withMigrationClusterFailing(t, convergePendingSTS, errors.New("apiserver said no"))
	withConvergePoll(t, healthResult{code: 0})
	err := runConverge(3600, 0, 0, ScopePlatform, true)
	if err == nil {
		t.Fatal("converge reported success on a cluster whose declared value it could not land")
	}
	if !strings.Contains(err.Error(), "brownfield migration") {
		t.Errorf("the failure must name what went wrong, got %v", err)
	}
}

// …and a failed repair is not retried on every poll either: the attempt is
// recorded before it runs, so one refusal costs one write, not one per poll.
//
// The budget is REAL here for the reason above — `--budget 0` skips the block, so
// the earlier version of this test could not have failed.
func TestAFailedRecreateIsNotRetriedEveryPoll(t *testing.T) {
	withMigrationClusterFailing(t, convergePendingSTS, errors.New("apiserver said no"))
	w := deps.Writer.(*recordingMigrationWriter)
	withConvergePoll(t, healthResult{code: 2}, healthResult{code: 2}, healthResult{code: 0})
	if err := runConverge(3600, 0, 0, ScopePlatform, true); err == nil {
		t.Fatal("a refused repair must stop the run reporting convergence")
	}
	if len(w.deletes) != 1 {
		t.Errorf("the refused delete was attempted %d times; once per run per migration is the contract: %v",
			len(w.deletes), w.deletes)
	}
}

// `llz --dry-run ci converge` must not delete a StatefulSet. deps.go's header is
// about exactly this flag: a DryRun field read at tree-build time was permanently
// false, and `llz --dry-run ci nudge-argo` issued real writes. The flag is read
// late here, in RunE, and the read's error is not discardable.
func TestTheGlobalDryRunFlagReachesTheOneWriteInThisLoop(t *testing.T) {
	cmd := ConvergeCmd()
	root := &cobra.Command{Use: "llz"}
	root.PersistentFlags().Bool("dry-run", false, "")
	root.AddCommand(cmd)

	w := withMigrationCluster(t, convergePendingSTS)
	withConvergePoll(t, healthResult{code: 0})
	root.SetArgs([]string{"converge", "--dry-run", "--budget", "3600", "--interval", "0"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	if err := root.Execute(); err != nil {
		t.Fatalf("converge --dry-run = %v, want nil", err)
	}
	if len(w.deletes) != 0 {
		t.Errorf("--dry-run recreated a live object: %v", w.deletes)
	}
}

// …and without it, the same invocation does act — otherwise the test above would
// pass for the wrong reason.
func TestWithoutDryRunTheSameInvocationLandsTheMigration(t *testing.T) {
	cmd := ConvergeCmd()
	root := &cobra.Command{Use: "llz"}
	root.PersistentFlags().Bool("dry-run", false, "")
	root.AddCommand(cmd)

	w := withMigrationCluster(t, convergePendingSTS)
	withConvergePoll(t, healthResult{code: 0}, healthResult{code: 0})
	root.SetArgs([]string{"converge", "--budget", "3600", "--interval", "0"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	if err := root.Execute(); err != nil {
		t.Fatalf("converge = %v, want nil", err)
	}
	if len(w.deletes) != 1 {
		t.Errorf("without --dry-run the migration must land, got %v", w.deletes)
	}
}

// A HEALTH SCAN CANNOT SEE A RECREATE THAT DID NOT HAPPEN — which is this whole
// mechanism's premise turned back on itself. After an orphan delete the pods keep
// running and Argo's Application status can still read Synced from before, so the
// next scan says Done with the StatefulSet gone. And nothing retries later: the
// attempt is latched for the run, and an absent object reads "nothing to migrate
// here" for every run after it. So the run must not end until the object is back
// carrying the value.
func TestConvergeDoesNotReportSuccessUntilTheObjectComesBack(t *testing.T) {
	w := withMigrationClusterThatNeverRecreates(t)
	// Done on every poll: the scan cannot see the missing field, which is the point.
	withConvergePollFunc(t, func(int) healthResult { return healthResult{code: 0} })
	err := runConverge(1, 0, 0, ScopePlatform, true)
	if err == nil {
		t.Fatal("converge reported success with the StatefulSet it deleted still gone")
	}
	if !strings.Contains(err.Error(), "was not recreated") {
		t.Errorf("the failure must name what is missing, got %v", err)
	}
	if len(w.deletes) != 1 {
		t.Errorf("and it must not keep deleting while it waits: %v", w.deletes)
	}
}

// …and when Argo DOES put it back carrying the value, the run ends clean.
func TestConvergeReportsSuccessOnceTheRecreatedObjectCarriesTheValue(t *testing.T) {
	w := withMigrationCluster(t, convergePendingSTS)
	withConvergePollFunc(t, func(int) healthResult { return healthResult{code: 0} })
	if err := runConverge(3600, 0, 0, ScopePlatform, true); err != nil {
		t.Fatalf("runConverge = %v, want nil once the object is back with the value", err)
	}
	if len(w.deletes) != 1 {
		t.Errorf("deletes = %v, want exactly one", w.deletes)
	}
}

// AN ABSENT OBJECT MUST NOT SETTLE THE RUN. An object can arrive mid-run — Argo
// recreating one from an earlier attempt is the obvious way — so latching
// "nothing to do" on its absence would leave a migration that becomes PENDING
// five minutes later unexamined for the rest of the budget.
func TestATransientlyAbsentObjectDoesNotStopTheRunLooking(t *testing.T) {
	w := &recordingMigrationWriter{Writer: capability.Denied()}
	prevW := deps.Writer
	deps.Writer = w
	t.Cleanup(func() { deps.Writer = prevW })

	// The FIRST object read says NotFound; every later one finds it. Keyed on the
	// read rather than on the poll number, because the migration block runs after
	// the poll — an earlier version of this test keyed on the poll and therefore
	// never returned absent at all, which made it pass with the fix removed.
	objectReads := 0
	prevExec := deps.Exec
	deps.Exec = func(_ string, args ...string) ([]byte, error) {
		for _, a := range args {
			if a == "application.argoproj.io" {
				return []byte(convergeSyncedOwner), nil
			}
			if a == "pods" {
				return []byte(""), nil
			}
		}
		objectReads++
		if objectReads == 1 {
			return nil, errors.New(`Error from server (NotFound): statefulsets.apps "loki-ingester" not found`)
		}
		if len(w.deletes) > 0 {
			return []byte(convergeMigratedSTS), nil
		}
		return []byte(convergePendingSTS), nil
	}
	t.Cleanup(func() { deps.Exec = prevExec })

	withConvergePollFunc(t, func(int) healthResult { return healthResult{code: 2} })
	// A budget that ends the run after a few polls; what matters is that the
	// migration was still being looked at once the object appeared.
	_ = runConverge(2, 0, 0, ScopePlatform, true)
	if len(w.deletes) != 1 {
		t.Errorf("the migration must be picked up once the object appears, got %v", w.deletes)
	}
}

// `--brownfield-migrate=false` says it "observes and reports without recreating
// anything". Skipping the whole block made it observe nothing — an operator who
// turned the repair off to keep a window lost the report telling them a window is
// needed.
func TestBrownfieldMigrateFalseStillReportsWhatItWouldHaveDone(t *testing.T) {
	w := withMigrationCluster(t, convergePendingSTS)
	withConvergePoll(t, healthResult{code: 0})
	out := captureStderr(t, func() {
		if err := runConverge(3600, 0, 0, ScopePlatform, false); err != nil {
			t.Fatalf("runConverge = %v, want nil", err)
		}
	})
	if len(w.deletes) != 0 {
		t.Errorf("--brownfield-migrate=false must write nothing, got %v", w.deletes)
	}
	if !strings.Contains(out, "outstanding and this run is not") {
		t.Errorf("it must still say what it is carrying:\n%s", out)
	}
}
