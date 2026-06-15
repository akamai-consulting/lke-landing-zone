package main

// ci_openbao.go implements the `llz ci bao-*` family — native ports of the
// instance-scripts/openbao/ CI steps that bootstrap-openbao.yml and
// openbao-auto-unseal.yml used to run as bash:
//
//   bao-status            the seal-state probe both workflows previously held
//                         as a VERBATIM-copy inline step (drift hazard)
//   bao-unseal            unseal-all.sh + the inline "Unseal pod 0" step
//   bao-unseal-followers  unseal-followers.sh (leader wait + retry_join poll)
//
// All of them drive the in-pod `bao` CLI via kubectl exec (OpenBao's API is
// only reachable in-cluster) through regenroot.go's baoExec, seamed here as
// baoExecFn so the poll/branch logic is unit-testable without a cluster. The
// quorum keys ride the UNSEAL_K1/2/3 env vars — the same contract as the
// retired scripts (Branch A gets them from bao-init's GITHUB_ENV write,
// Branch B from the infra-<region> environment secrets) — and are passed to
// the in-pod bao on the local kubectl argv exactly as the bash did.

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
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
// seconds, to register with the control-plane server. The e2e run on
// 2026-06-12 saw "No agent available" persist continuously for >2 minutes —
// through the whole bao-status probe and into bao-init — which blew the old
// 6-try / ~30s budget and failed the entire OpenBao bootstrap. The agent dials
// OUT to the server and the node-pool firewall's outbound policy is ACCEPT, so
// the firewall never gates this; the only safe lever is to wait the warmup out.
// Retrying is safe even for `operator init` because a transient transport
// failure means the in-pod command never ran (see transientExecMarkers).
//
// A later 2026-06-12 run still spent 17 of 18 tries on platform-openbao-0
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

// ── unseal ────────────────────────────────────────────────────────────────────

// unsealKeysFromEnv reads the 3 quorum keys from UNSEAL_K1/2/3.
func unsealKeysFromEnv() ([]string, error) {
	keys := make([]string, 0, 3)
	for _, v := range []string{"UNSEAL_K1", "UNSEAL_K2", "UNSEAL_K3"} {
		k := os.Getenv(v)
		if k == "" {
			return nil, fmt.Errorf("%s is not set — the 3 quorum unseal keys are required (infra-<region> secrets OPENBAO_UNSEAL_KEY_{1,2,3})", v)
		}
		keys = append(keys, k)
	}
	return keys, nil
}

// resolveUnsealPods maps a --pods spec ("all" or comma-separated ordinals,
// e.g. "0" / "1,2") to pod names.
func resolveUnsealPods(spec string) ([]string, error) {
	if spec == "" || spec == "all" {
		return openbaoPodNames, nil
	}
	var pods []string
	for _, part := range strings.Split(spec, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n < 0 || n >= len(openbaoPodNames) {
			return nil, fmt.Errorf("--pods must be 'all' or comma-separated ordinals 0-%d (got %q)", len(openbaoPodNames)-1, spec)
		}
		pods = append(pods, openbaoPodNames[n])
	}
	return pods, nil
}

// unsealPod submits the 3 quorum keys to one pod. Idempotent like the bash:
// `bao operator unseal` against an already-unsealed pod reports success.
func unsealPod(pod string, keys []string) error {
	for i, key := range keys {
		out, errOut, err := baoExecFn(pod, "", "", "operator", "unseal", key)
		if err != nil {
			return fmt.Errorf("unseal %s (key %d/%d): %s", pod, i+1, len(keys),
				strings.TrimSpace(firstNonEmpty(errOut, out)))
		}
	}
	fmt.Printf("%s: submitted %d/%d unseal keys\n", pod, len(keys), len(keys))
	return nil
}

func ciBaoUnsealCmd() *cobra.Command {
	var pods string
	c := &cobra.Command{
		Use:   "bao-unseal",
		Short: "unseal OpenBao pods with the 3 quorum keys (UNSEAL_K1/2/3)",
		Long: "Native port of unseal-all.sh (and, with --pods 0, of the inline \"Unseal\n" +
			"pod 0 (post-init)\" step). Submits UNSEAL_K1/2/3 to each selected pod.\n" +
			"Idempotent: an already-unsealed pod accepts the keys as no-ops, so the\n" +
			"auto-unseal schedule and Branch B can re-run it safely.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIBaoUnseal(gopts, pods) },
	}
	c.Flags().StringVar(&pods, "pods", "all", "pods to unseal: 'all' or comma-separated ordinals (e.g. 0)")
	return c
}

func runCIBaoUnseal(g globalOpts, spec string) error {
	pods, err := resolveUnsealPods(spec)
	if err != nil {
		return err
	}
	keys, err := unsealKeysFromEnv()
	if err != nil {
		return err
	}
	for _, pod := range pods {
		if g.dryRun {
			fmt.Fprintf(os.Stderr, "→ (dry-run) would submit 3 unseal keys to %s\n", pod)
			continue
		}
		if err := unsealPod(pod, keys); err != nil {
			return err
		}
	}
	return nil
}

// ── follower join + unseal (post-init Branch A) ───────────────────────────────

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

func ciBaoUnsealFollowersCmd() *cobra.Command {
	var leaderTimeout, joinTimeout int
	c := &cobra.Command{
		Use:   "bao-unseal-followers",
		Short: "post-init: wait for retry_join to initialize pods 1-2, then unseal them",
		Long: "Native port of unseal-followers.sh. First confirms pod-0 (the leader) is\n" +
			"unsealed and serving — a follower can only complete retry_join once the\n" +
			"leader's raft bootstrap challenge endpoint stops 503ing — then polls each\n" +
			"follower until retry_join flips it to initialized=true and submits the\n" +
			"UNSEAL_K1/2/3 quorum keys. There is NO manual raft join: the chart's\n" +
			"retry_join stanza handles membership, and a duplicate join attempt 500s\n" +
			"(see the project_openbao_raft_join_gotchas write-up). On timeout it dumps\n" +
			"`bao status` + recent pod logs for actionable diagnostics.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runCIBaoUnsealFollowers(gopts, time.Duration(leaderTimeout)*time.Second, time.Duration(joinTimeout)*time.Second)
		},
	}
	// 180s: leader election + API settle after pod-0's unseal. 300s: a cold
	// join on a resource-constrained cluster routinely outlives two retry_join
	// rounds — 120s lost that race repeatedly (script history).
	c.Flags().IntVar(&leaderTimeout, "leader-timeout", 180, "seconds to wait for pod-0 to report unsealed")
	c.Flags().IntVar(&joinTimeout, "join-timeout", 300, "seconds to wait for each follower to reach initialized=true")
	return c
}

func runCIBaoUnsealFollowers(g globalOpts, leaderTimeout, joinTimeout time.Duration) error {
	keys, err := unsealKeysFromEnv()
	if err != nil {
		return err
	}
	if g.dryRun {
		fmt.Fprintln(os.Stderr, "→ (dry-run) would wait for the leader, then poll+unseal pods 1-2")
		return nil
	}

	leader := openbaoPodNames[0]
	fmt.Printf("=== wait for %s (leader) to be unsealed ===\n", leader)
	if !waitForBaoState(leader, leaderTimeout, 5*time.Second, func(st baoPodStatus) bool {
		return st.Initialized && !st.Sealed
	}) {
		fmt.Fprintf(os.Stderr, "::error::%s not unsealed within %s — leader never settled, followers cannot join.\n", leader, leaderTimeout)
		dumpBaoDiagnostics(leader, false)
		return fmt.Errorf("leader %s not unsealed within %s", leader, leaderTimeout)
	}
	fmt.Printf("%s unsealed — followers can now retry_join.\n", leader)

	for _, pod := range openbaoPodNames[1:] {
		fmt.Printf("=== %s: wait for retry_join to initialize this follower ===\n", pod)
		if !waitForBaoState(pod, joinTimeout, 5*time.Second, func(st baoPodStatus) bool {
			return st.Initialized
		}) {
			fmt.Fprintf(os.Stderr, "::error::%s did not reach initialized=true within %s — retry_join likely failed (TLS SAN mismatch, NP block, or peer unreachable). Diagnostics:\n", pod, joinTimeout)
			dumpBaoDiagnostics(pod, true)
			return fmt.Errorf("follower %s did not initialize within %s", pod, joinTimeout)
		}
		fmt.Printf("%s: initialized=true — proceeding to unseal.\n", pod)
		if err := unsealPod(pod, keys); err != nil {
			return err
		}
	}
	return nil
}
