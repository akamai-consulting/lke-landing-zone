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

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/openbao"
)

// openbaoClient builds a client for an HA role from the OPENBAO_* env.
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
	c, err := openbaoClient(region)
	if err != nil {
		return err
	}
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

	active, err := openbaoClient(roleActive)
	if err != nil {
		return err
	}

	// Standalone deployment (no standby addressable) → single write to the
	// active. An HA pair → the transactional dual write.
	if !standbyConfigured() {
		fmt.Fprintf(os.Stderr, "→ single-write %d key(s) to %s (standalone — no standby configured)\n", len(data), path)
		if g.dryRun || !g.yes {
			fmt.Fprintln(os.Stderr, "  (dry-run — re-run with --yes to execute the write)")
			return nil
		}
		if err := active.Write(context.Background(), path, data); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "✓ wrote %s\n", path)
		return nil
	}

	standby, err := openbaoClient(roleStandby)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "→ dual-write %d key(s) to %s (active + standby)\n", len(data), path)
	if g.dryRun || !g.yes {
		fmt.Fprintln(os.Stderr, "  (dry-run — re-run with --yes to execute the transactional write)")
		return nil
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
