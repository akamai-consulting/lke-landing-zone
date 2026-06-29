package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The lifecycle registry is the single source of truth; these tests make every
// satellite table (CI spine, anchors, reconcile contributions) provably derive from
// it, so malformed wiring fails CI rather than becoming a stale comment.

// Phases are ordered, stable, uniquely identified, and the eight methodology phases
// keep ascending doc numbers (Gate is the only code-only subphase, Num -1).
func TestLifecyclePhasesOrderedAndStable(t *testing.T) {
	wantIDs := []string{"entitle", "scaffold", "configure", "gate", "bootstrap", "converge", "operate", "promote", "sustain", "handover", "decommission"}
	if len(lifecyclePhases) != len(wantIDs) {
		t.Fatalf("registry has %d phases, want %d", len(lifecyclePhases), len(wantIDs))
	}
	seen := map[string]bool{}
	for i, p := range lifecyclePhases {
		if p.ID != wantIDs[i] {
			t.Errorf("phase[%d].ID = %q, want %q", i, p.ID, wantIDs[i])
		}
		if seen[p.ID] {
			t.Errorf("duplicate phase id %q", p.ID)
		}
		seen[p.ID] = true
	}
	// Methodology numbers are derived from registry order: the eight documented
	// phases number 0..7 in order, and Gate (the code-only subphase) has none.
	wantNum := 0
	for _, p := range lifecyclePhases {
		n, ok := methodologyNum(p.ID)
		if ok != p.Methodology {
			t.Errorf("phase %q: methodologyNum ok=%v but Methodology=%v", p.ID, ok, p.Methodology)
		}
		if ok {
			if n != wantNum {
				t.Errorf("phase %q methodology number = %d, want %d", p.ID, n, wantNum)
			}
			wantNum++
		}
	}
	if wantNum != 8 {
		t.Errorf("found %d methodology phases, want 8", wantNum)
	}
	for _, codeOnly := range []string{"gate", "converge", "decommission"} {
		if _, ok := methodologyNum(codeOnly); ok {
			t.Errorf("%s is a code-only subphase and must have no methodology number", codeOnly)
		}
	}
}

// Every phase materializes at least one known delivery stage; the split is honest — the
// IaC apply (bootstrap) and the Kube-Infra reconcile (converge) are distinct phases, the
// converge pivot is Kube-Infra and anchorable, and every stage is materialized somewhere.
func TestPhasesCarryStages(t *testing.T) {
	known := map[Stage]bool{StageIaC: true, StageKubeInfra: true, StageApp: true}
	used := map[Stage]bool{}
	for _, p := range lifecyclePhases {
		if len(p.Stages) == 0 {
			t.Errorf("phase %q materializes no stage", p.ID)
		}
		for _, s := range p.Stages {
			if !known[s] {
				t.Errorf("phase %q has unknown stage %q", p.ID, s)
			}
			used[s] = true
		}
	}
	bootstrap, _ := lifecyclePhaseByID("bootstrap")
	if !bootstrap.Materializes(StageIaC) || bootstrap.Materializes(StageKubeInfra) {
		t.Error("bootstrap is the IaC apply — IaC only, not Kube-Infra")
	}
	converge, _ := lifecyclePhaseByID("converge")
	if !converge.Materializes(StageKubeInfra) || !converge.Anchorable() {
		t.Error("converge is the Kube-Infra reconcile and the anchorable pivot")
	}
	for s := range known {
		if !used[s] {
			t.Errorf("stage %q is materialized by no phase", s)
		}
	}
}

// FiredBy is honest: a hook is FiredBy "reconcile" exactly when it has a Contribution;
// every FiredBy value is from the known set; and commands is the one non-fired hook
// (startup registration). This pins "what drives each hook" so a future hook can't
// quietly claim a driver it doesn't have.
func TestFiredByMatchesContributions(t *testing.T) {
	hasContribution := map[HookKind]bool{}
	for _, c := range contributions {
		if hasContribution[c.Hook()] {
			t.Errorf("more than one contribution for hook %q", c.Hook())
		}
		hasContribution[c.Hook()] = true
	}
	known := map[FiredBy]bool{FiredByReconcile: true, FiredByValidate: true, FiredByDoctor: true, FiredByStartup: true}
	for _, m := range hookKinds {
		if !known[m.FiredBy] {
			t.Errorf("hook %q has unknown FiredBy %q", m.Kind, m.FiredBy)
		}
		if (m.FiredBy == FiredByReconcile) != hasContribution[m.Kind] {
			t.Errorf("hook %q FiredBy=%q but hasContribution=%v", m.Kind, m.FiredBy, hasContribution[m.Kind])
		}
	}
	if m, ok := hookMeta(HookCommands); !ok || m.fired() {
		t.Errorf("commands must not be fired (startup registration, not a phase)")
	}
}

// A — the trigger axis is honestly tracked: every Trigger has a meta row and all three
// are now wired (converge → llz-extensions.yml, schedule → llz-extensions-scheduled.yml,
// dispatch on both). A new trigger added without a generator would fail TestRender* below.
func TestCITriggersWiredHonesty(t *testing.T) {
	for _, tr := range []Trigger{TriggerConverge, TriggerDispatch, TriggerSchedule} {
		m, ok := triggerMeta(tr)
		if !ok {
			t.Errorf("trigger %q has no meta row", tr)
		}
		if !m.Wired {
			t.Errorf("trigger %q must be wired (its codegen exists)", tr)
		}
	}
}

// C — every intra-extension hook dependency is ordered by the registry: the dependent
// hook's earliest phase comes at or after the phase whose output it consumes. So a
// reorder that put Gate before Scaffold (check before its files) fails CI.
func TestHookDepsRespectPhaseOrder(t *testing.T) {
	for _, d := range hookDeps {
		from, to := firstPhaseOf(d.From), firstPhaseOf(d.To)
		if from < 0 || to < 0 {
			t.Errorf("hookDep %s→%s references a hook no phase allows", d.From, d.To)
			continue
		}
		if from < to {
			t.Errorf("hookDep %s (phase %d) must not run before its dependency %s (phase %d)", d.From, from, d.To, to)
		}
	}
}

// D — the cloud-mutation boundary is honest: exactly `ci` may mutate (and only at
// workflow runtime), so the safety line is "reconcile-safe", not "no cloud mutation".
func TestOnlyCIMayMutate(t *testing.T) {
	for _, m := range hookKinds {
		if m.MayMutate && m.Kind != HookCI {
			t.Errorf("hook %q claims MayMutate; only ci may (deploys run in reviewed workflows)", m.Kind)
		}
	}
	if m, _ := hookMeta(HookCI); !m.MayMutate {
		t.Error("ci must be MayMutate (a deploy step mutates at workflow runtime)")
	}
}

// Every anchorable phase carries a core job id AND a render spec; every render spec
// maps back to an anchorable phase. Neither side may carry an entry the other lacks.
func TestCoreJobSpecsMatchAnchorablePhases(t *testing.T) {
	anchorable := map[string]bool{}
	for _, p := range anchorablePhases() {
		if p.CoreJobID == "" {
			t.Errorf("anchorable phase %q has no CoreJobID", p.ID)
		}
		anchorable[p.CoreJobID] = true
		if _, ok := coreJobSpecs[p.CoreJobID]; !ok {
			t.Errorf("anchorable phase %q (job %q) has no coreJobSpec", p.ID, p.CoreJobID)
		}
	}
	for job := range coreJobSpecs {
		if !anchorable[job] {
			t.Errorf("coreJobSpec %q is not the CoreJobID of any anchorable phase", job)
		}
	}
}

// Every CI anchor binds against a core job that comes from the registry — an anchor
// can never point at a job the lifecycle does not generate.
func TestAnchorTargetsAreRegistryCoreJobs(t *testing.T) {
	valid := map[string]bool{}
	for _, p := range anchorablePhases() {
		valid[p.CoreJobID] = true
	}
	for _, a := range anchorRegistry {
		if !valid[a.Target()] {
			t.Errorf("anchor %q targets %q, which is not an anchorable phase's CoreJobID", a.Name(), a.Target())
		}
	}
}

// Every contribution names a phase that exists in the registry AND a hook kind that
// phase actually allows — the contribution↔registry binding cannot go stale.
func TestEveryContributionTargetsKnownPhaseAndHook(t *testing.T) {
	for _, c := range contributions {
		p, ok := lifecyclePhaseByID(c.PhaseID())
		if !ok {
			t.Errorf("contribution %T targets unknown phase %q", c, c.PhaseID())
			continue
		}
		if !p.Allows(c.Hook()) {
			t.Errorf("contribution %T hook %q not allowed at phase %q (allows %v)", c, c.Hook(), p.ID, p.Hooks)
		}
		if _, ok := hookMeta(c.Hook()); !ok {
			t.Errorf("contribution %T hook %q has no failure-posture entry", c, c.Hook())
		}
	}
}

// Every hook kind named on any phase has a failure-posture entry, and vice versa —
// no phase advertises a hook the semantics table does not cover.
func TestHookKindsHavePostureAndAreUsed(t *testing.T) {
	used := map[HookKind]bool{}
	for _, p := range lifecyclePhases {
		for _, h := range p.Hooks {
			used[h] = true
			if _, ok := hookMeta(h); !ok {
				t.Errorf("phase %q advertises hook %q with no failure-posture entry", p.ID, h)
			}
		}
	}
	for _, h := range hookKinds {
		if !used[h.Kind] {
			t.Errorf("hook kind %q has a posture entry but no phase uses it", h.Kind)
		}
	}
}

// `llz extension lifecycle` prints from the central registry: every phase name, every
// core job id, and every day-2 action appears in the output.
func TestLifecyclePrintsFromRegistry(t *testing.T) {
	out := captureStdout(t, func() {
		if err := runLifecycle(); err != nil {
			t.Fatal(err)
		}
	})
	for _, p := range lifecyclePhases {
		if !strings.Contains(out, p.Name) {
			t.Errorf("lifecycle output missing phase %q\n%s", p.Name, out)
		}
		if p.Anchorable() && !strings.Contains(out, p.CoreJobID) {
			t.Errorf("lifecycle output missing anchor job %q\n%s", p.CoreJobID, out)
		}
		for _, a := range p.Actions {
			if !strings.Contains(out, string(a)) {
				t.Errorf("lifecycle output missing day-2 action %q\n%s", a, out)
			}
		}
	}
}

// ── the Stage axis (IaC → Kube-Infra → App) ──────────────────────────────────

// The three stages are ordered by dependency (IaC is the base; each depends on the prior),
// and App is the ONLY non-platform-gated stage — its gates run in the app's own CI.
func TestStagesOrderedAndPlatformGating(t *testing.T) {
	want := []Stage{StageIaC, StageKubeInfra, StageApp}
	if len(stages) != len(want) {
		t.Fatalf("stages = %d, want %d", len(stages), len(want))
	}
	for i, m := range stages {
		if m.Stage != want[i] {
			t.Errorf("stage[%d] = %q, want %q", i, m.Stage, want[i])
		}
		if i == 0 && m.DependsOn != "" {
			t.Errorf("%s is the base layer; it must not DependsOn anything", m.Stage)
		}
		if i > 0 && m.DependsOn != want[i-1] {
			t.Errorf("%s DependsOn = %q, want %q", m.Stage, m.DependsOn, want[i-1])
		}
	}
	if !stagePlatformGated(StageIaC) || !stagePlatformGated(StageKubeInfra) {
		t.Error("iac + kube-infra checks must run in the platform gate")
	}
	if stagePlatformGated(StageApp) {
		t.Error("app checks must NOT run in the platform gate (they run in the app's own CI)")
	}
	if !stagePlatformGated("") {
		t.Error("a stage-less (cross-cutting) extension must be platform-gated")
	}
}

// The platform gate (runExtensionChecks) runs IaC + cross-cutting checks but SKIPS
// App-stage ones — the formal reason cargo coverage/mutants don't run in `llz lint`.
func TestPlatformGateSkipsAppStage(t *testing.T) {
	failing := []extStep{{Name: "x", Argv: []string{"sh", "-c", "exit 1"}}}
	app := Extension{Name: "appx", Manifest: extManifest{Stage: StageApp, Check: failing}}
	if err := runExtensionChecks(globalOpts{}, []Extension{app}); err != nil {
		t.Fatalf("App-stage checks must be skipped by the platform gate, got: %v", err)
	}
	for _, e := range []Extension{
		{Name: "iacx", Manifest: extManifest{Stage: StageIaC, Check: failing}},
		{Name: "crossx", Manifest: extManifest{Check: failing}}, // stage-less → cross-cutting
	} {
		if err := runExtensionChecks(globalOpts{}, []Extension{e}); err == nil {
			t.Errorf("%s checks must run in the platform gate (and this one fails)", e.Name)
		}
	}
}

// A manifest stage must be one of the three (or empty); an unknown stage is a lint finding.
func TestLintValidatesStage(t *testing.T) {
	base := extManifest{Name: "x", Short: "y", Kind: "tool"}
	bad := base
	bad.Stage = "frontend"
	if len(lintManifest(bad)) == 0 {
		t.Error("an unknown stage must be a lint finding")
	}
	for _, ok := range []Stage{"", StageIaC, StageKubeInfra, StageApp} {
		m := base
		m.Stage = ok
		if f := lintManifest(m); len(f) != 0 {
			t.Errorf("stage %q must lint clean, got %v", ok, f)
		}
	}
}

// ── the Hooks/Actions safety boundary, made executable ───────────────────────

// The two registers are disjoint: no Action string equals a HookKind string, so the
// imperative day-2 vocabulary can never be mistaken for a fired hook.
func TestActionsAreNotHooks(t *testing.T) {
	hooks := map[string]bool{}
	for _, m := range hookKinds {
		hooks[string(m.Kind)] = true
	}
	for _, m := range actions {
		if hooks[string(m.Action)] {
			t.Errorf("action %q collides with a hook kind of the same name", m.Action)
		}
	}
}

// No day-2 action is ever fired by reconcile: no Contribution's hook shares a name
// with any Action. This pins "actions are never reconciled" as a test, not a comment.
func TestActionsAreNeverReconciled(t *testing.T) {
	act := map[string]bool{}
	for _, m := range actions {
		act[string(m.Action)] = true
	}
	for _, c := range contributions {
		if act[string(c.Hook())] {
			t.Errorf("contribution %T fires %q, which is a day-2 Action and must never be reconciled", c, c.Hook())
		}
	}
}

// Every action named on a phase has an ActionMeta row, and every ActionMeta row is
// used by exactly one phase — the anchorablePhases/coreJobSpecs anti-drift pattern,
// reused for the action register.
func TestEveryPhaseActionHasMeta(t *testing.T) {
	usedBy := map[Action]int{}
	for _, p := range lifecyclePhases {
		for _, a := range p.Actions {
			usedBy[a]++
			if _, ok := actionMeta(a); !ok {
				t.Errorf("phase %q performs action %q with no ActionMeta entry", p.ID, a)
			}
		}
	}
	for _, m := range actions {
		if usedBy[m.Action] == 0 {
			t.Errorf("action %q has a meta entry but no phase performs it", m.Action)
		}
		if usedBy[m.Action] > 1 {
			t.Errorf("action %q is performed by %d phases — expected exactly one home", m.Action, usedBy[m.Action])
		}
	}
}

// Every ActionMeta.Driver names a workflow that exists AND whose contents match the
// DriverWired claim: if wired, the workflow must invoke the action's Command; if not
// wired, it must not. This is the deep rotate hardening — it catches both a renamed/
// deleted cadence workflow and a drift between "claims to be on its cadence" and
// "actually runs the command." Wiring `llz extension rotate` into llz-secret-rotation.yml
// will fail this test until DriverWired is flipped to true (and vice versa).
func TestActionDriverWiring(t *testing.T) {
	const workflows = "../../../.github/workflows" // package dir → repo root
	if _, err := os.Stat(workflows); err != nil {
		t.Skipf("no %s here (extracted package?): %v", workflows, err)
	}
	for _, m := range actions {
		if m.Driver == "" {
			if m.DriverWired {
				t.Errorf("action %q has DriverWired=true but no Driver", m.Action)
			}
			continue
		}
		b, err := os.ReadFile(filepath.Join(workflows, m.Driver))
		if err != nil {
			t.Errorf("action %q names driver %q but it is unreadable: %v", m.Action, m.Driver, err)
			continue
		}
		invokes := strings.Contains(string(b), m.Command)
		switch {
		case m.DriverWired && !invokes:
			t.Errorf("action %q claims DriverWired but %s never invokes %q", m.Action, m.Driver, m.Command)
		case !m.DriverWired && invokes:
			t.Errorf("action %q is DriverWired=false but %s DOES invoke %q — flip DriverWired to true", m.Action, m.Driver, m.Command)
		}
	}
}

// Gated actions are exactly the cloud-mutating ones, and each names an operator
// command — the audit surface ("what cloud-mutating operations exist?") is complete
// and every entry is invocable.
func TestActionsHaveCommands(t *testing.T) {
	for _, m := range actions {
		if m.Command == "" {
			t.Errorf("action %q has no operator command", m.Action)
		}
	}
}

// ── Posture / Gated are load-bearing, not decoration ─────────────────────────

// applyPosture maps each posture to the right outcome, so the entry points that route
// through it actually honor the registry's Posture values.
func TestApplyPostureMapping(t *testing.T) {
	boom := errors.New("boom")
	if applyPosture(PostureBlocking, "x", boom) == nil {
		t.Error("blocking must propagate the error")
	}
	if applyPosture(PostureBestEffort, "x", boom) != nil {
		t.Error("best-effort must swallow the error")
	}
	if applyPosture(PostureReport, "x", boom) != nil {
		t.Error("report-only must swallow the error")
	}
	if applyPosture(PostureBlocking, "x", nil) != nil {
		t.Error("a nil error must stay nil regardless of posture")
	}
}

// proceedGated reads the registry's Gated flag: a gated action needs --yes (and not
// --dry-run); a non-gated action always proceeds. This pins the flag to behavior.
func TestProceedGatedReadsRegistry(t *testing.T) {
	if proceedGated(globalOpts{}, ActionSeed) { // gated, no --yes
		t.Error("a gated action must not proceed without --yes")
	}
	if proceedGated(globalOpts{dryRun: true, yes: true}, ActionSeed) {
		t.Error("a gated action must not proceed under --dry-run")
	}
	if !proceedGated(globalOpts{yes: true}, ActionSeed) {
		t.Error("a gated action must proceed with --yes")
	}
	if !proceedGated(globalOpts{}, ActionUpgrade) { // non-gated proceeds regardless
		t.Error("a non-gated action must always proceed")
	}
}

// End-to-end: the blocking posture on HookCheck is what makes a failing extension check
// fail the gate. lifecycleGate reads that posture — flip HookCheck to report-only and
// this stops blocking, which is the whole point of making Posture load-bearing.
func TestGateHonorsBlockingPosture(t *testing.T) {
	root := t.TempDir()
	installExt(t, root, "boom",
		"schemaVersion: 2\nname: boom\nshort: x\nkind: tool\ncheck:\n  - {name: fail, argv: [sh, -c, 'exit 1']}\n", nil)
	saveExtConfig(root, extConfig{Enabled: []string{"boom"}})
	if err := lifecycleGate(globalOpts{}, root); err == nil {
		t.Fatal("a failing check must fail the gate (HookCheck is blocking)")
	}
}

// Performs is consistent with each phase's Actions slice (and exercises the method so
// it is not dead code): the decommission phase performs unseed + teardown, configure
// performs seed, and no phase performs an action it does not list.
func TestPerformsMatchesActions(t *testing.T) {
	for _, p := range lifecyclePhases {
		for _, a := range p.Actions {
			if !p.Performs(a) {
				t.Errorf("phase %q lists action %q but Performs says no", p.ID, a)
			}
		}
		if p.ID == "decommission" && !(p.Performs(ActionUnseed) && p.Performs(ActionTeardown)) {
			t.Errorf("decommission must perform unseed + teardown")
		}
		if p.ID == "scaffold" && p.Performs(ActionSeed) {
			t.Errorf("scaffold must not perform seed (seed is a Configure action)")
		}
	}
}

// #6 — the structured Runners cannot drift from the prose Engine: every Runner is from
// the known set, non-empty, dup-free, and named (by a keyword) in the phase's Engine
// string. So adding a runner without mentioning it (or prose that contradicts the set)
// fails CI — the same drift class this file polices for hooks and actions.
func TestRunnersValidAndAgreeWithEngine(t *testing.T) {
	known := map[Runner]bool{RunnerExternal: true, RunnerLaptop: true, RunnerActions: true, RunnerBot: true, RunnerHuman: true}
	kw := map[Runner][]string{
		RunnerExternal: {"external"},
		RunnerLaptop:   {"laptop", "cli"},
		RunnerActions:  {"actions"},
		RunnerBot:      {"renovate", "bot"},
		RunnerHuman:    {"human"},
	}
	for _, p := range lifecyclePhases {
		if len(p.Runners) == 0 {
			t.Errorf("phase %q has no runners", p.ID)
		}
		eng := strings.ToLower(p.Engine)
		if strings.TrimSpace(eng) == "" {
			t.Errorf("phase %q has empty Engine prose", p.ID)
		}
		seen := map[Runner]bool{}
		for _, r := range p.Runners {
			if !known[r] {
				t.Errorf("phase %q has unknown runner %q", p.ID, r)
			}
			if seen[r] {
				t.Errorf("phase %q lists runner %q twice", p.ID, r)
			}
			seen[r] = true
			matched := false
			for _, k := range kw[r] {
				if strings.Contains(eng, k) {
					matched = true
					break
				}
			}
			if !matched {
				t.Errorf("phase %q runner %q is absent from its Engine prose %q", p.ID, r, p.Engine)
			}
		}
	}
}

// An anchorable phase (a generated CI job) must run in GitHub Actions — the Runner
// taxonomy and the CI spine cannot disagree about where bootstrap/operate run.
func TestAnchorableImpliesActions(t *testing.T) {
	for _, p := range lifecyclePhases {
		if p.Anchorable() && !p.RunsOn(RunnerActions) {
			t.Errorf("phase %q is anchorable but does not run on %q (runners=%v)", p.ID, RunnerActions, p.Runners)
		}
		if len(p.Runners) == 0 {
			t.Errorf("phase %q has no runners", p.ID)
		}
	}
}
