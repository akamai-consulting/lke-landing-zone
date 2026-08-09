package main

import (
	"fmt"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/baoread"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/ghaout"
	"github.com/spf13/cobra"
)

// ci_bao_status.go — the `llz ci bao-status` flag set, and the GHA step-output
// helper 25 callers share.
//
// These came back OUT of internal/baoread during the exec-layer extraction. The
// exec layer is what everything reaches for; a cobra command and a
// GITHUB_OUTPUT append are not part of it. ghaout.Append in particular has 25
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
	return ghaout.Append("GITHUB_OUTPUT",
		fmt.Sprintf("initialized=%t", initialized),
		fmt.Sprintf("sealed=%t", sealedAny))
}
