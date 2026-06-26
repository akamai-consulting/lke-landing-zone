package main

// ci_openbao.go implements the `llz ci bao-*` family — native ports of the
// instance-scripts/openbao/ CI steps that bootstrap-openbao.yml runs:
//
//   bao-status   the seal-state probe the workflow previously held as a
//                VERBATIM-copy inline step (drift hazard)
//
// Under the chart's `seal "static"` auto-unseal the pods unseal themselves at
// boot, so the old Shamir bao-unseal / bao-unseal-followers commands and the
// openbao-auto-unseal.yml re-unseal cron are gone; bao-status remains for the
// bootstrap branch + diagnostics, waitForAutoUnseal is the post-init
// convergence wait (replacing the manual unseal steps), and recoveryKeysFromEnv
// feeds the generate-root quorum (bao-regen-root, ci_openbao_init.go). It drives
// the in-pod `bao` CLI via kubectl exec (OpenBao's API is only reachable
// in-cluster) through regenroot.go's baoExec, seamed here as baoExecFn so the
// poll/branch logic is unit-testable without a cluster.

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// openbaoPodNames is the fixed 3-pod raft StatefulSet the chart deploys;
// pod 0 is the init/unseal leader on a cold bootstrap.
var openbaoPodNames = []string{"platform-openbao-0", "platform-openbao-1", "platform-openbao-2"}

// Seams for tests: baoExecRawFn is one kubectl exec (regenroot.go's helper);
// baoExecFn is the retrying wrapper every bao-* caller goes through; baoSleep
// is the inter-poll/retry wait. Splitting raw vs wrapped lets the retry logic
// be unit-tested (inject baoExecRawFn) while existing tests that stub baoExecFn
// keep replacing the whole layer.
var (
	baoExecRawFn = baoExec
	baoExecFn    = baoExecResilient
	baoSleep     = time.Sleep
)

// transientExecMarkers are kube-apiserver/konnectivity transport failures that
// mean the exec stream never reached the pod, so the in-pod `bao` command did
// NOT run — making a retry safe even for non-idempotent ops like `operator
// init`. A real bao error ("already initialized", a sealed-pod status exit)
// carries none of these and is returned on the first try. The canonical case:
// konnectivity reports "No agent available" for up to a few minutes after a
// node/pod comes up — exactly when bao-init first execs — so one blip would
// otherwise fail the entire bootstrap.
var transientExecMarkers = []string{
	"No agent available",           // konnectivity: no tunnel agent registered yet
	"error dialing backend",        // apiserver could not dial the kubelet
	"unable to upgrade connection", // SPDY/exec stream never established
	"error sending request",        // request never delivered to the node
	"TLS handshake timeout",        // apiserver↔kubelet handshake stalled
}

func isTransientExecErr(stderr string) bool {
	for _, m := range transientExecMarkers {
		if strings.Contains(stderr, m) {
			return true
		}
	}
	return false
}

// baoExecRetries / baoExecBackoff govern baoExecResilient's transient retry.
// Backoff is linear and capped at 15s/wait, for a ~5m total budget over
// baoExecRetries tries — generous, but still well inside the job's 30m budget.
//
// SCAR: on a COLD bootstrap the konnectivity agent can take *minutes*, not
// seconds, to register with the control-plane server. An e2e run has seen
// "No agent available" persist continuously for >2 minutes —
// through the whole bao-status probe and into bao-init — which blew the old
// 6-try / ~30s budget and failed the entire OpenBao bootstrap. The agent dials
// OUT to the server and the node-pool firewall's outbound policy is ACCEPT, so
// the firewall never gates this; the only safe lever is to wait the warmup out.
// Retrying is safe even for `operator init` because a transient transport
// failure means the in-pod command never ran (see transientExecMarkers).
//
// A later run still spent 17 of 18 tries on platform-openbao-0
// before konnectivity registered — one attempt of margin — so the budget was
// widened again from 18 to 24 (~5m) to keep a slower-than-usual warmup from
// failing the bootstrap on the last try.
var (
	baoExecRetries = 24
	baoExecBackoff = func(attempt int) time.Duration {
		if d := time.Duration(attempt) * 2 * time.Second; d < 15*time.Second {
			return d
		}
		return 15 * time.Second
	}
)

// baoExecResilient wraps a single kubectl exec, retrying ONLY transient
// transport failures (isTransientExecErr) where the command never reached the
// pod. Non-transient errors — including every genuine bao error — return
// immediately, so this never masks a real failure or re-runs a command that
// already executed.
func baoExecResilient(pod, token, stdin string, args ...string) (stdout, stderr string, err error) {
	for attempt := 1; ; attempt++ {
		stdout, stderr, err = baoExecRawFn(pod, token, stdin, args...)
		if err == nil || attempt >= baoExecRetries || !isTransientExecErr(stderr) {
			return stdout, stderr, err
		}
		fmt.Fprintf(os.Stderr, "llz: transient exec error on %s (attempt %d/%d), retrying: %s\n",
			pod, attempt, baoExecRetries, strings.TrimSpace(stderr))
		baoSleep(baoExecBackoff(attempt))
	}
}

// ── status probe ──────────────────────────────────────────────────────────────

// baoPodStatus is the slice of `bao status -format=json` the CI branches key on.
type baoPodStatus struct {
	Initialized bool `json:"initialized"`
	Sealed      bool `json:"sealed"`
}

// parseBaoPodStatus parses `bao status -format=json` output. `bao status`
// exits non-zero when sealed/uninitialized but still prints valid JSON, so
// callers must parse stdout regardless of the exec error. ok=false means no
// JSON arrived at all (pod unreachable / exec failed) — callers treat that as
// uninitialized+sealed, the same default the scripts' ${STATUS:-…} applied.
func parseBaoPodStatus(out string) (baoPodStatus, bool) {
	var st baoPodStatus
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		return baoPodStatus{Initialized: false, Sealed: true}, false
	}
	return st, true
}

// aggregateBaoStatus folds per-pod states into the cluster-level flags the
// workflow branches on. `initialized` is a cluster-wide flag (same on every
// pod once init has run) so any pod reporting true counts; the cluster is
// treated as sealed if ANY pod is sealed — a partial seal (pod-0 unsealed,
// followers sealed after a rolling restart) leaves raft below quorum and must
// route to the re-unseal branch.
func aggregateBaoStatus(states []baoPodStatus) (initialized, sealedAny bool) {
	for _, st := range states {
		initialized = initialized || st.Initialized
		sealedAny = sealedAny || st.Sealed
	}
	return initialized, sealedAny
}

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
	states := make([]baoPodStatus, 0, len(openbaoPodNames))
	for _, pod := range openbaoPodNames {
		// The exec error is deliberately ignored: sealed pods exit 2 with good
		// JSON, and a dead pod yields unparseable output → the sealed default.
		out, _, _ := baoExecFn(pod, "", "", "status", "-format=json")
		st, _ := parseBaoPodStatus(out)
		fmt.Printf("  %s: initialized=%t sealed=%t\n", pod, st.Initialized, st.Sealed)
		states = append(states, st)
	}
	initialized, sealedAny := aggregateBaoStatus(states)
	fmt.Printf("initialized=%t\nsealed=%t\n", initialized, sealedAny)
	return appendGHAFile("GITHUB_OUTPUT",
		fmt.Sprintf("initialized=%t", initialized),
		fmt.Sprintf("sealed=%t", sealedAny))
}

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

// recoveryKeysFromEnv reads the 3 quorum recovery keys from RECOVERY_K1/2/3.
// Under the chart's `seal "static"` auto-unseal, `operator init` yields recovery
// shares (not unseal keys): the seal mechanism unseals every pod at boot, so
// there is no submit-keys-to-unseal step. The recovery keys exist only to
// authorize the `operator generate-root` quorum that bao-regen-root runs.
func recoveryKeysFromEnv() ([]string, error) {
	keys := make([]string, 0, 3)
	for _, v := range []string{"RECOVERY_K1", "RECOVERY_K2", "RECOVERY_K3"} {
		k := os.Getenv(v)
		if k == "" {
			return nil, fmt.Errorf("%s is not set — the 3 quorum recovery keys are required (infra-<region> secrets OPENBAO_RECOVERY_KEY_{1,2,3})", v)
		}
		keys = append(keys, k)
	}
	return keys, nil
}

// ── auto-unseal convergence wait (post-init / re-seal) ────────────────────────

// waitForBaoState polls one pod's `bao status` every interval until pred
// accepts it, for at most budget. Returns false on timeout. The first probe is
// immediate (no leading sleep), matching the scripts' until-loops.
func waitForBaoState(pod string, budget, interval time.Duration, pred func(baoPodStatus) bool) bool {
	for elapsed := time.Duration(0); ; elapsed += interval {
		out, _, _ := baoExecFn(pod, "", "", "status", "-format=json")
		if st, ok := parseBaoPodStatus(out); ok && pred(st) {
			return true
		}
		if elapsed+interval > budget {
			return false
		}
		baoSleep(interval)
	}
}

// dumpBaoDiagnostics prints `bao status` (and optionally the recent container
// log) for a pod that missed its deadline, so the operator gets actionable
// context (TLS SAN mismatch, NetworkPolicy block, peer unreachable) instead of
// one "not initialized" line.
func dumpBaoDiagnostics(pod string, withLogs bool) {
	out, errOut, _ := baoExecFn(pod, "", "", "status")
	fmt.Println("--- bao status ---")
	fmt.Println(strings.TrimSpace(firstNonEmpty(out, errOut)))
	if withLogs {
		fmt.Printf("--- %s openbao container log (last 50 lines) ---\n", pod)
		logs, err := execOutput("kubectl", "-n", openbaoNS, "logs", pod, "-c", "openbao", "--tail=50")
		if err != nil {
			fmt.Printf("(could not fetch logs: %v)\n", err)
			return
		}
		fmt.Println(strings.TrimSpace(string(logs)))
	}
}

// waitForAutoUnseal waits for every pod to converge to initialized && unsealed.
// Under the chart's `seal "static"` auto-unseal there is no key-submission step:
// each pod unseals itself at boot from the static seal key once retry_join has
// joined it to raft, so the bootstrap just waits for that convergence (the
// leader first — followers can only join a serving leader — then each follower).
// It replaces the old manual unseal-pod-0 + unseal-followers sequence and is
// also the re-seal path (an initialized-but-sealed pod after a restart self-
// unseals; we wait rather than submit keys). On timeout it dumps `bao status` +
// recent pod logs for the stuck pod so the operator gets actionable context
// (missing/unreadable openbao-unseal-key Secret, wrong static key, TLS SAN
// mismatch, NetworkPolicy block, peer unreachable).
func waitForAutoUnseal(leaderTimeout, joinTimeout time.Duration) error {
	leader := openbaoPodNames[0]
	fmt.Printf("=== wait for %s (leader) to auto-unseal ===\n", leader)
	if !waitForBaoState(leader, leaderTimeout, 5*time.Second, func(st baoPodStatus) bool {
		return st.Initialized && !st.Sealed
	}) {
		fmt.Fprintf(os.Stderr, "::error::%s not initialized+unsealed within %s — leader never auto-unsealed (openbao-unseal-key Secret missing/unreadable, wrong static key, or raft not bootstrapped).\n", leader, leaderTimeout)
		dumpBaoDiagnostics(leader, true)
		return fmt.Errorf("leader %s not initialized+unsealed within %s", leader, leaderTimeout)
	}
	fmt.Printf("%s auto-unsealed.\n", leader)

	for _, pod := range openbaoPodNames[1:] {
		fmt.Printf("=== %s: wait for retry_join + auto-unseal ===\n", pod)
		if !waitForBaoState(pod, joinTimeout, 5*time.Second, func(st baoPodStatus) bool {
			return st.Initialized && !st.Sealed
		}) {
			fmt.Fprintf(os.Stderr, "::error::%s not initialized+unsealed within %s — retry_join failed (TLS SAN mismatch, NP block, peer unreachable) or its static seal key differs from the leader's. Diagnostics:\n", pod, joinTimeout)
			dumpBaoDiagnostics(pod, true)
			return fmt.Errorf("follower %s not initialized+unsealed within %s", pod, joinTimeout)
		}
		fmt.Printf("%s: initialized + unsealed.\n", pod)
	}
	return nil
}
