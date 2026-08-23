package clusteraccess

// runner_acl_configmap.go is the persistence half of `llz ci runner-acl`: it
// records this runner's egress IP as a time-boxed lease in the
// firewall-runner-acl ConfigMap (kube-system) so the EAA firewall-controller
// UNIONS it into the LKE control-plane ACL on each reconcile instead of
// replacing the runner out.
//
// The problem it solves: the controller PUTs (replaces, not merges) the
// control-plane ACL with the EAA/bastion set every ~10 minutes. A long-running
// kubectl job — the convergence gate polls for up to 30 minutes — gets evicted
// mid-flight when a reconcile lands after `open` added the runner IP directly.
// The lease makes the controller preserve the IP for the lease's TTL; `revoke`
// removes the lease; a runner that dies without revoking self-heals when the
// lease expires (the controller ignores expired leases), so the ACL is never
// append-only.
//
// All ConfigMap writes here are BEST-EFFORT: the direct Linode-API ACL PUT in
// runner_acl.go already granted access, so a kubectl failure is a warning, not a
// step failure. The TTL is the backstop for both a failed register (the job may
// be clobbered later, as before this feature) and a failed deregister (the lease
// lapses on its own).

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	runnerACLConfigMapNS   = "kube-system"
	runnerACLConfigMapName = "firewall-runner-acl"
	// runnerACLLeaseTTL must exceed the longest cluster-facing kubectl job that
	// holds the ACL open without refreshing the lease — the convergence gate's
	// 30-minute budget — so a single register at `open` covers the whole job.
	runnerACLLeaseTTL = 45 * time.Minute
)

// runnerACLKubectlTimeout bounds each individual kubectl call.
//
// WITHOUT IT THE RETRY BUDGET IS FICTION. This lease is registered right after
// the control-plane ACL is opened, against an apiserver that may not yet accept
// this runner — which is the entire reason for the retry loop. kubectl's default
// client timeout is effectively unbounded for that case (it sits in connect/TLS
// until the kernel gives up, ~2 minutes), so "8 retries" silently means "8 times
// however long kubectl takes to despair".
//
// Measured on a live e2e run: the ACL-open step took 15m25s and emitted not one
// line, because 8 attempts × ~115s of blocked kubectl is 15m20s. The step then
// reported SUCCESS, since a lease failure is deliberately a warning (see below),
// so 15 minutes of a 70-minute job budget vanished with no failure and no signal.
// The job later died anyway on the same unreachable apiserver.
//
// 10s is far above a healthy apiserver's response and far below the point where
// waiting tells you anything: if the ACL has not taken effect, it has not taken
// effect. The whole loop is now bounded at roughly 8×(10s+8s) ≈ 2.5m.
const runnerACLKubectlTimeout = 10 * time.Second

// runnerACLLeaseBudget caps the ENTIRE lease phase. Must stay well under the
// caller's step timeout (6m on the cluster-access steps) so the loop always gets
// to say why it stopped, instead of being killed mid-attempt with no output.
// 8 attempts x (10s kubectl + 8s gap + a bounded re-assert) fits inside this
// with room to spare; exceeding it means something is wedged, not slow.
const runnerACLLeaseBudget = 90 * time.Second

// Seams (overridden in tests).
var (
	// runnerACLKubectlFn runs kubectl with args, piping stdin, and returns the
	// combined output. KUBECONFIG comes from the lke-runner-acl action when a
	// kubeconfig is wired, but NOT via the inherited environment any more — the
	// child's env is rebuilt by runnerACLKubectlEnv, which resolves the shell
	// variables the action used to expand with eval.
	runnerACLKubectlFn = func(stdin string, args ...string) (string, error) {
		cmd := exec.Command("kubectl", runnerACLKubectlArgv(args)...)
		cmd.Env = runnerACLKubectlEnv(os.Environ())
		if stdin != "" {
			cmd.Stdin = strings.NewReader(stdin)
		}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	runnerACLNow      = time.Now
	runnerACLPatchN   = 8               // register retries while the just-opened ACL propagates
	runnerACLPatchGap = 8 * time.Second // delay between register retries
	runnerACLSleep    = func(d time.Duration) { time.Sleep(d) }
)

// runnerACLKubectlArgv prepends the request timeout to a kubectl argv. Pure, and
// separated from the exec seam ON PURPOSE: the tests replace runnerACLKubectlFn
// wholesale, so anything inline in that closure is never exercised — which is
// exactly how an unbounded kubectl survived here in the first place.
//
// PREPENDED, not appended. These calls end in `-f -` or a resource name, and
// kubectl parses a flag placed after those as a positional argument.
func runnerACLKubectlArgv(args []string) []string {
	return append([]string{"--request-timeout=" + runnerACLKubectlTimeout.String()}, args...)
}

// apiServerUnreachableMarkers are kubectl/client-go signatures that mean the
// request never reached a serving apiserver — as opposed to reaching one and
// being refused (RBAC), or the object not existing (NotFound).
//
// The distinction is for the OPERATOR reading the log, not for the exit code:
// "the apiserver never answered" and "the apiserver refused this write" want
// different responses. An earlier version made the former fatal; measurement
// showed that is the normal state during a cold bootstrap (a fresh LKE-E cluster
// takes minutes to honour a newly added ACL address, vs ~35s once settled), so
// failing on it aborted every cold start before the API-readiness wait could do
// its job. See leaseOutcome.
//
// Observed verbatim across runs 30485106067 and 30499831638:
//
//	Get "https://lke637329.api.us-ord.enterprise.linodelke.net:6443/api?timeout=10s":
//	net/http: request canceled while waiting for connection (Client.Timeout exceeded ...)
//	client rate limiter Wait returned an error: context deadline exceeded
//	  - error from a previous attempt: EOF
var apiServerUnreachableMarkers = []string{
	"Client.Timeout exceeded",
	"context deadline exceeded",
	"couldn't get current server API group list",
	"Unable to connect to the server",
	"connection refused",
	"i/o timeout",
	"no route to host",
	"EOF",
	"TLS handshake timeout",
}

// isAPIServerUnreachable reports whether kubectl output shows the apiserver was
// never reached. Pure, so the classification is unit-tested against the real
// strings rather than guessed at.
func isAPIServerUnreachable(out string) bool {
	for _, m := range apiServerUnreachableMarkers {
		if strings.Contains(out, m) {
			return true
		}
	}
	return false
}

// runnerACLLeaseValue is the JSON stored in each ConfigMap data value. The data
// key is a sanitized IP (ConfigMap keys can't contain '/'); the controller reads
// the values, so cidr is authoritative.
type runnerACLLeaseValue struct {
	CIDR      string `json:"cidr"`
	ExpiresAt string `json:"expiresAt"`
}

// runnerACLDataKey sanitizes an IP into a valid ConfigMap data key
// (`[-._a-zA-Z0-9]+`): '1.2.3.4' -> 'ip-1.2.3.4'. The mapping need not be
// reversible — the lease value carries the real CIDR.
func runnerACLDataKey(ip string) string {
	var b strings.Builder
	b.WriteString("ip-")
	for _, r := range ip {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

// registerRunnerACLIP upserts a lease for ip (TTL from now) into the
// firewall-runner-acl ConfigMap and opportunistically prunes any expired leases
// it sees in the same patch. Retries while the just-opened ACL propagates to the
// apiserver. Best-effort: a persistent failure warns and returns.
// reassertACL is called between failed lease attempts to re-add the runner IP to
// the control-plane ACL. See registerRunnerACLIP for why that is the whole point.
// nil disables it (tests, and the no-client paths).
type reassertACL func() error

// registerRunnerACLIP leases ip in the ConfigMap. reassert may be nil.
//
// THE DEADLOCK THIS SOLVES. The caller has just added ip to the control-plane
// ACL directly and verified it — but the firewall controller PUTs (replaces) that
// ACL roughly every 10 minutes, and the lease below is what makes it PRESERVE the
// runner. Between the caller's verify and a successful lease there is an
// unprotected window: if a reconcile lands in it, the runner is evicted, and the
// lease can never be written because writing it needs the apiserver the runner
// was just locked out of. Every remaining retry is then guaranteed to fail — the
// loop retries the one thing that cannot succeed.
//
// Observed twice on live e2e runs (30473785350, 30478772336): "added <ip> to
// cluster <id> control-plane ACL" followed by kubectl hanging until the step
// budget ran out, and the apiserver never becoming reachable again.
//
// So a kubectl failure here is not just a failed write — it is EVIDENCE that we
// may have been evicted. Re-asserting the ACL before the next attempt turns a
// permanent deadlock into a race we win on the following try. It is safe to
// re-assert repeatedly: the caller's add is idempotent (WithIP is a set union)
// and cheap, and if we were never evicted it is a no-op.
// leaseOutcome reports a give-up. ALWAYS a warning, never an error.
//
// An earlier version hard-failed when the apiserver was unreachable, reasoning
// that it disproved the "the grant already worked" premise. That reasoning was
// wrong, and measurement showed why: on a FRESHLY CREATED LKE-E cluster the
// control-plane ACL takes minutes to begin honouring a newly added address —
// far longer than this lease loop can wait — while on a settled cluster the same
// operation takes ~35s. So "unreachable" here is the EXPECTED state during a cold
// bootstrap, not evidence of a broken grant.
//
// Hard-failing on it would abort every cold start at cluster-access, before
// `wait-cluster-ready` — the step that exists to wait exactly this out — ever
// runs. Reachability is that step's job, and it owns the verdict.
//
// The unreachable/answered distinction is still worth DRAWING, because the two
// mean different things to a reader, and isAPIServerUnreachable keeps that
// classification for the message. It just no longer changes the exit code.
func leaseOutcome(ip string, attempts int, e time.Duration, lastErr error, lastOut string) error {
	detail := firstLine(strings.TrimSpace(lastOut))
	if isAPIServerUnreachable(lastOut) || (lastErr != nil && isAPIServerUnreachable(lastErr.Error())) {
		fmt.Fprintf(os.Stderr, "::warning::runner-acl: could not lease %s after %s (%d/%d attempts) — the apiserver "+
			"is not reachable yet. On a newly created cluster the control-plane ACL takes minutes to honour a new "+
			"address; the API-readiness wait that follows is what waits for it. Not failing here. Last error: %v: %s\n",
			ip, e, attempts, runnerACLPatchN, lastErr, detail)
		return nil
	}
	fmt.Fprintf(os.Stderr, "::warning::runner-acl: could not lease %s after %s (%d/%d attempts): %v: %s — "+
		"the apiserver ANSWERED, so access works and only the lease is missing (eviction risk later in the job).\n",
		ip, e, attempts, runnerACLPatchN, lastErr, detail)
	return nil
}

func registerRunnerACLIP(ip string, reassert reassertACL) error {
	now := runnerACLNow()
	val, err := json.Marshal(runnerACLLeaseValue{
		CIDR:      ip,
		ExpiresAt: now.Add(runnerACLLeaseTTL).UTC().Format(time.RFC3339),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "::warning::runner-acl: marshaling lease for %s: %v\n", ip, err)
		return nil
	}

	// HARD CEILING on the whole lease phase. This is best-effort work — the ACL
	// grant already happened — so it must never be able to consume the step
	// budget above it. It did: an e2e run sat in this function for 5m48s and
	// emitted NOTHING, because the only warning was after all 8 attempts, so the
	// step timed out before the loop could explain itself. A phase that cannot
	// report why it gave up is worse than one that gives up early.
	phaseStart := runnerACLNow()
	elapsed := func() time.Duration { return runnerACLNow().Sub(phaseStart).Round(time.Second) }

	var lastOut string
	var lastErr error
	for attempt := 1; attempt <= runnerACLPatchN; attempt++ {
		if e := elapsed(); e >= runnerACLLeaseBudget {
			return leaseOutcome(ip, attempt-1, e, lastErr, lastOut)
		}
		// Build the merge patch fresh each attempt: null out leases that have
		// since expired, set our own. Merge-patch keys are independent, so a
		// concurrent runner adding a different IP is not clobbered.
		patch := map[string]any{}
		for key := range expiredRunnerACLKeys(now) {
			patch[key] = nil
		}
		patch[runnerACLDataKey(ip)] = string(val)
		body, _ := json.Marshal(map[string]any{"data": patch})

		out, perr := runnerACLKubectlFn("", "patch", "configmap", runnerACLConfigMapName,
			"-n", runnerACLConfigMapNS, "--type", "merge", "-p", string(body))
		if perr == nil {
			fmt.Printf("runner-acl: leased %s in %s/%s for %s.\n", ip, runnerACLConfigMapNS, runnerACLConfigMapName, runnerACLLeaseTTL)
			return nil
		}
		// The ConfigMap may not exist yet — create it with just this lease.
		if isNotFound(out) {
			manifest := runnerACLConfigMapManifest(map[string]string{runnerACLDataKey(ip): string(val)})
			if cout, cerr := runnerACLKubectlFn(manifest, "apply", "-f", "-"); cerr == nil {
				fmt.Printf("runner-acl: created %s/%s with %s lease.\n", runnerACLConfigMapNS, runnerACLConfigMapName, ip)
				return nil
			} else if !isAlreadyExists(cout) {
				lastOut, lastErr = cout, cerr
			} else {
				lastOut, lastErr = out, perr // raced a creator; retry the patch
			}
		} else {
			lastOut, lastErr = out, perr
			// Per-attempt, not just at the end: without this the whole phase is a
			// black box, which is exactly how 5m48s of silence happened.
			fmt.Fprintf(os.Stderr, "runner-acl: lease attempt %d/%d failed after %s (%v): %s\n",
				attempt, runnerACLPatchN, elapsed(), perr, strings.TrimSpace(out))
		}
		if attempt < runnerACLPatchN {
			// The kubectl call failed, so we may have just been evicted by a
			// controller reconcile — in which case retrying kubectl alone can
			// never succeed. Put the IP back first.
			if reassert != nil {
				rstart := runnerACLNow()
				if rerr := reassert(); rerr != nil {
					fmt.Fprintf(os.Stderr, "::warning::runner-acl: re-asserting %s in the control-plane ACL failed after %s: %v\n",
						ip, runnerACLNow().Sub(rstart).Round(time.Second), rerr)
				}
			}
			runnerACLSleep(runnerACLPatchGap)
		}
	}
	return leaseOutcome(ip, runnerACLPatchN, elapsed(), lastErr, lastOut)
}

// deregisterRunnerACLIP removes ip's lease so the controller stops re-adding it
// on the next reconcile. Best-effort: a failure warns — the lease TTL lapses it
// regardless.
func deregisterRunnerACLIP(ip string) {
	body, _ := json.Marshal(map[string]any{"data": map[string]any{runnerACLDataKey(ip): nil}})
	out, err := runnerACLKubectlFn("", "patch", "configmap", runnerACLConfigMapName,
		"-n", runnerACLConfigMapNS, "--type", "merge", "-p", string(body))
	if err != nil {
		if isNotFound(out) {
			return // nothing to remove
		}
		fmt.Fprintf(os.Stderr, "::warning::runner-acl: could not release %s lease (%v): %s — "+
			"it lapses on its own within %s.\n", ip, err, strings.TrimSpace(out), runnerACLLeaseTTL)
		return
	}
	fmt.Printf("runner-acl: released %s lease from %s/%s.\n", ip, runnerACLConfigMapNS, runnerACLConfigMapName)
}

// expiredRunnerACLKeys reads the ConfigMap and returns the data keys whose lease
// has expired (or is unparseable). Best-effort: a read failure prunes nothing.
func expiredRunnerACLKeys(now time.Time) map[string]struct{} {
	out := map[string]struct{}{}
	cmOut, err := runnerACLKubectlFn("", "get", "configmap", runnerACLConfigMapName,
		"-n", runnerACLConfigMapNS, "-o", "json")
	if err != nil {
		// Swallowing this hid the first sign of an unreachable apiserver: this
		// read runs BEFORE the patch, so it is the earliest evidence available.
		// NotFound is normal on a fresh cluster and stays quiet.
		if !isNotFound(cmOut) {
			fmt.Fprintf(os.Stderr, "runner-acl: reading %s/%s for expired leases failed (%v): %s\n",
				runnerACLConfigMapNS, runnerACLConfigMapName, err, strings.TrimSpace(cmOut))
		}
		return out
	}
	var cm struct {
		Data map[string]string `json:"data"`
	}
	if json.Unmarshal([]byte(cmOut), &cm) != nil {
		return out
	}
	for key, val := range cm.Data {
		var lv runnerACLLeaseValue
		if json.Unmarshal([]byte(val), &lv) != nil {
			out[key] = struct{}{} // unparseable — prune
			continue
		}
		if exp, perr := time.Parse(time.RFC3339, lv.ExpiresAt); perr != nil || exp.Before(now) {
			out[key] = struct{}{}
		}
	}
	return out
}

// runnerACLConfigMapManifest renders a minimal ConfigMap with the given data.
// Values are JSON-encoded, which is also a safely-quoted YAML scalar.
func runnerACLConfigMapManifest(data map[string]string) string {
	var sb strings.Builder
	sb.WriteString("apiVersion: v1\nkind: ConfigMap\nmetadata:\n")
	fmt.Fprintf(&sb, "  name: %s\n  namespace: %s\ndata:\n", runnerACLConfigMapName, runnerACLConfigMapNS)
	for k, v := range data {
		b, _ := json.Marshal(v)
		fmt.Fprintf(&sb, "  %s: %s\n", k, string(b))
	}
	return sb.String()
}

func isNotFound(out string) bool {
	return strings.Contains(out, "NotFound") || strings.Contains(out, "not found")
}

func isAlreadyExists(out string) bool {
	return strings.Contains(out, "AlreadyExists") || strings.Contains(out, "already exists")
}

// runnerACLKubectlEnv is the environment the kubectl child runs with. Pure, and
// separated from the exec seam FOR THE REASON THE FILE ALREADY GIVES ABOVE:
// tests replace runnerACLKubectlFn wholesale, so anything inline in that closure
// is never exercised. Inline, the expansion is invisible — deleting it leaves the
// package green, and since leaseOutcome always returns nil the regression is silent
// end to end: `runner-acl open` reports
// success, the ConfigMap lease is never written, and the EAA controller evicts
// the runner IP mid-job.
//
// KUBECONFIG arrives unexpanded now that lke-runner-acl's `eval echo` is gone —
// eval was the one repair that could execute a caller-supplied path.
func runnerACLKubectlEnv(env []string) []string {
	return kubeconfigExpandedEnv(env)
}

// kubeconfigExpandedEnv returns env with KUBECONFIG's shell variables expanded.
// Applied to the child's environment rather than the parent's so nothing else in
// this process observes a mutated KUBECONFIG.
func kubeconfigExpandedEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if name, val, ok := strings.Cut(kv, "="); ok && name == "KUBECONFIG" {
			// FAILS OPEN, DELIBERATELY, and unlike resolvePath. An unresolvable
			// KUBECONFIG here means every lease kubectl fails — but these writes are
			// best-effort by design (see the header), so aborting the ACL open over
			// them would trade a degraded lease for no cluster access at all. The
			// unresolved text is passed through so kubectl's own error names the
			// path, rather than a silently-mangled one, and leaseOutcome turns the
			// resulting failure into a ::warning:: rather than swallowing it. The
			// delivered action's docstring states this difference from the write
			// path, which DOES fail closed.
			kv = name + "=" + expandPathVars(val, func(v string) string {
				for _, e := range env {
					if n, ev, ok := strings.Cut(e, "="); ok && n == v {
						return ev
					}
				}
				return ""
			})
		}
		out = append(out, kv)
	}
	return out
}
