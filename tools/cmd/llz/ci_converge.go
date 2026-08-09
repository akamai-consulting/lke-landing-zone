package main

// ci_converge.go — the capability wiring for the `converge` extension
// (internal/converge).
//
// Unlike every other extension's cobra file, this one holds no commands: converge
// exports its own constructors and ci.go registers them directly. That is a
// deliberate difference and the reason is worth stating, because "lift the cobra
// commands back to package main" is the rule everywhere else.
//
// The rule exists so the CLI's shape stays in the CLI. It buys nothing here.
// converge's seven verbs (health, converge, health-incluster, nudge-argo,
// wait-pods, wait-cluster-ready, wait-apl-pipeline) share one flag vocabulary and
// one exit-code contract, and transcribing them into package main would move ~280
// lines of cobra boilerplate across the boundary while leaving every decision they
// encode on the other side of it. What package main actually owns here is the
// CAPABILITY SET, which is what this file is.
//
// Read this file as a preview of the action ABI: it is the hand-written version of
// what dispatch would do automatically — resolve an extension's declared grants
// into concrete handles, then hand them over.

import (
	"context"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/converge"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/reconciler"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/teardown"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baoread"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghaout"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/health"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kube"
)

// installConvergeDeps hands converge the capabilities it declares. Called once
// from ci.go before any verb runs.
func installConvergeDeps(g globalOpts) {
	converge.Install(converge.Deps{
		Exec:         execOutput,
		ExecCombined: execCombined,
		Summary:      ghaout.Append,
		DryRun:       g.DryRun,
		StripOversizedCRDLastApplied: func() []string {
			return teardown.StripOversizedCRDLastApplied(teardown.KubectlBoolViaExec(teardownDeps()))
		},
		BaoLoopbackEnv:         baoread.LoopbackEnv,
		FirewallDeploymentName: reconciler.FirewallDeploymentName,
		FirewallConfigMapName:  reconciler.FirewallConfigMapName,
		// The in-cluster client is built HERE, not in converge: it is the
		// reconciler's kube client and its nodeGetter interface, and a converge
		// package that imported either would depend on the reconciler's client
		// model to answer a question that only needs a verdict.
		ConvergenceReport: func(ctx context.Context) (health.Report, bool, error) {
			client, err := kube.NewInCluster()
			if err != nil {
				return health.Report{}, false, err
			}
			return reconciler.ConvergenceReport(ctx, client)
		},
	})
}
