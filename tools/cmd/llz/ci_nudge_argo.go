package main

// ci_nudge_argo.go implements `llz ci nudge-argo` — the native port of the
// "Nudge Argo CD to converge secrets (post-seed)" inline-bash loop in
// llz-bootstrap-openbao.yml. For each named Argo CD Application it forces a hard
// refresh and kicks a fresh sync operation, so the apps that own the
// ClusterSecretStore + ExternalSecrets converge the instant seeding unblocks
// them instead of at the next reconcile — AND so an earlier first-boot race that
// drove a sync to a terminally-failed state gets re-attempted (Argo CD does not
// auto-retry a failed sync to the same revision). Best-effort, like the bash:
// every kubectl call is `|| true`, and the command never fails the bootstrap.

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// defaultNudgeApps are the apps the post-seed nudge targets: the carved-out
// ClusterSecretStore app and the main bootstrap app-of-apps that owns the
// ExternalSecrets gated on the just-seeded KV paths.
var defaultNudgeApps = []string{"llz-secret-store", "platform-bootstrap"}

func ciNudgeArgoCmd() *cobra.Command {
	var apps []string
	c := &cobra.Command{
		Use:   "nudge-argo",
		Short: "force a hard refresh + sync of Argo CD Applications (best-effort)",
		Long: "Native port of the \"Nudge Argo CD to converge secrets (post-seed)\" step.\n" +
			"Annotates each Application with argocd.argoproj.io/refresh=hard and patches\n" +
			"a fresh sync operation onto it, collapsing the latency between seeding the\n" +
			"OpenBao KV paths and the ClusterSecretStore/ExternalSecrets going Ready, and\n" +
			"re-triggering any sync an earlier race drove to a terminal failure. Every\n" +
			"kubectl call is best-effort (the bash `|| true`); this never fails the job.\n" +
			"Defaults to the llz-secret-store + platform-bootstrap apps.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCINudgeArgo(gopts, apps) },
	}
	c.Flags().StringSliceVar(&apps, "apps", defaultNudgeApps, "Argo CD Applications (argocd namespace) to refresh + sync")
	return c
}

func runCINudgeArgo(g globalOpts, apps []string) error {
	const syncPatch = `{"operation":{"initiatedBy":{"username":"bootstrap-openbao"},"sync":{}}}`
	for _, app := range apps {
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
	return nil
}
