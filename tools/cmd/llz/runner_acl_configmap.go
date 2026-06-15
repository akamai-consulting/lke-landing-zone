package main

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

// Seams (overridden in tests).
var (
	// runnerACLKubectlFn runs kubectl with args, piping stdin, and returns the
	// combined output. KUBECONFIG reaches kubectl through the inherited
	// environment, set by the lke-runner-acl action when a kubeconfig is wired.
	runnerACLKubectlFn = func(stdin string, args ...string) (string, error) {
		cmd := exec.Command("kubectl", args...)
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
func registerRunnerACLIP(ip string) {
	now := runnerACLNow()
	val, err := json.Marshal(runnerACLLeaseValue{
		CIDR:      ip,
		ExpiresAt: now.Add(runnerACLLeaseTTL).UTC().Format(time.RFC3339),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "::warning::runner-acl: marshaling lease for %s: %v\n", ip, err)
		return
	}

	var lastOut string
	var lastErr error
	for attempt := 1; attempt <= runnerACLPatchN; attempt++ {
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
			return
		}
		// The ConfigMap may not exist yet — create it with just this lease.
		if isNotFound(out) {
			manifest := runnerACLConfigMapManifest(map[string]string{runnerACLDataKey(ip): string(val)})
			if cout, cerr := runnerACLKubectlFn(manifest, "apply", "-f", "-"); cerr == nil {
				fmt.Printf("runner-acl: created %s/%s with %s lease.\n", runnerACLConfigMapNS, runnerACLConfigMapName, ip)
				return
			} else if !isAlreadyExists(cout) {
				lastOut, lastErr = cout, cerr
			} else {
				lastOut, lastErr = out, perr // raced a creator; retry the patch
			}
		} else {
			lastOut, lastErr = out, perr
		}
		if attempt < runnerACLPatchN {
			runnerACLSleep(runnerACLPatchGap)
		}
	}
	fmt.Fprintf(os.Stderr, "::warning::runner-acl: could not lease %s after %d tries (%v): %s — "+
		"a controller reconcile may evict this runner before the job finishes.\n",
		ip, runnerACLPatchN, lastErr, strings.TrimSpace(lastOut))
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
