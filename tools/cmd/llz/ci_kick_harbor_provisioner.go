package main

// ci_kick_harbor_provisioner.go implements `llz ci kick-harbor-provisioner` —
// an event-driven first tick for the in-cluster harbor-robot-provisioner
// CronJob, run by the bootstrap right before the converge gate.
//
// Without it the converge tail is CRON-paced: llz-cert-automation (and the
// harbor-docker-config ExternalSecret it mounts) chains on the provisioner's
// */5 schedule seeding secret/harbor/robot — worst case a 5m dead wait for the
// first tick plus the ExternalSecret's 1m refreshInterval, the dominant
// always-pay tail the converge budget comment sizes itself around. Creating a
// one-off Job from the CronJob the moment Harbor is reachable collapses that
// wait to the Job's own runtime (seconds), and the post-run force-sync
// collapses the ESO refresh gap the same way nudge-argo does.
//
// Best-effort BY DESIGN: every failure path warns and exits 0. The CronJob's
// next scheduled tick is the standing safety net and `llz ci converge` is the
// verdict — a kick that could fail the bootstrap would turn an optimization
// into a new flake source. Safe to run anywhere: it no-ops cleanly when the
// CronJob doesn't exist (standby/minimal instances) and the provisioner itself
// is idempotent (steady state is a cheap no-op; Harbor not yet serving is a
// clean no-op).

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
)

const kickHarborJobName = "harbor-robot-provisioner-kick"

// Poll knobs — package vars so tests can shrink them (the Job runs under its
// own 240s activeDeadlineSeconds).
var (
	kickHarborJobTimeout  = 300 // seconds to wait for the kicked Job to finish
	kickHarborJobInterval = 5   // seconds between Job status polls
)

func ciKickHarborProvisionerCmd() *cobra.Command {
	var namespace, cronjob string
	var coreTimeout int
	c := &cobra.Command{
		Use:   "kick-harbor-provisioner",
		Short: "force one harbor-robot-provisioner tick now instead of waiting for its */5 schedule (best-effort)",
		Long: "Creates a one-off Job from the harbor-robot-provisioner CronJob so the robot\n" +
			"credentials are seeded NOW rather than at the next */5 tick — the cron wait is\n" +
			"the dominant tail of a fresh bootstrap's converge (robot seed → harbor-docker-\n" +
			"config ExternalSecret → cert-automation rollout). Waits briefly for harbor-core\n" +
			"to be Available first (a tick before Harbor serves is a clean no-op that saves\n" +
			"nothing), waits for the Job, then force-syncs all ExternalSecrets so the seeded\n" +
			"paths propagate immediately. Every step is best-effort and the command always\n" +
			"exits 0: the CronJob's own schedule is the safety net and `llz ci converge` is\n" +
			"the verdict.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			runKickHarborProvisioner(namespace, cronjob, coreTimeout)
			return nil
		},
	}
	c.Flags().StringVar(&namespace, "namespace", "harbor", "namespace of the provisioner CronJob")
	c.Flags().StringVar(&cronjob, "cronjob", "harbor-robot-provisioner", "CronJob to tick")
	c.Flags().IntVar(&coreTimeout, "core-timeout", 120, "seconds to wait for deploy/harbor-core Available before kicking (0 skips the wait)")
	return c
}

func runKickHarborProvisioner(namespace, cronjob string, coreTimeout int) {
	if !kExists("-n", namespace, "get", "cronjob", cronjob) {
		fmt.Printf("cronjob/%s not present in %s — nothing to kick (component disabled or standby).\n", cronjob, namespace)
		return
	}
	// A tick before harbor-core serves is a clean no-op (the provisioner is built
	// that way), so it would save nothing — give Harbor a short window to come up.
	// Best-effort: on timeout, kick anyway; the CronJob schedule is the safety net.
	if coreTimeout > 0 {
		if _, err := execOutput("kubectl", "-n", namespace, "wait", "deploy/harbor-core",
			"--for=condition=Available", fmt.Sprintf("--timeout=%ds", coreTimeout)); err != nil {
			fmt.Fprintf(os.Stderr, "::warning::harbor-core not Available within %ds — kicking anyway (a premature tick no-ops; the */5 schedule remains the safety net): %v\n", coreTimeout, err)
		}
	}
	// Fresh Job; drop a prior kick first so re-runs are clean.
	execCombined("kubectl", "-n", namespace, "delete", "job", kickHarborJobName, "--ignore-not-found")
	if out, err := execOutput("kubectl", "-n", namespace, "create", "job",
		"--from=cronjob/"+cronjob, kickHarborJobName); err != nil {
		// Most likely Kyverno signature admission (verify-llz-image-signature) still
		// settling — the preflight admit loop runs before this in the bootstrap, so
		// a denial here is surfaced there too; the cron tick will retry on schedule.
		fmt.Fprintf(os.Stderr, "::warning::could not create the kick Job from cronjob/%s (ignored — the */5 schedule will run it): %v\n%s\n", cronjob, err, out)
		return
	}
	fmt.Printf("kicked cronjob/%s (job %s/%s) — waiting up to %ds for it to finish…\n",
		cronjob, namespace, kickHarborJobName, kickHarborJobTimeout)

	succeeded, failed := waitKickHarborJob(namespace)
	switch {
	case succeeded:
		fmt.Println("✓ harbor-robot-provisioner tick completed")
	case failed:
		fmt.Fprintf(os.Stderr, "::warning::the kicked provisioner Job failed (ignored — converge is the verdict):\n%s\n",
			execCombined("kubectl", "-n", namespace, "logs", "job/"+kickHarborJobName, "--tail=50"))
	default:
		fmt.Fprintf(os.Stderr, "::warning::the kicked provisioner Job did not finish within %ds (ignored — converge is the verdict)\n", kickHarborJobTimeout)
	}
	// Propagate the just-seeded KV paths immediately instead of waiting out the
	// ExternalSecrets' refreshInterval — same annotation bump nudge-argo uses.
	stamp := fmt.Sprintf("force-sync=%d", nowUnix())
	if _, err := execOutput("kubectl", "annotate", "externalsecret", "--all-namespaces", "--all",
		stamp, "--overwrite"); err != nil {
		fmt.Fprintf(os.Stderr, "::warning::force-sync of ExternalSecrets after the kick failed (ignored): %v\n", err)
	} else {
		fmt.Println("force-synced all ExternalSecrets")
	}
}

// waitKickHarborJob polls the kicked Job until success/failure or the budget
// runs out (same status contract as waitBroadPATJob, reusing its parser).
func waitKickHarborJob(namespace string) (succeeded, failed bool) {
	deadline := time.Now().Add(time.Duration(kickHarborJobTimeout) * time.Second)
	for {
		out, _ := execOutput("kubectl", "-n", namespace, "get", "job", kickHarborJobName,
			"-o", "jsonpath={.status.succeeded}/{.status.failed}")
		if succ, fail := parseJobStatus(string(out)); succ || fail {
			return succ, fail
		}
		if !time.Now().Before(deadline) {
			return false, false
		}
		time.Sleep(time.Duration(kickHarborJobInterval) * time.Second)
	}
}
