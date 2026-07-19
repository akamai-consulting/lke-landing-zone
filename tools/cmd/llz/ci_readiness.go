package main

// ci_readiness.go implements `llz ci assert-loki` and `llz ci wait-harbor` — the
// native ports of assert-loki-bootstrapped.sh and wait-for-harbor.sh. The Loki
// classification (pod readiness, S3-config detection) is the tested internal/
// health predicates; this file is the kubectl orchestration + Harbor poll loops.

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/health"
	"github.com/spf13/cobra"
)

func ciAssertLokiCmd() *cobra.Command {
	var nameMatch string
	var settle, interval int
	c := &cobra.Command{
		Use:   "assert-loki",
		Short: "fail unless Loki is bootstrapped (workloads Ready + S3-backed) on the current cluster",
		Long: "Native port of assert-loki-bootstrapped.sh. Asserts Loki's workloads are Ready\n" +
			"AND its config references S3 object storage (the kyverno loki-s3-object-store\n" +
			"policy mutates object_store filesystem→s3 — \"s3-backed\" is the real signal log\n" +
			"persistence works). Best-effort reports the Loki Argo Application status\n" +
			"(non-gating). Polls for a short settle budget so a transient kubectl/apiserver\n" +
			"blip (or a brief readiness / kyverno-mutation lag) doesn't flake the gate — the\n" +
			"same treatment assert-scrape-targets/assert-reconciler already carry. Exit 0\n" +
			"bootstrapped, 1 otherwise.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runCIAssertLoki(nameMatch, time.Duration(settle)*time.Second, time.Duration(interval)*time.Second)
		},
	}
	c.Flags().StringVar(&nameMatch, "name-match", "loki", "substring/regex identifying Loki workloads/objects")
	c.Flags().IntVar(&settle, "settle", 120, "seconds to keep polling for Loki to bootstrap before failing (rides out a transient kubectl blip / readiness lag)")
	c.Flags().IntVar(&interval, "interval", 10, "seconds between poll attempts")
	return c
}

func ciWaitHarborCmd() *cobra.Command {
	var harborURL string
	var registryOnly bool
	c := &cobra.Command{
		Use:   "wait-harbor",
		Short: "wait for the harbor-registry rollout (the post-S3-seed gate)",
		Long: "Waits for harbor-registry to roll out. It mounts the harbor-registry-s3\n" +
			"Secret via secretKeyRef, so it stays in CreateContainerConfigError until that\n" +
			"Secret exists — seeded mid-bootstrap, then synced when the es-store-recovery\n" +
			"lane sees the store go Ready. Exit 0 rolled out, 1 on timeout.\n\n" +
			"This verb used to carry a second, PRE-seed half (admin Secret + control-plane\n" +
			"Deployments/StatefulSets + an API ping). That half gated the workflow's\n" +
			"`harbor` job, whose robot provisioning moved in-cluster in f0aa68f; the job\n" +
			"went with it and took the gate's only caller, leaving the code unreachable.\n" +
			"kick-harbor-provisioner now does its own harbor-core Available wait.\n\n" +
			"--registry-only and --harbor-url are accepted and IGNORED. Instance repos\n" +
			"vendor their workflows, so a rendered-but-not-yet-upgraded instance can still\n" +
			"pass --registry-only; rejecting it would break those instances on image bump\n" +
			"alone. They go once `llz upgrade` has carried the new call site everywhere.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runCIWaitHarbor(harborURL, registryOnly)
		},
	}
	c.Flags().StringVar(&harborURL, "harbor-url", os.Getenv("HARBOR_URL"), "accepted and ignored (vendored-workflow compatibility)")
	c.Flags().BoolVar(&registryOnly, "registry-only", false, "accepted and ignored — the registry rollout is now the only behavior (vendored-workflow compatibility)")
	_ = c.Flags().MarkDeprecated("harbor-url", "it is ignored; the API ping was retired with the pre-seed gate")
	_ = c.Flags().MarkDeprecated("registry-only", "it is ignored; the registry rollout is now the only behavior")
	return c
}

// ── assert-loki ──────────────────────────────────────────────────────────────

type lokiPod struct {
	ns, name string
	status   health.PodStatus
}

// runCIAssertLoki returns nil when Loki is bootstrapped and an error otherwise
// (cobra exits 1 on it). The ::error:: annotation is still written directly —
// GitHub parses an annotation only at the start of a line, and a returned error
// reaches stderr behind main.go's "llz: " prefix.
func runCIAssertLoki(nameMatch string, settle, interval time.Duration) error {
	fmt.Println("## Loki bootstrap assertion")

	// Poll the two gating conditions for a settle budget. lokiBootstrapped reads the
	// cluster via kItems, which collapses a kubectl/apiserver error to "no items" —
	// indistinguishable from a genuine absence. A single one-shot read therefore
	// turned a transient blip (an apiserver 5xx/429, an LKE-E HA control-plane replica
	// dropping seconds after wait-cluster-ready — a documented transient) into
	// "no Loki pods → exit 1". Re-evaluating until it passes (or the budget elapses)
	// rides out that blip and any brief readiness / kyverno-mutation lag, the same as
	// assert-scrape-targets. settle<=0 → a single evaluation (used by tests).
	var ok bool
	var msgs []string
	deadline := time.Now().Add(settle)
	for attempt := 1; ; attempt++ {
		ok, msgs = lokiBootstrapped(nameMatch)
		if ok || !time.Now().Before(deadline) {
			break
		}
		fmt.Printf("attempt %d: Loki not bootstrapped yet — retrying in %s\n", attempt, interval)
		time.Sleep(interval)
	}
	for _, m := range msgs {
		fmt.Println(m)
	}

	// Best-effort Argo CD Application status (non-gating).
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

	if !ok {
		fmt.Fprintln(os.Stderr, "::error::Loki is not bootstrapped")
		return fmt.Errorf("Loki is not bootstrapped")
	}
	fmt.Println("Loki is bootstrapped.")
	return nil
}

// lokiBootstrapped evaluates the two gating conditions — Loki workloads exist and
// are Ready, and the config is S3-backed — returning whether both hold plus the
// OK/FAIL lines to print. It is a pure read (no side effects), so the caller can
// re-run it across a settle budget: a transient kubectl failure surfaces here as
// "not bootstrapped" (kItems → empty) and is ridden out by the poll rather than
// hard-failing the gate on one blip.
func lokiBootstrapped(nameMatch string) (bool, []string) {
	ok := true
	var msgs []string

	// 1. Loki workloads exist and are Ready.
	pods := lokiPods(nameMatch)
	if len(pods) == 0 {
		msgs = append(msgs, fmt.Sprintf("FAIL: no Loki pods found (matched name~=%q)", nameMatch))
		ok = false
	} else {
		var notReady []string
		for _, p := range pods {
			if !health.LokiPodReady(p.status) {
				notReady = append(notReady, fmt.Sprintf("%s/%s phase=%s", p.ns, p.name, p.status.Phase))
			}
		}
		if len(notReady) > 0 {
			msgs = append(msgs, "FAIL: Loki pods not Ready:")
			for _, n := range notReady {
				msgs = append(msgs, "  "+n)
			}
			ok = false
		} else {
			msgs = append(msgs, fmt.Sprintf("OK: %d Loki pod(s) Ready", len(pods)))
		}
	}

	// 2. Loki is configured for S3 object storage (not the filesystem default).
	if health.LokiConfigUsesS3(lokiConfigText(nameMatch)) {
		msgs = append(msgs, "OK: Loki config references S3 object storage")
	} else {
		msgs = append(msgs, "FAIL: Loki config does not reference S3 — still on the filesystem default? (kyverno loki-s3-object-store may not have applied)")
		ok = false
	}
	return ok, msgs
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

// runCIWaitHarbor waits for the harbor-registry rollout — the post-S3-seed gate.
// harbor-registry mounts the harbor-registry-s3 Secret via secretKeyRef, so it
// stays in CreateContainerConfigError until that Secret exists (seeded
// mid-bootstrap, then synced by the es-store-recovery lane when the store goes
// Ready).
//
// The harborURL / registryOnly parameters are vestigial and ignored; see the
// command's help for why they are still accepted.
//
// It polls a REAL budget and self-reports non-fatally rather than running a
// single 2m `kubectl rollout status`. harbor-registry only becomes schedulable
// once its ExternalSecret syncs — a few minutes after the KV seed on a fresh
// bootstrap — so that single 2m wait predictably timed out, and the caller's
// `continue-on-error` then painted a green check over a scary
// "error: timed out … exit code 1" that carried no signal (it "passed" whether
// or not the registry came up). Now a healthy bootstrap goes green honestly, and
// a genuine stall is a visible ::warning:: — the convergence gate is the hard
// check — never a masked error. That is what lets the caller drop
// continue-on-error.
func runCIWaitHarbor(_ string, _ bool) error {
	allRolled := true
	for _, d := range health.HarborRegistryDeployments() {
		if waitPoll(harborWaitBudget, 10*time.Second, func() bool { return deploymentRolledOut("harbor", d) }) {
			fmt.Printf("harbor deployment %q rolled out.\n", d)
			continue
		}
		allRolled = false
		fmt.Fprintf(os.Stderr, "::warning::harbor deployment %q not rolled out within %s — its harbor-registry-s3 ExternalSecret may still be syncing; the convergence gate will catch a genuine stall.\n", d, harborWaitBudget)
	}
	if allRolled {
		fmt.Println("harbor-registry rolled out.")
	}
	return nil
}

// harborWaitBudget is the wall-clock deadline for each Harbor readiness poll
// (the former harborPoll's 60 attempts × 10s). waitPoll (ci_wait.go) bounds the
// total wait, so a slow probe can't stretch it the way the old attempt-count
// loop could. A var (not const) so tests can shrink it to avoid a real 10m poll.
var harborWaitBudget = 600 * time.Second

// deploymentRolledOut reports whether the deployment has all its desired replicas
// available. A quiet kubectl read (jsonpath), unlike `rollout status`, which prints
// a noisy "error: timed out" on its deadline — so the registry gate can poll a real
// budget without littering the log with per-attempt errors.
func deploymentRolledOut(namespace, name string) bool {
	out, err := kubectlOut("-n", namespace, "get", "deployment", name,
		"-o", "jsonpath={.status.availableReplicas}/{.spec.replicas}")
	if err != nil {
		return false
	}
	return replicasRolledOut(out)
}

// replicasRolledOut parses "<available>/<desired>" and reports whether every
// desired replica is available (desired>0, available>=desired). Pure — unit-tested.
func replicasRolledOut(availSlashDesired string) bool {
	parts := strings.SplitN(strings.TrimSpace(availSlashDesired), "/", 2)
	if len(parts) != 2 {
		return false
	}
	avail, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	desired, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil {
		return false
	}
	return desired > 0 && avail >= desired
}
