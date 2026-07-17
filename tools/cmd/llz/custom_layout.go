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
	"regexp"
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

// dnsLabelRe / dnsLabelMax are Kubernetes' RFC 1123 LABEL rule, which a Namespace name
// must satisfy. A namespaces/<ns>/ directory name becomes BOTH the generated
// Application's destination namespace ({{.path.basename}}) and part of its object name
// (instance-custom-<ns>), so a directory the operator can create freely on a
// case-preserving filesystem — "My_App" — yields an Application that Kubernetes rejects
// outright. The ApplicationSet then reports ErrorOccurred rather than anything that
// points at the directory, so catch it here where the message can name the file.
var dnsLabelRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

const dnsLabelMax = 63

// checkCustomLayout validates the escape-hatch tree at customDir (an instance root joined
// with clusterspec.CustomRoot). It returns nil when there is nothing to say — including
// when the tree is absent entirely, which is the case for any caller pointed at a tree
// with no instance scaffold.
func checkCustomLayout(customDir string) error {
	if _, err := os.Stat(customDir); err != nil {
		return nil // no escape hatch in this tree — nothing to check.
	}
	findings := append(checkNamespaceDirs(customDir), checkNoKustomize(customDir)...)
	if len(findings) == 0 {
		return nil
	}
	return errors.New("  • " + strings.Join(findings, "\n\n  • "))
}

// kustomizeFileNames are the filenames kustomize recognizes as a build root.
var kustomizeFileNames = map[string]bool{
	"kustomization.yaml": true,
	"kustomization.yml":  true,
	"Kustomization":      true,
}

// checkNoKustomize rejects a kustomize root anywhere under the escape hatch.
//
// The generated Applications set `spec.source.directory.recurse` so that subdirectories
// are organizational only — App Platform's documented semantics. But setting `directory`
// at all makes the source EXPLICITLY the directory type
// (ApplicationSource.ExplicitType keys off `source.Directory != nil`, and
// GetAppSourceType returns on the explicit type), so Argo's kustomize auto-detection
// never runs. A kustomization.yaml would therefore not be BUILT — Argo's manifest regex
// matches it like any other yaml, and it would try to apply `kind: Kustomization` to the
// cluster while separately applying every base directly, unkustomized.
//
// Recursion and kustomize are mutually exclusive here and recursion is what the
// convention documents, so kustomize is out — and out LOUDLY, because the failure is
// silent-ish and bizarre. Operators who want kustomize point their own Argo CD
// Application at a kustomize root, the same route the Helm/OCI escape uses.
func checkNoKustomize(customDir string) []string {
	var findings []string
	_ = filepath.WalkDir(customDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() || !kustomizeFileNames[d.Name()] {
			return nil
		}
		findings = append(findings, fmt.Sprintf(
			"%s: kustomize is not supported in %s/. The generated Applications sync with directory recursion (so subdirectories are organizational, per App Platform's convention), and Argo cannot do both: an explicit directory source disables its kustomize auto-detection, so this file would be applied to the cluster as a literal `kind: Kustomization` manifest rather than built.\n"+
				"    Use plain manifests here, or — if you need kustomize — drop your own Argo CD Application pointing at your kustomize root (it rides the instance-custom AppProject, which allows any source repo).",
			p, clusterspec.CustomRoot))
		return nil
	})
	return findings
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
		case len(name) > dnsLabelMax:
			findings = append(findings, fmt.Sprintf(
				"%s: %q is %d characters — a Kubernetes namespace name is capped at %d. This directory name becomes the generated Application's destination namespace verbatim, so Kubernetes would reject it. Shorten the directory name.",
				filepath.Join(nsDir, name), name, len(name), dnsLabelMax))
		case !dnsLabelRe.MatchString(name):
			findings = append(findings, fmt.Sprintf(
				"%s: %q is not a valid Kubernetes namespace name (RFC 1123: lowercase alphanumeric and '-', must start and end alphanumeric). This directory name becomes the generated Application's destination namespace verbatim and part of its object name (instance-custom-%s), so Kubernetes would reject both — the ApplicationSet reports ErrorOccurred without naming this directory. Rename it.",
				filepath.Join(nsDir, name), name, name))
		}
	}
	return findings
}
