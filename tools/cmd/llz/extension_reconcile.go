package main

// extension_reconcile.go is the TOP-DOWN spine the bottom-up evolution lacked: a
// Contribution interface the routers implement, and a single driver that runs
// them in lifecycle-phase order over the unified Extension set (built-ins +
// enabled). It replaces "the operator runs apply, then ci-workflow, then …" with
// one lifecycle-driven reconcile, and — crucially — it makes the Gate (check:)
// actually EXECUTE, which the per-command surface never did.
//
// Note the honest taxonomy: not every manifest section is a reconcilable artifact.
// files/ci are idempotent outputs (reconcile owns them); check is a read-only gate
// (reconcile runs it; production also folds it into runLint); vars/secrets is a
// readiness report (doctor owns the failing variant). seed/rotate are cloud-
// mutating day-2 actions and commands is live CLI registration — deliberately NOT
// in reconcile, because reconcile must be safe to run unattended.

import (
	"fmt"
	"os"
	"sort"
)

// Contribution is one phase's worth of work an extension can contribute. Each names
// the lifecycle phase it runs at (PhaseID) and the typed hook kind it produces (Hook)
// — both resolved against the central registry (lifecycle.go), so a contribution can
// never reference a phase/hook that does not exist (TestEveryContribution... enforces).
// Reconcile receives the full Extension set so per-extension contributions (files) and
// aggregate ones (ci, which builds one workflow from all) share one shape.
type Contribution interface {
	PhaseID() string
	Hook() HookKind
	Reconcile(g globalOpts, exts []Extension, root string, check bool) error
}

// configContribution (Configure / config): report unsatisfied declared inputs.
// Reconcile is advisory (PostureReport) — the failing gate is `llz extension doctor`.
type configContribution struct{}

func (configContribution) PhaseID() string { return "configure" }
func (configContribution) Hook() HookKind  { return HookConfig }
func (configContribution) Reconcile(_ globalOpts, exts []Extension, _ string, _ bool) error {
	for _, e := range exts {
		for _, f := range manifestConfigFindings(e.Name, e.Manifest, os.Getenv) {
			fmt.Fprintf(os.Stderr, "  %s %s/%s: %s\n", f.Kind, e.Name, f.Name, f.Status)
		}
	}
	return nil
}

// filesContribution (Scaffold / files): render each extension's files: into the
// instance — the extension hook at the Scaffold phase (where core runs copier).
type filesContribution struct{}

func (filesContribution) PhaseID() string { return "scaffold" }
func (filesContribution) Hook() HookKind  { return HookFiles }
func (filesContribution) Reconcile(g globalOpts, exts []Extension, root string, check bool) error {
	var firstErr error
	for _, e := range exts {
		if len(e.Manifest.Files) == 0 {
			continue
		}
		if err := applyExtensionFiles(g, e, root, check); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ciContribution (Bootstrap / ci): generate the one needs:-chained workflow from
// every extension's ci: steps — the extension hook at the Bootstrap phase.
type ciContribution struct{}

func (ciContribution) PhaseID() string { return "bootstrap" }
func (ciContribution) Hook() HookKind  { return HookCI }
func (ciContribution) Reconcile(g globalOpts, exts []Extension, root string, check bool) error {
	var jobs []extCIJob
	for _, e := range exts {
		jobs = append(jobs, manifestCIJobs(e.Manifest)...)
	}
	return runExtensionCIWorkflow(g, jobs, root, check)
}

// gateContribution (Gate / check): run each extension's check: steps. A missing tool
// skips (HookCheck.ToolSkip), so an absent linter never wedges reconcile — the same
// philosophy as core runLint, which fires this same hook via lifecycleGate.
type gateContribution struct{}

func (gateContribution) PhaseID() string { return "gate" }
func (gateContribution) Hook() HookKind  { return HookCheck }
func (gateContribution) Reconcile(g globalOpts, exts []Extension, _ string, _ bool) error {
	return runExtensionChecks(g, exts)
}

// runExtensionChecks runs every extension's check: steps (missing tool skips).
func runExtensionChecks(g globalOpts, exts []Extension) error {
	for _, e := range exts {
		for _, s := range e.Manifest.Check {
			if len(s.Argv) == 0 || !haveTool(s.Argv[0]) {
				continue
			}
			if err := run(g, s.Argv...); err != nil {
				return fmt.Errorf("extension check %s/%s: %w", e.Name, s.Name, err)
			}
		}
	}
	return nil
}

// runExtensionValidate runs every extension's validate: steps — the heavyweight CI tier
// (HookValidate). Unlike the lint-tier gate, a missing tool is a HARD FAILURE, not a
// skip: a checkov/conftest pass is meaningless if the tool was absent. A load failure is
// also fatal here (the strict CI tier must not silently drop), unlike the fast gate.
func runExtensionValidate(g globalOpts, root string) error {
	exts, err := loadAllExtensions(root)
	if err != nil {
		return fmt.Errorf("extension validate: %w", err)
	}
	for _, e := range exts {
		for _, s := range e.Manifest.Validate {
			if len(s.Argv) == 0 {
				continue
			}
			if !haveTool(s.Argv[0]) {
				return fmt.Errorf("extension validate %s/%s: required tool %q not found", e.Name, s.Name, s.Argv[0])
			}
			if err := run(g, s.Argv...); err != nil {
				return fmt.Errorf("extension validate %s/%s: %w", e.Name, s.Name, err)
			}
		}
	}
	return nil
}

// runExtensionHealth runs every enabled extension's health: probes report-only
// (HookHealth): it prints each probe's pass/fail and NEVER returns an error — a failing
// probe is a signal surfaced by doctor/status, not a gate. A missing tool skips.
func runExtensionHealth(root string) {
	exts, err := loadEnabledExtensions(root)
	if err != nil || len(exts) == 0 {
		return
	}
	header := false
	for _, e := range exts {
		for _, s := range e.Manifest.Health {
			if len(s.Argv) == 0 || !haveTool(s.Argv[0]) {
				continue
			}
			if !header {
				fmt.Println("\n" + bold("Extensions (health probes):"))
				header = true
			}
			if err := run(globalOpts{}, s.Argv...); err != nil {
				fmt.Printf("%s %s/%s: %v\n", yellow("⚠"), e.Name, s.Name, err)
			} else {
				fmt.Printf("%s %s/%s\n", green("✓"), e.Name, s.Name)
			}
		}
	}
}

// runExtensionGate is the runLint tail (the issue's "runLint gains a tail running
// each enabled recipe's check"): load the set and run its checks. A LOAD error
// warns rather than wedging the fast pre-commit gate — doctor/reconcile surface
// config problems loudly — while a failing CHECK fails lint, as it should.
func runExtensionGate(g globalOpts, root string) error {
	exts, err := loadAllExtensions(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "llz: extension gate skipped: %v\n", err)
		return nil
	}
	return runExtensionChecks(g, exts)
}

// contributions are the reconcilable artifacts — declared as a set, NOT in execution
// order. orderedContributions sorts them by their phase's position in the lifecycle
// registry, so the reconcile sequence is derived from the registry and cannot drift
// from a separately hand-maintained order.
var contributions = []Contribution{
	configContribution{},
	filesContribution{},
	ciContribution{},
	gateContribution{},
}

// orderedContributions returns the contributions in lifecycle (registry) order.
func orderedContributions() []Contribution {
	out := append([]Contribution(nil), contributions...)
	sort.SliceStable(out, func(i, j int) bool {
		return phaseIndex(out[i].PhaseID()) < phaseIndex(out[j].PhaseID())
	})
	return out
}

// runExtensionReconcile drives every contribution in lifecycle order over the unified
// Extension set — the lifecycle driver `llz upgrade` should call (and the seam
// `runLint` folds the Gate into via lifecycleGate). --check reports drift across all
// phases without writing. A contribution naming a phase outside the registry is a
// wiring bug, surfaced here rather than silently skipped.
func runExtensionReconcile(g globalOpts, root string, check bool) error {
	exts, err := loadAllExtensions(root)
	if err != nil {
		return err
	}
	for _, c := range orderedContributions() {
		p, ok := lifecyclePhaseByID(c.PhaseID())
		if !ok {
			return fmt.Errorf("contribution %T targets unknown lifecycle phase %q", c, c.PhaseID())
		}
		fmt.Fprintf(os.Stderr, "── %s ──\n", p.Name)
		if err := c.Reconcile(g, exts, root, check); err != nil {
			return err
		}
	}
	return nil
}
