package main

// extension_seed.go completes the Configure phase: `llz extension seed` WIRES an
// extension's declared secrets into their stores, reusing the existing secret
// machinery (`llz openbao set` / `gh secret set`) rather than reinventing it. The
// value is read from the environment at seed time — never stored in the manifest,
// the repo, or printed. Cloud-mutating, so it is gated (--yes); --dry-run / no
// --yes prints the plan (targets only) and writes nothing. A secret with no
// bao:/ghEnv: target stays declare-only (doctor still checks its presence).

import (
	"fmt"
	"os"
	"strings"
)

// Seamed writers default to the existing llz machinery; tests stub them.
var seedBaoFn = func(g globalOpts, path, key, value string) error {
	return runOpenbaoSet(g, path, []string{key + "=" + value})
}
var seedGHEnvFn = func(_ globalOpts, env, name, value string) error {
	return ghSecretSetStdin(name, env, value)
}

// seedSecretValue writes value to sec's declared target(s) — the per-secret write
// shared by `seed` and rotation's re-seed.
func seedSecretValue(g globalOpts, sec extSecret, value string) error {
	if sec.Bao != "" {
		path, key, err := parseBaoTarget(sec.Bao)
		if err != nil {
			return err
		}
		if err := seedBaoFn(g, path, key, value); err != nil {
			return err
		}
	}
	if sec.GHEnv != "" {
		if err := seedGHEnvFn(g, sec.GHEnv, sec.Name, value); err != nil {
			return err
		}
	}
	return nil
}

// parseBaoTarget splits an OpenBao "path#key" target.
func parseBaoTarget(s string) (path, key string, err error) {
	p, k, ok := strings.Cut(s, "#")
	if !ok || p == "" || k == "" {
		return "", "", fmt.Errorf("bao target %q must be \"path#key\"", s)
	}
	return p, k, nil
}

// seedAction is one resolved write — display fields plus the closure that does it.
// The value is captured but never put in target (which is shown to the operator).
type seedAction struct {
	ext, kind, target string
	do                func(globalOpts) error
}

func runExtensionSeed(g globalOpts, root string) error {
	exts, err := loadEnabledExtensions(root)
	if err != nil {
		return err
	}
	var actions []seedAction
	for _, e := range exts {
		for _, s := range e.Manifest.Secrets {
			if s.Bao == "" && s.GHEnv == "" {
				continue // declare-only secret — doctor checks it, seed leaves it
			}
			val := os.Getenv(s.Name)
			if val == "" {
				if s.Required {
					return fmt.Errorf("required secret %s (extension %q) is unset — cannot seed", s.Name, e.Name)
				}
				fmt.Fprintf(os.Stderr, "skip %s (extension %q): unset (optional)\n", s.Name, e.Name)
				continue
			}
			if s.Bao != "" {
				path, key, perr := parseBaoTarget(s.Bao)
				if perr != nil {
					return fmt.Errorf("extension %q secret %s: %w", e.Name, s.Name, perr)
				}
				p, k, v := path, key, val
				actions = append(actions, seedAction{e.Name, "openbao", p + "#" + k,
					func(g globalOpts) error { return seedBaoFn(g, p, k, v) }})
			}
			if s.GHEnv != "" {
				env, name, v := s.GHEnv, s.Name, val
				actions = append(actions, seedAction{e.Name, "gh-env", env + "/" + name,
					func(g globalOpts) error { return seedGHEnvFn(g, env, name, v) }})
			}
		}
	}
	if len(actions) == 0 {
		fmt.Fprintln(os.Stderr, "no seedable secrets (declare-only, or no targets set)")
		return nil
	}

	// Cloud-mutating — the gate is driven by ActionSeed.Gated in the registry, so the
	// plan is reviewable before any write.
	if !proceedGated(g, ActionSeed) {
		for _, a := range actions {
			fmt.Fprintf(os.Stderr, "→ would seed %s → %s (%s)\n", a.ext, a.target, a.kind)
		}
		if !g.yes && !g.dryRun {
			fmt.Fprintln(os.Stderr, "  (re-run with --yes to write)")
		}
		return nil
	}
	for _, a := range actions {
		if err := a.do(g); err != nil {
			return fmt.Errorf("seed %s → %s: %w", a.ext, a.target, err)
		}
		fmt.Fprintf(os.Stderr, "seeded %s → %s\n", a.ext, a.target)
	}
	return nil
}
