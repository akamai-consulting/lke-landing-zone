package selfupgrade

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/answers"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/llzver"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/proc"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/templateid"
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

func releaseDownloadArgv(repo, tag, asset, dir string) []string {
	return []string{"gh", "release", "download", tag, "--repo", repo,
		"--pattern", asset, "--pattern", "SHA256SUMS", "--dir", dir, "--clobber"}
}

// UpdateRepo is the repo self-update pulls llz releases from: the upstream
// template org (an instance's .copier-answers.yml answer, else the default).
func UpdateRepo() string {
	org := templateid.DefaultOrg
	if a, _ := answers.Read("."); a != nil && a.UpstreamOrg != "" {
		org = a.UpstreamOrg
	}
	return org + "/" + templateid.Name
}

// ── orchestration ────────────────────────────────────────────────────────────

func RunSelfUpdate(dryRun bool, repo, ref string) error {
	if repo == "" {
		repo = UpdateRepo()
	}

	tag := llzver.NormalizeLLZTag(ref)
	if tag == "" {
		latest, err := llzver.LatestRelease(repo)
		if err != nil {
			return err
		}
		tag = latest
	}

	// Skip the download when we're already on the target release. A "dev" build
	// has no parseable Version, so it always updates.
	if _, _, _, ok := llzver.Semver(Version); ok && !llzver.Less(Version, tag) && !llzver.Less(tag, Version) {
		fmt.Printf("llz is already on %s — nothing to do.\n", tag)
		return nil
	}

	asset := assetName(runtime.GOOS, runtime.GOARCH)
	self, err := selfPath()
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "→ updating llz %s → %s (%s) at %s\n",
		Version, tag, asset, self)
	if dryRun {
		return nil
	}

	dir, err := os.MkdirTemp("", "llz-self-update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	if err := proc.RunEcho(dryRun, releaseDownloadArgv(repo, tag, asset, dir)...); err != nil {
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
