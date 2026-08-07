package openbao

// go wires `llz openbao get|set|exec` over internal/openbao + kubectl.
// `get` reads one field from a cluster by HA role (read-only). `set` writes:
// for an HA pair it is the transactional dual write (active, then standby,
// rollback + hash-verify); for a standalone deployment (no OPENBAO_ADDR_STANDBY)
// it is a single write to the active. Gated by --yes. `exec` is a thin
// `kubectl exec … bao` passthrough for day-2 auth/policy admin (the
// openbao-accounts.md playbook). Both KV ops keep secret values off argv and
// ::add-mask:: them in CI. Named under `openbao` so it never collides with the
// GitHub-secrets `llz secrets` group.
//
// "Role" is the deployment's HA role: `active` (or the sole cluster of a
// standalone) and `standby`. The cluster addresses come from
// OPENBAO_ADDR_{ACTIVE,STANDBY} + OPENBAO_TOKEN_{ACTIVE,STANDBY}.
//
// ADDRESS DEFAULT — auto port-forward. OpenBao has no external ingress (all
// access is via the pods in the llz-openbao namespace), so an operator running
// `llz openbao get|set` from a laptop has no address to point at. When
// OPENBAO_ADDR_ACTIVE is unset and the deployment is a standalone (no standby
// configured), the client transparently opens an ephemeral `kubectl port-forward`
// to the leader pod — reusing the :0/announced-port idiom from prom_query.go —
// and talks to https://127.0.0.1:<port> with TLS verification skipped (a local
// loopback tunnel; the same posture every in-cluster baoExec uses). An explicitly
// set OPENBAO_ADDR_<role> always wins, so CI and HA dual-writes are unchanged.

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertobs"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/envtopology"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baoread"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghsecret"
)

// Client builds a client for an HA role from the OPENBAO_* env. Pure
// (env → client, no side effects); the auto port-forward default lives in
// ClientForward, which callers use.
func NewClientFor(role string) (*Client, error) {
	var addr, token string
	switch role {
	case envtopology.RoleActive:
		addr, token = os.Getenv("OPENBAO_ADDR_ACTIVE"), firstNonEmpty(os.Getenv("OPENBAO_TOKEN_ACTIVE"), os.Getenv("OPENBAO_TOKEN"))
	case envtopology.RoleStandby:
		addr, token = os.Getenv("OPENBAO_ADDR_STANDBY"), firstNonEmpty(os.Getenv("OPENBAO_TOKEN_STANDBY"), os.Getenv("OPENBAO_TOKEN"))
	default:
		return nil, fmt.Errorf("role must be 'active' or 'standby'; got %q", role)
	}
	if addr == "" {
		return nil, fmt.Errorf("OPENBAO_ADDR_%s is not set", strings.ToUpper(role))
	}
	if token == "" {
		return nil, fmt.Errorf("OPENBAO_TOKEN_%s (or OPENBAO_TOKEN) is not set — mint a team-scoped token with `eval \"$(llz openbao login --team <name>)\"`", strings.ToUpper(role))
	}
	return New(addr, token, os.Getenv("OPENBAO_NAMESPACE"), 30*time.Second), nil
}

// PortForwardFn opens an ephemeral kubectl port-forward to the OpenBao
// pod-0 (writes/reads request-forward to the raft leader) and returns the local
// https base URL plus a teardown func. A package var so tests can seam it
// (mirrors withPrometheus in prom_query.go).
var PortForwardFn = portForward

// ClientForward is Client plus the auto port-forward default. It
// returns a cleanup func the caller MUST defer (a no-op unless a port-forward was
// opened). When OPENBAO_ADDR_<role> is set it delegates to Client
// verbatim. Otherwise — only for the active role of a standalone deployment — it
// opens a port-forward and builds an insecure (loopback) client. A standby, or an
// active with a standby configured (an HA pair the operator addresses
// explicitly), keeps the plain env behavior and its "not set" error.
func ClientForward(role string) (*Client, func(), error) {
	noop := func() {}
	// An explicitly set address always wins — CI, HA, or a deliberate override.
	if os.Getenv("OPENBAO_ADDR_"+strings.ToUpper(role)) != "" {
		c, err := NewClientFor(role)
		return c, noop, err
	}
	// Auto-forward only the active cluster of a standalone deployment; anything
	// else keeps Client's explicit-addressing contract (and error text).
	if role != envtopology.RoleActive || standbyConfigured() {
		c, err := NewClientFor(role)
		return c, noop, err
	}
	// The port-forward supplies the address, never the token. Accept
	// OPENBAO_ROOT_TOKEN too: `llz openbao regen-root` → export it → seed is the
	// documented operator flow, so it should work with no extra env — but a
	// team-scoped token (`llz openbao login --team`) is preferred for day-2
	// reads/writes, so warn when only the root token is present.
	token := firstNonEmpty(os.Getenv("OPENBAO_TOKEN_ACTIVE"), os.Getenv("OPENBAO_TOKEN"))
	if token == "" {
		if rt := os.Getenv("OPENBAO_ROOT_TOKEN"); rt != "" {
			warnRootToken()
			token = rt
		}
	}
	if token == "" {
		return nil, noop, fmt.Errorf("no OpenBao token in env: set OPENBAO_TOKEN from `eval \"$(llz openbao login --team <name>)\"` (team-scoped, preferred) or export OPENBAO_ROOT_TOKEN — auto port-forward supplies the address but not the token")
	}
	addr, cleanup, err := PortForwardFn()
	if err != nil {
		return nil, noop, fmt.Errorf("auto port-forward to %s/%s: %w", baoread.Namespace, baoread.RootPod, err)
	}
	fmt.Fprintf(os.Stderr, "→ OPENBAO_ADDR_ACTIVE unset; port-forwarding %s/%s → %s (TLS verify skipped on loopback)\n", baoread.Namespace, baoread.RootPod, addr)
	c := NewWithClient(addr, token, os.Getenv("OPENBAO_NAMESPACE"), HTTPClientLoopback(30*time.Second))
	return c, cleanup, nil
}

// portForward runs `kubectl port-forward` to OpenBao pod-0 on a
// kubectl-chosen local port (":0"), waits for it to be announced + the tunnel to
// warm up, and returns the https base URL and a kill/reap teardown.
func portForward() (string, func(), error) {
	// Forward to the LOOPBACK listener (8210), not the mTLS network listener
	// (8200). port-forward is established inside the pod's network namespace, so
	// a 127.0.0.1-bound port is reachable — which is what lets an operator use
	// `llz openbao get/set` from a laptop that holds no client certificate.
	cmd := exec.Command("kubectl", "port-forward", "-n", baoread.Namespace, "pod/"+baoread.RootPod, ":"+baoread.LoopbackPort)
	// Surface kubectl's own stderr live: without this the common failure modes
	// (wrong kube-context, pod-0 absent, RBAC-denied on pods/portforward) are
	// swallowed and the operator only sees an opaque establish timeout. kubectl
	// writes "Forwarding from…"/"Handling connection…" to stdout, so stderr
	// carries errors alone — no normal-path noise.
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", nil, err
	}
	if err := cmd.Start(); err != nil {
		return "", nil, fmt.Errorf("kubectl port-forward: %w", err)
	}
	stop := func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }

	localPort, err := assertobs.ReadForwardPortTimeout(stdout, assertobs.ForwardEstablishTimeout)
	if err != nil {
		stop()
		return "", nil, err
	}
	// Keep draining stdout so kubectl's per-connection log lines can't fill the
	// pipe buffer and block its writer (same rationale as withPrometheus).
	go func() { _, _ = io.Copy(io.Discard, stdout) }()

	base := "https://127.0.0.1:" + localPort
	if err := warmUp(base); err != nil {
		stop()
		return "", nil, err
	}
	return base, stop, nil
}

// warmUp blocks (bounded) until the tunnel answers, so the first real KV
// call doesn't race the port-forward coming up. Any HTTP response — even a
// sealed/standby non-2xx from /v1/sys/seal-status — proves the tunnel is up.
func warmUp(base string) error {
	client := HTTPClientLoopback(5 * time.Second)
	var lastErr error
	for i := 0; i < 15; i++ {
		resp, err := client.Get(base + "/v1/sys/seal-status")
		if err == nil {
			resp.Body.Close()
			return nil
		}
		lastErr = err
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("port-forward tunnel never became ready: %w", lastErr)
}

// standbyConfigured reports whether a standby cluster is addressable — i.e. this
// is an HA pair, not a standalone deployment.
func standbyConfigured() bool { return os.Getenv("OPENBAO_ADDR_STANDBY") != "" }

// warnRootToken nudges an operator who supplied the OpenBao root token toward the
// team-scoped `llz openbao login` path. Root still works — this is a warning, not
// a block — but day-2 secret access should use a short-lived, attributed,
// least-privilege team token instead. Written to stderr so it never pollutes the
// value `get` prints to stdout, and suppressed when OPENBAO_ALLOW_ROOT is set (an
// escape hatch for genuine root-only automation that has no team identity).
func warnRootToken() {
	if os.Getenv("OPENBAO_ALLOW_ROOT") != "" {
		return
	}
	fmt.Fprintln(os.Stderr, "⚠ using the OpenBao ROOT token — prefer a team-scoped token for day-2 secret access:")
	fmt.Fprintln(os.Stderr, "    eval \"$(llz openbao login --team <name>)\"   # short-lived, attributed, least-privilege")
	fmt.Fprintln(os.Stderr, "  (set OPENBAO_ALLOW_ROOT=1 to silence this for root-only automation)")
}

func RunGet(region, path, key string) error {
	if err := ValidatePath(path); err != nil {
		return err
	}
	c, cleanup, err := ClientForward(region)
	if err != nil {
		return err
	}
	defer cleanup()
	val, ok, err := c.Get(context.Background(), path, key)
	if err != nil {
		return fmt.Errorf("read %s in %s: %w", path, region, err)
	}
	if !ok {
		return fmt.Errorf("key %q not found at %s in %s", key, path, region)
	}
	ghsecret.Mask(val)
	fmt.Print(val) // raw value to stdout; diagnostics went to stderr
	return nil
}

func RunSet(dryRun, yes bool, path string, kvPairs []string) error {
	if err := ValidatePath(path); err != nil {
		return err
	}
	data := map[string]string{}
	for _, kv := range kvPairs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return fmt.Errorf("argument must be key=value: %q", kv)
		}
		data[k] = v
		ghsecret.Mask(v)
	}
	if len(data) == 0 {
		//lint:ignore ST1005 usage string: the trailing ... is variadic-argument syntax, not sentence punctuation
		return fmt.Errorf("usage: llz openbao set <secret/path> <key=value>...")
	}

	// Standalone deployment (no standby addressable) → single write to the
	// active. An HA pair → the transactional dual write. Clients (and the auto
	// port-forward) are built only past the dry-run/--yes gate, so a dry-run
	// never opens a tunnel.
	if !standbyConfigured() {
		fmt.Fprintf(os.Stderr, "→ single-write %d key(s) to %s (standalone — no standby configured)\n", len(data), path)
		if dryRun || !yes {
			fmt.Fprintln(os.Stderr, "  (dry-run — re-run with --yes to execute the write)")
			return nil
		}
		active, cleanup, err := ClientForward(envtopology.RoleActive)
		if err != nil {
			return err
		}
		defer cleanup()
		if err := active.Write(context.Background(), path, data); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "✓ wrote %s\n", path)
		return nil
	}

	fmt.Fprintf(os.Stderr, "→ dual-write %d key(s) to %s (active + standby)\n", len(data), path)
	if dryRun || !yes {
		fmt.Fprintln(os.Stderr, "  (dry-run — re-run with --yes to execute the transactional write)")
		return nil
	}
	active, cleanup, err := ClientForward(envtopology.RoleActive)
	if err != nil {
		return err
	}
	defer cleanup()
	standby, err := NewClientFor(envtopology.RoleStandby)
	if err != nil {
		return err
	}
	if err := DualWrite(context.Background(), active, standby, path, data); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "✓ both clusters wrote %s\n", path)
	return nil
}

// ── openbao exec: kubectl-exec bao passthrough (day-2 auth/policy admin) ──────

// ── in-pod `bao` CLI: the loopback listener ──────────────────────────────────

func ExecArgv(pod, token string, args []string) []string {
	argv := []string{"-n", baoread.Namespace, "exec", "-i", "-c", "openbao", pod, "--", "env"}
	argv = append(argv, baoread.LoopbackEnv()...)
	// Both names, same reason as the address above. The chart does not set
	// BAO_TOKEN today, so VAULT_TOKEN alone happens to work — but it works by
	// luck, and the shadowing rule is the same one that broke the address.
	argv = append(argv, "BAO_TOKEN="+token, "VAULT_TOKEN="+token, "bao")
	return append(argv, args...)
}

// RunExec runs `bao <args>` in the OpenBao pod via kubectl exec, wiring
// the process stdio through so heredoc policy writes and JSON output work. The
// root token comes from OPENBAO_ROOT_TOKEN (never argv-visible to anyone but the
// in-cluster exec). Travels with the binary, so it works in an instance that
// carries no scripts/ (the openbao-accounts.md playbook used to call the
// now-retired instance-scripts/openbao/bao-exec.sh, which an instance never had).
func RunExec(dryRun bool, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: llz openbao exec <bao args...>  (e.g. llz openbao exec policy list)")
	}
	token := os.Getenv("OPENBAO_ROOT_TOKEN")
	if token == "" {
		return fmt.Errorf("OPENBAO_ROOT_TOKEN must be set (an OpenBao root/admin token for the cluster kubectl points at)")
	}
	// `exec` is genuinely root-only (auth/policy admin), but remind the operator
	// that day-2 secret reads/writes do NOT need root — `llz openbao get/set`
	// with a team-scoped token from `llz openbao login` cover those.
	warnRootToken()
	if dryRun {
		fmt.Fprintln(os.Stderr, "→ (dry-run) kubectl "+quote(ExecArgv(baoread.RootPod, "$OPENBAO_ROOT_TOKEN", args)))
		return nil
	}
	cmd := exec.Command("kubectl", ExecArgv(baoread.RootPod, token, args)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// contains is a COPY. It was defined in ci_pin_images.go, which moved to
// tools/internal/releasepublish; package main still uses it here. A three-line
// slice predicate travels by copy rather than becoming an exported symbol whose
// only job is to be reachable from both sides — the same call made for warn,
// firstNonEmpty, orAll and report.
// sliceContains is membership in a []string. Renamed from `contains` on the way
// into this package, where a test helper of the same name means SUBSTRING — two
// functions one letter apart in meaning and identical in name is how a caller
// ends up asking the wrong question and getting a plausible answer.
func sliceContains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
