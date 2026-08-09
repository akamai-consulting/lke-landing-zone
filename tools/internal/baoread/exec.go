package baoread

// exec.go — HOW ANYTHING TALKS TO OPENBAO. Moved here from cmd/llz/ci_openbao.go.
//
// This is the hub the rest of the OpenBao family reaches for: breakglass, init,
// configure and ensure-ready all go through ExecFn, and until it lived in a
// package none of them could move. It is an access layer, not an extension —
// many inbound edges, almost none outbound, the same shape as internal/credrotate.
//
// TWO EXEC SEAMS, AND THEY ARE NOT INTERCHANGEABLE:
//
//	ExecPod   the raw kubectl exec. Builds its own exec.Command because it needs
//	          stdin, which kubectlprobe.Exec has no signature for.
//	ExecRaw   ExecPod behind a var, so the retry wrapper can be tested.
//	ExecFn    execResilient — retry + backoff. WHAT EVERY CALLER SHOULD USE.
//	Exec      read.go's classifier facade, root pod baked in by baoread_deps.go.
//
// Stubbing ExecRaw when the code under test calls ExecFn leaves the retry wrapper
// live and multiplies each stubbed call by the backoff count; stubbing
// kubectlprobe.Exec reaches only DumpDiagnostics. Both mistakes have been made in
// this campaign, and neither errors — see execoutput_test.go.
//
// WHAT DID NOT COME WITH IT. `llz ci bao-status`'s cobra wiring and appendGHAFile
// went back to package main: a flag set is not an access layer, and appendGHAFile
// has 25 callers of which one is an OpenBao one. Moving it would have made every
// caller of a GITHUB_OUTPUT append import OpenBao.
//
// ORIGINAL HEADER, still accurate about the LANE:
//
// ci_openbao.go implements the `llz ci bao-*` family — native ports of the
// instance-scripts/openbao/ CI steps that bootstrap-openbao.yml runs:
//
//   bao-status   the seal-state probe the workflow previously held as a
//                VERBATIM-copy inline step (drift hazard)
//
// Under the chart's `seal "static"` auto-unseal the pods unseal themselves at
// boot, so the old Shamir bao-unseal / bao-unseal-followers commands and the
// openbao-auto-unseal.yml re-unseal cron are gone; bao-status remains for the
// bootstrap branch + diagnostics, WaitForAutoUnseal is the post-init
// convergence wait (replacing the manual unseal steps), and RecoveryKeysFromEnv
// feeds the generate-root quorum (bao-regen-root, ci_openbao_init.go). It drives
// the in-pod `bao` CLI via kubectl exec (OpenBao's API is only reachable
// in-cluster) through regenroot.go's baoExec, seamed here as ExecFn so the
// poll/branch logic is unit-testable without a cluster.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/kubectlprobe"
)

// PodNames is the fixed 3-pod raft StatefulSet the chart deploys;
// pod 0 is the init/unseal leader on a cold bootstrap.
var PodNames = []string{"platform-openbao-0", "platform-openbao-1", "platform-openbao-2"}

// Seams for tests: ExecRaw is one kubectl exec (regenroot.go's helper);
// ExecFn is the retrying wrapper every bao-* caller goes through; Sleep
// is the inter-poll/retry wait. Splitting raw vs wrapped lets the retry logic
// be unit-tested (inject ExecRaw) while existing tests that stub ExecFn
// keep replacing the whole layer.
var (
	ExecRaw = ExecPod
	ExecFn  = execResilient
	Sleep   = time.Sleep
)

// The transient-exec marker list moved to internal/
//
// It describes kube/konnectivity transport failures, and the only thing that
// REASONS about them is that package's read classifier — which has to tell "the
// pod answered: no such path" from "nothing answered at all". The fact lives once,
// next to the code that decides on it, and this file imports it. Same resolution
// as docsguard.DeliveredDocs and manifestguard.BootstrapValuePlaceholders.

func isTransientExecErr(stderr string) bool {
	for _, m := range TransientMarkers {
		if strings.Contains(stderr, m) {
			return true
		}
	}
	return false
}

// baoExecRetries / baoExecBackoff govern execResilient's transient retry.
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
// failure means the in-pod command never ran (see TransientMarkers).
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

// execResilient wraps a single kubectl exec, retrying ONLY transient
// transport failures (isTransientExecErr) where the command never reached the
// pod. Non-transient errors — including every genuine bao error — return
// immediately, so this never masks a real failure or re-runs a command that
// already executed.
func execResilient(pod, token, stdin string, args ...string) (stdout, stderr string, err error) {
	for attempt := 1; ; attempt++ {
		stdout, stderr, err = ExecRaw(pod, token, stdin, args...)
		if err == nil || attempt >= baoExecRetries || !isTransientExecErr(stderr) {
			return stdout, stderr, err
		}
		fmt.Fprintf(os.Stderr, "llz: transient exec error on %s (attempt %d/%d), retrying: %s\n",
			pod, attempt, baoExecRetries, strings.TrimSpace(stderr))
		Sleep(baoExecBackoff(attempt))
	}
}

// ── status probe ──────────────────────────────────────────────────────────────

// PodStatus is the slice of `bao status -format=json` the CI branches key on.
type PodStatus struct {
	Initialized bool `json:"initialized"`
	Sealed      bool `json:"sealed"`
}

// ParsePodStatus parses `bao status -format=json` output. `bao status`
// exits non-zero when sealed/uninitialized but still prints valid JSON, so
// callers must parse stdout regardless of the exec error. ok=false means no
// JSON arrived at all (pod unreachable / exec failed) — callers treat that as
// uninitialized+sealed, the same default the scripts' ${STATUS:-…} applied.
func ParsePodStatus(out string) (PodStatus, bool) {
	var st PodStatus
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		return PodStatus{Initialized: false, Sealed: true}, false
	}
	return st, true
}

// AggregateStatus folds per-pod states into the cluster-level flags the
// workflow branches on. `initialized` is a cluster-wide flag (same on every
// pod once init has run) so any pod reporting true counts; the cluster is
// treated as sealed if ANY pod is sealed — a partial seal (pod-0 unsealed,
// followers sealed after a rolling restart) leaves raft below quorum and must
// route to the re-unseal branch.
func AggregateStatus(states []PodStatus) (initialized, sealedAny bool) {
	for _, st := range states {
		initialized = initialized || st.Initialized
		sealedAny = sealedAny || st.Sealed
	}
	return initialized, sealedAny
}

func RecoveryKeysFromEnv() ([]string, error) {
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

// WaitForState polls one pod's `bao status` every interval until pred
// accepts it, for at most budget. Returns false on timeout. The first probe is
// immediate (no leading sleep), matching the scripts' until-loops.
func WaitForState(pod string, budget, interval time.Duration, pred func(PodStatus) bool) bool {
	for elapsed := time.Duration(0); ; elapsed += interval {
		out, _, _ := ExecFn(pod, "", "", "status", "-format=json")
		if st, ok := ParsePodStatus(out); ok && pred(st) {
			return true
		}
		if elapsed+interval > budget {
			return false
		}
		Sleep(interval)
	}
}

// DumpDiagnostics prints `bao status` (and optionally the recent container
// log) for a pod that missed its deadline, so the operator gets actionable
// context (TLS SAN mismatch, NetworkPolicy block, peer unreachable) instead of
// one "not initialized" line.
func DumpDiagnostics(pod string, withLogs bool) {
	out, errOut, _ := ExecFn(pod, "", "", "status")
	fmt.Println("--- bao status ---")
	fmt.Println(strings.TrimSpace(firstNonEmpty(out, errOut)))
	if withLogs {
		fmt.Printf("--- %s openbao container log (last 50 lines) ---\n", pod)
		logs, err := kubectlprobe.Exec("kubectl", "-n", Namespace, "logs", pod, "-c", "openbao", "--tail=50")
		if err != nil {
			fmt.Printf("(could not fetch logs: %v)\n", err)
			return
		}
		fmt.Println(strings.TrimSpace(string(logs)))
	}
}

// WaitForAutoUnseal waits for every pod to converge to initialized && unsealed.
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
func WaitForAutoUnseal(leaderTimeout, joinTimeout time.Duration) error {
	leader := PodNames[0]
	fmt.Printf("=== wait for %s (leader) to auto-unseal ===\n", leader)
	if !WaitForState(leader, leaderTimeout, 5*time.Second, func(st PodStatus) bool {
		return st.Initialized && !st.Sealed
	}) {
		fmt.Fprintf(os.Stderr, "::error::%s not initialized+unsealed within %s — leader never auto-unsealed (openbao-unseal-key Secret missing/unreadable, wrong static key, or raft not bootstrapped).\n", leader, leaderTimeout)
		DumpDiagnostics(leader, true)
		return fmt.Errorf("leader %s not initialized+unsealed within %s", leader, leaderTimeout)
	}
	fmt.Printf("%s auto-unsealed.\n", leader)

	for _, pod := range PodNames[1:] {
		fmt.Printf("=== %s: wait for retry_join + auto-unseal ===\n", pod)
		if !WaitForState(pod, joinTimeout, 5*time.Second, func(st PodStatus) bool {
			return st.Initialized && !st.Sealed
		}) {
			fmt.Fprintf(os.Stderr, "::error::%s not initialized+unsealed within %s — retry_join failed (TLS SAN mismatch, NP block, peer unreachable) or its static seal key differs from the leader's. Diagnostics:\n", pod, joinTimeout)
			DumpDiagnostics(pod, true)
			return fmt.Errorf("follower %s not initialized+unsealed within %s", pod, joinTimeout)
		}
		fmt.Printf("%s: initialized + unsealed.\n", pod)
	}
	return nil
}

// ExecPod is the single, unretried in-pod `bao` invocation.
// baoExec runs `bao <args>` inside the openbao container of pod via kubectl exec.
// token (if non-empty) sets the bao token; stdin (if non-empty) is piped in.
func ExecPod(pod, token, stdin string, args ...string) (stdout, stderr string, err error) {
	argv := []string{"-n", Namespace, "exec", "-i", "-c", "openbao", pod, "--", "env"}
	// Loopback listener + CA verification — the network listener now requires a
	// client certificate this in-pod caller does not have. See LoopbackEnv,
	// which also explains why the BAO_* names (not just VAULT_*) are required.
	argv = append(argv, LoopbackEnv()...)
	if token != "" {
		argv = append(argv, "BAO_TOKEN="+token, "VAULT_TOKEN="+token)
	}
	argv = append(argv, "bao")
	argv = append(argv, args...)
	cmd := exec.Command("kubectl", argv...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err = cmd.Run()
	return o.String(), e.String(), err
}

// readSecretLine reads one line from the terminal without echoing it.

// ── the in-pod addressing every exec shares ────────────────────────────────
//
// These moved here with the exec layer rather than staying beside the HTTP
// client. BAO_ADDR is the POD's own loopback listener, not the Service — an exec
// runs inside the container, and pointing it at the Service would route back out
// through the network the exec exists to avoid.
// OpenBao serves TWO listeners (llz-openbao-platform values.yaml):
//
//	[::]:8200        pod network — mTLS, client certificate REQUIRED
//	127.0.0.1:8210   loopback    — TLS, no client certificate
//
// Everything that drives OpenBao by exec-ing into the pod (`bao operator init`,
// unseal/status, generate-root, the `llz ci health` probes) must target the
// loopback listener: those callers run as the `bao` binary inside the container
// and hold no client identity. Pointing them at 8200 fails the handshake.
//
// `kubectl port-forward` also reaches 8210 — forwarding is set up inside the
// pod's network namespace, so a 127.0.0.1-bound listener is reachable. That is
// what keeps the operator paths (`llz openbao get/set`, `llz openbao login`)
// working without issuing client certs to laptops.
const (
	LoopbackPort = "8210"
	LoopbackAddr = "https://127.0.0.1:" + LoopbackPort
	// Mounted from the openbao-tls Secret. The serving cert carries a 127.0.0.1
	// SAN (templates/openbao-tls-cert.yaml), so in-pod callers VERIFY the server
	// rather than running VAULT_SKIP_VERIFY=true as they used to.
	PodCACert = "/openbao/tls/ca.crt"
)

// LoopbackEnv is the env every in-pod `bao` invocation shares. Pure, so the
// address/CA pairing is asserted once in tests instead of being restated (and
// drifting) at each of the four call sites that used to inline it.
//
// MUST set the BAO_* names, not just the VAULT_* aliases. OpenBao's env lookup
// prefers a PRESENT BAO_* variable over its VAULT_* alias unconditionally —
// api/env.go: "If the BAO_ version is present but set to the empty string, still
// prefer that over the VAULT_ prefixed version." The OpenBao chart hardcodes
// BAO_ADDR=https://127.0.0.1:8200 into the container, so exporting VAULT_ADDR
// alone was silently shadowed: every command below reached the mTLS listener
// without a client certificate and died on the handshake —
//
//	Get "https://127.0.0.1:8200/v1/sys/seal-status":
//	http2: client conn could not be established
//
// — which is how a bootstrap failure caused by ADR 0010 read as a probe bug.
// The chart now repoints BAO_ADDR at :8210 as well; both sides are set so
// neither depends on the other being right.
//
// The VAULT_* aliases stay for an older `bao` (or a `vault` binary) that does
// not know the BAO_ names. They are inert whenever the BAO_* names are honoured.
// LoopbackEnv is the env every in-pod `bao` invocation shares. Pure, so the
// address/CA pairing is asserted once in tests instead of being restated (and
// drifting) at each of the four call sites that used to inline it.
//
// MUST set the BAO_* names, not just the VAULT_* aliases. OpenBao's env lookup
// prefers a PRESENT BAO_* variable over its VAULT_* alias unconditionally —
// api/env.go: "If the BAO_ version is present but set to the empty string, still
// prefer that over the VAULT_ prefixed version." The OpenBao chart hardcodes
// BAO_ADDR=https://127.0.0.1:8200 into the container, so exporting VAULT_ADDR
// alone was silently shadowed: every command below reached the mTLS listener
// without a client certificate and died on the handshake —
//
//	Get "https://127.0.0.1:8200/v1/sys/seal-status":
//	http2: client conn could not be established
//
// — which is how a bootstrap failure caused by ADR 0010 read as a probe bug.
// The chart now repoints BAO_ADDR at :8210 as well; both sides are set so
// neither depends on the other being right.
//
// The VAULT_* aliases stay for an older `bao` (or a `vault` binary) that does
// not know the BAO_ names. They are inert whenever the BAO_* names are honoured.
func LoopbackEnv() []string {
	return []string{
		"BAO_ADDR=" + LoopbackAddr, "BAO_CACERT=" + PodCACert,
		"VAULT_ADDR=" + LoopbackAddr, "VAULT_CACERT=" + PodCACert,
	}
}

// baoExecArgv builds the kubectl argv that runs `bao <args>` inside the openbao
// container of pod with the standard env (token included). Pure, so the argv
// shape + token placement are unit-tested.

// firstNonEmpty returns the first non-empty string. Pure, localised — the eighth
// package in this campaign to keep its own three lines.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
