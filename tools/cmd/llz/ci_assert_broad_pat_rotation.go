package main

// ci_assert_broad_pat_rotation.go implements `llz ci assert-broad-pat-rotation
// --region <env>` — the e2e gate that actually EXERCISES the in-cluster broad-PAT
// rotator end-to-end, instead of only asserting it deployed.
//
// converge sees the rotator's carved App green the moment its CronJob + the two
// ESO Secrets exist — but the CronJob is weekly, so nothing proves the in-cluster
// mint → verify → OpenBao write → GitHub env-secret publish → revoke path works
// against real OpenBao k8s-auth, real Linode, and real GitHub until it runs. This
// forces one run now: it creates a one-off Job from the CronJob (which carries
// args ["ci","rotate-broad-pat","--apply"] and reads the rotated_at=0 the bootstrap
// seeded, so the tick is DUE), waits for it, and asserts the audit record reports
// action=rotated. A real in-cluster failure (bad OpenBao role, missing scope,
// GitHub write denied) surfaces as a failed Job / non-rotated action and fails e2e.
//
// SAFE BY CONSTRUCTION: the e2e deployment enables the rotator with an e2e-UNIQUE
// BROAD_PAT_LABEL and BROAD_PAT_DEPLOYMENTS=e2e, so the mint/revoke only ever touch
// the e2e PAT family and the only env secret rewritten is infra-e2e's. Self-gates
// on the component being enabled for the env, so it is a clean no-op anywhere it is
// not (it is wired behind the assert_loki e2e gate, which only release-e2e sets).

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
)

const (
	broadPATRotatorNS      = "llz-pat-rotator"
	broadPATRotatorCronJob = "broad-pat-rotator"
	broadPATRotatorE2EJob  = "broad-pat-rotator-e2e"
)

// Poll knobs — package vars so tests can shrink them (the real run polls the Job
// under its own 300s activeDeadlineSeconds + backoffLimit 1).
var (
	broadPATRotationPollTimeout  = 6 * time.Minute
	broadPATRotationPollInterval = 10 * time.Second
)

func ciAssertBroadPATRotationCmd() *cobra.Command {
	var region string
	c := &cobra.Command{
		Use:   "assert-broad-pat-rotation",
		Short: "e2e: force one broad-PAT rotation Job and assert it rotated (mint→OpenBao→GitHub→revoke)",
		Long: "e2e gate that EXERCISES the in-cluster broad-PAT rotator: creates a one-off\n" +
			"Job from the broad-pat-rotator CronJob (--apply, seeded rotated_at=0 makes it\n" +
			"due), waits for it, and asserts the audit record reports action=rotated. No-ops\n" +
			"unless spec.components.broadPatRotator is enabled for --region. Safe because e2e\n" +
			"enables the rotator with an e2e-unique label + broadPATDeployments=e2e, so the\n" +
			"mint/revoke touch only the e2e PAT family and only infra-e2e is rewritten. Uses\n" +
			"the job's default kubeconfig, same as the converge poll it runs after.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runAssertBroadPATRotation(region) },
	}
	c.Flags().StringVar(&region, "region", "", "deployment (spec env name) whose broadPatRotator toggle gates the exercise (required)")
	return c
}

func runAssertBroadPATRotation(region string) error {
	if region == "" {
		return fmt.Errorf("--region is required")
	}
	lz, err := clusterspec.LoadInstance(".")
	if err != nil {
		return fmt.Errorf("assert-broad-pat-rotation: load spec: %w", err)
	}
	if !broadPATSeedEnabled(lz, region) {
		fmt.Printf("broadPatRotator not enabled for %s — skipping rotation exercise.\n", region)
		return nil
	}

	// Fresh Job from the CronJob; drop a prior exercise Job first so re-runs are clean.
	execCombined("kubectl", "-n", broadPATRotatorNS, "delete", "job", broadPATRotatorE2EJob, "--ignore-not-found")
	if out, err := execOutput("kubectl", "-n", broadPATRotatorNS, "create", "job", broadPATRotatorE2EJob,
		"--from=cronjob/"+broadPATRotatorCronJob); err != nil {
		return fmt.Errorf("create rotation Job from cronjob/%s: %w\n%s", broadPATRotatorCronJob, err, out)
	}
	fmt.Printf("created Job %s/%s from cronjob/%s — waiting up to %s for it to finish…\n",
		broadPATRotatorNS, broadPATRotatorE2EJob, broadPATRotatorCronJob, broadPATRotationPollTimeout)

	succeeded, failed := waitBroadPATJob()
	logs := execCombined("kubectl", "-n", broadPATRotatorNS, "logs", "job/"+broadPATRotatorE2EJob, "--tail=-1")
	fmt.Println("── broad-pat-rotator Job logs ──")
	fmt.Println(logs)

	if !succeeded {
		reason := "timed out"
		if failed {
			reason = "the Job failed"
		}
		return fmt.Errorf("broad-PAT rotation did not succeed (%s) — see the Job logs above", reason)
	}
	action, ok := parseRotationAction(logs)
	if !ok {
		return fmt.Errorf("broad-PAT rotation Job produced no audit record (event=broad-pat-rotator) — see the logs above")
	}
	if action != "rotated" {
		return fmt.Errorf("broad-PAT rotation asserted action=rotated, got %q (rotated_at not due, --apply missing, or a partial publish?)", action)
	}
	fmt.Printf("✓ broad-PAT rotation exercised end-to-end (action=%s)\n", action)
	return nil
}

// waitBroadPATJob polls the exercise Job until it reports success or failure (or
// the poll budget runs out).
func waitBroadPATJob() (succeeded, failed bool) {
	deadline := time.Now().Add(broadPATRotationPollTimeout)
	for {
		out, _ := execOutput("kubectl", "-n", broadPATRotatorNS, "get", "job", broadPATRotatorE2EJob,
			"-o", "jsonpath={.status.succeeded}/{.status.failed}")
		if succ, fail := parseJobStatus(string(out)); succ || fail {
			return succ, fail
		}
		if !time.Now().Before(deadline) {
			return false, false
		}
		time.Sleep(broadPATRotationPollInterval)
	}
}

// parseJobStatus reads the `{.status.succeeded}/{.status.failed}` jsonpath output
// (each side empty/0 until set) into two booleans. Succeeded wins if both are set.
func parseJobStatus(s string) (succeeded, failed bool) {
	succStr, failStr, _ := strings.Cut(strings.TrimSpace(s), "/")
	return isPositiveCount(succStr), isPositiveCount(failStr)
}

// isPositiveCount reports whether a jsonpath count field is a number ≥ 1 (an
// unset field renders empty; a live one renders "1", "2", …).
func isPositiveCount(s string) bool {
	s = strings.TrimSpace(s)
	return s != "" && s != "0"
}

// parseRotationAction finds the rotator's JSON audit line (event=broad-pat-rotator,
// emitted once by cli.PrintRecord) in the Job logs and returns its "action". Other
// lines (masked-token warnings on stderr, non-JSON) are skipped.
func parseRotationAction(logs string) (string, bool) {
	for _, line := range strings.Split(logs, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["event"] != "broad-pat-rotator" {
			continue
		}
		if action, ok := rec["action"].(string); ok {
			return action, true
		}
	}
	return "", false
}
