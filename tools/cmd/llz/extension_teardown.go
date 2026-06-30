package main

// extension_teardown.go implements the Decommission phase — the inverse arc the
// lifecycle lacked (issue #10). Enable scaffolds files and seed wires secrets; nothing
// undid either, so a disabled extension orphaned both. `teardown` removes the files an
// extension owns (per the lock) and `unseed` revokes the secrets it seeded — the
// inverses of scaffold (files) and seed. Both are gated day-2 Actions: cloud/repo
// mutating, --yes required, never fired by reconcile.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// runExtensionTeardown removes the scaffolded files an extension owns (per the lock)
// and clears its lock entry — the inverse of `extension apply`. With no [only] it tears
// down every extension's outputs; pass a name for one. Gated: --dry-run / no --yes
// prints the plan and removes nothing.
func runExtensionTeardown(g globalOpts, root, only string, force bool) error {
	lock := loadExtLock(root)
	if len(lock.Outputs) == 0 {
		fmt.Fprintln(os.Stderr, "no scaffolded files recorded in the lock (nothing to tear down)")
		return nil
	}

	exts := make([]string, 0, len(lock.Outputs))
	for ext := range lock.Outputs {
		if only != "" && ext != only {
			continue
		}
		exts = append(exts, ext)
	}
	sort.Strings(exts) // deterministic plan + removal order
	if only != "" && len(exts) == 0 {
		return fmt.Errorf("no extension named %q owns scaffolded files", only)
	}

	// Dependency-aware (hookDeps): a check/validate/ci hook CONSUMES files. Removing an
	// extension's files while it is still enabled breaks its own live hooks, so refuse —
	// the inverse arc is disable → teardown. --force overrides.
	if !force {
		dependents := hookKindsDependingOn(HookFiles)
		enabled, _ := loadEnabledExtensions(root)
		live := map[string]extManifest{}
		for _, e := range enabled {
			live[e.Name] = e.Manifest
		}
		for _, ext := range exts {
			m, isLive := live[ext]
			if !isLive {
				continue
			}
			for _, k := range dependents {
				if manifestDeclaresHook(m, k) {
					return fmt.Errorf("extension %q is still enabled and its %s hook consumes these files — disable it first (`llz extension disable %s`) or pass --force", ext, k, ext)
				}
			}
		}
	}

	if !proceedGated(g, ActionTeardown) {
		for _, ext := range exts {
			for _, f := range lock.Outputs[ext] {
				fmt.Fprintf(os.Stderr, "→ would remove %s (%s)\n", f.Path, ext)
			}
		}
		if !g.yes && !g.dryRun {
			fmt.Fprintln(os.Stderr, "  (re-run with --yes to remove the files)")
		}
		return nil
	}

	for _, ext := range exts {
		for _, f := range lock.Outputs[ext] {
			p := filepath.Join(root, f.Path)
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "warning: remove %s (%s): %v\n", f.Path, ext, err)
				continue
			}
			fmt.Fprintf(os.Stderr, "removed %s (%s)\n", f.Path, ext)
		}
		delete(lock.Outputs, ext)
	}
	return saveExtLock(root, lock)
}

// runExtensionUnseed revokes the secrets an extension seeded — the inverse of seed,
// closing the disable→orphaned-credential gap. GitHub Environment secrets are deleted
// (reusing the same gh machinery `llz ci clear-cluster-secrets` uses). OpenBao
// `bao:` targets are NOT deleted automatically: a path may hold sibling keys, so a
// blind delete is unsafe — the exact per-key removal is printed for the operator to
// run. Gated: --dry-run / no --yes prints the plan and deletes nothing.
func runExtensionUnseed(g globalOpts, root, only string) error {
	exts, err := loadEnabledExtensions(root)
	if err != nil {
		return err
	}

	type ghDel struct{ ext, env, name string }
	var ghDels []ghDel
	var baoManual []string
	for _, e := range exts {
		if only != "" && e.Name != only {
			continue
		}
		vals := varValues(e.Manifest, os.Getenv) // resolve <@ .var @> targets, same as seed
		for _, s := range e.Manifest.Secrets {
			s = resolveSecretTargets(s, vals)
			if s.GHEnv != "" {
				ghDels = append(ghDels, ghDel{e.Name, s.GHEnv, s.Name})
			}
			if s.Bao != "" {
				if path, key, perr := parseBaoTarget(s.Bao); perr == nil {
					baoManual = append(baoManual, fmt.Sprintf("llz openbao exec kv patch -remove=%s %s   # %s (extension %q)", key, path, s.Name, e.Name))
				}
			}
		}
	}
	if only != "" && len(ghDels) == 0 && len(baoManual) == 0 {
		return fmt.Errorf("no enabled extension named %q declares a seeded secret target", only)
	}
	if len(ghDels) == 0 && len(baoManual) == 0 {
		fmt.Fprintln(os.Stderr, "no seeded secret targets to revoke")
		return nil
	}

	if !proceedGated(g, ActionUnseed) {
		for _, d := range ghDels {
			fmt.Fprintf(os.Stderr, "→ would delete GH env secret %s/%s (%s)\n", d.env, d.name, d.ext)
		}
		for _, m := range baoManual {
			fmt.Fprintf(os.Stderr, "→ OpenBao (manual — shared-path safe): %s\n", m)
		}
		if !g.yes && !g.dryRun {
			fmt.Fprintln(os.Stderr, "  (re-run with --yes to delete the GH env secrets; OpenBao removals stay manual)")
		}
		return nil
	}

	var firstErr error
	for _, d := range ghDels {
		if err := ghDeleteSecretFn(d.name, d.env); err != nil {
			fmt.Fprintf(os.Stderr, "warning: delete %s/%s (%s): %v\n", d.env, d.name, d.ext, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		fmt.Fprintf(os.Stderr, "deleted GH env secret %s/%s (%s)\n", d.env, d.name, d.ext)
	}
	if len(baoManual) > 0 {
		fmt.Fprintln(os.Stderr, "\nOpenBao removals are not automated (a bao path may hold sibling keys). Run manually:")
		for _, m := range baoManual {
			fmt.Fprintf(os.Stderr, "  %s\n", m)
		}
	}
	return firstErr
}
