// Package llzver is the version-tag vocabulary for llz's own releases: parsing a
// vX.Y.Z, ordering two of them, normalising the llz/v* CLI tag track, and asking
// GitHub which release is newest.
//
// IT CAME DOWN FROM internal/extensions/selfupgrade, WHICH IS WHERE IT WAS
// WRITTEN AND NOT WHERE IT BELONGED. self-update was simply the first caller to
// need "is this a real release, and which is newer" — but internal/shared/copier
// needs exactly the same four answers to decide what ref `llz new` pins a scaffold
// to, and a shared package importing an extension inverts the layering the
// boundary test now enforces. None of this is a self-update concern; it is what a
// version STRING means, which is substrate.
//
// semverLess CAME ACROSS EXPORTED, as Less. That is not an export bought to
// satisfy a test: selfupgrade's own "already up to date" check calls it, and once
// the function lives here that call crosses a package line. LatestLLZTag needs it
// too, so leaving it behind would have split the ordering rule from the picker
// that depends on it.
package llzver

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

// execOutput delegates to kubectlprobe.Exec through a CLOSURE, never by
// assignment: an assignment would snapshot whatever the seam pointed at when this
// package initialised, losing the swap-at-call-time property tests rely on.
func execOutput(name string, args ...string) ([]byte, error) { return kubectlprobe.Exec(name, args...) }

// Semver parses the numeric MAJOR.MINOR.PATCH out of a Version or release tag,
// tolerating a leading "llz/" and/or "v" and any "-pre"/"+build" suffix. ok is
// false when the core isn't three integers (e.g. "dev", "dev-<sha>").
func Semver(s string) (maj, min, patch int, ok bool) {
	s = strings.TrimPrefix(s, "llz/")
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	var nums [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return 0, 0, 0, false
		}
		nums[i] = n
	}
	return nums[0], nums[1], nums[2], true
}

// Less reports whether a sorts before b. Unparseable versions sort lowest,
// so a "dev" build is always considered older than any real release.
func Less(a, b string) bool {
	am, an, ap, aok := Semver(a)
	bm, bn, bp, bok := Semver(b)
	if aok != bok {
		return !aok // unparseable (false) < parseable (true)
	}
	if !aok {
		return false // neither parses — treat as equal
	}
	if am != bm {
		return am < bm
	}
	if an != bn {
		return an < bn
	}
	return ap < bp
}

// NormalizeLLZTag canonicalises a user-supplied --ref to the bare umbrella release
// tag scheme `vX.Y.Z`. It accepts "1.2.3", "v1.2.3", or a legacy "llz/v1.2.3"; an
// empty ref stays empty (the caller then resolves the latest tag).
func NormalizeLLZTag(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	ref = strings.TrimPrefix(ref, "llz/")
	ref = strings.TrimPrefix(ref, "v")
	return "v" + ref
}

// LatestLLZTag picks the highest-Semver bare `vX.Y.Z` umbrella release tag from a
// release-tag list, ignoring any prefixed tag (the legacy llz/v* CLI tags and any
// other track). ok is false when none match.
func LatestLLZTag(tags []string) (string, bool) {
	best := ""
	for _, t := range tags {
		if strings.Contains(t, "/") || !strings.HasPrefix(t, "v") {
			continue
		}
		if _, _, _, ok := Semver(t); !ok {
			continue
		}
		if best == "" || Less(best, t) {
			best = t
		}
	}
	return best, best != ""
}

// LatestRelease resolves the newest bare `vX.Y.Z` release tag in repo via gh.
func LatestRelease(repo string) (string, error) {
	out, err := execOutput(releaseListArgv(repo)[0], releaseListArgv(repo)[1:]...)
	if err != nil {
		return "", fmt.Errorf("list releases for %s: %w (is `gh` authenticated?)", repo, err)
	}
	var releases []struct {
		TagName      string `json:"tagName"`
		IsDraft      bool   `json:"isDraft"`
		IsPrerelease bool   `json:"isPrerelease"`
	}
	if err := json.Unmarshal(out, &releases); err != nil {
		return "", fmt.Errorf("parse release list: %w", err)
	}
	// Only full releases are update targets: a draft has no usable tag, and a
	// pre-release is an unpromoted e2e candidate (RELEASING.md) — neither should be
	// served to `self-update`/`new`.
	tags := make([]string, 0, len(releases))
	for _, r := range releases {
		if r.IsDraft || r.IsPrerelease {
			continue
		}
		tags = append(tags, r.TagName)
	}
	tag, ok := LatestLLZTag(tags)
	if !ok {
		return "", fmt.Errorf("no full vX.Y.Z releases found in %s — pass --ref to target a tag explicitly", repo)
	}
	return tag, nil
}

func releaseListArgv(repo string) []string {
	return []string{"gh", "release", "list", "--repo", repo, "--limit", "200", "--json", "tagName,isDraft,isPrerelease"}
}
