// Package guardwalk is the shared traversal every manifest guard runs: find the
// YAML under a set of roots, decode the documents that matter, and sort the
// findings so two runs over the same tree print the same thing.
//
// NINE PACKAGES CALL Walk or CollectPaths — seven guards plus manifestguard and
// assertobs, which are assertions rather than guards. (The header said "ten
// guards" from the extensions/shared split until it was re-counted; the number
// had drifted with the tree, and the buckets were never all guards.) The catalog
// names guard_walk.go as one of the "shared
// libraries every extension links" and says outright that it belongs in
// tools/internal/* rather than in any one extension — this is that move, made when
// the fourth gate extraction (guard-charts) needed it and the count made the case
// by itself.
//
// SortFindings exists because output stability is a correctness property here, not
// a nicety: a guard whose findings reorder between runs produces a diff on every
// CI run and gets ignored.
package guardwalk

// guard_walk.go — the one manifest walk the tree-scanning guards share.
//
// wave-health-guard, wave-dependency-guard, mesh-egress-guard,
// monitoring-label-guard and externalsecret-paths each carried their own copy of
// this loop, and the copies had DIVERGED:
//
//   - Three matched only "*.yaml"; monitoring-label-guard matched ".yaml" and
//     ".yml". A *.yml manifest was therefore policed by one guard and invisible
//     to the other three — including wave-health-guard, which exists to prevent
//     the #142 bootstrap wedge.
//   - wave-health-guard was the only one WITHOUT the missing-directory guard, so
//     it hard-errored on a layout its three siblings tolerated.
//
// Both are latent today (the scanned trees hold zero *.yml files), which is
// exactly why they were worth collapsing rather than patching in three places:
// with five copies, the next divergence is a matter of time.

// THE WALK IS WHERE THE read-repo FENCE BECOMES REAL FOR THE GUARDS. Converting
// it converts most of the bucket at once (see the count above) — and unlike
// the other four capabilities, `read-repo` had no seam to intercept: its callers
// used os.ReadFile directly, 124 times across 40 packages. This function is the
// closest thing to one that existed, which is why it is the first customer.
//
// Every caller now supplies the capability.Repo its BINDING entitles it to, and
// the dirs it passes are repo-relative rather than joined onto a root — so the
// paths a guard prints are repo-relative too. That is a fix as well as a
// consequence: the ::error file=… annotations used to carry `../platform-apl/…`,
// which GitHub cannot anchor to a file.

import (
	"errors"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/guardkit"
	"gopkg.in/yaml.v3"
)

// manifestExts are the extensions a Kubernetes manifest may use. Both, always —
// the guards disagreeing on this is the divergence this file exists to end.
var manifestExts = []string{".yaml", ".yml"}

// Walk calls fn(path, contents) for every YAML manifest under each dir,
// and returns how many files were read (the count requireCorpus gates on).
//
// A dir that does not exist is skipped, not an error: the guards run over layouts
// where a given tree may legitimately be absent. An empty RESULT is still a
// failure — that is requireCorpus's job, not this walk's.
//
// A dir OUTSIDE THE TREE is not the same thing and is not skipped. The fence
// returns ErrOutsideRepo rather than a not-exist error precisely so that the two
// cannot be confused here: an absent tree is a layout this guard tolerates, and a
// guard pointed out of the repository is a defect that must be loud rather than
// quietly walked over. This is why the skip matches on fs.ErrNotExist rather than
// on "Stat failed".
//
// templates/ is skipped: Helm template dirs hold Go-templated YAML ("{{ ... }}"),
// which is not a manifest and parses as garbage.
func Walk(r capability.Repo, dirs []string, fn func(path string, raw []byte) error) (examined int, err error) {
	for _, dir := range dirs {
		if _, statErr := r.Stat(dir); errors.Is(statErr, fs.ErrNotExist) {
			continue
		}
		walkErr := r.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "templates" {
					return filepath.SkipDir
				}
				return nil
			}
			if !hasExt(path) {
				return nil
			}
			raw, readErr := r.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			examined++
			return fn(path, raw)
		})
		if walkErr != nil {
			return examined, walkErr
		}
	}
	return examined, nil
}

// CollectPaths returns the sorted paths of every YAML manifest under the
// given dirs — Walk for the callers that want the file LIST rather than
// the contents. Same rules: absent dirs are skipped, templates/ is skipped, both
// extensions match.
//
// On error it returns the paths found SO FAR alongside it, never a bare nil. A
// caller that drops the error must not also be handed an empty slice: the guards
// read "no files" as the clean skip-or-succeed case, so returning nil on an
// unreadable subtree would turn a partial scan into a silent color.Green — the walk
// aborting is exactly when the corpus looks emptiest.
func CollectPaths(r capability.Repo, dirs []string) ([]string, error) {
	var paths []string
	_, err := Walk(r, dirs, func(path string, _ []byte) error {
		paths = append(paths, path)
		return nil
	})
	sort.Strings(paths)
	return paths, err
}

// hasExt reports whether path carries a YAML manifest extension.
func hasExt(path string) bool {
	for _, ext := range manifestExts {
		if strings.HasSuffix(path, ext) {
			return true
		}
	}
	return false
}

// DecodeDocs decodes every document of a multi-doc YAML file into T, returning
// the ones keep accepts. The guards each carried their own byte-identical copy
// of this loop, one per doc type.
//
// A document whose SHAPE does not match T (a kustomize patch, a CRD whose field
// collides with the guard's minimal struct) is skipped and the file keeps being
// read — which is what every copy's comment claimed ("skipping docs that fail to
// parse") but only the argocd-rendered-apps copy did: the other four broke out of
// the file entirely, so one odd document hid every manifest after it in the same
// file. Any other decode error (EOF, or a syntax error the parser cannot resume
// from) ends the file.
func DecodeDocs[T any](raw string, keep func(T) bool) []T {
	var docs []T
	dec := yaml.NewDecoder(strings.NewReader(raw))
	for {
		var d T
		if err := dec.Decode(&d); err != nil {
			// A yaml.TypeError means this document parsed but did not fit T; the
			// decoder is still positioned to read the next one.
			var typeErr *yaml.TypeError
			if errors.As(err, &typeErr) {
				continue
			}
			break
		}
		if keep(d) {
			docs = append(docs, d)
		}
	}
	return docs
}

// SortFindings orders findings by file, then by a per-guard secondary key,
// so a guard's annotations come out in a stable order regardless of walk order.
func SortFindings[T any](findings []T, key func(T) (file, secondary string)) {
	sort.Slice(findings, func(i, j int) bool {
		fi, si := key(findings[i])
		fj, sj := key(findings[j])
		if fi != fj {
			return fi < fj
		}
		return si < sj
	})
}

// SEVEN non-test callers, all of them guards.
// It is the manifest-roots half of the same question Walk answers — where does
// this repo keep the YAML a guard must scan — so it belongs beside it rather
// than in whichever extension happened to be extracted first.

// PlatformTreeDirs returns the three shared platform-bootstrap manifest roots the
// wave/mesh guards scan: platform-apl/manifest (the always-on base) and
// platform-apl/components (the per-component kustomize Components). Since the
// platform-apl move they live at the repo ROOT, outside the instance scaffold.
//
// It said "two" until this extraction, while the body had returned three since
// manifest-secret-store was added — the header was not updated with the fix it
// documents below. Caught by writing the function's first direct test.
func PlatformTreeDirs(r capability.Repo) []string {
	p := guardkit.RepoPath(r, "platform-apl")
	// manifest-secret-store is a real deployed unit — ci_bootstrap_cluster_manifests
	// gives it its own llz-secret-store Application ("path":
	// "platform-apl/manifest-secret-store") — and it holds the two
	// ClusterSecretStores every ExternalSecret in the repo binds to. It was absent
	// here, so the four guards that share these roots (wave-health,
	// wave-dependency, mesh-egress, and the refreshInterval check) never opened it.
	// No live finding today; a negative-wave annotation added there would have been
	// invisible to the #142 gate.
	return []string{
		filepath.Join(p, "manifest"),
		filepath.Join(p, "manifest-secret-store"),
		filepath.Join(p, "components"),
	}
}
