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
//  2. Wait for the `openbao` ClusterSecretStore to actually go Ready, then bump a
//     `force-sync` annotation on every ExternalSecret. ESO's ExternalSecret
//     controller does NOT watch SecretStore status, so when the store recovers
//     (post unseal+seed) the secrets would otherwise idle until their own
//     refreshInterval — observed as ~14m of CreateContainerConfigError on
//     harbor-registry / loki-0 after the store was already Ready. The annotation
//     bump forces an immediate reconcile, collapsing that gap to seconds.

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
// binds to; the force-sync waits on its Ready condition first.
const defaultSecretStore = "openbao"

// nowUnix is a seam so the force-sync annotation value is deterministic in tests.
var nowUnix = func() int64 { return time.Now().Unix() }

type nudgeOpts struct {
	apps         []string
	store        string // ClusterSecretStore to wait Ready before force-syncing ("" skips the force-sync)
	storeTimeout int    // seconds to wait for the store Ready condition
}

func ciNudgeArgoCmd() *cobra.Command {
	o := nudgeOpts{}
	c := &cobra.Command{
		Use:   "nudge-argo",
		Short: "refresh+sync Argo apps, then force-sync ExternalSecrets once the store is Ready (best-effort)",
		Long: "Native port of the \"Nudge Argo CD to converge secrets (post-seed)\" step.\n" +
			"First annotates each Application with argocd.argoproj.io/refresh=hard and\n" +
			"patches a fresh sync operation onto it (re-triggering any sync an earlier race\n" +
			"drove to a terminal failure). Then waits for the ClusterSecretStore to go Ready\n" +
			"and bumps a force-sync annotation on every ExternalSecret — ESO doesn't\n" +
			"re-trigger ExternalSecrets when their store recovers, so without this they idle\n" +
			"until their own refreshInterval. Every kubectl call is best-effort; this never\n" +
			"fails the job. Defaults to the llz-secret-store + platform-bootstrap apps and\n" +
			"the openbao store.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCINudgeArgo(gopts, o) },
	}
	c.Flags().StringSliceVar(&o.apps, "apps", defaultNudgeApps, "Argo CD Applications (argocd namespace) to refresh + sync")
	c.Flags().StringVar(&o.store, "secret-store", defaultSecretStore, "ClusterSecretStore to wait Ready before force-syncing ExternalSecrets (empty skips the force-sync)")
	c.Flags().IntVar(&o.storeTimeout, "store-timeout", 300, "seconds to wait for the ClusterSecretStore Ready condition before force-syncing")
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
		fmt.Fprintf(os.Stderr, "→ (dry-run) would wait for clustersecretstore/%s Ready then force-sync all ExternalSecrets\n", o.store)
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
	// Block until the store can actually serve (post unseal + bao-configure), THEN
	// force every ExternalSecret to reconcile NOW. Best-effort: even if the store
	// never reports Ready within the budget we still bump the annotation (harmless;
	// ESO will retry), so a slow store doesn't strand the secrets at 1h.
	if _, err := execOutput("kubectl", "wait", "--for=condition=Ready",
		"clustersecretstore/"+o.store, fmt.Sprintf("--timeout=%ds", o.storeTimeout)); err != nil {
		fmt.Fprintf(os.Stderr, "nudge: clustersecretstore/%s not Ready within %ds (force-syncing anyway): %v\n", o.store, o.storeTimeout, err)
	} else {
		fmt.Printf("clustersecretstore/%s Ready\n", o.store)
	}
	// A changing annotation value is what triggers an immediate ESO reconcile;
	// --all-namespaces --all covers every store-gated ExternalSecret without
	// hardcoding their names/namespaces.
	stamp := fmt.Sprintf("force-sync=%d", nowUnix())
	if _, err := execOutput("kubectl", "annotate", "externalsecret", "--all-namespaces", "--all",
		stamp, "--overwrite"); err != nil {
		fmt.Fprintf(os.Stderr, "nudge: force-sync ExternalSecrets failed (ignored): %v\n", err)
	} else {
		fmt.Println("force-synced all ExternalSecrets")
	}
	return nil
}
