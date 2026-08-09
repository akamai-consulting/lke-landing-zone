// Package guardkit is the scaffolding every file-scanning guard shares: WHERE its
// corpus is, and whether it actually examined one.
//
// Eight guards resolve their roots and check their corpus the same way, and while
// both rules lived in package main each was one file's private helper that the
// others happened to call. That is the shape this repo keeps paying for — two
// copies of a rule agreeing by accident — so the extraction of `posture-at-rest`
// (the first guard to move out) turned them into a package rather than duplicating
// them.
//
// RepoPath answers "where is this tree, in either layout". RequireCorpus answers
// "did the walk see anything at all".
//
// The manifest guards walk a set of roots and report what they find. Each one
// skipped a root that did not exist (`os.Stat` → `continue`), which is sensible
// per-root but means a guard whose corpus is entirely absent walks zero files,
// finds zero problems, and exits 0 — reporting the same green as a guard that
// examined everything and found nothing wrong.
//
// That is not hypothetical. monitoring-label-guard's whole reason for existing
// (the openbao ServiceMonitor renders its `prometheus: system` label from
// serviceMonitor.selectorLabels, so only the RENDERED tree shows the real value)
// lives under rendered/ — and rendered/ not being built is exactly the skipped
// case. The Makefile prereq protects it only when it is invoked via make.
//
// These guards run in template-repo CI (`make lint-k8s`), never in an instance,
// so there is no layout where an empty corpus is legitimate. It always means the
// tree was not rendered or the paths moved — both of which should be loud.
//
// Two guards already fail closed this way (argocd-rendered-apps, check-coverage);
// this is that behavior, shared.
//
// This comment used to name wave-health-guard as a third — it was not one. That
// guard called walkManifests and DISCARDED the examined count, so it was the one
// tree-scanning guard that still passed on an empty corpus, which is precisely
// the hole this file exists to close. It now calls requireCorpus like its
// siblings (see TestWaveHealthGuardFailsOnEmptyCorpus).

package guardkit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RepoPath resolves a repo-relative path, tolerating the template layout
// where the instance content (bootstrap workflows, apl-values) lives under
// instance-template/ rather than at the repo root.
func RepoPath(root, rel string) string {
	direct := filepath.Join(root, filepath.FromSlash(rel))
	if _, err := os.Stat(direct); err == nil {
		return direct
	}
	nested := filepath.Join(root, "instance-template", filepath.FromSlash(rel))
	if _, err := os.Stat(nested); err == nil {
		return nested
	}
	return direct
}

// RequireCorpus fails when a guard examined no files at all. Call it after the
// walk, with the number of files actually read and the roots that were searched.
func RequireCorpus(guard string, examined int, dirs []string) error {
	if examined > 0 {
		return nil
	}
	return fmt.Errorf("%s: examined 0 manifest files under %s — refusing to pass on an empty corpus. "+
		"A guard that had nothing to check reports the same green as one that checked everything, "+
		"so this fails instead. Run `make render-charts` if a rendered tree is expected, or update the "+
		"guard's roots if the manifests moved", guard, strings.Join(dirs, ", "))
}
