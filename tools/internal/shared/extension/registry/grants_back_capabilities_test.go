package registry

// grants_back_capabilities_test.go — a seam an extension HOLDS must be a
// capability it DECLARED.
//
// A GRANT INVARIANT ENFORCED RATHER THAN DESCRIBED. Every declaration in the
// catalog is prose plus a grant list, and the only other thing checking a grant
// against the code is a human reading both. A declaration should be true; this
// makes one class of untruth fail the build.
//
// THE RULE: a package whose Deps carries an `Exec` seam — the stdout-capturing
// shell-out, the one uniform capability in the catalog (fourteen packages, one
// identical signature) — is running something and reading what it said. That is a
// read, and it must appear in the declaration as cloud-read, cluster-read,
// read-repo or secret-read.
//
// IT FOUND ONE ON THE WAY IN. internal/extensions/clusteraccess held an Exec seam
// and declared only cloud-mutate, cluster-write and secret-custody. It runs
// `tofu output -json` and `tofu state list` against a remote object-storage
// backend to find the cluster it is fetching access for — a cloud read that was
// simply never written down. cloud-mutate does NOT imply cloud-read here: the
// model keeps grants explicit and eight other extensions declare both.
//
// WHAT IT DELIBERATELY DOES NOT DO is map each seam to a SPECIFIC grant. `Exec`
// runs kubectl, gh, git and tofu depending on the caller, so demanding
// cluster-read specifically would be wrong for the packages that only ever shell
// out to gh. "Some read grant" is the strongest claim the evidence supports, and
// a check that overreaches is one someone weakens rather than satisfies.

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var (
	execSeamRe = regexp.MustCompile(`(?m)^\s+Exec\s+func\(name string, args \.\.\.string\)`)
	readGrant  = regexp.MustCompile(`extension\.(CloudRead|ClusterRead|ReadRepo|SecretRead)`)
	declares   = regexp.MustCompile(`extension\.Extension\{`)
)

// extensionsDir walks up to the repo root rather than hard-coding a depth: this
// package has already moved once (internal/extension -> internal/shared/extension)
// and a relative literal would have silently pointed at nothing afterwards.
func extensionsDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		cand := filepath.Join(dir, "internal", "extensions")
		if fi, err := os.Stat(cand); err == nil && fi.IsDir() {
			return cand
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not find internal/extensions from the test's working directory")
	return ""
}

// packageDirs returns every directory under root that holds Go source.
//
// IT WALKS RATHER THAN LISTING, and that is not incidental. This used to
// `os.ReadDir(internal/extensions)` and treat each entry as a package, which was
// true while the tree was flat. Sub-dividing it into capabilities/ lifecycle/
// assertions/ guards/ turned those entries into BUCKETS: the glob matched no .go
// files, `checked` fell to zero, and the corpus check below is the only reason
// anyone found out. Walking is indifferent to how deep the tree gets.
func packageDirs(t *testing.T, root string) []string {
	t.Helper()
	seen := map[string]bool{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".go") {
			seen[filepath.Dir(p)] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	dirs := make([]string, 0, len(seen))
	for d := range seen {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	return dirs
}

func TestAnExecSeamIsBackedByAReadGrant(t *testing.T) {
	checked := 0
	for _, dir := range packageDirs(t, extensionsDir(t)) {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		var src strings.Builder
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			src.Write(b)
		}
		s := src.String()
		if !declares.MatchString(s) || !execSeamRe.MatchString(s) {
			continue
		}
		checked++
		if !readGrant.MatchString(s) {
			t.Errorf("%s holds an Exec seam but declares no read grant — it shells out and "+
				"reads the result, so cloud-read / cluster-read / read-repo / secret-read "+
				"is missing from the declaration rather than absent from the code",
				filepath.Base(dir))
		}
	}
	// A corpus check, for the reason requireCorpus exists elsewhere in this tree: a
	// guard that matched nothing would report the same green as one that checked
	// every package.
	if checked < 10 {
		t.Fatalf("only %d packages matched the Exec seam — the pattern has drifted from the "+
			"code and this guard is no longer checking anything", checked)
	}
}
