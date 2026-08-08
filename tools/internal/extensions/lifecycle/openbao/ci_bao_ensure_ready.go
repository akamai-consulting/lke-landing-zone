package openbao

// ci_bao_ensure_ready.go implements `llz ci bao-ensure-ready` — the single
// command that collapses bootstrap-openbao.yml's seal/token-lifecycle steps
// (status probe → first-init OR wait-for-auto-unseal → root-token load/regen →
// availability gate) into one place. It COMPOSES the existing, individually-
// tested bao-* run functions rather than reimplementing them, so the init
// payload is still produced exactly once by RunInit and the quorum regen
// path is still RunRegenRootCI's. The workflow shrinks from ~8 conditional
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
// steps rides the PROCESS env — RunInit / RunRegenRootCI os.Setenv what
// they also append to $GITHUB_ENV — so the in-process regen-after-init and the
// availability check below see the values the inline steps used to receive via
// GitHub Actions' between-step $GITHUB_ENV injection.

import (
	"fmt"
	"os"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baoread"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghaout"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghsecret"
)

func RunEnsureReady(dryRun bool, region string, leaderTimeout, joinTimeout time.Duration) error {
	if region == "" {
		return fmt.Errorf("--region is required")
	}
	if dryRun {
		fmt.Fprintln(os.Stderr, "→ (dry-run) would probe OpenBao and init/wait-for-auto-unseal/regen-root as needed")
		return nil
	}

	// 1. Probe all pods — the same aggregation `bao-status` emits (a partial seal
	//    reads as sealed so the re-unseal branch fires).
	states := make([]baoread.PodStatus, 0, len(baoread.PodNames))
	for _, pod := range baoread.PodNames {
		out, _, _ := baoread.ExecFn(pod, "", "", "status", "-format=json")
		st, _ := baoread.ParsePodStatus(out)
		fmt.Printf("  %s: initialized=%t sealed=%t\n", pod, st.Initialized, st.Sealed)
		states = append(states, st)
	}
	initialized, sealedAny := baoread.AggregateStatus(states)
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
		if err := RunInit(dryRun, region); err != nil {
			return err
		}
		if err := baoread.WaitForAutoUnseal(leaderTimeout, joinTimeout); err != nil {
			return err
		}
	case sealedAny:
		// Branch B — initialized but (partially) sealed after a restart. Under
		// static seal the pod self-unseals once it's back up; wait for that rather
		// than submitting keys (there are none).
		if err := baoread.WaitForAutoUnseal(leaderTimeout, joinTimeout); err != nil {
			return err
		}
	}

	// 2. Root token. A fresh init just minted one (now in the process env), so
	//    there's nothing to validate on that path. For an already-initialized
	//    cluster the loaded OPENBAO_ROOT_TOKEN may be the value a prior run
	//    revoked — bao-regen-root validates it and regenerates via quorum if so,
	//    re-exporting the fresh token to $GITHUB_ENV and the process env.
	//
	//    The recovery-quorum half was unreachable until this condition included it.
	//    RunRegenRootCI has an explicit "No OPENBAO_ROOT_TOKEN set — regenerating
	//    via quorum" branch, and the workflow passes RECOVERY_K1/2/3 in for exactly
	//    that — but gating on a NON-EMPTY token meant it never ran. The state it was
	//    written for is the one the tooling itself creates: bootstrap tells the
	//    operator to delete OPENBAO_ROOT_TOKEN afterwards (and `llz status` nags
	//    until they do), so every RE-RUN of bootstrap-openbao arrived with no token,
	//    reported available=false, silently skipped configure + every seed, and died
	//    ~20 minutes later at the converge gate complaining about unconverged apps
	//    rather than about a root token. Regenerate whenever we have either input.
	_, haveQuorum := baoread.RecoveryKeysFromEnv()
	if initialized && (os.Getenv("OPENBAO_ROOT_TOKEN") != "" || haveQuorum == nil) {
		if err := RunRegenRootCI(dryRun, region); err != nil {
			return err
		}
	}

	// 3. Availability gate + re-export. RunInit / RunRegenRootCI already
	//    wrote a minted/regenerated token to $GITHUB_ENV; this also covers the
	//    third case — a loaded token that validated WITHOUT regeneration — so
	//    downstream steps (separate processes) always find OPENBAO_ROOT_TOKEN.
	token := os.Getenv("OPENBAO_ROOT_TOKEN")
	if token == "" {
		fmt.Println("Root token unavailable — configure and seed steps will be skipped.")
		if err := ghaout.Append("GITHUB_STEP_SUMMARY",
			"Root token unavailable — configure and seed steps were skipped.",
			fmt.Sprintf("To re-configure: set OPENBAO_ROOT_TOKEN as an infra-%s environment secret and re-run,", region),
			"or ensure OPENBAO_RECOVERY_KEY_{1,2,3} are set so the workflow can regenerate it."); err != nil {
			return err
		}
		return ghaout.Append("GITHUB_OUTPUT", "available=false")
	}
	ghsecret.Mask(token)
	if err := ghaout.Append("GITHUB_ENV", "OPENBAO_ROOT_TOKEN="+token); err != nil {
		return err
	}
	return ghaout.Append("GITHUB_OUTPUT", "available=true")
}
