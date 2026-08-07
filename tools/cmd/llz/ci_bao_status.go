package main

import (
	"fmt"
	"os"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/baoread"
	"github.com/spf13/cobra"
)

// ci_bao_status.go — the `llz ci bao-status` flag set, and the GHA step-output
// helper 25 callers share.
//
// These came back OUT of internal/baoread during the exec-layer extraction. The
// exec layer is what everything reaches for; a cobra command and a
// GITHUB_OUTPUT append are not part of it. appendGHAFile in particular has 25
// callers across the command tree and only one of them was ever an OpenBao one —
// moving it into an OpenBao package would have made every other caller import
// OpenBao to write a step output.

func ciBaoStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "bao-status",
		Short: "probe all OpenBao pods and emit initialized/sealed step outputs",
		Long: "Native port of the \"Check OpenBao status\" step that bootstrap-openbao.yml\n" +
			"and openbao-auto-unseal.yml previously duplicated verbatim. Probes `bao\n" +
			"status` on all 3 pods (not just pod-0: a partial seal must read as sealed,\n" +
			"or the emergency-reunseal branch never fires and raft sits below quorum)\n" +
			"and writes initialized=<any pod true> and sealed=<any pod sealed> to\n" +
			"$GITHUB_OUTPUT. An unreachable pod counts as uninitialized+sealed.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIBaoStatus() },
	}
}

func runCIBaoStatus() error {
	states := make([]baoread.PodStatus, 0, len(baoread.PodNames))
	for _, pod := range baoread.PodNames {
		// The exec error is deliberately ignored: sealed pods exit 2 with good
		// JSON, and a dead pod yields unparseable output → the sealed default.
		out, _, _ := baoread.ExecFn(pod, "", "", "status", "-format=json")
		st, _ := baoread.ParsePodStatus(out)
		fmt.Printf("  %s: initialized=%t sealed=%t\n", pod, st.Initialized, st.Sealed)
		states = append(states, st)
	}
	initialized, sealedAny := baoread.AggregateStatus(states)
	fmt.Printf("initialized=%t\nsealed=%t\n", initialized, sealedAny)
	return appendGHAFile("GITHUB_OUTPUT",
		fmt.Sprintf("initialized=%t", initialized),
		fmt.Sprintf("sealed=%t", sealedAny))
}

// appendGHAFile appends lines to the GitHub Actions command file named by
// envVar (GITHUB_OUTPUT / GITHUB_ENV / GITHUB_STEP_SUMMARY). Outside Actions
// the variable is unset and the write is skipped, keeping the commands
// runnable from a workstation.
// appendGHAFile appends lines to the GitHub Actions command file named by
// envVar (GITHUB_OUTPUT / GITHUB_ENV / GITHUB_STEP_SUMMARY). Outside Actions
// the variable is unset and the write is skipped, keeping the commands
// runnable from a workstation.
func appendGHAFile(envVar string, lines ...string) error {
	path := os.Getenv(envVar)
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open $%s: %w", envVar, err)
	}
	for _, l := range lines {
		if _, err := fmt.Fprintln(f, l); err != nil {
			f.Close()
			return fmt.Errorf("write $%s: %w", envVar, err)
		}
	}
	return f.Close()
}

// ── recovery keys ─────────────────────────────────────────────────────────────

// baoread.RecoveryKeysFromEnv reads the 3 quorum recovery keys from RECOVERY_K1/2/3.
// Under the chart's `seal "static"` auto-unseal, `operator init` yields recovery
// shares (not unseal keys): the seal mechanism unseals every pod at boot, so
// there is no submit-keys-to-unseal step. The recovery keys exist only to
// authorize the `operator generate-root` quorum that bao-regen-root runs.
