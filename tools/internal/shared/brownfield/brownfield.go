// Package brownfield answers one question and performs one repair: which overlay
// changes can this cluster's EXISTING objects not accept, and how does an object
// come to accept one.
//
// IT IS A SHARED PACKAGE BECAUSE TWO CALLERS DRIVE IT AND THEY MUST NOT DISAGREE.
// `llz ci brownfield-migrations` / `brownfield-migrate` are the operator's
// handles, and `llz ci converge` applies pending migrations as part of driving the
// platform to convergence. One deciding a migration is pending while the other
// thinks it is done — or, worse, each carrying its own copy of the recreate — is
// the split-contract shape docs/e2e-gates.md warns about, on a step that DELETES
// a live object.
package brownfield

// brownfield.go — how an upstream overlay change reaches a cluster whose objects
// predate it.
//
// THE PROBLEM, ONCE. LLZ ships a change; the overlay declares it; apl-core
// renders it; Argo compares it. On a cluster built after the change, the object
// is CREATED in that shape and there is nothing to do. On every other cluster the
// change has to be applied to an object that already exists — and if it touches a
// field the API server fixes at create time, that apply is refused. Argo computes
// its diff by dry-run-applying, so the refusal produces no diff, the Application
// reads Synced, and nothing ever happens. `llz ci assert-overlay-applied` is what
// notices. This is what an operator runs afterwards.
//
// TWO ONE-SHOTS ALREADY EXIST AND THAT IS WHY THIS IS A MECHANISM. `llz ci
// prepare-apl-upgrade` annotates the apl-operator Deployment because apl-core
// 6.1.0 rewrites immutable fields of the 6.0.0 one; bootstrap-cluster deletes and
// recreates LKE's stock StorageClasses because `parameters` are immutable. Both
// are the same shape — eager, idempotent, precondition-guarded, self-retiring —
// and both were written from scratch. A third is where the shape becomes a table.
//
// THERE IS NO LEDGER, DELIBERATELY. The obvious design records "049 ran here" in a
// ConfigMap so a fleet can be surveyed. It would be a second source of truth about
// a question the cluster already answers definitively: a migration is pending iff
// the field it remedies is still undelivered, which OverlayFieldDelivered reads
// off the live object. A recorded "done" that disagrees with the object is worse
// than no record — it is the kind of state that lets a site report success while
// running the old shape.
//
// THE PRECONDITION IS THE GATE'S OWN COMPARISON, not a copy of it
// (clusterspec.OverlayFieldDelivered). A migration that thought a field was
// undelivered while the gate thought it was fine would recreate live
// infrastructure for nothing.
//
// WHO APPLIES IT. `llz ci converge` does, once per run, on the platform scope —
// the same shape as its two existing self-heals (the argocd-redis realign and the
// CRD annotation strip). A pending migration is a cluster carrying a change it
// reports as Synced; leaving that for someone to run by hand is how a fleet
// drifts. `llz ci brownfield-migrate --id <id> --yes` remains the deliberate
// single-migration path, and `--brownfield-migrate=false` opts a converge run out.
//
// WHAT MAKES THAT DEFENSIBLE RATHER THAN RECKLESS, stated so the next strategy is
// held to the same bar:
//
//   - `--cascade=orphan` leaves every pod running. What is deleted is the
//     CONTROLLER, for as long as it takes Argo to recreate it, and the pods are
//     adopted back by selector.
//   - The precondition is re-read immediately before acting, so a migration
//     someone else has already landed is a no-op.
//   - An object that is ABSENT reads as nothing-to-do, so a recreate that has not
//     completed is never re-attempted — including by the next converge run.
//   - Only strategies marked Auto run unattended. A future migration whose repair
//     is genuinely disruptive sets Auto=false, is reported, and waits for a human.
//   - The pod ROLL is never automated. See Migration.Then.
//
// The residual risk is stated rather than glossed: between the delete and Argo's
// recreate the workload has no controller. If the owning Application cannot sync,
// the pods keep serving but nothing manages them, and converge fails on the
// missing object — visibly, which is the point.

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	yaml "gopkg.in/yaml.v3"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/health"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

// Deps is what the engine cannot reach for itself: a kubectl runner and a clock.
type Deps struct {
	// Kubectl runs kubectl and returns, on success, STDOUT ONLY — and on failure
	// the diagnostic text, which is how an absent object is recognised
	// (kubectlprobe.IsAbsentText).
	//
	// STDOUT ONLY IS LOAD-BEARING, and a combined runner broke it. The success
	// output is fed to json.Unmarshal, so a single line of kubectl stderr — an
	// apiserver deprecation warning, a klog auth-plugin notice — makes the decode
	// fail, which reads as MigrationUnknown, which makes `brownfield-migrate --yes`
	// refuse to act. A migration that cannot be landed by hand because the cluster
	// printed a warning is a worse failure than the one being repaired.
	Kubectl func(args ...string) (string, bool)
	Now     func() time.Time
	Sleep   func(time.Duration)
}

// DefaultDeps is the runner both callers use.
//
// ONE CONSTRUCTOR, BECAUSE TWO HAND-BUILT ADAPTERS DIVERGED. bootstrap-cluster
// passed its combined-output runner and converge passed a stdout-only one; they
// answered differently for the same cluster, and only one of them was right. The
// process environment is what points kubectl at a cluster (see
// bootstrapcluster.PinKubeconfig), so both callers now reach it the same way and
// there is one place to be wrong.
func DefaultDeps() Deps {
	return Deps{
		Kubectl: func(args ...string) (string, bool) {
			out, err := kubectlprobe.Exec("kubectl", args...)
			if err != nil {
				return kubectlprobe.ErrText(err), false
			}
			return string(out), true
		},
		Now:   time.Now,
		Sleep: time.Sleep,
	}
}

// Migration is one brownfield step: a field that cannot be applied in place, and
// the strategy that lands it.
type Migration struct {
	// ID is the identifier the overlay field names as its remedy. One spelling,
	// two sides — clusterspec.OverlayFields()[…].Migration is the other.
	ID string
	// Strategy is how the object is made to accept the field.
	Strategy Strategy
	// Why is what stays broken while the migration is pending.
	Why string
	// Then is what the operator still has to do afterwards, in one sentence. It is
	// a field rather than a printf because a strategy that leaves work behind must
	// say so at the point it is declared, not in the log of the run that did it.
	Then string
	// Auto permits `llz ci converge` to apply this migration unattended.
	//
	// IT IS PER-MIGRATION, NOT PER-STRATEGY, and false is the safe value a zero
	// Migration takes. orphan-recreate is safe to automate for a workload whose
	// pods survive the delete; the same strategy against something whose absence
	// breaks admission, ingress or the sync that has to perform the recreate is a
	// different decision, and the person adding it makes that decision explicitly
	// rather than inheriting it from the strategy name.
	Auto bool
}

// Strategy is the shape of the recreate.
type Strategy string

const (
	// StrategyOrphanRecreate deletes the object while leaving its children
	// running, so Argo recreates it in the declared shape and adopts the pods by
	// selector. The pods keep the OLD spec until they are rolled — which is a
	// separate step, on purpose: rolling the ingest path is not something a
	// migration should do to a cluster while nobody is watching.
	StrategyOrphanRecreate Strategy = "orphan-recreate"
)

// Migrations is the registry. One entry today; the shape is the point.
func Migrations() []Migration { return migrations() }

// migrations is the seam. A package var because the ONE property that cannot be
// tested against the real table is a migration declared unsafe to automate —
// there is no such entry yet, and the arm that defers it is the safety valve for
// the next one.
var migrations = func() []Migration {
	return []Migration{{
		ID:       clusterspec.LokiWALPVCMigration,
		Strategy: StrategyOrphanRecreate,
		Why: "the ingester's WAL is on an emptyDir, so an ingester that OOMs mid-replay replays the " +
			"identical WAL on every restart and the loop cannot self-heal — the only way out is deleting the " +
			"pod, which discards un-flushed chunks. The claim template that fixes it cannot be added to a " +
			"StatefulSet that already exists, and while it is pending Argo's per-object diff also discards " +
			"every other change to that StatefulSet, including the memory limit that would stop the OOM",
		Then: "the recreated StatefulSet's template differs from the adopted pods, so its controller rolls " +
			"them itself — watch that roll rather than driving it, and expect each ingester's un-flushed " +
			"chunks to go with its pod (they are already being lost to the crash loop this repairs)",
		// AUTOMATABLE, and the reason is specific to this object rather than to the
		// strategy: deleting the StatefulSet with --cascade=orphan leaves both
		// ingesters serving and Argo recreates the object within a sync, adopting
		// them by selector. Nothing stops ingesting at that moment.
		//
		// WHAT FOLLOWS IS NOT UNDER THIS CODE'S CONTROL, and an earlier version of
		// this comment claimed otherwise: the recreated StatefulSet's pod template
		// differs from the adopted pods' revision, so the controller rolls them on its
		// own. This migration does not roll them, but it does CAUSE them to be rolled,
		// and each replacement loses that ingester's un-flushed chunks. The cost is
		// accepted here for one reason — the ingesters this repairs are
		// OOM-crashlooping, so those chunks are being lost already on every restart,
		// and the alternative is losing them indefinitely. A migration whose workload
		// is HEALTHY must not reason from this precedent.
		Auto: true,
	}}
}

// MigrationFor returns the registered migration with an id, and whether there is
// one. A field naming an id nothing registers is a wiring bug the coupling test
// catches at PR time; at runtime it must not read as "nothing to do".
func MigrationFor(id string) (Migration, bool) {
	for _, m := range Migrations() {
		if m.ID == id {
			return m, true
		}
	}
	return Migration{}, false
}

// ── what the cluster says about one migration ────────────────────────────────

// MigrationState is where one migration stands on THIS cluster.
type MigrationState int

const (
	// MigrationDone: the field is delivered. Nothing to do, and on a greenfield
	// cluster this is the state every migration is born in.
	MigrationDone MigrationState = iota
	// MigrationPending: the object exists and does not carry the field.
	MigrationPending
	// MigrationNotHere: the object does not exist — the app is not deployed on this
	// instance, so there is nothing to migrate.
	MigrationNotHere
	// MigrationUnknown: the cluster did not answer. Never "done".
	MigrationUnknown
)

func (s MigrationState) String() string {
	switch s {
	case MigrationDone:
		return "DONE"
	case MigrationPending:
		return "PENDING"
	case MigrationNotHere:
		return "NOT HERE"
	default:
		return "UNKNOWN"
	}
}

// MigrationStatus is one migration's state plus the evidence for it.
//
// ADVISORY is the field that distinguishes the two kinds of UNKNOWN. "The cluster
// did not answer" is a fact nobody can override — acting on it would be acting
// blind. "A recreate already happened here and did not deliver" is a JUDGEMENT
// about what a second attempt would achieve, and an operator who has just fixed
// the render knows something this code does not. The detail text has always told
// them to force it; without this they had nothing to force it with.
type MigrationStatus struct {
	Migration Migration
	Field     clusterspec.OverlayField
	State     MigrationState
	Detail    string
	// Declared is the overlay value this status was computed against — carried so
	// the owner check can ask whether the Application wants the same thing, without
	// resolving the path a second time and risking a different answer.
	Declared any
	// Advisory marks an UNKNOWN that --force may override. False on every state
	// that rests on a read this code could not make.
	Advisory bool
}

// ObjectLacksValue reports whether the object EXISTS and does not carry the
// declared value — the state a failed recreate leaves behind.
//
// IT IS NOT JUST PENDING, and reading it as Pending is how the durable
// anti-repeat record came to be never written. Immediately after a recreate the
// object is younger than its adopted pods, so this same code classifies it
// ADVISORY UNKNOWN — and that is precisely the moment the record has to be made,
// because minutes later the controller rolls the pods, the ages invert, and the
// object reads PENDING again with nothing to say it was already tried.
func (s MigrationStatus) ObjectLacksValue() bool {
	return s.State == MigrationPending || (s.State == MigrationUnknown && s.Advisory)
}

// readObject is the transport seam: one object as JSON, or absent/unanswered.
var readObject = func(d Deps, kind, namespace, name string) (raw []byte, absent, answered bool) {
	out, ok := d.Kubectl("-n", namespace, "get", kind, name, "-o", "json")
	if ok {
		return []byte(out), false, true
	}
	if kubectlprobe.IsAbsentText(out) {
		return nil, true, true
	}
	return nil, false, false
}

// MigrationStatuses evaluates every registered migration against the cluster.
//
// Driven from the FIELD MAP, not from the registry: a migration exists to land a
// declared field, so the field is what says whether it is needed. A registry entry
// no field names would be evaluated against nothing, which the coupling test
// refuses at PR time.
func MigrationStatuses(d Deps) []MigrationStatus {
	raw := clusterspec.AplAppRawValues()
	var out []MigrationStatus
	for _, f := range clusterspec.OverlayFields() {
		if !f.CreateOnly || f.Migration == "" {
			continue
		}
		m, ok := MigrationFor(f.Migration)
		if !ok {
			out = append(out, MigrationStatus{Field: f, State: MigrationUnknown,
				Migration: Migration{ID: f.Migration},
				Detail: fmt.Sprintf("%s names migration %q and nothing registers it — the field can never "+
					"be landed by anything here", clusterspec.OverlayFieldPath(f), f.Migration)})
			continue
		}
		out = append(out, migrationStatus(d, m, f, raw[f.App]))
	}
	return out
}

func migrationStatus(d Deps, m Migration, f clusterspec.OverlayField, rv map[string]any) MigrationStatus {
	st := MigrationStatus{Migration: m, Field: f}
	declared, ok := clusterspec.RawValue(rv, f.Value...)
	if !ok {
		st.State, st.Detail = MigrationUnknown, "the overlay declares no "+clusterspec.OverlayFieldPath(f)+
			" — there is no value for this migration to land"
		return st
	}
	st.Declared = declared
	rawObj, absent, answered := readObject(d, f.Kind, f.Namespace, f.Name)
	switch {
	case !answered:
		st.State, st.Detail = MigrationUnknown, fmt.Sprintf("could not read %s %s/%s — 'could not tell' is "+
			"not 'nothing to do'", f.Kind, f.Namespace, f.Name)
		return st
	case absent:
		st.State, st.Detail = MigrationNotHere, fmt.Sprintf("%s %s/%s does not exist here",
			f.Kind, f.Namespace, f.Name)
		return st
	}
	var live map[string]any
	if err := json.Unmarshal(rawObj, &live); err != nil {
		st.State, st.Detail = MigrationUnknown, fmt.Sprintf("%s %s/%s did not decode: %v",
			f.Kind, f.Namespace, f.Name, err)
		return st
	}
	match, delivered, readable := clusterspec.OverlayFieldDelivered(f, declared, live)
	// AFTER the comparison, not before: the pod list is only needed to decide
	// whether an UNDELIVERED field is worth a second attempt, and asking for it on
	// every converged read is an apiserver call per migration per poll that can
	// change no answer.
	// HAS THIS MIGRATION ALREADY BEEN TRIED HERE? A record written BEFORE the
	// delete answers it, and nothing else in this package answers it reliably.
	//
	// THREE ATTEMPTS AT INFERRING IT FROM THE CLUSTER FAILED, each in the same
	// direction — the repair repeating forever on an object it cannot fix. A
	// wall-clock grace could not span converge runs that are days apart. The object
	// being younger than its adopted pods was true only until the controller rolled
	// them, minutes later. An annotation on the object had to be written from the
	// failure paths, and every path that was missed (the poll-budget exit, the
	// re-check exit, the advisory-Unknown classification) put the repeat straight
	// back. The state being recorded is "we deleted this object once for this
	// migration", which the object itself cannot hold, because the delete takes it.
	//
	// So it lives in a ConfigMap, written before the delete. It records an ATTEMPT
	// and never a verdict: whether the migration is DONE is still read off the live
	// object every time, so this record cannot lie about the current state. Its
	// worst failure is refusing a retry, which --force overrides (and clears).
	var attemptedAt time.Time
	var alreadyAttempted bool
	if readable && !match {
		attemptedAt, alreadyAttempted = d.attemptRecord(m.ID)
	}
	switch {
	case !readable:
		st.State, st.Detail = MigrationUnknown, fmt.Sprintf("%s does not resolve on the live %s %s/%s — the "+
			"chart has moved what this migration aims at", strings.Join(f.Live, "."), f.Kind, f.Namespace, f.Name)
	case match:
		st.State, st.Detail = MigrationDone, fmt.Sprintf("%s carries %v", clusterspec.OverlayFieldPath(f), declared)
	case alreadyAttempted:
		st.Advisory = true
		st.State, st.Detail = MigrationUnknown, fmt.Sprintf(
			"this migration was already applied to %s %s/%s %s ago and %s is STILL %s. A recreate applies "+
				"the whole desired object, so a field missing after one is a field the chart is not "+
				"delivering; doing it again reproduces the same shape. Check the rendered values reached "+
				"apl-core, then `llz ci brownfield-migrate --id %s --yes --force` if you disagree",
			f.Kind, f.Namespace, f.Name, d.Now().Sub(attemptedAt).Round(time.Minute),
			clusterspec.OverlayFieldPath(f), delivered, m.ID)
	default:
		st.State, st.Detail = MigrationPending, fmt.Sprintf("%s declares %v; %s %s/%s has %s",
			clusterspec.OverlayFieldPath(f), declared, f.Kind, f.Namespace, f.Name, delivered)
	}
	return st
}

// ── reporting ────────────────────────────────────────────────────────────────

// ReportMigrations prints where every migration stands. Read-only, and it never
// fails: the gate that FAILS on an undelivered field is assert-overlay-applied.
// This exists so a site LEARNS what it is carrying, on every bootstrap, without
// anyone remembering to ask.
func ReportMigrations(d Deps) (pending int) {
	for _, st := range MigrationStatuses(d) {
		fmt.Printf("  %-9s %-20s %s\n", st.State, st.Migration.ID, st.Detail)
		// ObjectLacksValue, not just PENDING. Once a migration has been applied and
		// the value still has not arrived, its state is advisory UNKNOWN forever — and
		// a report that counted only PENDING went quiet at exactly the moment the
		// cluster was carrying an undelivered change nothing was going to fix.
		if !st.ObjectLacksValue() {
			continue
		}
		pending++
		fmt.Printf("      what stays broken: %s\n", st.Migration.Why)
		if st.Migration.Auto {
			fmt.Printf("      landed by:         the next `llz ci converge` (platform scope), or now with "+
				"`llz ci brownfield-migrate --id %s --yes`\n", st.Migration.ID)
		} else {
			fmt.Printf("      to land it:        llz ci brownfield-migrate --id %s --yes  (NOT applied "+
				"automatically — this migration's repair needs a human)\n", st.Migration.ID)
		}
	}
	return pending
}

// ReportMigrationsBestEffort is the bootstrap call site. It warns and never
// aborts: a pending migration is a cluster carrying an undelivered change, which
// is a state to surface, not a reason to fail the apply that is placing the
// bridge.
func ReportMigrationsBestEffort(d Deps) {
	if pending := ReportMigrations(d); pending > 0 {
		warn(fmt.Sprintf("%d brownfield migration(s) are PENDING on this cluster — an overlay change is "+
			"declared, rendered and reported Synced while the object refuses it. Run `llz ci "+
			"brownfield-migrations` for the detail.", pending))
	}
}

// ── the attempt record ───────────────────────────────────────────────────────

// AttemptsConfigMap holds one key per migration that has been APPLIED on this
// cluster, with the time it was applied.
//
// WHY A CONFIGMAP AND NOT THE OBJECT. The state being recorded is "we deleted
// this object once for this migration", and the object cannot hold it — the
// delete takes it. Everything else in this package is read off live objects
// precisely so no record can disagree with the cluster; this one cannot be,
// because the thing it remembers is an event, not a state. It records an ATTEMPT
// and never a verdict: whether the migration is DONE is still read off the object
// on every pass, so a stale record cannot make an undelivered value look
// delivered. Its worst failure is refusing a retry, which --force overrides.
//
// IN llz-observability because that namespace is LLZ-owned and precreated by
// bootstrap-cluster (managedLLZNamespaces), so the record does not depend on a
// namespace apl-core might reconcile.
const (
	AttemptsConfigMap = "llz-brownfield-attempts"
	AttemptsNamespace = "llz-observability"
)

// attemptRecord returns when this migration was last applied on this cluster.
//
// A READ THAT FAILS ANSWERS "no record", deliberately: the alternative is that an
// unreadable ConfigMap blocks every migration, which turns a missing namespace on
// a fresh cluster into a permanently inert repair. The cost of the other
// direction — one extra recreate after an unreadable read — is bounded by every
// other precondition in front of the delete.
func (d Deps) attemptRecord(id string) (time.Time, bool) {
	out, ok := d.Kubectl("-n", AttemptsNamespace, "get", "configmap", AttemptsConfigMap,
		"-o", "jsonpath={.data."+strings.ReplaceAll(id, ".", "\\.")+"}")
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(out))
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// recordAttempt writes the record BEFORE the delete, which is the whole point:
// recorded after, every failure path that forgets to write it — and three
// successive versions of this design forgot a different one — puts the unbounded
// repeat straight back.
//
// A FAILURE TO RECORD ABORTS THE MIGRATION. That is the opposite of the usual
// best-effort posture here, and deliberate: without the record this repair has no
// cross-run bound at all, and an unbounded orphan-delete of a live StatefulSet is
// worse than a repair that did not happen.
func recordAttempt(w capability.Writer, id string, now time.Time) error {
	manifest := fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: %s
data:
  %s: %q
`, AttemptsConfigMap, AttemptsNamespace, id, now.UTC().Format(time.RFC3339))
	// ONE FIELD MANAGER PER MIGRATION. Server-side apply prunes the fields a manager
	// previously owned and no longer sends, so a single shared manager writing one
	// key at a time would DELETE the other migrations' keys — reinstating the
	// unbounded repeat this record exists to stop, in the act of recording it.
	// Per-migration managers own exactly their own key, which is what SSA's
	// ownership model is for.
	if _, err := w.ApplyStdin(manifest, "llz-brownfield-"+id); err != nil {
		return fmt.Errorf("recording the attempt of %s in %s/%s: %w — refusing to delete without it, "+
			"because the record is what stops this repair repeating on an object it cannot fix",
			id, AttemptsNamespace, AttemptsConfigMap, err)
	}
	return nil
}

// clearAttempt removes the record, so a --force run starts from a clean slate and
// the next failure records itself again.
func clearAttempt(w capability.Writer, id string) {
	if _, err := w.PatchMerge(AttemptsNamespace, "configmap", AttemptsConfigMap,
		fmt.Sprintf(`{"data":{%q:null}}`, id)); err != nil {
		warn(fmt.Sprintf("could not clear the previous attempt record for %s (%v) — harmless, but the "+
			"next run may report this migration as already tried", id, err))
	}
}

// warn prints a GitHub Actions warning annotation. A copy, not an import: two
// lines of printf whose only job is to be reachable do not earn a shared symbol —
// the same call bootstrapcluster's own warn() makes.
func warn(msg string) { fmt.Printf("::warning::%s\n", msg) }

// warnOnce warns the first time a migration is skipped in a run, and stays quiet
// after.
//
// THE RETRY IS NOT SUPPRESSED, only the noise. A skipped migration is re-read on
// every poll on purpose — the thing blocking it may clear, and an earlier round
// of this work established that latching on a bad read disables the repair for a
// whole run. What must not repeat sixty times is the annotation, which is how a
// warning stops being read at all. The key is namespaced so it cannot collide
// with the attempt record in the same map.
func warnOnce(attempted map[string]bool, id, msg string) {
	key := "warned:" + id
	if attempted[key] {
		return
	}
	attempted[key] = true
	warn(msg)
}

// ── executing ────────────────────────────────────────────────────────────────

// recreateBudget/recreateInterval bound the wait for Argo to put the object back.
// Generous, because the sync that recreates it is on Argo's own reconcile cadence
// (~3m worst case) rather than on ours.
const (
	recreateBudget   = 6 * time.Minute
	recreateInterval = 10 * time.Second
)

// RunMigration executes one migration. It re-reads the precondition first: acting
// on a status computed a minute ago is how a migration recreates an object
// somebody else has already fixed.
// RunMigration is the operator's single-migration path. force overrides the
// ADVISORY refusals only — never a read that failed, and never a precondition
// about whether the object can come back.
func RunMigration(d Deps, w capability.Writer, id string, confirmed, dryRun, force bool) error {
	var target *MigrationStatus
	for _, st := range MigrationStatuses(d) {
		if st.Migration.ID == id {
			s := st
			target = &s
			break
		}
	}
	if target == nil {
		return fmt.Errorf("no brownfield migration %q — `llz ci brownfield-migrations` lists them", id)
	}
	switch target.State {
	case MigrationDone:
		fmt.Printf("%s is already applied here: %s\n", id, target.Detail)
		return nil
	case MigrationNotHere:
		fmt.Printf("%s has nothing to do here: %s\n", id, target.Detail)
		return nil
	case MigrationUnknown:
		if dryRun {
			// A DRY RUN ANSWERS "what would happen", and "it would refuse, for this
			// reason" is an answer. Erroring here made `llz --dry-run ci
			// brownfield-migrate` exit non-zero with no plan — the same conflation of a
			// refusal with a report that --yes already had one branch lower.
			fmt.Printf("%s would NOT run here: %s\n--dry-run: nothing was written.\n", id, target.Detail)
			return nil
		}
		if !target.Advisory {
			return fmt.Errorf("%s: %s — refusing to act on a cluster that did not answer", id, target.Detail)
		}
		if !force {
			return fmt.Errorf("%s: %s\n  Pass --force if you have fixed what made the last attempt fail",
				id, target.Detail)
		}
		fmt.Printf("--force: proceeding despite the state above.\n")
	}

	f := target.Field
	// THE PLAN PRINTS FIRST, AND THAT ORDER IS THE POINT. Without --yes this verb
	// is documented as "prints the plan, writes nothing" — an operator's way to see
	// what would happen. Running the preconditions ahead of it turned that
	// invocation into an error whenever the owning Application was merely mid-sync,
	// which answers a question nobody asked instead of the one they did.
	ownerOK, why := ownerCanRecreate(d, f, target.Declared)
	fmt.Printf("%s is PENDING: %s\n", id, target.Detail)
	fmt.Printf("  strategy: %s\n", target.Migration.Strategy)
	fmt.Printf("  it will:  delete %s %s/%s with --cascade=orphan (the pods keep running), then wait for "+
		"Argo to recreate it carrying %s\n", f.Kind, f.Namespace, f.Name, clusterspec.OverlayFieldPath(f))
	fmt.Printf("  after:    %s\n", target.Migration.Then)
	if !ownerOK {
		fmt.Printf("  BLOCKED:  %s\n", why)
	}
	// A DRY RUN IS NOT A MISSING --yes, and folding the two together produced
	// "refusing … without --yes" from an invocation that passed --yes. A dry run
	// asked to see the plan and got it; that is a success.
	if dryRun {
		fmt.Println("--dry-run: nothing was written.")
		return nil
	}
	if !confirmed {
		return fmt.Errorf("refusing to recreate %s %s/%s without --yes — this is a deliberate write to live "+
			"infrastructure", f.Kind, f.Namespace, f.Name)
	}
	if !ownerOK {
		return fmt.Errorf("%s: %s. Nothing was deleted", id, why)
	}

	if err := strategySupported(target.Migration); err != nil {
		return err
	}
	if force {
		// A forced run is a fresh attempt: clear the old record so this one's outcome
		// is what a later run reads.
		clearAttempt(w, id)
	}
	if err := recordAttempt(w, id, d.Now()); err != nil {
		return err
	}
	if err := recreate(d, w, target.Migration, f); err != nil {
		clearAttempt(w, id)
		return err
	}
	fmt.Printf("→ deleted %s %s/%s; its pods are still running and will be adopted by the recreated object.\n",
		f.Kind, f.Namespace, f.Name)

	deadline := d.Now().Add(recreateBudget)
	for {
		st := migrationStatus(d, target.Migration, f, clusterspec.AplAppRawValues()[f.App])
		if st.State == MigrationDone {
			fmt.Printf("→ %s recreated and carrying the declared value: %s\n", f.Kind, st.Detail)
			fmt.Printf("\nSTILL TO DO: %s\n", target.Migration.Then)
			return nil
		}
		if !d.Now().Before(deadline) {
			// The object is gone, or back without the value. Say exactly that: the
			// recovery is Argo's to perform and an operator has to know it is owed.
			//
			return fmt.Errorf("%s %s/%s has not come back carrying %s within %s (state: %s) — Argo owns "+
				"the recreate, so check the owning Application synced. The pods keep serving throughout",
				f.Kind, f.Namespace, f.Name, clusterspec.OverlayFieldPath(f), recreateBudget, st.State)
		}
		d.Sleep(recreateInterval)
	}
}

// ownerCanRecreate reports whether the Argo Application that declares this
// object is in a state to put it back, and why not when it is not.
//
// THIS IS THE GUARD THAT MAKES THE DELETE REVERSIBLE. Everything else here is
// bounded by "the object still lacks the field"; once it is deleted, ABSENT reads
// as nothing-to-do and no later run retries — so if Argo cannot sync at the
// moment of the delete, the workload is left with no controller and nothing to
// recreate it. Converge's two wedge signals (a repo-server cache auth split, an
// annotation-limit wedge) cover the cases where NO app can sync; this covers the
// one that matters, which is whether THIS app can.
//
// FAIL-CLOSED, AND NOT ON HEALTH. A missing Application, an unreadable one, a
// spec error, or a sync status that is not Synced all defer the migration. Health
// is deliberately not consulted: the object this migration exists for is
// Progressing precisely BECAUSE the value has not landed, so requiring Healthy
// would refuse to repair exactly the cluster that needs it.
func ownerCanRecreate(d Deps, f clusterspec.OverlayField, declared any) (ok bool, why string) {
	if f.OwnerApp == "" {
		return false, "the field map names no owning Application for " + clusterspec.OverlayFieldPath(f) +
			", so nothing here can say whether the object would be recreated"
	}
	out, got := d.Kubectl("-n", clusterspec.ArgoNamespace, "get", "application.argoproj.io", f.OwnerApp, "-o", "json")
	if !got {
		return false, fmt.Sprintf("could not read Application %s/%s (%s) — 'could not tell' is not "+
			"'safe to delete'", clusterspec.ArgoNamespace, f.OwnerApp, health.FirstLine(out))
	}
	app, err := health.ParseArgoApp([]byte(out))
	if err != nil {
		return false, fmt.Sprintf("Application %s did not parse: %v", f.OwnerApp, err)
	}
	if app.SpecErr != "" {
		return false, fmt.Sprintf("Application %s cannot compute its target state (%s) — it could not "+
			"recreate what this migration deletes", f.OwnerApp, health.FirstLine(app.SpecErr))
	}
	if app.Sync != "Synced" {
		return false, fmt.Sprintf("Application %s is %s, not Synced — deleting an object it is not currently "+
			"applying risks leaving it deleted", f.OwnerApp, app.Sync)
	}

	var policy ownerPolicy
	if err := json.Unmarshal([]byte(out), &policy); err != nil {
		return false, fmt.Sprintf("Application %s: could not read its sync policy or desired values: %v",
			f.OwnerApp, err)
	}
	// SELFHEAL IS WHAT PUTS A DELETED OBJECT BACK, and nothing else does. `automated`
	// syncs when the DESIRED state changes; a cluster-side deletion is drift, and
	// Argo corrects drift only with selfHeal. apl-core owns this Application's
	// syncPolicy — LLZ does not set it — so a chart change could turn it off, and
	// then this migration's delete would be permanent: the object stays gone, ABSENT
	// reads as nothing-to-do, and no later run retries. Checked rather than assumed
	// on the strength of what it happens to be today.
	if !policy.selfHealing() {
		return false, fmt.Sprintf("Application %s does not self-heal (syncPolicy.automated.selfHeal is not "+
			"true) — Argo would not put back what this migration deletes, so the delete would be permanent",
			f.OwnerApp)
	}
	// AND THE DESIRED STATE HAS TO ALREADY CARRY THE VALUE. This is what stops a
	// repair that cannot work from being retried on every converge run forever: if
	// apl-core has not yet rendered the overlay into this Application, deleting the
	// object recreates it in the SAME shape, the field is still missing, and the
	// next run deletes it again. The object being absent bounds the repair within a
	// run; this is what bounds it across runs.
	docs := policy.valueDocs()
	if len(docs) == 0 {
		// NO VALUES DOCUMENT AT ALL is not "the Application does not want this" — it
		// is "this guard cannot see what it wants", and the two must not share an
		// answer. An Application built from a shape not read here (a plugin, a
		// kustomize source, a values file this guard does not fetch) would otherwise
		// leave every migration permanently deferred behind a warning nobody reads,
		// and the repair silently inert — which is this PR's own failure mode.
		why := "no spec.source.helm.values/valuesObject/parameters and no spec.sources[] with one"
		if unreadable := policy.unreadableSources(); len(unreadable) > 0 {
			why = "its values live in " + strings.Join(unreadable, "; ") + ", which are files in the source " +
				"repo rather than fields on the Application"
		}
		return false, fmt.Sprintf("Application %s carries no Helm values this check can read (%s) — the "+
			"guard cannot tell whether recreating would deliver %s, so it will not delete. If apl-core has "+
			"changed how it renders, ownerPolicy.valueDocs needs the new shape",
			f.OwnerApp, why, clusterspec.OverlayFieldPath(f))
	}
	if got, ok := desiredValue(docs, f.Value); !ok {
		// A SHAPE THIS GUARD CANNOT READ IS NOT AN ABSENT VALUE, and conflating them
		// is definitive in the wrong direction: an Application with BOTH inline values
		// and valueFiles has documents to read, so the len(docs)==0 branch above never
		// fires, and "the overlay has not reached apl-core's render yet" would be
		// asserted about a file this code never opened.
		if unreadable := policy.unreadableSources(); len(unreadable) > 0 {
			return false, fmt.Sprintf("Application %s does not declare %s in the values this guard can "+
				"read, and it also carries %s — files in the source repo that this cannot open, so "+
				"\"not declared\" cannot be concluded", f.OwnerApp, clusterspec.OverlayFieldPath(f),
				strings.Join(unreadable, "; "))
		}
		return false, fmt.Sprintf("Application %s does not yet declare %s in its rendered values — "+
			"recreating the object would reproduce the same shape. The overlay has not reached apl-core's "+
			"render yet", f.OwnerApp, clusterspec.OverlayFieldPath(f))
	} else if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", declared) {
		return false, fmt.Sprintf("Application %s declares %s = %v, but this migration is landing %v — "+
			"recreating would deliver the Application's value, not the overlay's",
			f.OwnerApp, clusterspec.OverlayFieldPath(f), got, declared)
	}
	return true, ""
}

// ownerPolicy is the slice of an Application this guard reads beyond what
// health.ParseArgoApp exposes: whether it self-heals, and what it actually wants.
type ownerPolicy struct {
	Spec struct {
		SyncPolicy struct {
			Automated *struct {
				SelfHeal bool `json:"selfHeal"`
			} `json:"automated"`
		} `json:"syncPolicy"`
		// AN APPLICATION CARRIES ITS VALUES IN FOUR PLACES, and reading one of them
		// is how this guard would go quietly inert: a `sources[]` (multi-source) or
		// `valuesObject` Application yields nothing, every migration defers behind a
		// warning, and converge stays green having repaired nothing. apl-core owns
		// this shape and can change it in a chart release without telling anyone.
		Source  appSource   `json:"source"`
		Sources []appSource `json:"sources"`
	} `json:"spec"`
}

type appSource struct {
	Helm struct {
		Values string `json:"values"`
		// valuesObject is the structured form; Argo accepts either.
		ValuesObject map[string]any `json:"valuesObject"`
		// parameters are --set equivalents: dotted paths with scalar values, which
		// is a shape this guard can resolve without the chart.
		Parameters []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"parameters"`
		// valueFiles NAMES files this guard cannot read — they live in the source
		// repo, not in the Application. Recorded so an Application that carries only
		// those is reported as unreadable-by-this-guard rather than as an Application
		// that does not want the value: the first defers with a reason someone can
		// act on, the second would be a lie about apl-core's intent.
		ValueFiles []string `json:"valueFiles"`
	} `json:"helm"`
}

// valueDocs returns every values document this Application carries, in the order
// Argo would layer them.
func (p ownerPolicy) valueDocs() []map[string]any {
	var out []map[string]any
	// ASCENDING PRECEDENCE, which desiredValue reads as "last match wins": within
	// one source `valuesObject` is the structured form and takes precedence over
	// the string, so it goes last. Argo discourages setting both; a caller that
	// does gets the one Argo would render rather than whichever this loop saw
	// first.
	add := func(s appSource) {
		if doc, ok := parseValues(s.Helm.Values); ok {
			out = append(out, doc)
		}
		if len(s.Helm.ValuesObject) > 0 {
			out = append(out, s.Helm.ValuesObject)
		}
		if doc := parametersDoc(s.Helm.Parameters); doc != nil {
			out = append(out, doc)
		}
	}
	add(p.Spec.Source)
	for _, s := range p.Spec.Sources {
		add(s)
	}
	return out
}

func (p ownerPolicy) selfHealing() bool {
	a := p.Spec.SyncPolicy.Automated
	return a != nil && a.SelfHeal
}

// desiredValue resolves a declared overlay path inside an Application's rendered
// Helm values — the chart-shaped document, which is keyed exactly as the overlay
// keys its _rawValues, because that is where those values came from.
//
// THE LAST DOCUMENT THAT CARRIES THE PATH WINS, because valueDocs emits them in
// ASCENDING precedence and that is the one Argo will render. Taking the first
// would approve a delete whose recreate then delivers a different value — the
// across-runs bound this guard exists to provide, inverted into a StatefulSet
// re-deleted on every converge run.
func desiredValue(docs []map[string]any, path []string) (any, bool) {
	var found any
	var ok bool
	for _, doc := range docs {
		if v, has := clusterspec.RawValue(doc, path...); has {
			found, ok = v, true
		}
	}
	return found, ok
}

// parametersDoc turns `helm.parameters` (dotted name, scalar value) into the
// nested document the rest of this guard walks. Highest precedence in Argo, so
// valueDocs appends it last.
func parametersDoc(params []struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}) map[string]any {
	if len(params) == 0 {
		return nil
	}
	doc := map[string]any{}
	for _, p := range params {
		parts := splitHelmPath(p.Name)
		cur := doc
		ok := true
		for _, seg := range parts[:len(parts)-1] {
			next, isMap := cur[seg].(map[string]any)
			if !isMap {
				if _, taken := cur[seg]; taken {
					// `a` is already a scalar and `a.b` wants it to be a map. Helm would
					// reject the pair; overwriting silently would drop the scalar from the
					// document this guard compares against, which is a value it would then
					// report as undeclared.
					ok = false
					break
				}
				next = map[string]any{}
				cur[seg] = next
			}
			cur = next
		}
		if ok {
			cur[parts[len(parts)-1]] = p.Value
		}
	}
	return doc
}

// splitHelmPath splits a `--set`-style key on unescaped dots. Helm lets a key
// contain a literal dot as `\.`, and splitting on every dot turns one such key
// into a nested structure that matches nothing.
func splitHelmPath(name string) []string {
	var parts []string
	var cur strings.Builder
	for i := 0; i < len(name); i++ {
		switch {
		case name[i] == '\\' && i+1 < len(name) && name[i+1] == '.':
			cur.WriteByte('.')
			i++
		case name[i] == '.':
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(name[i])
		}
	}
	return append(parts, cur.String())
}

// unreadableSources names the value shapes this guard can see but not resolve, so
// the deferral says which one is in the way.
func (p ownerPolicy) unreadableSources() []string {
	var out []string
	for _, s := range append([]appSource{p.Spec.Source}, p.Spec.Sources...) {
		if len(s.Helm.ValueFiles) > 0 {
			out = append(out, "helm.valueFiles ("+strings.Join(s.Helm.ValueFiles, ", ")+")")
		}
	}
	return out
}

func parseValues(values string) (map[string]any, bool) {
	if strings.TrimSpace(values) == "" {
		return nil, false
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(values), &doc); err != nil {
		return nil, false
	}
	return doc, true
}

// recreate performs one migration's repair, DISPATCHING ON ITS STRATEGY.
//
// THE SWITCH IS THE POINT, not ceremony. Strategy was declared, printed in the
// plan, and then never read — both callers issued an orphan delete
// unconditionally, so a second strategy (or a zero-value one on a hand-built
// Migration) would silently get the StatefulSet treatment. The `default` arm
// refusing is what makes adding a strategy a decision rather than an omission:
// the new one does not work until someone writes what it does.
// strategySupported reports whether anything here implements this migration's
// repair. Asked BEFORE the attempt is recorded: recording an attempt for a repair
// that is then refused would block a later legitimate one, on the strength of
// something that never touched the cluster.
func strategySupported(m Migration) error {
	if m.Strategy == StrategyOrphanRecreate {
		return nil
	}
	return fmt.Errorf("%s declares strategy %q, which nothing here implements — refusing to guess at "+
		"a repair for a live object", m.ID, m.Strategy)
}

func recreate(d Deps, w capability.Writer, m Migration, f clusterspec.OverlayField) error {
	switch m.Strategy {
	case StrategyOrphanRecreate:
		if err := createOnlyStillHolds(d, f); err != nil {
			return fmt.Errorf("%s: %w", m.ID, err)
		}
		if _, err := w.DeleteOrphan(f.Namespace, f.Kind, f.Name); err != nil {
			return fmt.Errorf("%s: deleting %s %s/%s (--cascade=orphan): %w",
				m.ID, f.Kind, f.Namespace, f.Name, err)
		}
		return nil
	default:
		return fmt.Errorf("%s declares strategy %q, which nothing here implements — refusing to guess at "+
			"a repair for a live object", m.ID, m.Strategy)
	}
}

// ── the claim, checked against the cluster it is about to act on ─────────────

// migrationProbe server-dry-run-patches the LIVE object. Its own seam, and not
// readObject's, for the reason the appliability lane keeps its two apart: a test
// double installed for a read must not answer for a write-shaped probe.
var migrationProbe = func(d Deps, f clusterspec.OverlayField, patch string) (out string, accepted bool) {
	args := migrationProbeArgs(f, patch)
	// THE DRY-RUN FLAG IS CHECKED, NOT ASSUMED. This argv is one token away from a
	// real write against whatever cluster the kubeconfig points at, and unlike the
	// sibling probe in assertplatform it does not pass through a declared
	// cluster-read handle that would refuse it. A local invariant is the cheaper
	// half of the same guarantee: a later edit that drops the flag fails here rather
	// than mutating a live StatefulSet from the middle of a safety check.
	for _, a := range args {
		if a == "--dry-run=server" {
			return d.Kubectl(args...)
		}
	}
	return "migrationProbe built a patch argv with no --dry-run=server; refusing to run it", false
}

// migrationProbeArgs is the argv, in one place so a test can assert what it
// contains without shelling out.
func migrationProbeArgs(f clusterspec.OverlayField, patch string) []string {
	return []string{"-n", f.Namespace, "patch", f.Kind, f.Name,
		"--dry-run=server", "-o", "json", "-p", patch}
}

// ProbeInconclusive marks a refusal to delete that came from not being able to
// ASK, rather than from an answer.
//
// IT EXISTS SO ONE EXTRA APISERVER CALL CANNOT TURN A POLL INTO A FAILURE. Before
// this check, a 5xx or a webhook timeout on the migration path simply meant the
// delete was attempted; now it means the delete is refused, and reporting that as
// an ERROR would fail a whole converge run on a blip — a new red-flake surface on
// the e2e path, introduced by a guard meant to make things safer. Converge is a
// poll loop: "could not ask" is a reason to look again, not a reason to stop. A
// permission denial or an unclassified refusal is NOT this — those are answers,
// and they stay fatal.
type ProbeInconclusive struct{ Err error }

func (p ProbeInconclusive) Error() string { return p.Err.Error() }
func (p ProbeInconclusive) Unwrap() error { return p.Err }

// IsProbeInconclusive reports whether an error is the could-not-ask kind.
func IsProbeInconclusive(err error) bool {
	var p ProbeInconclusive
	return errors.As(err, &p)
}

// createOnlyStillHolds asks the apiserver whether this field really is fixed at
// create time — on the real object, immediately before deleting it.
//
// THE PR-TIME GATE CANNOT COVER THIS, and that is the whole reason it exists.
// `llz ci assert-overlay-appliability` asks a kind apiserver whether each
// CreateOnly claim is true, which protects the NEXT change to the field map. An
// instance runs whatever table shipped in its binary, so a row that was wrong when
// it shipped reaches a cluster that never sees the gate — and "the guard would
// have caught it" is not something a cluster can check. UnmappedOverlayPaths two
// files over is deliberately run at BOTH times for exactly this reason; the claim
// that DELETES A LIVE STATEFULSET had only the PR-time half.
//
// IT IS THE SAME QUESTION, ASKED WHERE IT IS ACTED ON. A brownfield object exists
// and lacks the field, so dry-run-patching it with the declared value IS the
// appliability probe. If the apiserver accepts, the value could have been patched
// in and the delete is a destructive repair for a problem that does not exist.
//
// FAIL-CLOSED, AND THE ASYMMETRY IS THE ARGUMENT. Refusing costs a migration that
// does not run, with a message naming why, which an operator can act on. Deleting
// costs a live workload's controller on the strength of a claim nothing verified,
// which nobody can undo. So an unverifiable probe refuses too.
func createOnlyStillHolds(d Deps, f clusterspec.OverlayField) error {
	rv, ok := clusterspec.AplAppRawValues()[f.App]
	if !ok {
		return fmt.Errorf("the overlay declares no _rawValues for app %s, so this migration's CreateOnly "+
			"claim cannot be checked against the cluster — refusing to delete %s %s/%s on a claim "+
			"nothing can confirm", f.App, f.Kind, f.Namespace, f.Name)
	}
	declared, ok := clusterspec.RawValue(rv, f.Value...)
	if !ok {
		return fmt.Errorf("the overlay declares no %s, so this migration's CreateOnly claim cannot be "+
			"checked against the cluster — refusing to delete %s %s/%s on a claim nothing can confirm",
			clusterspec.OverlayFieldPath(f), f.Kind, f.Namespace, f.Name)
	}
	patch, err := f.Patch(rv)
	if err != nil {
		return fmt.Errorf("could not build the probe that would confirm %s is create-only (%v) — "+
			"refusing to delete %s %s/%s on a claim nothing can confirm",
			clusterspec.OverlayFieldPath(f), err, f.Kind, f.Namespace, f.Name)
	}
	// THE PROBE MUST BE ABOUT THIS ROW'S FIELD, and the PR-time lane says why in the
	// same words: a StatefulSet's immutability refusal is a WHOLE-SPEC message,
	// byte-identical for any non-whitelisted spec key, so a CREATE-ONLY verdict on
	// its own establishes "the patch touched sts.spec outside the mutable whitelist",
	// not "this field is create-only". Without this, a patch aimed at an unrelated
	// key drew the identical refusal and CLEARED the delete — the same false green
	// the lane closes, reproduced here because only one half had the check.
	if err := clusterspec.PatchTargetsField(patch, f, declared); err != nil {
		return fmt.Errorf("cannot confirm %s is create-only before deleting %s %s/%s: %w. "+
			"Whatever the apiserver says about that patch is evidence about a DIFFERENT field",
			clusterspec.OverlayFieldPath(f), f.Kind, f.Namespace, f.Name, err)
	}
	out, accepted := migrationProbe(d, f, patch)
	if accepted {
		return fmt.Errorf("%s is declared CreateOnly, but this cluster's apiserver ACCEPTED the change "+
			"as an ordinary patch to the existing %s %s/%s. Deleting it would recreate a live workload's "+
			"controller to land a value a patch would have landed. Refusing. Either the field map's "+
			"CreateOnly is wrong for this apiserver version, or the row's Patch is not sending what it "+
			"claims — `llz ci assert-overlay-appliability` is the PR-time half of this question",
			clusterspec.OverlayFieldPath(f), f.Kind, f.Namespace, f.Name)
	}
	if !health.IsImmutableFieldRejection(out) {
		err := fmt.Errorf("could not confirm %s is create-only before deleting %s %s/%s: the apiserver "+
			"refused the probe for a reason this does not classify as immutability, which may be a "+
			"permission or transport fault rather than a verdict about the field. 'Could not tell' is "+
			"not 'go ahead and delete'. The apiserver said: %s",
			clusterspec.OverlayFieldPath(f), f.Kind, f.Namespace, f.Name, health.RefusalText(out))
		// A BLIP IS NOT A VERDICT. Marked inconclusive so converge looks again on the
		// next poll instead of failing the run; a 403 or any other answer stays fatal.
		if health.IsTransientFetchError(out) {
			return ProbeInconclusive{Err: err}
		}
		return err
	}
	return nil
}

// ── the unattended path ──────────────────────────────────────────────────────

// ApplyPending performs the recreate step of every PENDING migration marked Auto,
// and returns what it did, what it deliberately left, and what went wrong.
//
// IT DOES NOT WAIT. RunMigration blocks for the recreate because an operator ran
// it and is owed an answer; converge is already a poll loop whose whole job is to
// watch the cluster reach a state, so blocking one of its polls for six minutes
// would buy nothing and hide the wait from the report. The delete happens here;
// the recreate is observed by the polls that follow.
//
// SAFE TO CALL ON EVERY RUN, and it has to be: converge runs repeatedly, on
// clusters in every state. A migration whose object is already right reads DONE; a
// recreate still in flight leaves the object ABSENT, which reads NOT HERE and is
// never re-deleted; a cluster that did not answer reads UNKNOWN and is never acted
// on. Only PENDING — the object exists and demonstrably lacks the field — is
// touched.
// ApplyResult is what one unattended pass did, and — the field that matters to a
// caller deciding whether to try again — what it could not determine.
type ApplyResult struct {
	// Applied names the migrations whose repair was performed.
	Applied []string
	// Deferred names PENDING migrations that are not cleared to run unattended.
	Deferred []string
	// Inconclusive names migrations whose state the cluster did not answer for. A
	// caller must not read an empty Applied plus a non-empty Inconclusive as "there
	// was nothing to do".
	Inconclusive []string
	// NotHere names migrations whose object does not exist on this cluster. It is
	// reported separately because "not here" is not permanent: an object can arrive
	// mid-run — Argo recreating one from an earlier attempt is the obvious way — and
	// a caller that latched "nothing to do" on it would stop looking.
	NotHere []string
	// Errs are the repairs that were attempted and failed.
	Errs []error
}

// attempted is the caller's record of which migrations this RUN has already
// tried, and the caller OWNS it: pass the same non-nil map on every pass of a run.
//
// A NIL MAP IS ACCEPTED AND RECORDS NOTHING — Go cannot grow a nil map through a
// parameter — so a caller that passes nil each time gets no memory and will
// re-delete on every poll. That is safe for a single one-shot call (the operator
// path) and wrong for a loop, which is why converge allocates one map per run.
// Said plainly here because the previous wording claimed the opposite, on a
// function that deletes live objects.
//
// PER MIGRATION, NOT A SINGLE FLAG. A global "migrations handled" latch has to be
// set on one of two occasions and is wrong on both: set it when anything was
// attempted and one unreadable migration disables every other for the run; set it
// only on a fully conclusive pass and a second migration's bad read leaves the
// first free to be orphan-deleted on every poll of the budget. Recording the ids
// makes "once per run" literally what the comment says, whatever the registry
// grows to.
func ApplyPending(d Deps, w capability.Writer, attempted map[string]bool) ApplyResult {
	var r ApplyResult
	if attempted == nil {
		attempted = map[string]bool{}
	}
	for _, st := range MigrationStatuses(d) {
		if attempted[st.Migration.ID] {
			continue
		}
		if st.State == MigrationNotHere {
			r.NotHere = append(r.NotHere, st.Migration.ID)
			continue
		}
		if st.State == MigrationUnknown {
			// THE CLUSTER DID NOT ANSWER, or answered something this engine will not act
			// on — neither of which is "nothing to do". The caller needs to know,
			// because one that latches "migrations handled" on this pass would disable
			// them for the rest of its run on the strength of a failed read.
			//
			// AND THE DETAIL IS PRINTED, not just counted: "inconclusive" on its own
			// tells an operator that something was skipped and nothing about what to do,
			// which is how a warning becomes noise.
			r.Inconclusive = append(r.Inconclusive, st.Migration.ID)
			warnOnce(attempted, st.Migration.ID, fmt.Sprintf("brownfield migration %s not applied: %s",
				st.Migration.ID, st.Detail))
			continue
		}
		if st.State != MigrationPending {
			continue
		}
		if !st.Migration.Auto {
			r.Deferred = append(r.Deferred, st.Migration.ID)
			warnOnce(attempted, st.Migration.ID,
				fmt.Sprintf("brownfield migration %s is PENDING and is NOT applied automatically — %s. "+
					"Run `llz ci brownfield-migrate --id %s --yes` when you have a window.",
					st.Migration.ID, st.Migration.Why, st.Migration.ID))
			continue
		}
		f := st.Field
		if canRecreate, why := ownerCanRecreate(d, f, st.Declared); !canRecreate {
			// NOT an attempt and NOT a failure: the cluster is not ready for this
			// repair, and a later poll — or a later run — should ask again. Recorded
			// as inconclusive so the caller does not read it as "nothing to do".
			r.Inconclusive = append(r.Inconclusive, st.Migration.ID)
			warnOnce(attempted, st.Migration.ID, fmt.Sprintf("brownfield migration %s deferred: %s",
				st.Migration.ID, why))
			continue
		}
		fmt.Printf("::notice::applying brownfield migration %s: %s\n", st.Migration.ID, st.Detail)
		// RECORDED BEFORE THE ATTEMPT, not after. A repair that fails must not be
		// retried on the next poll: the failure was the apiserver refusing a delete,
		// and asking again inside the same run adds nothing but another write.
		attempted[st.Migration.ID] = true
		if err := strategySupported(st.Migration); err != nil {
			r.Errs = append(r.Errs, err)
			continue
		}
		if err := recordAttempt(w, st.Migration.ID, d.Now()); err != nil {
			r.Errs = append(r.Errs, err)
			continue
		}
		if err := recreate(d, w, st.Migration, f); err != nil {
			if IsProbeInconclusive(err) {
				// The cluster did not answer the pre-delete probe. Nothing was deleted, so
				// the attempt record must not stand — and this is a poll, not a failure.
				clearAttempt(w, st.Migration.ID)
				r.Inconclusive = append(r.Inconclusive, st.Migration.ID)
				fmt.Printf("::notice::brownfield migration %s deferred: %v\n", st.Migration.ID, err)
				continue
			}
			// THE RECORD SAYS "WE DELETED THIS OBJECT", so a delete that did not happen
			// must not leave one. Without this, a transient refusal — a 5xx, a webhook,
			// an RBAC blip — permanently disables the repair behind a message claiming
			// it was already applied.
			clearAttempt(w, st.Migration.ID)
			r.Errs = append(r.Errs, err)
			continue
		}
		r.Applied = append(r.Applied, st.Migration.ID)
		fmt.Printf("→ %s %s/%s deleted with --cascade=orphan; its pods are still running, and Argo recreates "+
			"the object carrying %s. STILL TO DO AFTERWARDS: %s\n",
			f.Kind, f.Namespace, f.Name, clusterspec.OverlayFieldPath(f), st.Migration.Then)
	}
	return r
}
