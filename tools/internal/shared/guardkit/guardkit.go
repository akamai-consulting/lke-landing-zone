// Package guardkit is the scaffolding every file-scanning guard shares: WHERE its
// corpus is, and whether it actually examined one.
//
// TEN PACKAGES resolve their roots and check their corpus the same way — seven
// guards, two assertions (manifestguard, assertobs) and one lifecycle extension
// (atrest) — plus guardwalk itself. (This said "eight guards" from the
// extensions/shared split; re-counted rather than carried forward, since the
// point of the sentence is the DUPLICATION the count evidences.) While
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
// wave-health-guard calls requireCorpus too (TestWaveHealthGuardFailsOnEmptyCorpus
// pins it), but it is not a third INDEPENDENT implementation: it used to call
// walkManifests and DISCARD the examined count, which is exactly the hole this file
// closes — a tree-scanning guard that passes on an empty corpus.

package guardkit

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
)

// RepoPath resolves a repo-relative path, tolerating the template layout
// where the instance content (bootstrap workflows, apl-values) lives under
// instance-template/ rather than at the repo root.
//
// IT TAKES THE READER RATHER THAN THE ROOT, and that is the whole shape of the
// read-repo fence arriving in the guards. It used to take a root string and hand
// back a path joined onto it — which every caller then passed to os.ReadFile, so
// the "where is this tree" answer and the "may I read there" question were never
// asked in the same place. The reader is now the only thing that knows the root,
// so the returned path is REPO-RELATIVE and is only usable through the handle
// that produced it. A path that cannot be read without the fence cannot be read
// around it.
//
// The probe itself goes through the handle too, so a layout question about a
// directory outside the tree is refused rather than answered.
func RepoPath(r capability.Repo, rel string) string {
	direct := filepath.FromSlash(rel)
	if _, err := r.Stat(direct); err == nil {
		return direct
	}
	nested := filepath.Join("instance-template", filepath.FromSlash(rel))
	if _, err := r.Stat(nested); err == nil {
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
