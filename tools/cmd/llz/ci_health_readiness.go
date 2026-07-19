package main

// ci_health_readiness.go implements the warning-only cluster readiness
// scheduled checks — `llz ci health-openbao` (Raft seal state across the 3 pods
// + ESO ClusterSecretStore/ExternalSecrets) and `llz ci health-certmanager`
// (every Certificate's Ready condition) — the native ports of the openbao-health
// and certmanager-health jobs. Both reuse the unit-tested health predicates
// (ParseBaoStatus/ClassifyBaoSeal, FindReady) and only emit warnings + a step
// summary; neither fails the job (the jobs are continue-on-error).

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/health"
	"github.com/spf13/cobra"
)

// apl-core 6.x ships ESO as a core app in the `external-secrets` namespace; the
// landing zone no longer runs its own controller. (The `openbao`
// ClusterSecretStore probed below is cluster-scoped, so the -n is cosmetic, but
// keep it pointed at the live ESO namespace.)
const esoNamespace = "external-secrets"

func ciHealthOpenbaoCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "health-openbao",
		Short: "report OpenBao seal state + ESO readiness to the step summary (warn-only)",
		Long: "Native port of the openbao-health scheduled job. Probes each of the 3 OpenBao\n" +
			"Raft pods' seal state (an unreachable pod counts as sealed) and the ESO\n" +
			"ClusterSecretStore + every ExternalSecret's Ready condition, emitting warnings\n" +
			"and a step summary. Warning-only — never fails the job.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runHealthOpenbao() },
	}
}

func runHealthOpenbao() error {
	reg := schedRegion()
	summary := []string{
		fmt.Sprintf("## OpenBao Health — %s — %s", reg, schedStamp()), "",
		"| Pod | Initialized | Sealed | HA Enabled | Leader |",
		"|-----|-------------|--------|------------|--------|",
	}

	sealed, unknown := 0, 0
	for _, pod := range []string{"platform-openbao-0", "platform-openbao-1", "platform-openbao-2"} {
		st, ok := baoStatus(pod)
		if !ok {
			// Distinct from sealed: the exec never answered (after the transient
			// retries), so this pod's seal state is simply not known.
			unknown++
			fmt.Fprintf(os.Stderr, "::warning::OpenBao pod %s (%s): could not read `bao status` — seal state UNKNOWN (exec failed; not a seal failure)\n", pod, reg)
			summary = append(summary, fmt.Sprintf("| %s | ? | ? | ? | ? |", pod))
			continue
		}
		if st.Sealed {
			sealed++
			fmt.Fprintf(os.Stderr, "::warning::OpenBao pod %s (%s) is SEALED\n", pod, reg)
		}
		haEnabled := st.HAMode != "standalone"
		leader := st.HAMode == "active"
		summary = append(summary, fmt.Sprintf("| %s | %t | %t | %t | %t |", pod, st.Initialized, st.Sealed, haEnabled, leader))
	}

	summary = append(summary, "")
	if sealed > 0 {
		fmt.Fprintf(os.Stderr, "::warning::%d/3 OpenBao pod(s) sealed on %s — pods auto-unseal from the static seal key at boot, so a persistently sealed pod means the openbao-unseal-key Secret is missing/unreadable, the static key is wrong, or Raft storage is unhealthy.\n", sealed, reg)
		summary = append(summary, fmt.Sprintf("> **Sealed pods detected on %s.** Pods auto-unseal from the static seal key (`seal \"static\"`) at boot — a persistently sealed pod means the `openbao-unseal-key` Secret is missing/unreadable, the 32-byte static key is wrong, or Raft storage is unhealthy. Inspect `bao status` and the openbao pod logs.", reg))
	} else if unknown == 0 {
		fmt.Printf("All OpenBao pods unsealed on %s.\n", reg)
		summary = append(summary, "> All pods unsealed.")
	}
	if unknown > 0 {
		// Say what is actually wrong. Reporting these as sealed sent operators to
		// the unseal key, the static key and Raft storage — none of which is the
		// problem when the exec channel itself is down.
		fmt.Fprintf(os.Stderr, "::warning::%d/3 OpenBao pod(s) on %s could not be read — seal state unknown. This is an EXEC/connectivity failure (konnectivity tunnel, apiserver→kubelet dial), not evidence about the seal.\n", unknown, reg)
		summary = append(summary, fmt.Sprintf("> **%d/3 pod(s) unreadable on %s** — `kubectl exec` did not answer after the transient retries, so seal state is unknown. Check konnectivity/agent health before suspecting the seal key.", unknown, reg))
	}

	// ── ESO ClusterSecretStore + ExternalSecrets ──
	summary = append(summary, "", fmt.Sprintf("### ESO ClusterSecretStore — %s", reg), "")
	css, cssAnswered := kJSONPathOK("-n", esoNamespace, "get", "clustersecretstores.external-secrets.io", "openbao", "-o", `jsonpath={.status.conditions[?(@.type=="Ready")].status}`)
	switch {
	case css == "True":
		fmt.Printf("ClusterSecretStore openbao (%s): Ready\n", reg)
		summary = append(summary, "- ClusterSecretStore `openbao`: Ready")
	case !cssAnswered:
		// Same distinction the exec probe above already draws: an unreadable store
		// is not a NotFound, and reporting it as one sends the operator to ESO logs
		// and OpenBao connectivity for what is an apiserver-read failure.
		fmt.Fprintf(os.Stderr, "::warning::ClusterSecretStore openbao (%s) could not be read — state unknown (apiserver read failed after retries), NOT evidence the store is missing\n", reg)
		summary = append(summary, "- **ClusterSecretStore `openbao`: UNKNOWN** — the read failed; check apiserver reachability before suspecting ESO")
	default:
		if css == "" {
			css = "NotFound"
		}
		fmt.Fprintf(os.Stderr, "::warning::ClusterSecretStore openbao (%s) not Ready (status: %s)\n", reg, css)
		summary = append(summary, fmt.Sprintf("- **ClusterSecretStore `openbao`: %s** — check ESO logs and OpenBao connectivity", css))
	}

	var unhealthy []string
	esRaw, esAnswered := kItemsOK("get", "externalsecrets.external-secrets.io", "-A")
	for _, raw := range esRaw {
		var it readyResourceItem
		if json.Unmarshal(raw, &it) != nil {
			continue
		}
		status, _, _ := health.FindReady(it.Status.Conditions)
		if status != "True" {
			line := fmt.Sprintf("%s/%s: %s", it.Metadata.Namespace, it.Metadata.Name, readyCell(status))
			fmt.Fprintf(os.Stderr, "::warning::Unhealthy ExternalSecret (%s): %s\n", reg, line)
			unhealthy = append(unhealthy, line)
		}
	}
	switch {
	case len(unhealthy) > 0:
		summary = append(summary, "", "**Unhealthy ExternalSecrets:**", "```")
		summary = append(summary, unhealthy...)
		summary = append(summary, "```")
	case !esAnswered:
		// An empty list from a failed read would otherwise print "All ExternalSecrets
		// Ready" — the report is identical whether every secret is healthy or none
		// were examined.
		fmt.Fprintf(os.Stderr, "::warning::Could not list ExternalSecrets (%s) — health unknown, not 'all Ready'\n", reg)
		summary = append(summary, "- **ExternalSecrets: UNKNOWN** — the list call failed; nothing was examined")
	default:
		fmt.Printf("All ExternalSecrets Ready on %s.\n", reg)
		summary = append(summary, "- All ExternalSecrets: Ready")
	}

	return appendGHAFile("GITHUB_STEP_SUMMARY", summary...)
}

func ciHealthCertManagerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "health-certmanager",
		Short: "report every cert-manager Certificate's Ready state to the step summary (warn-only)",
		Long: "Native port of the certmanager-health scheduled job. Checks every cert-manager\n" +
			"Certificate across all namespaces for Ready=True (a stuck ACME renewal leaves\n" +
			"one Ready=False indefinitely), emitting warnings + a step summary. Warning-only.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runHealthCertManager() },
	}
}

func runHealthCertManager() error {
	reg := schedRegion()
	summary := []string{
		fmt.Sprintf("## cert-manager Certificate Health — %s — %s", reg, schedStamp()), "",
		"| Namespace | Certificate | Ready | Message |",
		"|-----------|-------------|-------|---------|",
	}

	notReady := 0
	for _, raw := range kItems("get", "certificates.cert-manager.io", "-A") {
		var it readyResourceItem
		if json.Unmarshal(raw, &it) != nil {
			continue
		}
		status, _, msg := health.FindReady(it.Status.Conditions)
		ns, name := it.Metadata.Namespace, it.Metadata.Name
		if status != "True" {
			fmt.Fprintf(os.Stderr, "::warning::Certificate %s/%s (%s) not Ready: %s\n", ns, name, reg, msg)
			summary = append(summary, fmt.Sprintf("| %s | **%s** | **%s** | %s |", ns, name, readyCell(status), msg))
			notReady++
		} else {
			summary = append(summary, fmt.Sprintf("| %s | %s | %s | |", ns, name, status))
		}
	}

	summary = append(summary, "")
	if notReady > 0 {
		fmt.Fprintf(os.Stderr, "::warning::%d Certificate(s) not Ready on %s — check cert-manager logs and ACME challenge status\n", notReady, reg)
		summary = append(summary, fmt.Sprintf("> **Action required:** %d Certificate(s) not Ready. Run: kubectl describe certificate -A", notReady))
	} else {
		fmt.Printf("All cert-manager Certificates Ready on %s.\n", reg)
		summary = append(summary, "> All Certificates Ready.")
	}

	return appendGHAFile("GITHUB_STEP_SUMMARY", summary...)
}

// baoStatusOrSealed runs `bao status -format=json` in the pod and parses it,
// falling back to the fail-safe sealed/uninitialized default on any exec or
// parse error — the Go form of the job's `|| echo '{...sealed:true...}'`.
// baoStatus reports a pod's seal state, and whether it could be determined AT
// ALL. The second return is the point.
//
// This used to collapse every failure into {Sealed: true} and go through a bare
// execOutput, which meant two separate problems:
//
//  1. It bypassed baoExecFn, so it did not retry the exec failures this repo
//     documents as transient — konnectivity's "No agent available" (observed
//     burning 17 of 18 tries in one e2e), "error dialing backend", SPDY upgrade
//     failures. A tunnel blip is not a seal state.
//  2. It reported "could not ask" as "SEALED", which sends the operator to the
//     openbao-unseal-key Secret, the 32-byte static key, and Raft storage —
//     three places that are all fine. A false alarm pointing at the wrong
//     subsystem is worse than no alarm.
//
// Now the exec retries, and an undeterminable pod is reported as unknown rather
// than silently counted as sealed.
func baoStatus(pod string) (st health.BaoStatus, ok bool) {
	stdout, _, err := baoExecFn(pod, "", "",
		"env", "VAULT_ADDR=https://127.0.0.1:8200", "VAULT_SKIP_VERIFY=true", "bao", "status", "-format=json")
	if err != nil {
		return health.BaoStatus{}, false
	}
	parsed, perr := health.ParseBaoStatus([]byte(stdout))
	if perr != nil {
		return health.BaoStatus{}, false
	}
	return parsed, true
}

// readyCell renders a Ready-condition status for a summary cell, showing
// "Unknown" when the resource has no Ready condition yet (empty status).
func readyCell(status string) string {
	if status == "" {
		return "Unknown"
	}
	return status
}
