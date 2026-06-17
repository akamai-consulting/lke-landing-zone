package main

// ci_readiness.go implements `llz ci assert-loki` and `llz ci wait-harbor` — the
// native ports of assert-loki-bootstrapped.sh and wait-for-harbor.sh. The Loki
// classification (pod readiness, S3-config detection) is the tested internal/
// health predicates; this file is the kubectl orchestration + Harbor poll loops.

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/health"
	"github.com/spf13/cobra"
)

func ciAssertLokiCmd() *cobra.Command {
	var nameMatch string
	c := &cobra.Command{
		Use:   "assert-loki",
		Short: "fail unless Loki is bootstrapped (workloads Ready + S3-backed) on the current cluster",
		Long: "Native port of assert-loki-bootstrapped.sh. Asserts Loki's workloads are Ready\n" +
			"AND its config references S3 object storage (the kyverno loki-s3-object-store\n" +
			"policy mutates object_store filesystem→s3 — \"s3-backed\" is the real signal log\n" +
			"persistence works). Best-effort reports the Loki Argo Application status\n" +
			"(non-gating). Exit 0 bootstrapped, 1 otherwise.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			os.Exit(runCIAssertLoki(nameMatch))
			return nil
		},
	}
	c.Flags().StringVar(&nameMatch, "name-match", "loki", "substring/regex identifying Loki workloads/objects")
	return c
}

func ciWaitHarborCmd() *cobra.Command {
	var harborURL string
	c := &cobra.Command{
		Use:   "wait-harbor",
		Short: "wait for Harbor to be ready (admin Secret, deployment/STS rollouts, API ping)",
		Long: "Native port of wait-for-harbor.sh. Polls for the harbor-admin-password Secret\n" +
			"(10s × up to 600s), waits for the Harbor Deployments + StatefulSets to roll\n" +
			"out, then — if --harbor-url is set — pings the Harbor API. Exit 0 ready, 1 on\n" +
			"any timeout.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			os.Exit(runCIWaitHarbor(harborURL))
			return nil
		},
	}
	c.Flags().StringVar(&harborURL, "harbor-url", os.Getenv("HARBOR_URL"), "Harbor base URL for the API ping (empty skips the ping)")
	return c
}

// ── assert-loki ──────────────────────────────────────────────────────────────

type lokiPod struct {
	ns, name string
	status   health.PodStatus
}

func runCIAssertLoki(nameMatch string) int {
	fail := false
	fmt.Println("## Loki bootstrap assertion")

	// 1. Loki workloads exist and are Ready.
	pods := lokiPods(nameMatch)
	if len(pods) == 0 {
		fmt.Printf("FAIL: no Loki pods found (matched name~=%q)\n", nameMatch)
		fail = true
	} else {
		var notReady []string
		for _, p := range pods {
			if !health.LokiPodReady(p.status) {
				notReady = append(notReady, fmt.Sprintf("%s/%s phase=%s", p.ns, p.name, p.status.Phase))
			}
		}
		if len(notReady) > 0 {
			fmt.Println("FAIL: Loki pods not Ready:")
			for _, n := range notReady {
				fmt.Println("  " + n)
			}
			fail = true
		} else {
			fmt.Printf("OK: %d Loki pod(s) Ready\n", len(pods))
		}
	}

	// 2. Loki is configured for S3 object storage (not the filesystem default).
	if health.LokiConfigUsesS3(lokiConfigText(nameMatch)) {
		fmt.Println("OK: Loki config references S3 object storage")
	} else {
		fmt.Println("FAIL: Loki config does not reference S3 — still on the filesystem default? (kyverno loki-s3-object-store may not have applied)")
		fail = true
	}

	// 3. Best-effort Argo CD Application status (non-gating).
	if kExists("get", "crd", "applications.argoproj.io") {
		re := regexp.MustCompile(nameMatch)
		for _, raw := range kItems("get", "applications.argoproj.io", "-A") {
			a, err := health.ParseArgoApp(raw)
			if err != nil || !re.MatchString(a.Name) {
				continue
			}
			if a.Sync == "Synced" && a.Health == "Healthy" {
				fmt.Printf("OK: Argo Application %s Synced + Healthy\n", a.Name)
			} else {
				fmt.Printf("WARN: Argo Application %s sync=%s health=%s (not gating on this)\n", a.Name, a.Sync, a.Health)
			}
			break
		}
	}

	if fail {
		fmt.Fprintln(os.Stderr, "::error::Loki is not bootstrapped")
		return 1
	}
	fmt.Println("Loki is bootstrapped.")
	return 0
}

// lokiPods returns the Loki pods, preferring the app.kubernetes.io/name label and
// falling back to a name-regex match over all pods (so it doesn't depend on one
// labelling convention).
func lokiPods(match string) []lokiPod {
	items := kItems("get", "pods", "-A", "-l", "app.kubernetes.io/name="+match)
	filterByName := false
	if len(items) == 0 {
		items = kItems("get", "pods", "-A")
		filterByName = true
	}
	re := regexp.MustCompile(match)
	var out []lokiPod
	for _, raw := range items {
		var p struct {
			Metadata struct {
				Namespace string `json:"namespace"`
				Name      string `json:"name"`
			} `json:"metadata"`
			Status health.PodStatus `json:"status"`
		}
		if json.Unmarshal(raw, &p) != nil {
			continue
		}
		if filterByName && !re.MatchString(p.Metadata.Name) {
			continue
		}
		out = append(out, lokiPod{p.Metadata.Namespace, p.Metadata.Name, p.Status})
	}
	return out
}

// lokiConfigText concatenates the data values of every name-matching ConfigMap
// (where the rendered Loki config lives) so the S3 detection can scan it.
func lokiConfigText(match string) string {
	re := regexp.MustCompile(match)
	var b strings.Builder
	for _, raw := range kItems("get", "configmap", "-A") {
		var cm struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Data map[string]string `json:"data"`
		}
		if json.Unmarshal(raw, &cm) != nil || !re.MatchString(cm.Metadata.Name) {
			continue
		}
		for _, v := range cm.Data {
			b.WriteString(v)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// ── wait-harbor ──────────────────────────────────────────────────────────────

func runCIWaitHarbor(harborURL string) int {
	fmt.Println("Waiting for harbor-admin-password Secret in harbor namespace...")
	if !waitPoll(harborWaitBudget, 10*time.Second, func() bool {
		return kExists("-n", "harbor", "get", "secret", "harbor-admin-password")
	}) {
		fmt.Fprintln(os.Stderr, "::error::Timed out waiting for harbor/harbor-admin-password; Harbor did not create the admin Secret needed for credential seeding.")
		return 1
	}
	fmt.Println("harbor-admin-password Secret is present.")

	for _, d := range health.HarborDeployments() {
		if harborRollout("deployment/"+d) != nil {
			return 1
		}
	}
	for _, s := range health.HarborStatefulSets() {
		if harborRollout("statefulset/"+s) != nil {
			return 1
		}
	}

	if harborURL == "" {
		fmt.Println("HARBOR_URL not set; Harbor Kubernetes workloads are Ready, API readiness check skipped.")
		return 0
	}
	fmt.Printf("Waiting for Harbor API at %s...\n", harborURL)
	if waitPoll(harborWaitBudget, 10*time.Second, func() bool { return harborPingOK(harborURL) }) {
		fmt.Println("Harbor API is reachable.")
		return 0
	}
	fmt.Fprintf(os.Stderr, "::error::Timed out waiting for Harbor API at %s; admin and robot credential seeding would fail.\n", harborURL)
	return 1
}

// harborWaitBudget is the wall-clock deadline for each Harbor readiness poll
// (the former harborPoll's 60 attempts × 10s). waitPoll (ci_wait.go) bounds the
// total wait, so a slow probe can't stretch it the way the old attempt-count
// loop could.
const harborWaitBudget = 600 * time.Second

// harborRollout polls `<ref>` to a Ready rollout using the kstatus readiness
// primitive (health.ResourceStatus) instead of `kubectl rollout status`: it fetches
// the object as JSON and lets kstatus decide Current/InProgress/Failed from the
// controller's status, so the rollout verdict comes from the same library the
// kubectl ecosystem uses rather than a separate shell-out. Returns nil once the
// workload is Current, and an error on a Failed rollout or a 2-minute timeout
// (matching the former `--timeout=2m`). A package var so tests can stub the wait.
var harborRollout = func(ref string) error {
	const (
		rolloutTimeout = 2 * time.Minute
		pollInterval   = 5 * time.Second
	)
	deadline := time.Now().Add(rolloutTimeout)
	for {
		raw, err := execOutput("kubectl", "-n", "harbor", "get", ref, "-o", "json")
		if err == nil {
			switch cat, label := health.ResourceStatus(raw); cat {
			case health.CatOK:
				fmt.Printf("OK: %s rolled out\n", label)
				return nil
			case health.CatFail:
				fmt.Fprintf(os.Stderr, "::error::%s\n", label)
				return fmt.Errorf("rollout failed: %s", label)
			default:
				fmt.Printf("  waiting: %s\n", label)
			}
		} else {
			// get failed — object not created yet or a transient apiserver blip;
			// keep polling against the deadline rather than failing immediately.
			fmt.Printf("  waiting: harbor/%s not yet queryable\n", ref)
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "::error::timed out after %s waiting for %s to roll out\n", rolloutTimeout, ref)
			return fmt.Errorf("timed out waiting for %s", ref)
		}
		time.Sleep(pollInterval)
	}
}

// harborPingOK reports whether GET <url>/api/v2.0/ping returns 2xx (curl -ksSf:
// insecure TLS, success only on a 2xx).
func harborPingOK(url string) bool {
	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec
	}
	resp, err := client.Get(strings.TrimRight(url, "/") + "/api/v2.0/ping")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
