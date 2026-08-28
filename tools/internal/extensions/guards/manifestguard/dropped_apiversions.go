package manifestguard

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
)

// dropped_apiversions.go — THE FOURTH LANE, and this extension's Incomplete
// said it was missing.
//
// It was entangled with checks.go in both directions: it called that file's
// scanDroppedAPIVersions/reportDroppedAPIVersions/ScannedManifestTrees, and
// checks_test.go drove its entry point. The entanglement turned out to be one
// of LOCATION, not of logic — the whole cluster only ever had callers here and
// in the `llz ci` face of the same guard. Nothing of `llz check` came with it.

// IsDeclaredAPIVersion reports whether a YAML line declares `apiVersion: <api>`
// (allowing indentation, quotes, and a trailing comment). It matches only a real
// manifest key, so a prose/comment mention (e.g. a changelog "… v1beta1 → v1")
// does not false-positive.
// droppedAPIs are apiVersions a converged apl-core dependency no longer serves, so
// a manifest still declaring one fails to apply ("no matches for kind … in version
// …") and surfaces only as an opaque Argo SyncFailed at deploy. Add an entry when a
// dependency drops one.
//
// Verify `served: false` (not merely "a newer version exists") before adding a row
// — a CRD commonly keeps an old version listed but unserved:
//
//	helm template eso external-secrets/external-secrets --version <v> --set crds.create=true |
//	  yq 'select(.kind=="CustomResourceDefinition") | .spec.names.kind + " " +
//	      .spec.versions[].name + " served=" + (.spec.versions[].served|tostring)'
var droppedAPIs = []struct{ apiVersion, since, fix string }{
	{
		apiVersion: "external-secrets.io/v1beta1",
		since:      "apl-core v6 (bundled external-secrets 2.4.1)",
		fix:        "bump to external-secrets.io/v1 (drop-in for standard ExternalSecret specs)",
	},
	// NB: external-secrets.io/v1alpha1 is NOT dropped — PushSecret/ClusterPushSecret
	// and the generators are v1alpha1-only in 2.4.1 (served, and the storage version).
	// platform-apl/components/harbor/harbor-admin-push.yaml is correct as-is.
}

// ScannedManifestTrees are the manifest roots the dropped-apiVersion guard walks.
// Two classes, each uncovered by every other gate:
//
//   - Operator-OWNED escape hatches (kubernetes-custom/, kubernetes-charts/).
//     `llz upgrade` deliberately never rewrites these, so a dependency's version
//     drop leaves them stale until the operator migrates them by hand.
//   - The template's own SHARED tree (platform-apl/). No CRD-aware gate reads it:
//     k8s-lint, k8s-validate and the kind server-side dry-run all scan $RENDER_DIR,
//     which render-charts.sh builds from kubernetes-charts/*/ ONLY. Every instance
//     fetches platform-apl/ remotely (clusterspec's kustomize remoteBasePrefix), so
//     one stale apiVersion here breaks every instance at once with nothing upstream
//     to catch it — exactly how llz-cidr-firewall's ExternalSecret shipped on
//     external-secrets.io/v1beta1.
//
// Prefixes are repo-root-relative and cover both layouts: an instance repo carries
// kubernetes-custom/ at its root, the template carries the scaffold copy under
// instance-template/.
var ScannedManifestTrees = []string{
	"kubernetes-custom/",
	"instance-template/kubernetes-custom/",
	"kubernetes-charts/",
	"platform-apl/",
}

type droppedAPIHit struct{ loc, api, since, fix string }

func IsDeclaredAPIVersion(line, api string) bool {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, "apiVersion:") {
		return false
	}
	v := strings.TrimSpace(strings.TrimPrefix(s, "apiVersion:"))
	if i := strings.Index(v, "#"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return strings.Trim(v, `"'`) == api
}

// ManifestYAMLFiles returns every .yaml/.yml path under ScannedManifestTrees,
// rooted at root. Paths are returned root-relative with forward slashes.
//
// Filesystem walk, deliberately NOT a `git ls-files` scan. The Kubernetes lint job
// runs inside the ci-kubernetes container, where git against the mounted checkout
// fails — a git-based scan reports "not a git repo" and takes the whole gate down
// with it. Every sibling manifest guard walks the filesystem for the same reason.
//
// A tree that does not exist is skipped, not an error: an instance repo has no
// platform-apl/ (it fetches that tree remotely) and the template has no
// kubernetes-custom/ at its root.
//
// Vendored subchart directories (kubernetes-charts/<chart>/charts/, which `helm dep
// build` populates during the same lint run) are skipped — upstream chart templates
// are not ours to gate and would false-positive.
//
// HAND-ROLLED RATHER THAN guardwalk.Walk, and the difference is not cosmetic:
// guardwalk skips `templates/` and does NOT skip vendored `charts/`. Swapping to
// it would silently change what this gate catches in both directions. It now goes
// through capability.Repo, which is what the read-repo fence actually requires —
// the fence is about WHERE a gate may read, not about which walker it uses.
func ManifestYAMLFiles(repo capability.Repo) ([]string, error) {
	var out []string
	for _, tree := range ScannedManifestTrees {
		base := filepath.FromSlash(tree)
		if _, statErr := repo.Stat(base); errors.Is(statErr, fs.ErrNotExist) {
			continue // tree absent in this layout
		}
		err := repo.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return fs.SkipDir
				}
				return err
			}
			if d.IsDir() {
				if n := d.Name(); n == ".git" || (n == "charts" && p != base) {
					return fs.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(p, ".yaml") || strings.HasSuffix(p, ".yml") {
				out = append(out, filepath.ToSlash(p))
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("scan %s: %w", tree, err)
		}
	}
	return out, nil
}

// ScanDroppedAPIVersions looks for any droppedAPIs apiVersion in the manifest trees
// rooted at root (""/"." = cwd). examined counts the files actually read, so a
// caller can refuse to pass on an empty corpus.
func ScanDroppedAPIVersions(root string) (hits []droppedAPIHit, examined int, err error) {
	if root == "" {
		root = "."
	}
	repo := capability.RepoForGate(Extension(), root)
	files, err := ManifestYAMLFiles(repo)
	if err != nil {
		return nil, 0, err
	}
	for _, f := range files {
		data, readErr := repo.ReadFile(filepath.FromSlash(f))
		if readErr != nil {
			continue // race with a delete / unreadable — not this check's concern
		}
		examined++
		for i, ln := range strings.Split(string(data), "\n") {
			for _, d := range droppedAPIs {
				if IsDeclaredAPIVersion(ln, d.apiVersion) {
					hits = append(hits, droppedAPIHit{fmt.Sprintf("%s:%d", f, i+1), d.apiVersion, d.since, d.fix})
				}
			}
		}
	}
	return hits, examined, nil
}

// ReportDroppedAPIVersions prints hits and returns the gate error, or nil if clean.
func ReportDroppedAPIVersions(hits []droppedAPIHit) error {
	if len(hits) == 0 {
		return nil
	}
	fmt.Fprintf(os.Stderr, "dropped apiVersion(s) in %d location(s):\n", len(hits))
	for _, h := range hits {
		// ::error file=…:: so each lands as a PR annotation rather than being buried
		// in log output, matching the other manifest guards.
		fmt.Fprintf(os.Stderr, "::error file=%s::%s no longer served since %s; %s\n",
			strings.SplitN(h.loc, ":", 2)[0], h.api, h.since, h.fix)
		fmt.Fprintf(os.Stderr, "  • %s — %s no longer served since %s; %s\n", h.loc, h.api, h.since, h.fix)
	}
	return fmt.Errorf("manifest(s) declare an apiVersion the cluster no longer serves — they fail to " +
		"apply (Argo SyncFailed at deploy). Migrate them by hand: `llz upgrade` does not rewrite " +
		"operator-owned files, and no CRD-aware gate covers the shared platform-apl/ tree")
}
