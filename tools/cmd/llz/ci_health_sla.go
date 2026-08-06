package main

// ci_health_sla.go — the cobra surface for the `health-sla` extension
// (internal/healthsla). The checks themselves, their SLA ladders and their
// summary rendering live in the package; what stays here is flag parsing and the
// Deps wiring, which is the only part that has to know how THIS binary reaches a
// cluster.

import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/healthsla"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/kubectlprobe"
	"github.com/spf13/cobra"
)

// healthSLADeps hands the extension the capabilities it declares. Every field is
// a real implementation: a nil func here panics rather than failing, and a
// fixture that no-ops would make the package's own summary assertions vacuous.
func healthSLADeps() healthsla.Deps {
	return healthsla.Deps{
		Summary: appendGHAFile,
		BaoExec: func(pod, addr, token string, args ...string) (string, string, error) {
			return baoExecFn(pod, addr, token, args...)
		},
		Exec:        execOutput,
		Reachable:   kubectlprobe.Reachable,
		BaoExecArgv: baoExecArgv,
		RootPod:     rootOpenbaoPod,
	}
}

func ciHealthLKEAdminRotationCmd() *cobra.Command {
	var warnDays, criticalDays int
	c := &cobra.Command{
		Use:   "health-lke-admin-rotation",
		Short: "fail when the newest lke-admin-token Secret breaches the rotation SLA",
		Long: "Native port of the lke-admin-rotation-health scheduled job. Reads the newest\n" +
			"lke-admin-token Secret's age in kube-system and fails the job past --critical-days\n" +
			"(the hard SLA), warning past --warn-days. Skips cleanly when the cluster API is\n" +
			"unreachable (a torn-down cluster, or a stale kubeconfig in TF state).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return healthsla.RunLKEAdminRotation(healthSLADeps(), warnDays, criticalDays)
		},
	}
	c.Flags().IntVar(&warnDays, "warn-days", 35, "warn when the newest token is older than this many days")
	c.Flags().IntVar(&criticalDays, "critical-days", 90, "fail when the newest token is older than this many days (hard SLA)")
	return c
}

func ciHealthLokiObjkeyRotationCmd() *cobra.Command {
	var warnDays, criticalDays int
	c := &cobra.Command{
		Use:   "health-loki-objkey-rotation",
		Short: "fail when the Loki object-store key in OpenBao breaches the rotation SLA",
		Long: "Native port of the loki-objkey-rotation-health scheduled job. Reads the age of\n" +
			"the secret/loki/object-store version in OpenBao (via kubectl exec bao) and fails\n" +
			"the job past --critical-days (the 120-day Guidelines SLA), warning past\n" +
			"--warn-days. Reads OPENBAO_ROOT_TOKEN; a missing secret/token is a non-fatal warn.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return healthsla.RunLokiObjkeyRotation(healthSLADeps(), warnDays, criticalDays)
		},
	}
	c.Flags().IntVar(&warnDays, "warn-days", 105, "warn when the key is older than this many days")
	c.Flags().IntVar(&criticalDays, "critical-days", 120, "fail when the key is older than this many days (hard SLA)")
	return c
}

func ciHealthOpenbaoCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "health-openbao",
		Short: "report OpenBao seal state + ESO readiness to the step summary (warn-only)",
		Long: "Native port of the openbao-health scheduled job. Probes each of the 3 OpenBao\n" +
			"Raft pods' seal state (an unreachable pod counts as sealed) and the ESO\n" +
			"ClusterSecretStore + every ExternalSecret's Ready condition, emitting warnings\n" +
			"and a step summary. Warning-only — never fails the job.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return healthsla.RunOpenbao(healthSLADeps()) },
	}
}

func ciHealthCertManagerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "health-certmanager",
		Short: "report every Certificate's Ready condition to the step summary (warn-only)",
		Long: "Native port of the certmanager-health scheduled job. Reports every\n" +
			"cert-manager Certificate's Ready condition, emitting warnings and a step\n" +
			"summary. Warning-only — never fails the job.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return healthsla.RunCertManager(healthSLADeps()) },
	}
}
