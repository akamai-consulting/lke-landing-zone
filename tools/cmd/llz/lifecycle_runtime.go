package main

// lifecycle_runtime.go holds the named lifecycle entry points — the seams core commands
// fire phases through, so a command never reaches into an extension internal directly.
// Each names the lifecycle event it wants; the posture (blocking / best-effort /
// report-only) is READ from the registry in lifecycle.go, not re-encoded here. The
// registry tables stay in lifecycle.go as pure policy; this file is the thin wiring over
// the extension_* runners.

import (
	"fmt"
	"os"
)

// lifecycleGate fires the Gate phase (check: hook) over the instance at root: load
// the extension set and run every check: step. A missing tool skips (HookCheck.ToolSkip),
// and a load/trust failure warns loudly rather than silently dropping the gate. Whether
// a failing check fails the caller (runLint) is the HookCheck posture, read here — so
// the blocking decision lives in the registry, not at the call site.
func lifecycleGate(g globalOpts, root string) error {
	m, _ := hookMeta(HookCheck)
	return applyPosture(m.Posture, "extension gate", runExtensionGate(g, root))
}

// lifecycleSustain fires the Sustain phase (files: hook) over the instance at root:
// re-apply every extension's files so a changed template/binary propagates on upgrade
// without a manual re-scaffold. files: is blocking by default, but upgrade deliberately
// downgrades to best-effort so a misconfigured extension cannot abort the upgrade —
// and that downgrade is applied HERE, in the entry point, by reading PostureBestEffort,
// not silently swallowed at the call site.
func lifecycleSustain(g globalOpts, root string) error {
	return applyPosture(PostureBestEffort, "extension files apply during upgrade", runExtensionApplyAll(g, root, false))
}

// lifecycleDoctor fires the Configure phase's readiness check (config: hook) as a
// report folded into `llz doctor`: it surfaces every enabled extension's declared
// vars/secrets that are unsatisfied. Report-only in spirit, but a REQUIRED secret left
// unsatisfied returns an error so the unified doctor gate fails — config's posture is
// report-only until called as the strict gate, which doctor is. A load failure warns
// rather than wedging doctor; an instance with no enabled extensions stays silent.
func lifecycleDoctor(root string) error {
	exts, err := loadEnabledExtensions(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "llz: extension config check skipped: %v\n", err)
		return nil
	}
	if len(exts) == 0 {
		return nil
	}
	fmt.Println("\n" + bold("Extensions (Configure readiness):"))
	// Declared tools: that aren't installed — otherwise the check silently skips, giving
	// false assurance that an enabled lint/policy pack is running when it isn't.
	reportMissingExtTools(exts)
	return runExtensionConfigDoctor(root)
}

// lifecycleValidate fires the Gate phase's CI tier (validate: hook) folded into
// `llz validate`: every enabled extension's heavyweight validate: steps. Unlike the
// lint-tier gate, tools are REQUIRED (HookValidate.ToolSkip=false) — a missing tool
// fails rather than skips, because a checkov/conftest pass means nothing if the tool was
// absent. Blocking, read from HookValidate's posture. A load failure is a hard error
// here (the strict CI tier must not silently drop), unlike the fast pre-commit gate.
func lifecycleValidate(g globalOpts, root string) error {
	m, _ := hookMeta(HookValidate)
	return applyPosture(m.Posture, "extension validate", runExtensionValidate(g, root))
}

// lifecycleHealth fires the Operate phase's report-only health probes (health: hook),
// surfaced by `llz doctor` / `llz status`. Report-only (HookHealth posture): it prints
// each probe's result and never fails the caller — a failing probe is a signal, not a
// gate. A missing tool skips. Silent when no enabled extension declares a probe.
func lifecycleHealth(root string) {
	runExtensionHealth(root)
}

// lifecycleDrift reports extension output drift (a scaffolded file hand-edited, missing,
// or orphaned vs the lock) as part of `llz drift` — the Sustain-phase health view.
// Report-only: it prints findings and never writes, leaving any blocking decision to
// the caller. A load failure or an instance with no enabled extensions stays silent.
func lifecycleDrift(g globalOpts, root string) {
	exts, err := loadEnabledExtensions(root)
	if err != nil || len(exts) == 0 {
		return
	}
	fmt.Println(bold("Extension drift (scaffolded files vs lock):"))
	if err := runExtensionApplyAll(g, root, true); err != nil {
		fmt.Printf("%s %v\n", yellow("drift:"), err)
		return
	}
	fmt.Printf("%s extension-owned files match the lock.\n", green("✓"))
}
