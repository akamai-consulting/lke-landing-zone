package main

// ci_nudge_argo.go implements `llz ci nudge-argo` — the native port of the
// "Nudge Argo CD to converge secrets (post-seed)" inline-bash loop in
// llz-bootstrap-openbao.yml. It does two things, both best-effort (never fails
// the bootstrap):
//
//  1. For each named Argo CD Application, force a hard refresh + a fresh sync, so
//     the apps that own the ClusterSecretStore + ExternalSecrets re-apply now
//     instead of at the next reconcile — and so an earlier first-boot race that
//     drove a sync to a terminally-failed state gets re-attempted (Argo CD does
//     not auto-retry a failed sync to the same revision).
//
//  2. Bump a revalidation annotation on the `openbao` ClusterSecretStore and wait
//     for it to go Ready — a converge precondition CI is uniquely placed to
//     assert, because only CI knows seeding just finished. (ESO revalidates a
//     store on its own cadence; the bump makes that Ready wait event-paced.)
//
// It NO LONGER force-syncs the ExternalSecrets (secrets-before-apps Phase 3): the
// in-cluster es-store-recovery reconciler lane watches this store and force-syncs
// every ExternalSecret AND PushSecret on the not-Ready→Ready transition the bump
// above triggers. Hand-off is evidence-backed — a cold e2e reported
// llz_es_recovery_nudges_total=1 — and the lane is strictly broader than the CI
// half was (PushSecrets too, plus day-2 store blips CI never observes).

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
)

// defaultNudgeApps are the apps the post-seed nudge refreshes: the carved-out
// ClusterSecretStore app and the main bootstrap app-of-apps that owns the
// ExternalSecrets gated on the just-seeded KV paths.
var defaultNudgeApps = []string{"llz-secret-store", "platform-bootstrap"}

// defaultSecretStore is the ClusterSecretStore every platform ExternalSecret
// binds to; the nudge revalidates it and waits on its Ready condition.
const defaultSecretStore = "openbao"

// nowUnix is a seam so the revalidation annotation value is deterministic in tests.
var nowUnix = func() int64 { return time.Now().Unix() }

type nudgeOpts struct {
	apps         []string
	store        string // ClusterSecretStore to revalidate + wait Ready ("" skips that half)
	storeTimeout int    // seconds to wait for the store Ready condition
}

func ciNudgeArgoCmd() *cobra.Command {
	o := nudgeOpts{}
	c := &cobra.Command{
		Use:   "nudge-argo",
		Short: "refresh+sync Argo apps and revalidate the ClusterSecretStore post-seed (best-effort)",
		Long: "Native port of the \"Nudge Argo CD to converge secrets (post-seed)\" step.\n" +
			"First annotates each Application with argocd.argoproj.io/refresh=hard and\n" +
			"patches a fresh sync operation onto it (re-triggering any sync an earlier race\n" +
			"drove to a terminal failure). Then bumps a revalidation annotation on the\n" +
			"ClusterSecretStore and waits for it to go Ready — the converge precondition\n" +
			"only CI can assert, since only CI knows seeding just finished.\n\n" +
			"It does NOT force-sync the ExternalSecrets: the in-cluster es-store-recovery\n" +
			"reconciler lane owns that, firing on the Ready transition this bump triggers\n" +
			"(and covering PushSecrets + day-2 blips the CI half never did). Every kubectl\n" +
			"call is best-effort; this never fails the job. Defaults to the llz-secret-store\n" +
			"+ platform-bootstrap apps and the openbao store.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCINudgeArgo(gopts, o) },
	}
	c.Flags().StringSliceVar(&o.apps, "apps", defaultNudgeApps, "Argo CD Applications (argocd namespace) to refresh + sync")
	c.Flags().StringVar(&o.store, "secret-store", defaultSecretStore, "ClusterSecretStore to revalidate and wait Ready (empty skips that half)")
	c.Flags().IntVar(&o.storeTimeout, "store-timeout", 300, "seconds to wait for the ClusterSecretStore Ready condition")
	return c
}

func runCINudgeArgo(g globalOpts, o nudgeOpts) error {
	const syncPatch = `{"operation":{"initiatedBy":{"username":"bootstrap-openbao"},"sync":{}}}`
	for _, app := range o.apps {
		if g.dryRun {
			fmt.Fprintf(os.Stderr, "→ (dry-run) would hard-refresh + sync application %s\n", app)
			continue
		}
		// Best-effort: a refresh/patch failure (app not yet created, transient
		// apiserver blip) must not fail the bootstrap — the in-cluster
		// argo-resync-nudger CronJob is the standing safety net. Errors are
		// logged, not returned.
		if _, err := execOutput("kubectl", "-n", "argocd", "annotate", "application", app,
			"argocd.argoproj.io/refresh=hard", "--overwrite"); err != nil {
			fmt.Fprintf(os.Stderr, "nudge %s: refresh annotate failed (ignored): %v\n", app, err)
		}
		if _, err := execOutput("kubectl", "-n", "argocd", "patch", "application", app,
			"--type", "merge", "-p", syncPatch); err != nil {
			fmt.Fprintf(os.Stderr, "nudge %s: sync patch failed (ignored): %v\n", app, err)
		}
		fmt.Printf("nudged %s\n", app)
	}

	if o.store == "" {
		return nil
	}
	if g.dryRun {
		fmt.Fprintf(os.Stderr, "→ (dry-run) would revalidate clustersecretstore/%s and wait for it to go Ready\n", o.store)
		return nil
	}
	// The store's Ready condition only flips when ESO re-VALIDATES the store, and
	// ESO does that on its own retry/refresh cadence — observed as ~2m of pure
	// waiting in the post-seed nudge while OpenBao was already serving. A changing
	// annotation on the ClusterSecretStore triggers an immediate reconcile (and
	// with it the validation), making the Ready wait below event-paced instead of
	// timer-paced. Best-effort like everything else here.
	stampStore := fmt.Sprintf("force-sync=%d", nowUnix())
	if _, err := execOutput("kubectl", "annotate", "clustersecretstore", o.store,
		stampStore, "--overwrite"); err != nil {
		fmt.Fprintf(os.Stderr, "nudge: clustersecretstore/%s revalidation bump failed (ignored): %v\n", o.store, err)
	}
	// Block until the store can actually serve (post unseal + bao-configure) — a
	// converge precondition CI is uniquely placed to assert, since it alone knows
	// seeding just finished. Best-effort: a store that never reports Ready in the
	// budget is left to converge to adjudicate.
	if _, err := execOutput("kubectl", "wait", "--for=condition=Ready",
		"clustersecretstore/"+o.store, fmt.Sprintf("--timeout=%ds", o.storeTimeout)); err != nil {
		fmt.Fprintf(os.Stderr, "nudge: clustersecretstore/%s not Ready within %ds (converge will adjudicate): %v\n", o.store, o.storeTimeout, err)
	} else {
		fmt.Printf("clustersecretstore/%s Ready\n", o.store)
	}
	// NOTE (secrets-before-apps Phase 3): the blanket
	// `kubectl annotate externalsecret --all-namespaces --all force-sync=<ts>`
	// that used to run here is GONE. The in-cluster es-store-recovery reconciler
	// lane owns it now — it watches this very store and force-syncs every
	// ExternalSecret AND PushSecret on the not-Ready→Ready transition this bump
	// triggers. That hand-off is evidence-backed, not assumed: a cold e2e reported
	// `llz_es_recovery_nudges_total=1`, i.e. the lane fired exactly once on the
	// recovery. The lane is strictly better than the CI half was — it also covers
	// PushSecrets (which the CI annotate never touched) and day-2 store blips CI
	// never sees.
	return nil
}
