package main

// lifecycle_cmd.go renders `llz extension lifecycle` — the operator-facing table over
// the registry. Display only; the policy lives in lifecycle.go.

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
)

// runStages prints the three top-level delivery stages (IaC → Kube-Infra → App) — the
// coarse layer axis above the phases — with each stage's engine, gate vocabulary, whether
// its checks run in the platform gate, and the enabled extensions that target it. It makes
// the App-vs-platform gate boundary visible: App-stage checks run in the app's own CI.
func runStages(root string) error {
	exts, _ := loadAllExtensions(root) // best-effort; show the structure even with none enabled
	byStage := map[Stage][]string{}
	var crosscut []string
	for _, e := range exts {
		if e.Manifest.Stage == "" {
			crosscut = append(crosscut, e.Name)
			continue
		}
		byStage[e.Manifest.Stage] = append(byStage[e.Manifest.Stage], e.Name)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "STAGE\tDEPENDS ON\tENGINE\tGATE\tPLATFORM-GATED\tEXTENSIONS")
	for _, m := range stages {
		dep := "—"
		if m.DependsOn != "" {
			if d, ok := stageMeta(m.DependsOn); ok {
				dep = d.Name
			}
		}
		gated := "yes"
		if !m.PlatformGated {
			gated = "no — app's own CI"
		}
		exts := "—"
		if len(byStage[m.Stage]) > 0 {
			exts = strings.Join(byStage[m.Stage], ", ")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", m.Name, dep, m.Engine, m.Gate, gated, exts)
	}
	tw.Flush()
	if len(crosscut) > 0 {
		fmt.Fprintf(os.Stderr, "\ncross-cutting (no stage; platform-gated): %s\n", strings.Join(crosscut, ", "))
	}
	fmt.Fprintln(os.Stderr, "App-stage checks run in the app's own scaffolded CI, never the platform gate (`llz lint`/`validate`).")
	return nil
}

// runLifecycle prints the lifecycle registry straight from the central table: every
// phase, the engine that runs it, the core CI job it is anchorable through (if any),
// the typed HOOK kinds fired there, and the day-2 ACTIONS run there — so "where can an
// extension tie in, and what touches each phase?" is one table. Hooks are fired by the
// phase; actions never are (gated operator commands / cadence workflows only).
func runLifecycle() error {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PHASE\tNAME\tENGINE\tANCHOR JOB\tHOOKS\tDAY-2 ACTIONS")
	anyUnfired, anyUnwired := false, false
	for _, p := range lifecyclePhases {
		num := "—"
		if n, ok := methodologyNum(p.ID); ok {
			num = fmt.Sprintf("%d", n)
		}
		job := "—"
		if p.Anchorable() {
			job = p.CoreJobID
		}
		hooks := "—"
		if len(p.Hooks) > 0 {
			parts := make([]string, len(p.Hooks))
			for i, h := range p.Hooks {
				label := string(h)
				if m, ok := hookMeta(h); ok && !m.fired() { // advertised but not fired by a phase
					label += "*"
					anyUnfired = true
				}
				parts[i] = label
			}
			hooks = strings.Join(parts, ", ")
		}
		acts := "—"
		if len(p.Actions) > 0 {
			parts := make([]string, len(p.Actions))
			for i, a := range p.Actions {
				label := string(a)
				if m, ok := actionMeta(a); ok && m.Driver != "" && !m.DriverWired { // belongs to a cadence, not yet wired in
					label += "†"
					anyUnwired = true
				}
				parts[i] = label
			}
			acts = strings.Join(parts, ", ")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", num, p.Name, p.Engine, job, hooks, acts)
	}
	tw.Flush()
	if anyUnfired {
		fmt.Fprintln(os.Stderr, "\n* hook registered at CLI startup, not fired by a lifecycle phase")
	}
	if anyUnwired {
		fmt.Fprintln(os.Stderr, "† belongs to a cadence workflow but is operator-invoked today (not yet wired into it)")
	}
	fmt.Fprintf(os.Stderr, "\nci: anchors usable in this workflow: %s\n", strings.Join(ciAnchors, ", "))
	fmt.Fprintln(os.Stderr, "DAY-2 ACTIONS run only via gated operator commands / cadence workflows — never by reconcile.")
	return nil
}
