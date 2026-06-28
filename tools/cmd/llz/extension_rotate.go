package main

// extension_rotate.go gives extensions a TokenRotator interface so a plugin that
// owns a credential plugs into the secret-rotation lifecycle (llz-secret-
// rotation.yml) the same way core rotators do. The interface erases origin: a
// declarative extension satisfies it via its rotate: manifest block (adapted by
// declRotator), and a built-in Go extension (or an existing core rotator like the
// Linode cred rotator) can satisfy it directly — the rotation lifecycle programs
// against TokenRotator, not against either. Rotate MINTS a new token; the
// framework then RE-SEEDS it through the same seed targets (OpenBao/GH env), so
// rotation reuses the Configure-phase wiring instead of reinventing it.

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// RotationResult is what a rotation produces: declared-secret-name → new value.
// The framework re-seeds each via seedSecretValue.
type RotationResult struct {
	Secrets map[string]string
}

// TokenRotator is the interface a token-owning plugin implements to join the
// rotation lifecycle.
type TokenRotator interface {
	Name() string
	Rotate(g globalOpts) (RotationResult, error)
}

// execCapture runs argv and returns its trimmed stdout — seamed so rotation
// routing unit-tests without a real mint.
var execCapture = func(argv []string) (string, error) {
	out, err := exec.Command(argv[0], argv[1:]...).Output()
	return strings.TrimSpace(string(out)), err
}

// declRotator adapts a declarative rotate: block to TokenRotator: it runs the
// argv (which mints + prints the new token) and reports it against the named
// secret.
type declRotator struct {
	name, secret string
	argv         []string
}

func (r declRotator) Name() string { return r.name }

func (r declRotator) Rotate(globalOpts) (RotationResult, error) {
	val, err := execCapture(r.argv)
	if err != nil {
		return RotationResult{}, fmt.Errorf("mint failed: %w", err)
	}
	if val == "" {
		return RotationResult{}, fmt.Errorf("rotate argv produced no token on stdout")
	}
	return RotationResult{Secrets: map[string]string{r.secret: val}}, nil
}

func runExtensionRotate(g globalOpts, root, only string) error {
	exts, err := loadEnabledExtensions(root)
	if err != nil {
		return err
	}
	matched := false
	for _, e := range exts {
		if e.Manifest.Rotate == nil || (only != "" && e.Name != only) {
			continue
		}
		matched = true
		r := declRotator{name: e.Name, secret: e.Manifest.Rotate.Secret, argv: e.Manifest.Rotate.Argv}

		// Cloud-mutating (mints a real token + writes a store) — gate + plan.
		if g.dryRun || !g.yes {
			fmt.Fprintf(os.Stderr, "→ would rotate %s: mint via %s, re-seed %q\n", e.Name, shellQuote(r.argv), r.secret)
			if !g.yes && !g.dryRun {
				fmt.Fprintln(os.Stderr, "  (re-run with --yes to rotate)")
			}
			continue
		}

		res, rerr := r.Rotate(g)
		if rerr != nil {
			return fmt.Errorf("rotate %s: %w", e.Name, rerr)
		}
		if err := reseedRotation(g, e.Manifest, res); err != nil {
			return fmt.Errorf("rotate %s: %w", e.Name, err)
		}
		fmt.Fprintf(os.Stderr, "rotated %s → re-seeded %q\n", e.Name, r.secret)
	}
	if only != "" && !matched {
		return fmt.Errorf("no enabled extension named %q declares a rotate: block", only)
	}
	if !matched {
		fmt.Fprintln(os.Stderr, "no enabled extensions implement rotate")
	}
	return nil
}

// reseedRotation writes each rotated value to its declared secret target.
func reseedRotation(g globalOpts, m extManifest, res RotationResult) error {
	for name, val := range res.Secrets {
		sec, ok := findSecret(m, name)
		if !ok {
			return fmt.Errorf("rotation produced undeclared secret %q", name)
		}
		if err := seedSecretValue(g, sec, val); err != nil {
			return err
		}
	}
	return nil
}
