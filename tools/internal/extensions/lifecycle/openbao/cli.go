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
	"os"
	"os/exec"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baoread"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/envtopology"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghsecret"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/openbao"
)

// openbao.PortForwardFn opens an ephemeral kubectl port-forward to the OpenBao
// pod-0 (writes/reads request-forward to the raft leader) and returns the local
// https base URL plus a teardown func. A package var so tests can seam it
// (mirrors withPrometheus in prom_query.go).

func RunGet(region, path, key string) error {
	if err := openbao.ValidatePath(path); err != nil {
		return err
	}
	c, cleanup, err := openbao.ClientForward(region)
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
	if !openbao.StandbyConfigured() {
		fmt.Fprintf(os.Stderr, "→ single-write %d key(s) to %s (standalone — no standby configured)\n", len(data), path)
		if dryRun || !yes {
			fmt.Fprintln(os.Stderr, "  (dry-run — re-run with --yes to execute the write)")
			return nil
		}
		active, cleanup, err := openbao.ClientForward(envtopology.RoleActive)
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
	active, cleanup, err := openbao.ClientForward(envtopology.RoleActive)
	if err != nil {
		return err
	}
	defer cleanup()
	standby, err := openbao.NewClientFor(envtopology.RoleStandby)
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

// ── in-pod `bao` CLI: the loopback listener ──────────────────────────────────

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
	openbao.WarnRootToken()
	if dryRun {
		fmt.Fprintln(os.Stderr, "→ (dry-run) kubectl "+quote(openbao.ExecArgv(baoread.RootPod, "$OPENBAO_ROOT_TOKEN", args)))
		return nil
	}
	cmd := exec.Command("kubectl", openbao.ExecArgv(baoread.RootPod, token, args)...)
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
