package main

// ci_readiness.go implements `llz ci assert-loki` — the native port of
// assert-loki-bootstrapped.sh. The Loki classification (pod readiness,
// S3-config detection) is the tested internal/health predicates; this file is
// the kubectl orchestration around them.
//
// `llz ci wait-harbor` also lived here. Its last half waited on the
// harbor-registry rollout post-S3-seed — continue-on-error, so it never gated,
// and always paid in series ahead of a converge poll that already adjudicates
// the Harbor Argo apps concurrently with everything else. Retired with its call
// site; see llz-bootstrap-openbao.yml where the step used to run.

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
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
		RunE: func(_ *cobra.Command, _ []string) error {
			os.Exit(runCIAssertLoki(nameMatch, time.Duration(settle)*time.Second, time.Duration(interval)*time.Second))
			return nil
		},
	}
	c.Flags().StringVar(&nameMatch, "name-match", "loki", "substring/regex identifying Loki workloads/objects")
	c.Flags().IntVar(&settle, "settle", 120, "seconds to keep polling for Loki to bootstrap before failing (rides out a transient kubectl blip / readiness lag)")
	c.Flags().IntVar(&interval, "interval", 10, "seconds between poll attempts")
	return c
}

// ── assert-loki ──────────────────────────────────────────────────────────────

type lokiPod struct {
	ns, name string
	status   health.PodStatus
}

func runCIAssertLoki(nameMatch string, settle, interval time.Duration) int {
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
		return 1
	}
	fmt.Println("Loki is bootstrapped.")
	return 0
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
