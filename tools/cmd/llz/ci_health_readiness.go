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

const esoNamespace = "llz-external-secrets"

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

	sealed := 0
	for _, pod := range []string{"platform-openbao-0", "platform-openbao-1", "platform-openbao-2"} {
		st := baoStatusOrSealed(pod)
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
		fmt.Fprintf(os.Stderr, "::warning::%d/3 OpenBao pod(s) sealed on %s — openbao-auto-unseal.yml runs every 10m and should pick this up. Manual run: gh workflow run openbao-auto-unseal.yml --field region=%s\n", sealed, reg, reg)
		summary = append(summary, fmt.Sprintf("> **Sealed pods detected.** `openbao-auto-unseal.yml` runs every 10m and should auto-remediate — no manual action expected unless this warning persists across several check cycles. Manual re-dispatch: `openbao-auto-unseal.yml` → `%s`. If still sealed after several auto-unseal cycles, verify `OPENBAO_UNSEAL_KEY_{1,2,3}` on `infra-%s` and inspect `bao status` on the affected pod.", reg, reg))
	} else {
		fmt.Printf("All OpenBao pods unsealed on %s.\n", reg)
		summary = append(summary, "> All pods unsealed.")
	}

	// ── ESO ClusterSecretStore + ExternalSecrets ──
	summary = append(summary, "", fmt.Sprintf("### ESO ClusterSecretStore — %s", reg), "")
	css := kJSONPath("-n", esoNamespace, "get", "clustersecretstores.external-secrets.io", "openbao", "-o", `jsonpath={.status.conditions[?(@.type=="Ready")].status}`)
	if css == "True" {
		fmt.Printf("ClusterSecretStore openbao (%s): Ready\n", reg)
		summary = append(summary, "- ClusterSecretStore `openbao`: Ready")
	} else {
		if css == "" {
			css = "NotFound"
		}
		fmt.Fprintf(os.Stderr, "::warning::ClusterSecretStore openbao (%s) not Ready (status: %s)\n", reg, css)
		summary = append(summary, fmt.Sprintf("- **ClusterSecretStore `openbao`: %s** — check ESO logs and OpenBao connectivity", css))
	}

	var unhealthy []string
	for _, raw := range kItems("get", "externalsecrets.external-secrets.io", "-A") {
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
	if len(unhealthy) > 0 {
		summary = append(summary, "", "**Unhealthy ExternalSecrets:**", "```")
		summary = append(summary, unhealthy...)
		summary = append(summary, "```")
	} else {
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
func baoStatusOrSealed(pod string) health.BaoStatus {
	sealedDefault := health.BaoStatus{Initialized: false, Sealed: true, HAMode: "standalone"}
	out, err := execOutput("kubectl", "-n", openbaoNamespace, "exec", pod, "--",
		"env", "VAULT_ADDR=https://127.0.0.1:8200", "VAULT_SKIP_VERIFY=true", "bao", "status", "-format=json")
	if err != nil {
		return sealedDefault
	}
	st, perr := health.ParseBaoStatus(out)
	if perr != nil {
		return sealedDefault
	}
	return st
}

// readyCell renders a Ready-condition status for a summary cell, showing
// "Unknown" when the resource has no Ready condition yet (empty status).
func readyCell(status string) string {
	if status == "" {
		return "Unknown"
	}
	return status
}
