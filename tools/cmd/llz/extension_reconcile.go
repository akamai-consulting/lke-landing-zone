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
)

// Contribution is one phase's worth of work an extension can contribute. Reconcile
// receives the full Extension set so per-extension contributions (files) and
// aggregate ones (ci, which builds one workflow from all) share one shape.
type Contribution interface {
	Phase() string
	Reconcile(g globalOpts, exts []Extension, root string, check bool) error
}

// configContribution (Configure): report unsatisfied declared inputs. Reconcile is
// advisory — the failing gate is `llz extension doctor` / seed.
type configContribution struct{}

func (configContribution) Phase() string { return "Configure" }
func (configContribution) Reconcile(_ globalOpts, exts []Extension, _ string, _ bool) error {
	for _, e := range exts {
		for _, f := range manifestConfigFindings(e.Name, e.Manifest, os.Getenv) {
			fmt.Fprintf(os.Stderr, "  %s %s/%s: %s\n", f.Kind, e.Name, f.Name, f.Status)
		}
	}
	return nil
}

// filesContribution (Scaffold): render each extension's files: into the instance.
type filesContribution struct{}

func (filesContribution) Phase() string { return "Scaffold" }
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

// ciContribution (Bootstrap): generate the one needs:-chained workflow from every
// extension's ci: steps.
type ciContribution struct{}

func (ciContribution) Phase() string { return "Bootstrap" }
func (ciContribution) Reconcile(g globalOpts, exts []Extension, root string, check bool) error {
	var jobs []extCIJob
	for _, e := range exts {
		jobs = append(jobs, manifestCIJobs(e.Manifest)...)
	}
	return runExtensionCIWorkflow(g, jobs, root, check)
}

// gateContribution (Gate): run each extension's check: steps. A missing tool skips
// (haveTool), so an absent linter never wedges reconcile — the same philosophy as
// core runLint, which production folds these into.
type gateContribution struct{}

func (gateContribution) Phase() string { return "Gate" }
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

// contributions are the reconcilable phases, in lifecycle order.
var contributions = []Contribution{
	configContribution{},
	filesContribution{},
	ciContribution{},
	gateContribution{},
}

// runExtensionReconcile drives every contribution in phase order over the unified
// Extension set — the lifecycle driver `llz upgrade` should call (and the seam
// `runLint` should fold the Gate into). --check reports drift across all phases
// without writing.
func runExtensionReconcile(g globalOpts, root string, check bool) error {
	exts, err := loadAllExtensions(root)
	if err != nil {
		return err
	}
	for _, c := range contributions {
		fmt.Fprintf(os.Stderr, "── %s ──\n", c.Phase())
		if err := c.Reconcile(g, exts, root, check); err != nil {
			return err
		}
	}
	return nil
}
