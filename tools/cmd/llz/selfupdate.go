package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// self-update replaces the running llz binary with a release build from the
// template repo. It reuses `gh` for the download — the same tool llz already
// depends on — so it inherits gh's auth (and works against a private template
// repo) instead of hand-rolling an authenticated HTTP client. The
// downloaded asset is checksum-verified against the release's SHA256SUMS before
// it overwrites the current executable.

// ── pure helpers (covered by selfupdate_test.go) ─────────────────────────────

// assetName is the release asset llz publishes for a platform: llz-<os>-<arch>
// (see .github/workflows/llz-release.yml). os/arch come from runtime.GOOS/GOARCH.
func assetName(goos, goarch string) string {
	return fmt.Sprintf("llz-%s-%s", goos, goarch)
}

// normalizeLLZTag canonicalises a user-supplied --ref to the bare umbrella release
// tag scheme `vX.Y.Z`. It accepts "1.2.3", "v1.2.3", or a legacy "llz/v1.2.3"; an
// empty ref stays empty (the caller then resolves the latest tag).
func normalizeLLZTag(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	ref = strings.TrimPrefix(ref, "llz/")
	ref = strings.TrimPrefix(ref, "v")
	return "v" + ref
}

// semver parses the numeric MAJOR.MINOR.PATCH out of a version or release tag,
// tolerating a leading "llz/" and/or "v" and any "-pre"/"+build" suffix. ok is
// false when the core isn't three integers (e.g. "dev", "dev-<sha>").
func semver(s string) (maj, min, patch int, ok bool) {
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

// semverLess reports whether a sorts before b. Unparseable versions sort lowest,
// so a "dev" build is always considered older than any real release.
func semverLess(a, b string) bool {
	am, an, ap, aok := semver(a)
	bm, bn, bp, bok := semver(b)
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

// latestLLZTag picks the highest-semver bare `vX.Y.Z` umbrella release tag from a
// release-tag list, ignoring any prefixed tag (the legacy llz/v* CLI tags and any
// other track). ok is false when none match.
func latestLLZTag(tags []string) (string, bool) {
	best := ""
	for _, t := range tags {
		if strings.Contains(t, "/") || !strings.HasPrefix(t, "v") {
			continue
		}
		if _, _, _, ok := semver(t); !ok {
			continue
		}
		if best == "" || semverLess(best, t) {
			best = t
		}
	}
	return best, best != ""
}

// checksumFor returns the hex sha256 recorded for asset in a `sha256sum`-style
// SHA256SUMS body ("<hex>  <filename>" per line; an optional leading '*' marks
// binary mode).
func checksumFor(sha256sums, asset string) (string, bool) {
	for _, line := range strings.Split(sha256sums, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimPrefix(fields[len(fields)-1], "*")
		if name == asset {
			return fields[0], true
		}
	}
	return "", false
}

func releaseListArgv(repo string) []string {
	return []string{"gh", "release", "list", "--repo", repo, "--limit", "200", "--json", "tagName,isDraft,isPrerelease"}
}

func releaseDownloadArgv(repo, tag, asset, dir string) []string {
	return []string{"gh", "release", "download", tag, "--repo", repo,
		"--pattern", asset, "--pattern", "SHA256SUMS", "--dir", dir, "--clobber"}
}

// updateRepo is the repo self-update pulls llz releases from: the upstream
// template org (an instance's .copier-answers.yml answer, else the default).
func updateRepo() string {
	org := defaultTemplateOrg
	if a, _ := readAnswers("."); a != nil && a.UpstreamOrg != "" {
		org = a.UpstreamOrg
	}
	return org + "/" + templateName
}

// ── orchestration ────────────────────────────────────────────────────────────

func runSelfUpdate(g globalOpts, repo, ref string) error {
	if repo == "" {
		repo = updateRepo()
	}

	tag := normalizeLLZTag(ref)
	if tag == "" {
		latest, err := latestRelease(repo)
		if err != nil {
			return err
		}
		tag = latest
	}

	// Skip the download when we're already on the target release. A "dev" build
	// has no parseable version, so it always updates.
	if _, _, _, ok := semver(version); ok && !semverLess(version, tag) && !semverLess(tag, version) {
		fmt.Printf("llz is already on %s — nothing to do.\n", tag)
		return nil
	}

	asset := assetName(runtime.GOOS, runtime.GOARCH)
	self, err := selfPath()
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "→ updating llz %s → %s (%s) at %s\n",
		version, tag, asset, self)
	if g.dryRun {
		return nil
	}

	dir, err := os.MkdirTemp("", "llz-self-update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	if err := run(g, releaseDownloadArgv(repo, tag, asset, dir)...); err != nil {
		return fmt.Errorf("download %s@%s: %w (is `gh` authenticated for %s?)", asset, tag, err, repo)
	}

	binPath := filepath.Join(dir, asset)
	if err := verifyChecksum(binPath, filepath.Join(dir, "SHA256SUMS"), asset); err != nil {
		return err
	}

	if err := replaceExecutable(self, binPath); err != nil {
		return err
	}
	fmt.Printf("llz updated to %s.\n", tag)
	return nil
}

// latestRelease resolves the newest bare `vX.Y.Z` release tag in repo via gh.
func latestRelease(repo string) (string, error) {
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
	tag, ok := latestLLZTag(tags)
	if !ok {
		return "", fmt.Errorf("no full vX.Y.Z releases found in %s — pass --ref to target a tag explicitly", repo)
	}
	return tag, nil
}

// selfPath is the absolute, symlink-resolved path of the running binary — the
// file replaceExecutable overwrites.
func selfPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe, nil
}

func verifyChecksum(file, sumsFile, asset string) error {
	sums, err := os.ReadFile(sumsFile)
	if err != nil {
		return fmt.Errorf("read SHA256SUMS: %w", err)
	}
	want, ok := checksumFor(string(sums), asset)
	if !ok {
		return fmt.Errorf("no checksum for %s in SHA256SUMS", asset)
	}
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return fmt.Errorf("checksum mismatch for %s: got %s, want %s", asset, got, want)
	}
	return nil
}

// replaceExecutable atomically swaps the new binary in for the running one. It
// stages a temp file in the SAME directory (so os.Rename is an atomic in-place
// swap, not a cross-filesystem move), then renames over self. On Unix the
// running process keeps its open inode, so replacing a live binary is safe.
func replaceExecutable(self, newBin string) error {
	dir := filepath.Dir(self)
	tmp, err := os.CreateTemp(dir, ".llz-update-*")
	if err != nil {
		return fmt.Errorf("stage update in %s: %w (need write access — reinstall with sudo if llz lives in a system dir)", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	src, err := os.Open(newBin)
	if err != nil {
		tmp.Close()
		return err
	}
	defer src.Close()
	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmpName, self); err != nil {
		return fmt.Errorf("install over %s: %w", self, err)
	}
	return nil
}
