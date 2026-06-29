package main

// lifecycle.go is the single source of truth for the LLZ lifecycle (issue #10).
//
// Core owns the lifecycle. Extensions contribute typed artifacts to its phases.
// Core commands fire the phases. Everything that used to carry its own lifecycle
// vocabulary — the CI anchor spine (extension_ci.go), the reconcile contributions
// (extension_reconcile.go), the runLint/runUpgrade tails — now DERIVES from this
// registry, so the tables cannot drift independently (lifecycle_test.go enforces it).
//
// The delivery methodology (docs/delivery-methodology.md) has eight numbered phases
// run by five engines (the Runner enum). Gate and Decommission are code-only phases the
// methodology does not number: Gate is the pre-bootstrap verification subphase (check:
// execution), Decommission the teardown arc (the inverses of scaffold and seed). They
// live here — tracked, ordered, typed — rather than surviving as untracked vocabulary
// words in a local comment.
//
// An extension touches a phase through one of TWO disjoint registers:
//   - a HookKind — a declarative artifact FIRED idempotently by the phase (a
//     Contribution); safe to run unattended under reconcile.
//   - an Action — an imperative, usually cloud-mutating day-2/maintenance operation
//     (seed, rotate, upgrade, unseed, teardown) run ONLY via a gated operator command
//     or a cadence workflow, and NEVER fired by reconcile.
// Keeping Actions as their own typed register (rather than smuggling seed/rotate back
// into HookKind) is what makes the "never reconciled" safety boundary something the
// registry states and lifecycle_test.go enforces, instead of an absence.

import (
	"fmt"
	"os"
)

// HookKind is the finite, typed set of declarative artifacts an extension may
// contribute to a phase — the WHAT ceiling. There is no open/arbitrary callback hook.
// Cloud-mutating day-2 work (seed, rotate, upgrade) is deliberately NOT a hook kind:
// it lives in the Action register below, never fired by unattended reconcile.
type HookKind string

const (
	HookConfig   HookKind = "config"   // vars/secrets declared (readiness report)
	HookFiles    HookKind = "files"    // artifacts rendered into the instance
	HookCheck    HookKind = "check"    // lint-tier verification steps (the Gate; missing tool skips)
	HookValidate HookKind = "validate" // CI-tier heavyweight validation (checkov/conftest; tools REQUIRED)
	HookCI       HookKind = "ci"       // generated workflow jobs, anchored to a core job
	HookHealth   HookKind = "health"   // report-only health probe surfaced by doctor/status
	HookCommands HookKind = "commands" // live operator CLI registration
)

// Posture is how a hook's failure is treated when its phase fires. Defined once here
// so decisions like "lint skips load errors" or "upgrade tolerates a bad files: apply"
// are a named policy, not an ad hoc local comment. The posture on the hook is the
// DEFAULT; a caller may deliberately downgrade (e.g. runUpgrade runs files best-effort)
// — but the downgrade is then an explicit, documented choice, never a silent one.
type Posture string

const (
	PostureBlocking   Posture = "blocking"    // a failure fails the command
	PostureReport     Posture = "report-only" // advisory; never fails its caller
	PostureBestEffort Posture = "best-effort" // warns and continues; does not abort
)

// FiredBy names the mechanism that drives a hook when its phase fires. It is policy
// (TestFiredByMatchesContributions enforces it), so it is a typed enum like every other
// axis (HookKind / Action / Trigger / Runner / Posture), not a bare string.
type FiredBy string

const (
	FiredByReconcile FiredBy = "reconcile" // a Contribution (config/files/check/ci); unattended-safe
	FiredByValidate  FiredBy = "validate"  // lifecycleValidate (heavyweight CI-tier gate, runValidate)
	FiredByDoctor    FiredBy = "doctor"    // lifecycleHealth (report-only, runDoctor/status)
	FiredByStartup   FiredBy = "startup"   // registered at CLI start (commands), not driven by a phase
)

// HookMeta is the failure semantics for one hook kind, in one place.
//
//   - check/ci are blocking (a failing check or workflow drift fails the gate).
//   - files is blocking when explicitly invoked (extension apply); runUpgrade
//     deliberately downgrades it to best-effort so a misconfigured extension cannot
//     abort an upgrade.
//   - config/doctor is report-only unless called as the strict doctor gate.
//   - ToolSkip is true only for hooks that may skip on a missing external tool; a
//     remote source/load/cache trust failure is NOT a tool-skip and must not silently
//     remove a blocking hook (it warns loudly instead).
type HookMeta struct {
	Kind     HookKind
	Posture  Posture
	ToolSkip bool
	// FiredBy is WHAT drives this hook when its phase fires — in the table, not folklore.
	FiredBy FiredBy
	// MayMutate records that this hook is permitted to cloud-mutate. Only `ci` is — and
	// ONLY at workflow runtime (a reviewed post-converge deploy), NEVER in reconcile,
	// which merely GENERATES the workflow. So the honest safety line is "reconcile-safe",
	// not "no cloud mutation": Actions stay out of reconcile, but a ci: deploy step
	// legitimately mutates in its generated Actions job (ohttp/akamai-functions).
	MayMutate bool
	Summary   string
}

var hookKinds = []HookMeta{
	{Kind: HookConfig, Posture: PostureReport, FiredBy: FiredByReconcile,
		Summary: "declared vars/secrets readiness report (doctor owns the failing gate)"},
	{Kind: HookFiles, Posture: PostureBlocking, FiredBy: FiredByReconcile,
		Summary: "render artifacts into the instance (upgrade downgrades to best-effort)"},
	{Kind: HookCheck, Posture: PostureBlocking, ToolSkip: true, FiredBy: FiredByReconcile,
		Summary: "lint-tier gate — a missing tool skips, a failing check blocks"},
	{Kind: HookValidate, Posture: PostureBlocking, ToolSkip: false, FiredBy: FiredByValidate,
		Summary: "CI-tier validation (checkov/conftest); tools REQUIRED (a missing tool fails, not skips); runValidate, not pre-commit"},
	{Kind: HookCI, Posture: PostureBlocking, FiredBy: FiredByReconcile, MayMutate: true,
		Summary: "generated workflow jobs; --check is the drift gate; a ci: job may cloud-mutate at workflow runtime (deploy)"},
	{Kind: HookHealth, Posture: PostureReport, ToolSkip: true, FiredBy: FiredByDoctor,
		Summary: "report-only health probe surfaced by doctor/status — never a blocking gate"},
	{Kind: HookCommands, Posture: PostureReport, FiredBy: FiredByStartup,
		Summary: "operator CLI registration at startup (addExtCommands), not fired by a phase"},
}

func hookMeta(k HookKind) (HookMeta, bool) {
	for _, h := range hookKinds {
		if h.Kind == k {
			return h, true
		}
	}
	return HookMeta{}, false
}

// fired reports whether the lifecycle drives this hook at all (everything but startup).
func (m HookMeta) fired() bool { return m.FiredBy != FiredByStartup }

// applyPosture maps a phase/hook error onto the caller's outcome under posture p, so
// the blocking / best-effort / report-only decision is READ from the registry's Posture
// values instead of being re-encoded ad hoc at each call site (which let Posture rot
// into decoration). Blocking propagates the error; best-effort logs it and continues;
// report-only swallows it silently. The entry points below route through this, so
// flipping a hook's Posture in the table actually changes behavior.
func applyPosture(p Posture, label string, err error) error {
	if err == nil {
		return nil
	}
	switch p {
	case PostureBestEffort:
		fmt.Fprintf(os.Stderr, "llz: %s (best-effort): %v\n", label, err)
		return nil
	case PostureReport:
		return nil
	default: // PostureBlocking
		return err
	}
}

// Action is a typed, finite day-2 / maintenance operation an extension can be the
// subject of — the imperative companion to HookKind. Unlike a hook it is NEVER fired
// by unattended reconcile: it is cloud-mutating or stateful, runs only via an explicit
// gated operator command or a cadence workflow, and is recorded here for legibility
// and audit, not for firing. The Action strings are kept disjoint from the HookKind
// strings (TestActionsAreNotHooks) so the two registers can never be confused.
type Action string

const (
	ActionSeed     Action = "seed"     // wire declared secrets into their stores (OpenBao / GH env)
	ActionRotate   Action = "rotate"   // mint a fresh token and re-seed it (TokenRotator)
	ActionUpgrade  Action = "upgrade"  // migrate manifest schema + re-apply files + emit copier/renovate wiring
	ActionUnseed   Action = "unseed"   // revoke seeded secrets from their stores (the inverse of seed)
	ActionTeardown Action = "teardown" // remove an extension's scaffolded files (the inverse of scaffold)
)

// ActionMeta records, in one place, how a day-2 action is invoked and what drives it —
// so the answer to "where does token rotation live, what runs it, what interface backs
// it?" is the registry, not a grep across command files.
type ActionMeta struct {
	Action  Action
	Gated   bool   // requires --yes (cloud-mutating); --dry-run / no --yes prints the plan
	Command string // the operator entry point
	// Driver names the cadence/CI workflow this action's lifecycle BELONGS to (a
	// filename under .github/workflows), or "" for operator-only. DriverWired records
	// whether that workflow ACTUALLY invokes Command today. The two are separate
	// because they can legitimately diverge: extension rotate belongs to the
	// llz-secret-rotation.yml lifecycle (it implements the same TokenRotator the core
	// rotators do) but is operator-invoked for now — no workflow step runs `llz
	// extension rotate` yet. TestActionDriverWiring asserts the flag matches reality in
	// BOTH directions, so wiring the step in (or a step regressing) flips a tested bool
	// instead of silently leaving an action off — or falsely on — its cadence.
	Driver      string
	DriverWired bool
	Iface       string // the Go interface a built-in extension satisfies, or ""
	Summary     string
}

var actions = []ActionMeta{
	{Action: ActionSeed, Gated: true, Command: "llz extension seed",
		Summary: "wire declared secrets into OpenBao / GH env (values read from env at seed time, never stored)"},
	{Action: ActionRotate, Gated: true, Command: "llz extension rotate",
		Driver: "llz-secret-rotation.yml", DriverWired: false, Iface: "TokenRotator",
		Summary: "mint a fresh token and re-seed it through the Configure seed targets; operator-invoked today — the TokenRotator step is not yet wired into the cadence workflow"},
	{Action: ActionUpgrade, Gated: false, Command: "llz extension upgrade",
		Summary: "migrate manifest schema, re-apply files, emit copier/renovate wiring"},
	{Action: ActionUnseed, Gated: true, Command: "llz extension unseed",
		Summary: "revoke seeded secrets — delete the GH env secret; print the bao removal (shared-path safety) — the inverse of seed, closing the disable→orphaned-credential gap"},
	{Action: ActionTeardown, Gated: true, Command: "llz extension teardown",
		Summary: "remove an extension's scaffolded files (per the lock) and clear its lock entries — the inverse of scaffold"},
}

func actionMeta(a Action) (ActionMeta, bool) {
	for _, m := range actions {
		if m.Action == a {
			return m, true
		}
	}
	return ActionMeta{}, false
}

// proceedGated reports whether gated action a may execute now. It READS the action's
// Gated flag from the registry — so the flag actually governs whether --yes is required,
// rather than each command hardcoding a check that could silently diverge from the
// table. A gated action without --yes (or under --dry-run) returns false: the caller
// prints its plan and stops. A non-gated action always proceeds.
func proceedGated(g globalOpts, a Action) bool {
	if m, ok := actionMeta(a); ok && m.Gated && (g.dryRun || !g.yes) {
		return false
	}
	return true
}

// Trigger is WHEN a ci: step's generated job runs — the axis the anchor model (which is
// WHERE, relative to converge) lacks. All three are emitted today: `converge` anchors a
// job into the bootstrap DAG (llz-extensions.yml), `schedule` emits a cron job into a
// separate llz-extensions-scheduled.yml (renderScheduledWorkflow), and `dispatch` is the
// manual trigger both workflows carry. A ci: step picks `schedule` by setting a cron on
// the manifest step; otherwise it is converge-anchored. (Putting ActionRotate on its
// cadence now reduces to emitting a scheduled `llz extension rotate` step.)
type Trigger string

const (
	TriggerConverge Trigger = "converge" // ordered into the generated bootstrap DAG via an anchor
	TriggerDispatch Trigger = "dispatch" // the generated workflow runs on workflow_dispatch
	TriggerSchedule Trigger = "schedule" // cron cadence
)

// TriggerMeta records whether the ci-workflow generator actually emits a trigger today —
// the same DriverWired-style honesty applied to the trigger axis, so "schedule is modeled
// but not yet generated" is a tracked, tested fact (TestCITriggersWiredHonesty), not a
// silent absence.
type TriggerMeta struct {
	Trigger Trigger
	Wired   bool
	Summary string
}

var ciTriggers = []TriggerMeta{
	{TriggerConverge, true, "anchored into the generated converge DAG (pre/post-converge, operate)"},
	{TriggerDispatch, true, "both generated workflows run on workflow_dispatch"},
	{TriggerSchedule, true, "a `schedule:` cron emits a job into llz-extensions-scheduled.yml (scheduled-checks, cluster-health, secret-rotation cadence)"},
}

func triggerMeta(t Trigger) (TriggerMeta, bool) {
	for _, m := range ciTriggers {
		if m.Trigger == t {
			return m, true
		}
	}
	return TriggerMeta{}, false
}

// hookDeps records, within a SINGLE extension, that one hook kind consumes another's
// output — the intra-extension coupling the per-phase registers are otherwise blind to
// (a check reads a scaffolded file; a ci deploy runs a scaffolded script). Phase order
// satisfies these today (Scaffold runs before Gate/Bootstrap); TestHookDepsRespectPhaseOrder
// makes that ordering a tested invariant rather than a lucky accident — and dependency-aware
// teardown (remove a file only after its dependents are gone) is a second-pass consumer.
var hookDeps = []struct{ From, To HookKind }{
	{HookCheck, HookFiles},    // tflint reads .tflintrc.hcl; conftest reads policy files
	{HookValidate, HookFiles}, // checkov/conftest read scaffolded policy
	{HookCI, HookFiles},       // a ci deploy step runs a scaffolded script
}

// hookKindsDependingOn returns the hook kinds that consume hook kind `to`'s output
// (per hookDeps) — e.g. hookKindsDependingOn(HookFiles) = {check, validate, ci}. Used by
// dependency-aware teardown: don't remove an extension's files while a live consumer
// (an enabled check/ci hook) still needs them.
func hookKindsDependingOn(to HookKind) []HookKind {
	var out []HookKind
	for _, d := range hookDeps {
		if d.To == to {
			out = append(out, d.From)
		}
	}
	return out
}

// firstPhaseOf returns the lowest registry index of a phase that allows hook kind k, or
// -1 if no phase does — used to check a dependency's phases are correctly ordered.
func firstPhaseOf(k HookKind) int {
	for i, p := range lifecyclePhases {
		if p.Allows(k) {
			return i
		}
	}
	return -1
}

// Runner is an engine that runs a phase — the taxonomy the methodology names, made
// queryable (the prose Engine field stays for the human table). A phase carries a SET
// of runners, because real phases span engines: Gate runs on the laptop (pre-commit)
// and in Actions (the validate job); Operate is scheduled Actions plus the llz CLI;
// Sustain is the laptop plus the Renovate bot. Anchorable phases must include
// RunnerActions (TestAnchorableImpliesActions).
type Runner string

const (
	RunnerExternal Runner = "external" // accounts / InfoSec — outside llz
	RunnerLaptop   Runner = "laptop"   // the llz CLI on a workstation
	RunnerActions  Runner = "actions"  // GitHub Actions
	RunnerBot      Runner = "bot"      // an automated service (e.g. Renovate)
	RunnerHuman    Runner = "human"    // a person
)

// LifecyclePhase is one phase of the LLZ lifecycle. Hooks lists the typed artifact
// kinds an extension may contribute here (empty → core-only, no extension surface).
// CoreJobID, when set, names the generated CI job this phase runs as — which is
// exactly the set of phases a ci: step can anchor to (Anchorable()). The CI-rendering
// specifics for that job (step argv, comment) live in extension_ci.go; this owns only
// WHICH phases are jobs, not HOW each renders.
type LifecyclePhase struct {
	ID          string     // stable identifier Contributions and tests reference
	Methodology bool       // one of the eight documented methodology phases (vs a code-only subphase like Gate)
	Name        string     // human name
	Runners     []Runner   // engine(s) that run the phase (structured; a phase may span several)
	Engine      string     // human-readable engine detail (for the table)
	Hooks       []HookKind // typed declarative artifacts FIRED at this phase
	Actions     []Action   // typed day-2 operations run here, NEVER fired by reconcile
	CoreJobID   string     // non-empty → generated CI job id (phase is Anchorable)
}

// Anchorable reports whether the phase runs as a generated job a ci: step can attach
// to. It is exactly "has a core job id".
func (p LifecyclePhase) Anchorable() bool { return p.CoreJobID != "" }

// RunsOn reports whether engine r runs this phase.
func (p LifecyclePhase) RunsOn(r Runner) bool {
	for _, x := range p.Runners {
		if x == r {
			return true
		}
	}
	return false
}

// Allows reports whether hook kind k may be contributed at this phase.
func (p LifecyclePhase) Allows(k HookKind) bool {
	for _, h := range p.Hooks {
		if h == k {
			return true
		}
	}
	return false
}

// Performs reports whether day-2 action a runs at this phase.
func (p LifecyclePhase) Performs(a Action) bool {
	for _, x := range p.Actions {
		if x == a {
			return true
		}
	}
	return false
}

// lifecyclePhases is THE registry: the ordered, canonical lifecycle. Index in this
// slice is lifecycle order (entitle → decommission); the eight methodology phases keep
// their doc numbers, while Gate (the pre-bootstrap verification subphase, between
// Configure and Bootstrap) and Decommission (the teardown arc, after Handover) are
// code-only. Anchor and core-job tables in extension_ci.go, and the reconcile
// contributions, all derive from this — none re-declares it.
var lifecyclePhases = []LifecyclePhase{
	{ID: "entitle", Methodology: true, Name: "Entitle", Runners: []Runner{RunnerExternal}, Engine: "external (accounts / InfoSec)"},
	{ID: "scaffold", Methodology: true, Name: "Scaffold", Runners: []Runner{RunnerLaptop}, Engine: "copier + laptop (llz new)", Hooks: []HookKind{HookFiles}},
	{ID: "configure", Methodology: true, Name: "Configure", Runners: []Runner{RunnerLaptop}, Engine: "laptop (llz render)", Hooks: []HookKind{HookConfig}, Actions: []Action{ActionSeed}},
	{ID: "gate", Name: "Gate", Runners: []Runner{RunnerLaptop, RunnerActions}, Engine: "laptop (llz lint) + Actions (validate job)", Hooks: []HookKind{HookCheck, HookValidate}},
	{ID: "bootstrap", Methodology: true, Name: "Bootstrap", Runners: []Runner{RunnerActions}, Engine: "GitHub Actions (llz-terraform.yml → converge)", Hooks: []HookKind{HookCI}, CoreJobID: "converge"},
	{ID: "operate", Methodology: true, Name: "Operate", Runners: []Runner{RunnerActions, RunnerLaptop}, Engine: "GitHub Actions (scheduled) + the llz CLI", Hooks: []HookKind{HookCI, HookHealth, HookCommands}, Actions: []Action{ActionRotate}, CoreJobID: "operate"},
	{ID: "promote", Methodology: true, Name: "Promote", Runners: []Runner{RunnerActions}, Engine: "GitHub Actions (promote.yml — a separate generated workflow)"},
	{ID: "sustain", Methodology: true, Name: "Sustain", Runners: []Runner{RunnerLaptop, RunnerBot}, Engine: "laptop + Renovate (llz upgrade / drift)", Hooks: []HookKind{HookFiles}, Actions: []Action{ActionUpgrade}},
	{ID: "handover", Methodology: true, Name: "Handover", Runners: []Runner{RunnerHuman}, Engine: "human"},
	{ID: "decommission", Name: "Decommission", Runners: []Runner{RunnerLaptop}, Engine: "laptop (llz extension teardown / unseed)", Actions: []Action{ActionUnseed, ActionTeardown}},
}

// lifecyclePhaseByID resolves a phase by its stable id.
func lifecyclePhaseByID(id string) (LifecyclePhase, bool) {
	for _, p := range lifecyclePhases {
		if p.ID == id {
			return p, true
		}
	}
	return LifecyclePhase{}, false
}

// phaseIndex returns a phase's position in registry (lifecycle) order, or -1.
// Contributions sort by this so their execution sequence is derived, not hand-kept.
func phaseIndex(id string) int {
	for i, p := range lifecyclePhases {
		if p.ID == id {
			return i
		}
	}
	return -1
}

// methodologyNum returns a phase's display number among the eight documented
// methodology phases (0-based, in registry order), or ok=false for a code-only
// subphase like Gate. The number is derived from registry order, never stored — so a
// reorder cannot leave a stale ordinal behind.
func methodologyNum(id string) (int, bool) {
	n := 0
	for _, p := range lifecyclePhases {
		if !p.Methodology {
			if p.ID == id {
				return 0, false
			}
			continue
		}
		if p.ID == id {
			return n, true
		}
		n++
	}
	return 0, false
}

// anchorablePhases returns the phases that run as a generated CI job, in lifecycle
// order — the source coreSpine and the anchor targets must agree with.
func anchorablePhases() []LifecyclePhase {
	var out []LifecyclePhase
	for _, p := range lifecyclePhases {
		if p.Anchorable() {
			out = append(out, p)
		}
	}
	return out
}
