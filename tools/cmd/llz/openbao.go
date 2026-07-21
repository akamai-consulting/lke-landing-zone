package main

// openbao.go wires `llz openbao get|set|exec` over internal/openbao + kubectl.
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

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/openbao"
)

// openbaoClient builds a client for an HA role from the OPENBAO_* env. Pure
// (env → client, no side effects); the auto port-forward default lives in
// openbaoClientForward, which callers use.
func openbaoClient(role string) (*openbao.Client, error) {
	var addr, token string
	switch role {
	case roleActive:
		addr, token = os.Getenv("OPENBAO_ADDR_ACTIVE"), firstNonEmpty(os.Getenv("OPENBAO_TOKEN_ACTIVE"), os.Getenv("OPENBAO_TOKEN"))
	case roleStandby:
		addr, token = os.Getenv("OPENBAO_ADDR_STANDBY"), firstNonEmpty(os.Getenv("OPENBAO_TOKEN_STANDBY"), os.Getenv("OPENBAO_TOKEN"))
	default:
		return nil, fmt.Errorf("role must be 'active' or 'standby'; got %q", role)
	}
	if addr == "" {
		return nil, fmt.Errorf("OPENBAO_ADDR_%s is not set", strings.ToUpper(role))
	}
	if token == "" {
		return nil, fmt.Errorf("OPENBAO_TOKEN_%s (or OPENBAO_TOKEN) is not set", strings.ToUpper(role))
	}
	return openbao.New(addr, token, os.Getenv("OPENBAO_NAMESPACE"), 30*time.Second), nil
}

// portForwardOpenbaoFn opens an ephemeral kubectl port-forward to the OpenBao
// pod-0 (writes/reads request-forward to the raft leader) and returns the local
// https base URL plus a teardown func. A package var so tests can seam it
// (mirrors withPrometheus in prom_query.go).
var portForwardOpenbaoFn = portForwardOpenbao

// openbaoClientForward is openbaoClient plus the auto port-forward default. It
// returns a cleanup func the caller MUST defer (a no-op unless a port-forward was
// opened). When OPENBAO_ADDR_<role> is set it delegates to openbaoClient
// verbatim. Otherwise — only for the active role of a standalone deployment — it
// opens a port-forward and builds an insecure (loopback) client. A standby, or an
// active with a standby configured (an HA pair the operator addresses
// explicitly), keeps the plain env behavior and its "not set" error.
func openbaoClientForward(role string) (*openbao.Client, func(), error) {
	noop := func() {}
	// An explicitly set address always wins — CI, HA, or a deliberate override.
	if os.Getenv("OPENBAO_ADDR_"+strings.ToUpper(role)) != "" {
		c, err := openbaoClient(role)
		return c, noop, err
	}
	// Auto-forward only the active cluster of a standalone deployment; anything
	// else keeps openbaoClient's explicit-addressing contract (and error text).
	if role != roleActive || standbyConfigured() {
		c, err := openbaoClient(role)
		return c, noop, err
	}
	// The port-forward supplies the address, never the token. Accept
	// OPENBAO_ROOT_TOKEN too: `llz openbao regen-root` → export it → seed is the
	// documented operator flow, so it should work with no extra env.
	token := firstNonEmpty(os.Getenv("OPENBAO_TOKEN_ACTIVE"), os.Getenv("OPENBAO_TOKEN"), os.Getenv("OPENBAO_ROOT_TOKEN"))
	if token == "" {
		return nil, noop, fmt.Errorf("no OpenBao token in env: set OPENBAO_TOKEN (or OPENBAO_ROOT_TOKEN) — auto port-forward supplies the address but not the token")
	}
	addr, cleanup, err := portForwardOpenbaoFn()
	if err != nil {
		return nil, noop, fmt.Errorf("auto port-forward to %s/%s: %w", openbaoNS, rootOpenbaoPod, err)
	}
	fmt.Fprintf(os.Stderr, "→ OPENBAO_ADDR_ACTIVE unset; port-forwarding %s/%s → %s (TLS verify skipped on loopback)\n", openbaoNS, rootOpenbaoPod, addr)
	c := openbao.NewWithClient(addr, token, os.Getenv("OPENBAO_NAMESPACE"), openbao.HTTPClientInsecure(30*time.Second))
	return c, cleanup, nil
}

// portForwardOpenbao runs `kubectl port-forward` to OpenBao pod-0 on a
// kubectl-chosen local port (":0"), waits for it to be announced + the tunnel to
// warm up, and returns the https base URL and a kill/reap teardown.
func portForwardOpenbao() (string, func(), error) {
	cmd := exec.Command("kubectl", "port-forward", "-n", openbaoNS, "pod/"+rootOpenbaoPod, ":8200")
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

	localPort, err := readForwardPortTimeout(stdout, forwardEstablishTimeout)
	if err != nil {
		stop()
		return "", nil, err
	}
	// Keep draining stdout so kubectl's per-connection log lines can't fill the
	// pipe buffer and block its writer (same rationale as withPrometheus).
	go func() { _, _ = io.Copy(io.Discard, stdout) }()

	base := "https://127.0.0.1:" + localPort
	if err := warmUpOpenbao(base); err != nil {
		stop()
		return "", nil, err
	}
	return base, stop, nil
}

// warmUpOpenbao blocks (bounded) until the tunnel answers, so the first real KV
// call doesn't race the port-forward coming up. Any HTTP response — even a
// sealed/standby non-2xx from /v1/sys/seal-status — proves the tunnel is up.
func warmUpOpenbao(base string) error {
	client := openbao.HTTPClientInsecure(5 * time.Second)
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

// maskGHA emits ::add-mask:: for a value when running in GitHub Actions.
func maskGHA(v string) {
	if os.Getenv("GITHUB_ACTIONS") != "" && v != "" {
		fmt.Printf("::add-mask::%s\n", v)
	}
}

func runOpenbaoGet(region, path, key string) error {
	if err := openbao.ValidatePath(path); err != nil {
		return err
	}
	c, cleanup, err := openbaoClientForward(region)
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
	maskGHA(val)
	fmt.Print(val) // raw value to stdout; diagnostics went to stderr
	return nil
}

func runOpenbaoSet(g globalOpts, path string, kvPairs []string) error {
	if err := openbao.ValidatePath(path); err != nil {
		return err
	}
	data := map[string]string{}
	for _, kv := range kvPairs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return fmt.Errorf("argument must be key=value: %q", kv)
		}
		data[k] = v
		maskGHA(v)
	}
	if len(data) == 0 {
		return fmt.Errorf("usage: llz openbao set <secret/path> <key=value>...")
	}

	// Standalone deployment (no standby addressable) → single write to the
	// active. An HA pair → the transactional dual write. Clients (and the auto
	// port-forward) are built only past the dry-run/--yes gate, so a dry-run
	// never opens a tunnel.
	if !standbyConfigured() {
		fmt.Fprintf(os.Stderr, "→ single-write %d key(s) to %s (standalone — no standby configured)\n", len(data), path)
		if g.dryRun || !g.yes {
			fmt.Fprintln(os.Stderr, "  (dry-run — re-run with --yes to execute the write)")
			return nil
		}
		active, cleanup, err := openbaoClientForward(roleActive)
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
	if g.dryRun || !g.yes {
		fmt.Fprintln(os.Stderr, "  (dry-run — re-run with --yes to execute the transactional write)")
		return nil
	}
	active, cleanup, err := openbaoClientForward(roleActive)
	if err != nil {
		return err
	}
	defer cleanup()
	standby, err := openbaoClient(roleStandby)
	if err != nil {
		return err
	}
	if err := openbao.DualWrite(context.Background(), active, standby, path, data); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "✓ both clusters wrote %s\n", path)
	return nil
}

// ── openbao exec: kubectl-exec bao passthrough (day-2 auth/policy admin) ──────

// rootOpenbaoPod is the pod `llz openbao exec` targets — fixed, as the
// retired bao-exec.sh was. Writes are forwarded to the raft leader
// by OpenBao's standby request-forwarding, so pod-0 is fine for day-2 admin.
const rootOpenbaoPod = "platform-openbao-0"

// baoExecArgv builds the kubectl argv that runs `bao <args>` inside the openbao
// container of pod with the standard VAULT_* env (token included). Pure, so the
// argv shape + token placement are unit-tested.
func baoExecArgv(pod, token string, args []string) []string {
	argv := []string{"-n", openbaoNS, "exec", "-i", "-c", "openbao", pod, "--",
		"env", "VAULT_ADDR=https://127.0.0.1:8200", "VAULT_SKIP_VERIFY=true", "VAULT_TOKEN=" + token, "bao"}
	return append(argv, args...)
}

// runOpenbaoExec runs `bao <args>` in the OpenBao pod via kubectl exec, wiring
// the process stdio through so heredoc policy writes and JSON output work. The
// root token comes from OPENBAO_ROOT_TOKEN (never argv-visible to anyone but the
// in-cluster exec). Travels with the binary, so it works in an instance that
// carries no scripts/ (the openbao-accounts.md playbook used to call the
// now-retired instance-scripts/openbao/bao-exec.sh, which an instance never had).
func runOpenbaoExec(g globalOpts, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: llz openbao exec <bao args...>  (e.g. llz openbao exec policy list)")
	}
	token := os.Getenv("OPENBAO_ROOT_TOKEN")
	if token == "" {
		return fmt.Errorf("OPENBAO_ROOT_TOKEN must be set (an OpenBao root/admin token for the cluster kubectl points at)")
	}
	if g.dryRun {
		fmt.Fprintln(os.Stderr, "→ (dry-run) kubectl "+shellQuote(baoExecArgv(rootOpenbaoPod, "$OPENBAO_ROOT_TOKEN", args)))
		return nil
	}
	cmd := exec.Command("kubectl", baoExecArgv(rootOpenbaoPod, token, args)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
