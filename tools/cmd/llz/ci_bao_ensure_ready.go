package main

// ci_bao_ensure_ready.go implements `llz ci bao-ensure-ready` — the single
// command that collapses bootstrap-openbao.yml's seal/token-lifecycle steps
// (status probe → first-init OR wait-for-auto-unseal → root-token load/regen →
// availability gate) into one place. It COMPOSES the existing, individually-
// tested bao-* run functions rather than reimplementing them, so the init
// payload is still produced exactly once by runCIBaoInit and the quorum regen
// path is still runCIBaoRegenRoot's. The workflow shrinks from ~8 conditional
// steps to one and the branch selection becomes unit-testable Go.
//
// CONVERGENCE CONTRACT — same detect → choose-a-path → re-verify shape the
// cluster-health gate uses, applied to OpenBao seal state. Under the chart's
// `seal "static"` auto-unseal the pods unseal themselves at boot, so there is no
// key-submission step — the bootstrap just waits for that convergence:
//
//   uninitialized        → init (generate recovery keys + root, persist), then
//                          wait for every pod to auto-unseal (leader, then the
//                          retry_join'd followers)
//   initialized + sealed → wait for the pod(s) to self-unseal after a restart
//                          (transient; no keys are submitted)
//   initialized          → validate the loaded root token, regenerate via quorum
//                          if a prior run's revoke left a dead value behind
//
// Emits available=<bool> to $GITHUB_OUTPUT (the gate every downstream
// configure/seed step keys on) and re-exports the effective OPENBAO_ROOT_TOKEN
// to $GITHUB_ENV for those steps. The keys/token handoff between the composed
// steps rides the PROCESS env — runCIBaoInit / runCIBaoRegenRoot os.Setenv what
// they also append to $GITHUB_ENV — so the in-process regen-after-init and the
// availability check below see the values the inline steps used to receive via
// GitHub Actions' between-step $GITHUB_ENV injection.

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
)

func ciBaoEnsureReadyCmd() *cobra.Command {
	var region string
	var leaderTimeout, joinTimeout int
	c := &cobra.Command{
		Use:   "bao-ensure-ready",
		Short: "probe OpenBao and drive it to unsealed + a usable root token (init/wait/regen)",
		Long: "Orchestrates the OpenBao seal/token lifecycle bootstrap-openbao.yml used to\n" +
			"run as eight separate steps: probe all pods, then — on an uninitialized\n" +
			"cluster — run `bao operator init` and wait for every pod to auto-unseal\n" +
			"from the static seal key (leader first, then the retry_join'd followers);\n" +
			"on an initialized-but-sealed cluster wait for the pod(s) to self-unseal\n" +
			"after a restart; and on an initialized cluster validate the loaded root\n" +
			"token and regenerate it via quorum if a prior run revoked it. Composes the\n" +
			"same bao-init / bao-regen-root logic the individual commands run. Writes\n" +
			"available=<bool> to $GITHUB_OUTPUT (the gate downstream configure/seed steps\n" +
			"check) and re-exports the effective OPENBAO_ROOT_TOKEN to $GITHUB_ENV. Reads\n" +
			"RECOVERY_K1/2/3 + OPENBAO_ROOT_TOKEN (infra-<region> secrets) and\n" +
			"GH_TOKEN/GH_REPO (first-init persistence).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runCIBaoEnsureReady(gopts, region,
				time.Duration(leaderTimeout)*time.Second, time.Duration(joinTimeout)*time.Second)
		},
	}
	c.Flags().StringVar(&region, "region", "", "region whose infra-<region> GHA environment holds the keys/token (required)")
	c.Flags().IntVar(&leaderTimeout, "leader-timeout", 180, "seconds to wait for pod-0 to report unsealed (first-init)")
	c.Flags().IntVar(&joinTimeout, "join-timeout", 300, "seconds to wait for each follower to reach initialized=true (first-init)")
	return c
}

func runCIBaoEnsureReady(g globalOpts, region string, leaderTimeout, joinTimeout time.Duration) error {
	if region == "" {
		return fmt.Errorf("--region is required")
	}
	if g.dryRun {
		fmt.Fprintln(os.Stderr, "→ (dry-run) would probe OpenBao and init/wait-for-auto-unseal/regen-root as needed")
		return nil
	}

	// 1. Probe all pods — the same aggregation `bao-status` emits (a partial seal
	//    reads as sealed so the re-unseal branch fires).
	states := make([]baoPodStatus, 0, len(openbaoPodNames))
	for _, pod := range openbaoPodNames {
		out, _, _ := baoExecFn(pod, "", "", "status", "-format=json")
		st, _ := parseBaoPodStatus(out)
		fmt.Printf("  %s: initialized=%t sealed=%t\n", pod, st.Initialized, st.Sealed)
		states = append(states, st)
	}
	initialized, sealedAny := aggregateBaoStatus(states)
	fmt.Printf("cluster: initialized=%t sealed=%t\n", initialized, sealedAny)

	switch {
	case !initialized:
		// Branch A — first-time bootstrap. bao-init needs the secrets-write PAT to
		// persist the keys; fail early and friendly if it's absent (the inline
		// "verify GitHub secrets-write token for first init" preflight).
		if os.Getenv("GH_TOKEN") == "" {
			return fmt.Errorf("OpenBao is uninitialized and GH_TOKEN (OPENBAO_SECRETS_WRITE_TOKEN) is not set — first-time init must persist the recovery keys + root token as infra-%s secrets", region)
		}
		// Generates the 5 recovery shares + root token, persists keys 1-3 + the
		// root token, and exports RECOVERY_K1/2/3 + OPENBAO_ROOT_TOKEN to
		// $GITHUB_ENV AND the process env (for the in-process regen path + the
		// availability gate). Under static seal each pod then unseals itself at
		// boot once retry_join has joined it — wait for that convergence.
		if err := runCIBaoInit(g, region); err != nil {
			return err
		}
		if err := waitForAutoUnseal(leaderTimeout, joinTimeout); err != nil {
			return err
		}
	case sealedAny:
		// Branch B — initialized but (partially) sealed after a restart. Under
		// static seal the pod self-unseals once it's back up; wait for that rather
		// than submitting keys (there are none).
		if err := waitForAutoUnseal(leaderTimeout, joinTimeout); err != nil {
			return err
		}
	}

	// 2. Root token. A fresh init just minted one (now in the process env), so
	//    there's nothing to validate on that path. For an already-initialized
	//    cluster the loaded OPENBAO_ROOT_TOKEN may be the value a prior run
	//    revoked — bao-regen-root validates it and regenerates via quorum if so,
	//    re-exporting the fresh token to $GITHUB_ENV and the process env.
	if initialized && os.Getenv("OPENBAO_ROOT_TOKEN") != "" {
		if err := runCIBaoRegenRoot(g, region); err != nil {
			return err
		}
	}

	// 3. Availability gate + re-export. runCIBaoInit / runCIBaoRegenRoot already
	//    wrote a minted/regenerated token to $GITHUB_ENV; this also covers the
	//    third case — a loaded token that validated WITHOUT regeneration — so
	//    downstream steps (separate processes) always find OPENBAO_ROOT_TOKEN.
	token := os.Getenv("OPENBAO_ROOT_TOKEN")
	if token == "" {
		fmt.Println("Root token unavailable — configure and seed steps will be skipped.")
		if err := appendGHAFile("GITHUB_STEP_SUMMARY",
			"Root token unavailable — configure and seed steps were skipped.",
			fmt.Sprintf("To re-configure: set OPENBAO_ROOT_TOKEN as an infra-%s environment secret and re-run,", region),
			"or ensure OPENBAO_RECOVERY_KEY_{1,2,3} are set so the workflow can regenerate it."); err != nil {
			return err
		}
		return appendGHAFile("GITHUB_OUTPUT", "available=false")
	}
	maskGHA(token)
	if err := appendGHAFile("GITHUB_ENV", "OPENBAO_ROOT_TOKEN="+token); err != nil {
		return err
	}
	return appendGHAFile("GITHUB_OUTPUT", "available=true")
}
