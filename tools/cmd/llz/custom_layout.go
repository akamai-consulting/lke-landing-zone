package main

// custom_layout.go guards the operator escape hatch's directory contract —
// kubernetes-custom/ (clusterspec.CustomRoot), the instance-owned tree whose content the
// instance-custom ApplicationSet syncs.
//
// The layout mirrors App Platform's documented GitOps convention
// (https://techdocs.akamai.com/app-platform/docs/gitops):
//
//	kubernetes-custom/namespaces/<ns>/   → one Application per directory, synced into <ns>
//	kubernetes-custom/global/            → cluster-scoped resources
//
// Both are matched by git directory generators (clusterspec.RenderInstanceCustom), so a
// directory that does not exist yet yields zero Applications rather than an error — the
// tree is `owned` and an instance may legitimately never create either.
//
// What this guards is the two directory names an operator can pick that would generate a
// BROKEN Application rather than a working one. Both are hard errors, not warnings: each
// produces an Argo CD Application that fights either apl-core or its own sibling, and the
// symptom (resources flapping between two controllers) is far more expensive to diagnose
// than a failed render is to fix.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
)

// aplReservedPrefix is the directory prefix App Platform reserves for operator-owned
// (apl-operator-written) trees in the values repo — apl-secrets/, apl-users/, and any
// future sibling. The GitOps doc's rule is "do not add, edit, or delete files in any
// apl- prefixed directory"; here the hazard is concrete rather than advisory. A
// namespaces/apl-*/ directory would point an instance-custom Application at a namespace
// apl-core's OWN gitops-ns-apl-* Application already manages, putting two Argo CD
// Applications in contention over the same resources.
const aplReservedPrefix = "apl-"

// checkCustomLayout validates the escape-hatch tree at customDir (an instance root joined
// with clusterspec.CustomRoot). It returns nil when there is nothing to say — including
// when the tree is absent entirely, which is the case for any caller pointed at a tree
// with no instance scaffold.
func checkCustomLayout(customDir string) error {
	if _, err := os.Stat(customDir); err != nil {
		return nil // no escape hatch in this tree — nothing to check.
	}
	findings := checkNamespaceDirs(customDir)
	if len(findings) == 0 {
		return nil
	}
	return errors.New("  • " + strings.Join(findings, "\n\n  • "))
}

// checkNamespaceDirs returns one finding per namespaces/<ns> directory whose name would
// collide with apl-core's own gitops Applications or with the generated global App.
func checkNamespaceDirs(customDir string) []string {
	nsDir := filepath.Join(customDir, clusterspec.CustomNamespacesDirName)
	entries, err := os.ReadDir(nsDir)
	if err != nil {
		return nil // no namespaces/ yet — the generator yields zero Apps. Fine.
	}
	var findings []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		switch {
		case strings.HasPrefix(name, aplReservedPrefix):
			findings = append(findings, fmt.Sprintf(
				"%s: %q is an apl-core-owned namespace (the %q prefix is reserved). apl-core's own gitops-ns-%s Application already manages it, and a second Argo CD Application over the same resources puts them in contention. Deploy into a namespace you own instead.\n"+
					"    See https://techdocs.akamai.com/app-platform/docs/gitops (\"do not add, edit, or delete files in any apl- prefixed directory\").",
				filepath.Join(nsDir, name), name, aplReservedPrefix, name))
		case name == clusterspec.CustomGlobalDirName:
			findings = append(findings, fmt.Sprintf(
				"%s: %q is reserved. Its generated Application name (instance-custom-%s) would collide with the one the %s/%s/ generator emits. Rename the directory, or move cluster-scoped resources to %s/%s/.",
				filepath.Join(nsDir, name), name, name,
				clusterspec.CustomRoot, clusterspec.CustomGlobalDirName,
				clusterspec.CustomRoot, clusterspec.CustomGlobalDirName))
		}
	}
	return findings
}
