package main

// ci_workflows_fresh.go implements `llz ci workflows-fresh` — the drift guard
// specified in docs/designs/cross-org-reuse-pattern.md ("a hand-edited instance
// graph fails CI rather than silently diverging") and left unbuilt until now.
//
// WHY A HASH LOCK AND NOT A RE-RENDER: the obvious check is "render the template
// at the pinned ref and diff", but `copier` ships only in the devcontainer image —
// the reusable workflows run in ci-terraform (vars.TF_IMAGE), which has no copier
// and, on an air-gapped GHE, no route to the template repo either (ADR 0003). So
// the template instead SHIPS the expected digests in .template-workflows.lock and
// the guard recomputes them locally: no network, no Python, no template checkout.
//
// WHY THAT IS SOUND: every `managed` file under .github/ is token-free (no
// `<@ … @>` copier substitutions — asserted at --write time), so the rendered
// instance bytes are byte-identical to the template source bytes. That is exactly
// the property that also lets these files be `managed` rather than `merge`
// (.template-manifest), so the two decisions stand or fall together.

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// vendoredLockPath is the digest list, relative to the scaffold root. It is
// itself `managed`, so `llz upgrade` refreshes it alongside the files it covers.
const vendoredLockPath = ".template-workflows.lock"

// vendoredLockScope limits the guard to the vendored CI surface (.github/**):
// the llz-*.yml reusable bodies and the composite actions. Those are the files an
// instance carries verbatim and must never hand-edit.
const vendoredLockScope = ".github/"

func ciWorkflowsFreshCmd() *cobra.Command {
	var write bool
	var root string
	c := &cobra.Command{
		Use:   "workflows-fresh",
		Short: "fail when a vendored .github/ file drifts from the template",
		Long: "Verifies every `managed` file under .github/ (the vendored llz-*.yml reusable\n" +
			"bodies and composite actions) still matches the digest the template shipped in\n" +
			vendoredLockPath + ". These files are template-owned: `llz upgrade` overwrites them\n" +
			"from a clean render, so a local edit is silently lost on the next bump. Failing\n" +
			"here turns that silent loss into a CI error.\n\n" +
			"Runs offline — no copier, no template checkout, no network.\n\n" +
			"--write regenerates the lock; it is for the TEMPLATE repo (CI asserts the lock\n" +
			"is current), not for instances.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runWorkflowsFresh(root, write, os.Stdout, os.Stderr)
		},
	}
	c.Flags().BoolVar(&write, "write", false, "regenerate the lock from the scaffold (template repo only)")
	c.Flags().StringVar(&root, "root", "", "scaffold root containing .template-manifest (default: auto-detect instance-template/ or .)")
	return c
}

func runWorkflowsFresh(root string, write bool, out, errOut io.Writer) error {
	m, err := loadTemplateManifest(root)
	if err != nil {
		return err
	}
	lockPath := filepath.Join(m.root, vendoredLockPath)
	if m.root == "." {
		lockPath = vendoredLockPath
	}

	if write {
		return writeVendoredLock(m, lockPath, out)
	}

	want, err := readVendoredLock(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Instances rendered before the lock existed simply have nothing to
			// check — don't fail their lint gate; the next `llz upgrade` ships one.
			fmt.Fprintf(errOut, "workflows-fresh: no %s — skipping (upgrade to a template version that ships it)\n", vendoredLockPath)
			return nil
		}
		return err
	}

	var drifted, missing []string
	for _, rel := range sortedKeys(want) {
		sum, err := sha256File(filepath.Join(m.root, filepath.FromSlash(rel)))
		if err != nil {
			missing = append(missing, rel)
			continue
		}
		if sum != want[rel] {
			drifted = append(drifted, rel)
		}
	}
	if len(drifted) == 0 && len(missing) == 0 {
		fmt.Fprintf(out, "workflows-fresh: OK — %d vendored file(s) match %s\n", len(want), vendoredLockPath)
		return nil
	}

	for _, rel := range missing {
		fmt.Fprintf(errOut, "::error file=%s::vendored template file is missing\n", rel)
	}
	for _, rel := range drifted {
		fmt.Fprintf(errOut, "::error file=%s::vendored template file was edited locally — it is template-owned (`managed`) and `llz upgrade` will overwrite it\n", rel)
	}
	fmt.Fprintf(errOut, "\n%s %d vendored file(s) drifted from the template:\n", red("✗"), len(drifted)+len(missing))
	for _, rel := range append(append([]string{}, missing...), drifted...) {
		fmt.Fprintf(errOut, "    %s\n", rel)
	}
	fmt.Fprintf(errOut, "\nThese are `managed` in .template-manifest — the template owns them and the next\n"+
		"`llz upgrade` overwrites them from a clean render, so a local edit here is lost.\n"+
		"Fix by either:\n"+
		"  • reverting the edit (`llz upgrade` re-syncs them), or\n"+
		"  • sending the change upstream to the template, where it belongs.\n")
	return fmt.Errorf("workflows-fresh: %d vendored file(s) drifted", len(drifted)+len(missing))
}

// writeVendoredLock regenerates the digest list from the scaffold. It refuses to
// lock a file carrying a copier token: a token means the rendered bytes differ
// per instance, so the digest could never match and the file belongs in `merge`
// rather than `managed`. That check is what keeps this guard honest as the
// scaffold evolves.
func writeVendoredLock(m templateManifest, lockPath string, out io.Writer) error {
	files, err := scaffoldManifestFiles(m.root)
	if err != nil {
		return err
	}
	sums := map[string]string{}
	var tokenful []string
	for _, rel := range files {
		if !strings.HasPrefix(rel, vendoredLockScope) || m.classify(rel) != "managed" {
			continue
		}
		abs := filepath.Join(m.root, filepath.FromSlash(rel))
		data, err := os.ReadFile(abs)
		if err != nil {
			return fmt.Errorf("workflows-fresh: read %s: %w", rel, err)
		}
		if strings.Contains(string(data), "<@") {
			tokenful = append(tokenful, rel)
			continue
		}
		sum := sha256.Sum256(data)
		sums[rel] = hex.EncodeToString(sum[:])
	}
	if len(tokenful) > 0 {
		for _, rel := range tokenful {
			fmt.Fprintf(os.Stderr, "  - %s\n", rel)
		}
		return fmt.Errorf("workflows-fresh: %d `managed` file(s) under %s carry a copier token — "+
			"their rendered bytes differ per instance, so they cannot be digest-locked; "+
			"reclassify them as `merge` in .template-manifest", len(tokenful), vendoredLockScope)
	}
	if len(sums) == 0 {
		return fmt.Errorf("workflows-fresh: no `managed` files under %s in %s — refusing to write an empty lock", vendoredLockScope, m.root)
	}

	var b strings.Builder
	b.WriteString("# .template-workflows.lock — digests of the vendored, template-owned CI surface.\n")
	b.WriteString("#\n")
	b.WriteString("# GENERATED by `llz ci workflows-fresh --write` — do not hand-edit.\n")
	b.WriteString("# Covers every `managed` file under .github/ (the llz-*.yml reusable bodies and\n")
	b.WriteString("# the composite actions). All are token-free, so an instance's rendered bytes are\n")
	b.WriteString("# byte-identical to the template's — which is what makes this digest check valid.\n")
	b.WriteString("#\n")
	b.WriteString("# `llz ci workflows-fresh` (part of `llz lint`) recomputes these offline and fails\n")
	b.WriteString("# when an instance hand-edits a file the next `llz upgrade` would overwrite.\n")
	b.WriteString("#\n")
	b.WriteString("# FORMAT: <sha256>  <path>\n")
	for _, rel := range sortedKeys(sums) {
		fmt.Fprintf(&b, "%s  %s\n", sums[rel], rel)
	}
	if err := os.WriteFile(lockPath, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("workflows-fresh: write %s: %w", lockPath, err)
	}
	fmt.Fprintf(out, "workflows-fresh: wrote %s (%d file(s))\n", lockPath, len(sums))
	return nil
}

func readVendoredLock(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sums := map[string]string{}
	s := bufio.NewScanner(f)
	lineNo := 0
	for s.Scan() {
		lineNo++
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 2 || len(parts[0]) != 64 {
			return nil, fmt.Errorf("workflows-fresh: %s:%d bad entry (expected `<sha256>  <path>`): %q", path, lineNo, line)
		}
		sums[filepath.ToSlash(parts[1])] = parts[0]
	}
	if err := s.Err(); err != nil {
		return nil, fmt.Errorf("workflows-fresh: read %s: %w", path, err)
	}
	return sums, nil
}

func sha256File(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
