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
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
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

	// Build the exercise Job from the CronJob's template with ROTATE_AFTER_DAYS=0
	// injected, so the tick is DUE by construction — regardless of rotated_at.
	// The bootstrap seed writes rotated_at=0 but is --skip-if-present on the token,
	// so a REUSED cluster keeps the last rotation's recent timestamp and a plain
	// `create job --from=cronjob` run (correctly) skips as "not due". An earlier
	// iteration reset rotated_at via `bao kv patch` with OPENBAO_ROOT_TOKEN — but
	// the workflow REVOKES the root token before the e2e asserts run, so that
	// patch 403s (observed live). Overriding the threshold in the Job itself needs
	// no credential at all: the rotator runs under its own k8s-auth role, which
	// legitimately writes secret/linode/broad-pat.
	cronJSON, err := execOutput("kubectl", "-n", broadPATRotatorNS, "get",
		"cronjob", broadPATRotatorCronJob, "-o", "json")
	if err != nil {
		return fmt.Errorf("read cronjob/%s: %w", broadPATRotatorCronJob, err)
	}
	jobJSON, err := e2eRotationJobJSON(cronJSON)
	if err != nil {
		return fmt.Errorf("build the exercise Job from cronjob/%s: %w", broadPATRotatorCronJob, err)
	}

	// Fresh Job; drop a prior exercise Job first so re-runs are clean.
	execCombined("kubectl", "-n", broadPATRotatorNS, "delete", "job", broadPATRotatorE2EJob, "--ignore-not-found")
	if out, err := kubectlApplyStdin(jobJSON); err != nil {
		return fmt.Errorf("create rotation Job (ROTATE_AFTER_DAYS=0): %w\n%s", err, out)
	}
	fmt.Printf("created Job %s/%s from cronjob/%s — waiting up to %s for it to finish…\n",
		broadPATRotatorNS, broadPATRotatorE2EJob, broadPATRotatorCronJob, broadPATRotationPollTimeout)

	succeeded, failed := waitBroadPATJob()
	logs := broadPATJobLogs()
	fmt.Println("── broad-pat-rotator Job logs ──")
	fmt.Println(logs)

	if !succeeded {
		// `kubectl logs job/<j>` (and the logs above) block on a RUNNING pod and, once
		// the pod is gone — killed at the Job's activeDeadlineSeconds, or never started
		// (ImagePullBackOff / admission-denied / Pending) — print only "timed out
		// waiting for the condition", masking WHY. Dump the pod state + events so the
		// real cause is visible in the e2e log (we cannot introspect the e2e cluster
		// out-of-band — it holds no long-lived kubeconfig).
		dumpBroadPATRotatorDiag()
		reason := "timed out"
		if failed {
			reason = "the Job failed"
		}
		return fmt.Errorf("broad-PAT rotation did not succeed (%s) — see the diagnostics above", reason)
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

// e2eRotationJobJSON builds the one-off exercise Job from the CronJob's own
// jobTemplate, with ROTATE_AFTER_DAYS=0 upserted into the rotate container's env
// so the tick is due BY CONSTRUCTION (isDue: now - rotated_at >= 0 always). Pure —
// unit-tested against a real CronJob shape. Everything else (image, k8s-auth SA,
// secrets, network policy labels) is exactly what the CronJob would run.
func e2eRotationJobJSON(cronJobJSON []byte) ([]byte, error) {
	var cj map[string]any
	if err := json.Unmarshal(cronJobJSON, &cj); err != nil {
		return nil, fmt.Errorf("parse CronJob JSON: %w", err)
	}
	spec, _ := cj["spec"].(map[string]any)
	jt, _ := spec["jobTemplate"].(map[string]any)
	jtSpec, _ := jt["spec"].(map[string]any)
	if jtSpec == nil {
		return nil, fmt.Errorf("CronJob has no .spec.jobTemplate.spec")
	}

	// Upsert two env overrides on every container (there is exactly one):
	//   ROTATE_AFTER_DAYS=0 — the tick is DUE by construction (isDue always true).
	//   GRACE_DAYS=0        — revoke ALL superseded siblings immediately, so the
	//                         exercise self-reaps. The e2e forces a rotation every
	//                         run (autonomous keep_cluster loop → many runs/day);
	//                         the production 7-day grace would otherwise leave
	//                         dozens of live account:read_write PATs accumulating
	//                         under the e2e label. Single-cluster e2e needs no
	//                         propagation grace. (Reviewer finding #3.)
	overrides := map[string]string{"ROTATE_AFTER_DAYS": "0", "GRACE_DAYS": "0"}
	tmpl, _ := jtSpec["template"].(map[string]any)
	tmplSpec, _ := tmpl["spec"].(map[string]any)
	containers, _ := tmplSpec["containers"].([]any)
	if len(containers) == 0 {
		return nil, fmt.Errorf("CronJob jobTemplate has no containers")
	}
	for _, c := range containers {
		cm, _ := c.(map[string]any)
		env, _ := cm["env"].([]any)
		for name, val := range overrides {
			replaced := false
			for _, e := range env {
				em, _ := e.(map[string]any)
				if em["name"] == name {
					em["value"] = val
					delete(em, "valueFrom")
					replaced = true
				}
			}
			if !replaced {
				env = append(env, map[string]any{"name": name, "value": val})
			}
		}
		cm["env"] = env
	}

	job := map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]any{
			"name":      broadPATRotatorE2EJob,
			"namespace": broadPATRotatorNS,
			"labels":    map[string]any{"app.kubernetes.io/created-by": "llz-e2e"},
		},
		"spec": jtSpec,
	}
	return json.Marshal(job)
}

// kubectlApplyStdin applies a manifest via `kubectl apply -f -` (stdin), returning
// combined output. Local helper — the assert file drives raw exec, not a deps seam.
var kubectlApplyStdin = func(manifest []byte) (string, error) {
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = bytes.NewReader(manifest)
	out, ok := runCombined(cmd)
	if !ok {
		return out, fmt.Errorf("kubectl apply failed")
	}
	return out, nil
}

// broadPATJobLogs fetches the exercise pods' logs by POD NAME (current + previous),
// which — unlike `kubectl logs job/<j>` — does not block waiting for a Running pod,
// so it still returns a terminated/killed pod's output. On the success path this
// carries the audit record parseRotationAction reads; on failure it captures
// whatever the container managed to print before it died.
func broadPATJobLogs() string {
	sel := "job-name=" + broadPATRotatorE2EJob
	names := execCombined("kubectl", "-n", broadPATRotatorNS, "get", "pods", "-l", sel,
		"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	var b strings.Builder
	for _, p := range strings.Fields(names) {
		b.WriteString(execCombined("kubectl", "-n", broadPATRotatorNS, "logs", p, "--tail=-1"))
		b.WriteString("\n")
	}
	if strings.TrimSpace(b.String()) == "" {
		// No pod (or no logs yet) — fall back to the job selector form so a
		// completed-and-still-present pod's logs are not lost.
		return execCombined("kubectl", "-n", broadPATRotatorNS, "logs", "job/"+broadPATRotatorE2EJob, "--tail=-1")
	}
	return b.String()
}

// dumpBroadPATRotatorDiag prints the exercise Job's pod state, events, and
// terminated-container logs so a failed run reports the ACTUAL cause (ImagePullBackOff,
// admission-denied, Pending-no-node, or a real rotate error) instead of the masking
// "timed out waiting for the condition". Best-effort and read-only — every call is a
// plain kubectl get/describe/logs whose output is echoed into the e2e job log.
func dumpBroadPATRotatorDiag() {
	sel := "job-name=" + broadPATRotatorE2EJob
	fmt.Println("── broad-pat-rotator diagnostics (why the Job did not succeed) ──")
	fmt.Println(execCombined("kubectl", "-n", broadPATRotatorNS, "get", "pods", "-l", sel, "-o", "wide"))
	fmt.Println(execCombined("kubectl", "-n", broadPATRotatorNS, "describe", "job", broadPATRotatorE2EJob))
	fmt.Println(execCombined("kubectl", "-n", broadPATRotatorNS, "describe", "pods", "-l", sel))
	fmt.Println(execCombined("kubectl", "-n", broadPATRotatorNS, "get", "events", "--sort-by=.lastTimestamp"))
	names := execCombined("kubectl", "-n", broadPATRotatorNS, "get", "pods", "-l", sel,
		"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	for _, p := range strings.Fields(names) {
		fmt.Printf("── logs %s (previous) ──\n%s\n", p,
			execCombined("kubectl", "-n", broadPATRotatorNS, "logs", p, "--previous", "--tail=-1"))
	}
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
