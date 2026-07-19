package main

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

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// manifestExts are the extensions a Kubernetes manifest may use. Both, always —
// the guards disagreeing on this is the divergence this file exists to end.
var manifestExts = []string{".yaml", ".yml"}

// walkManifests calls fn(path, contents) for every YAML manifest under each dir,
// and returns how many files were read (the count requireCorpus gates on).
//
// A dir that does not exist is skipped, not an error: the guards run over layouts
// where a given tree may legitimately be absent. An empty RESULT is still a
// failure — that is requireCorpus's job, not this walk's.
//
// templates/ is skipped: Helm template dirs hold Go-templated YAML ("{{ ... }}"),
// which is not a manifest and parses as garbage.
func walkManifests(dirs []string, fn func(path string, raw []byte) error) (examined int, err error) {
	for _, dir := range dirs {
		if _, statErr := os.Stat(dir); os.IsNotExist(statErr) {
			continue
		}
		walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "templates" {
					return filepath.SkipDir
				}
				return nil
			}
			if !hasManifestExt(path) {
				return nil
			}
			raw, readErr := os.ReadFile(path)
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

// collectManifestPaths returns the sorted paths of every YAML manifest under the
// given dirs — walkManifests for the callers that want the file LIST rather than
// the contents. Same rules: absent dirs are skipped, templates/ is skipped, both
// extensions match.
//
// On error it returns the paths found SO FAR alongside it, never a bare nil. A
// caller that drops the error must not also be handed an empty slice: the guards
// read "no files" as the clean skip-or-succeed case, so returning nil on an
// unreadable subtree would turn a partial scan into a silent green — the walk
// aborting is exactly when the corpus looks emptiest.
func collectManifestPaths(dirs []string) ([]string, error) {
	var paths []string
	_, err := walkManifests(dirs, func(path string, _ []byte) error {
		paths = append(paths, path)
		return nil
	})
	sort.Strings(paths)
	return paths, err
}

// hasManifestExt reports whether path carries a YAML manifest extension.
func hasManifestExt(path string) bool {
	for _, ext := range manifestExts {
		if strings.HasSuffix(path, ext) {
			return true
		}
	}
	return false
}

// decodeDocs decodes every document of a multi-doc YAML file into T, returning
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
func decodeDocs[T any](raw string, keep func(T) bool) []T {
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

// sortGuardFindings orders findings by file, then by a per-guard secondary key,
// so a guard's annotations come out in a stable order regardless of walk order.
func sortGuardFindings[T any](findings []T, key func(T) (file, secondary string)) {
	sort.Slice(findings, func(i, j int) bool {
		fi, si := key(findings[i])
		fj, sj := key(findings[j])
		if fi != fj {
			return fi < fj
		}
		return si < sj
	})
}
