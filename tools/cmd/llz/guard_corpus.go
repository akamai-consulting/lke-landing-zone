package main

// guard_corpus.go — the shared "did this guard actually examine anything?" check.
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

import (
	"fmt"
	"strings"
)

// requireCorpus fails when a guard examined no files at all. Call it after the
// walk, with the number of files actually read and the roots that were searched.
func requireCorpus(guard string, examined int, dirs []string) error {
	if examined > 0 {
		return nil
	}
	return fmt.Errorf("%s: examined 0 manifest files under %s — refusing to pass on an empty corpus. "+
		"A guard that had nothing to check reports the same green as one that checked everything, "+
		"so this fails instead. Run `make render-charts` if a rendered tree is expected, or update the "+
		"guard's roots if the manifests moved", guard, strings.Join(dirs, ", "))
}
