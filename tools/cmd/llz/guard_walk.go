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
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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

// hasManifestExt reports whether path carries a YAML manifest extension.
func hasManifestExt(path string) bool {
	for _, ext := range manifestExts {
		if strings.HasSuffix(path, ext) {
			return true
		}
	}
	return false
}
