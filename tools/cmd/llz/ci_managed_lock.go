package main

// ci_managed_lock.go implements `llz ci managed-fresh` — the drift guard
// specified in docs/designs/cross-org-reuse-pattern.md ("a hand-edited instance
// graph fails CI rather than silently diverging") and left unbuilt until now.
//
// WHY A HASH LOCK AND NOT A RE-RENDER: the obvious check is "render the template
// at the pinned ref and diff", but `copier` ships only in the devcontainer image —
// the reusable workflows run in ci-tofu (vars.TF_IMAGE), which has no copier
// and, on an air-gapped GHE, no route to the template repo either (ADR 0003). So
// the template instead SHIPS the expected digests in .template-managed.lock and
// the guard recomputes them locally: no network, no Python, no template checkout.
//
// WHY THAT IS SOUND: a `managed` file that carries no `<@ … @>` copier
// substitution renders byte-identically in every instance, so one digest covers
// them all. Tokenful `managed` files render per-instance and cannot be locked at
// all; they are recorded in the lock header as declared exclusions so the gap is
// visible rather than silently absent.
//
// SCOPE IS THE MANIFEST, NOT A PREFIX: this guard used to cover only `.github/`,
// which left 16 of the 31 lockable `managed` files (.tflintrc.hcl, .checkov.yaml,
// .gitleaks.toml, apl-values/values.yaml, …) overwritten by `llz upgrade` with no
// drift warning at all — the exact failure this exists to prevent, on more than
// half the surface. The lock is now a projection of .template-manifest's
// digestLocked classes, so the two cannot drift apart.

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/color"
)

// managedLockPath is the digest list, relative to the scaffold root. It is
// itself `managed`, so `llz upgrade` refreshes it alongside the files it covers.
// Named for the CLASS it locks, not a directory: it used to be
// .template-workflows.lock, scoped to .github/, and the name outlived the scope.
const managedLockPath = ".template-managed.lock"

func ciManagedFreshCmd() *cobra.Command {
	var write bool
	var root string
	c := &cobra.Command{
		Use:   "managed-fresh",
		Short: "fail when a template-owned scaffold file drifts from the template",
		Long: "Verifies every token-free file in a digest-locked class of .template-manifest\n" +
			"(today: `managed` — the vendored llz-*.yml bodies, composite actions and the\n" +
			"template-owned configs) still matches the digest the template shipped in\n" +
			managedLockPath + ". These files are template-owned: `llz upgrade` overwrites them\n" +
			"from a clean render, so a local edit is silently lost on the next bump. Failing\n" +
			"here turns that silent loss into a CI error.\n\n" +
			"Runs offline — no copier, no template checkout, no network.\n\n" +
			"--write regenerates the lock; it is for the TEMPLATE repo (CI asserts the lock\n" +
			"is current), not for instances.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runManagedFresh(root, write, os.Stdout, os.Stderr)
		},
	}
	c.Flags().BoolVar(&write, "write", false, "regenerate the lock from the scaffold (template repo only)")
	c.Flags().StringVar(&root, "root", "", "scaffold root containing .template-manifest (default: auto-detect instance-template/ or .)")
	return c
}

func runManagedFresh(root string, write bool, out, errOut io.Writer) error {
	m, err := loadTemplateManifest(root)
	if err != nil {
		return err
	}
	lockPath := filepath.Join(m.root, managedLockPath)
	if m.root == "." {
		lockPath = managedLockPath
	}

	if write {
		return writeManagedLock(m, lockPath, out)
	}

	want, err := readManagedLock(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Instances rendered before the lock existed simply have nothing to
			// check — don't fail their lint gate; the next `llz upgrade` ships one.
			fmt.Fprintf(errOut, "managed-fresh: no %s — skipping (upgrade to a template version that ships it)\n", managedLockPath)
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
		if err := checkLockComplete(m, want, errOut); err != nil {
			return err
		}
		fmt.Fprintf(out, "managed-fresh: OK — %d template-owned file(s) match %s\n", len(want), managedLockPath)
		return nil
	}

	for _, rel := range missing {
		fmt.Fprintf(errOut, "::error file=%s::template-owned file is missing\n", rel)
	}
	for _, rel := range drifted {
		fmt.Fprintf(errOut, "::error file=%s::file was edited locally — the template owns it and `llz upgrade` will overwrite it\n", rel)
	}
	fmt.Fprintf(errOut, "\n%s %d template-owned file(s) drifted from the template:\n", color.Red("✗"), len(drifted)+len(missing))
	for _, rel := range append(append([]string{}, missing...), drifted...) {
		fmt.Fprintf(errOut, "    %s\n", rel)
	}
	fmt.Fprintf(errOut, "\nThese are in a digest-locked class in .template-manifest — the template owns\n"+
		"them and the next `llz upgrade` overwrites them from a clean render, so a local\n"+
		"edit here is lost.\n"+
		"Fix by either:\n"+
		"  • reverting the edit (`llz upgrade` re-syncs them), or\n"+
		"  • sending the change upstream to the template, where it belongs.\n")
	return fmt.Errorf("managed-fresh: %d template-owned file(s) drifted", len(drifted)+len(missing))
}

// lockableFiles splits the scaffold into what the digest lock can cover and what
// it deliberately cannot. A file is lockable when its class is digestLocked (the
// manifest is the authority — there is no path prefix here) and it carries no
// copier token, since a token makes the rendered bytes per-instance.
//
// The lock file itself is excluded by construction: it is `managed`, so it would
// otherwise try to record a digest of the bytes being written.
func lockableFiles(m templateManifest, files []string) (sums map[string]string, tokenful []string, err error) {
	sums = map[string]string{}
	for _, rel := range files {
		if rel == managedLockPath {
			continue
		}
		c, ok := lookupTemplateClass(m.classify(rel))
		if !ok || !c.digestLocked {
			continue
		}
		data, err := os.ReadFile(filepath.Join(m.root, filepath.FromSlash(rel)))
		if err != nil {
			return nil, nil, fmt.Errorf("managed-fresh: read %s: %w", rel, err)
		}
		if strings.Contains(string(data), "<@") {
			tokenful = append(tokenful, rel)
			continue
		}
		sum := sha256.Sum256(data)
		sums[rel] = hex.EncodeToString(sum[:])
	}
	sort.Strings(tokenful)
	return sums, tokenful, nil
}

// checkLockComplete fails when a file the lock COULD cover is absent from it —
// the "someone added a managed file and forgot to regenerate" hole. Without it
// the verify pass only walks the lock's own keys, so an unlocked managed file is
// indistinguishable from one that does not exist, and the lock silently decays
// into a subset of the manifest instead of a projection of it.
//
// Template-repo only (gated on copier.yml next to the scaffold root, the same
// guard checkCopierFencing uses): an instance is rendered output, and holding it
// to the template's completeness invariant would fail instances whose lock simply
// predates a newly-classified file.
func checkLockComplete(m templateManifest, want map[string]string, errOut io.Writer) error {
	if !fileExists(filepath.Join(filepath.Dir(m.root), "copier.yml")) &&
		!fileExists(filepath.Join(filepath.Dir(m.root), "copier.yaml")) {
		return nil
	}
	files, err := scaffoldManifestFiles(m.root)
	if err != nil {
		return err
	}
	sums, _, err := lockableFiles(m, files)
	if err != nil {
		return err
	}
	var unlocked []string
	for _, rel := range sortedKeys(sums) {
		if _, ok := want[rel]; !ok {
			unlocked = append(unlocked, rel)
		}
	}
	if len(unlocked) == 0 {
		return nil
	}
	for _, rel := range unlocked {
		fmt.Fprintf(errOut, "::error file=%s::template-owned file is missing from %s\n", rel, managedLockPath)
	}
	fmt.Fprintf(errOut, "\n%s %d template-owned file(s) are not covered by %s:\n", color.Red("✗"), len(unlocked), managedLockPath)
	for _, rel := range unlocked {
		fmt.Fprintf(errOut, "    %s\n", rel)
	}
	fmt.Fprintf(errOut, "\n`llz upgrade` overwrites these from a clean render, so an instance's local edit\n"+
		"is silently lost — which is exactly what this lock exists to catch. Regenerate it:\n"+
		"    llz ci managed-fresh --root %s --write\n", m.root)
	return fmt.Errorf("managed-fresh: %d template-owned file(s) missing from the lock", len(unlocked))
}

// writeManagedLock regenerates the digest list from the scaffold, covering every
// token-free file in a digestLocked class. Tokenful ones cannot be locked at all
// (their rendered bytes differ per instance) and are recorded in the header as
// declared exclusions rather than dropped silently.
func writeManagedLock(m templateManifest, lockPath string, out io.Writer) error {
	files, err := scaffoldManifestFiles(m.root)
	if err != nil {
		return err
	}
	sums, tokenful, err := lockableFiles(m, files)
	if err != nil {
		return err
	}
	if len(sums) == 0 {
		return fmt.Errorf("managed-fresh: no digest-lockable files in %s — refusing to write an empty lock", m.root)
	}

	var b strings.Builder
	b.WriteString("# " + managedLockPath + " — digests of the template-owned scaffold surface.\n")
	b.WriteString("#\n")
	b.WriteString("# GENERATED by `llz ci managed-fresh --write` — do not hand-edit.\n")
	b.WriteString("# Covers every token-free file in a digest-locked class of .template-manifest\n")
	b.WriteString("# (today: `managed`). Those render byte-identically in every instance, which is\n")
	b.WriteString("# what makes one digest valid for all of them.\n")
	b.WriteString("#\n")
	b.WriteString("# `llz ci managed-fresh` (part of `llz lint`) recomputes these offline and fails\n")
	b.WriteString("# when an instance hand-edits a file the next `llz upgrade` would overwrite.\n")
	if len(tokenful) > 0 {
		b.WriteString("#\n")
		b.WriteString("# DELIBERATELY UNLOCKED — these are digest-locked-class files that carry a copier\n")
		b.WriteString("# token, so their rendered bytes differ per instance and no single digest can\n")
		b.WriteString("# cover them. `llz upgrade` still overwrites them from a clean render (which is\n")
		b.WriteString("# correct — the render substitutes each instance's own tokens); they simply get\n")
		b.WriteString("# no drift detection. Listed so the gap is auditable rather than invisible:\n")
		for _, rel := range tokenful {
			b.WriteString("#   " + rel + "\n")
		}
	}
	b.WriteString("#\n")
	b.WriteString("# FORMAT: <sha256>  <path>\n")
	for _, rel := range sortedKeys(sums) {
		fmt.Fprintf(&b, "%s  %s\n", sums[rel], rel)
	}
	if err := os.WriteFile(lockPath, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("managed-fresh: write %s: %w", lockPath, err)
	}
	fmt.Fprintf(out, "managed-fresh: wrote %s (%d file(s))\n", lockPath, len(sums))
	return nil
}

func readManagedLock(path string) (map[string]string, error) {
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
			return nil, fmt.Errorf("managed-fresh: %s:%d bad entry (expected `<sha256>  <path>`): %q", path, lineNo, line)
		}
		sums[filepath.ToSlash(parts[1])] = parts[0]
	}
	if err := s.Err(); err != nil {
		return nil, fmt.Errorf("managed-fresh: read %s: %w", path, err)
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
