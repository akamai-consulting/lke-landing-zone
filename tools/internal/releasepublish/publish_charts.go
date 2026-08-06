package releasepublish

// ci_publish_charts.go implements `llz ci publish-charts` — packages every
// first-party Helm chart under a directory and pushes + keyless-cosign-signs it to
// an OCI registry, immutably (a version already published + signed is skipped).
//
// It replaces the inline bash the publish-charts workflow used to carry so the
// decision logic — the immutability guard (published AND signed → skip; published
// but UNSIGNED → re-sign in place; else package+push+sign), the digest parsing, and
// the transient-failure retry — is unit-tested Go instead of untestable CI shell.
//
// Two registry-ref forms matter: helm push wants the `oci://` scheme, but cosign
// wants a BARE ref — an `oci://` prefix makes cosign parse the registry as host
// `oci` (`Get "https://oci/v2/": lookup oci …`), so sign/verify use the bare form.
//
// The shell-outs (helm, cosign) are reached only through package-var seams.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/chartguard"
)

var pcDigestRe = regexp.MustCompile(`(?i)digest:\s*(sha256:[0-9a-f]+)`)

// Seams (package vars) so the publish decisions are testable without helm/cosign.
var (
	// pcInspect returns a chart dir's name + version (helm show chart).
	pcInspect = func(dir string) (name, version string, err error) {
		out, e := exec.Command("helm", "show", "chart", dir).Output()
		if e != nil {
			return "", "", fmt.Errorf("helm show chart %s: %w", dir, e)
		}
		return chartguard.ChartName(string(out)), chartguard.ChartVersion(string(out)), nil
	}
	// pcPublished reports whether ociRef:version already exists in the registry.
	pcPublished = func(ociRef, version string) bool {
		return exec.Command("helm", "show", "chart", ociRef, "--version", version).Run() == nil
	}
	// pcSigned reports whether a legacy .sig signature exists for regRef (bare ref).
	pcSigned = func(regRef string) bool {
		return exec.Command("cosign", "download", "signature", regRef).Run() == nil
	}
	// pcPackage runs `helm dependency build` (best-effort) then `helm package`.
	pcPackage = func(dir, destDir string) error {
		_ = exec.Command("helm", "dependency", "build", dir).Run()
		return runCapture("helm", "package", dir, "--destination", destDir)
	}
	// pcPush pushes a chart tarball and returns helm's raw output (carrying "Digest:").
	pcPush = func(tgz, ociDest string) (string, error) {
		out, err := exec.Command("helm", "push", tgz, ociDest).CombinedOutput()
		return string(out), err
	}
	// pcSign keyless-signs a bare ref, forcing legacy `.sig`-tag storage.
	pcSign = func(ref string) error {
		return runCapture("cosign", "sign", "--yes",
			"--use-signing-config=false", "--new-bundle-format=false", ref)
	}
	pcSleep = func(d time.Duration) { time.Sleep(d) }
)

type PublishChartsOpts struct {
	ChartsDir, Selected       string
	Registry, Owner, RepoPath string
	DestDir                   string
	Retries                   int
	Interval                  time.Duration
}

func runCapture(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		if s := strings.TrimSpace(string(out)); s != "" {
			return fmt.Errorf("%s: %w — %s", name, err, s)
		}
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// parseHelmPushDigest extracts the sha256 digest helm push prints. Pure.
func parseHelmPushDigest(out string) string {
	if m := pcDigestRe.FindStringSubmatch(out); m != nil {
		return m[1]
	}
	return ""
}

// chartDirs returns the sorted subdirectories of root that contain a Chart.yaml.
func chartDirs(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if _, statErr := os.Stat(filepath.Join(dir, "Chart.yaml")); statErr == nil {
			dirs = append(dirs, dir)
		}
	}
	sort.Strings(dirs)
	return dirs, nil
}

// retryPC runs fn up to o.Retries times, sleeping o.Interval between attempts.
func retryPC(o PublishChartsOpts, what string, fn func() error) error {
	var err error
	for n := 1; n <= Max1(o.Retries); n++ {
		if err = fn(); err == nil {
			return nil
		}
		if n < Max1(o.Retries) {
			fmt.Fprintf(os.Stderr, "::warning::%s failed (attempt %d/%d): %v — retrying in %s\n", what, n, o.Retries, err, o.Interval)
			pcSleep(o.Interval)
		}
	}
	return fmt.Errorf("%s failed after %d attempts: %w", what, Max1(o.Retries), err)
}

func RunPublishCharts(o PublishChartsOpts) error {
	if o.Owner == "" {
		return fmt.Errorf("publish-charts: --owner is required")
	}
	ociDest := "oci://" + o.Registry + "/" + o.Owner + "/" + o.RepoPath // helm push (needs oci://)
	regDest := o.Registry + "/" + o.Owner + "/" + o.RepoPath            // cosign (bare ref)

	if o.DestDir != "" {
		if err := os.MkdirAll(o.DestDir, 0o755); err != nil {
			return fmt.Errorf("creating package dir %s: %w", o.DestDir, err)
		}
	}

	dirs, err := chartDirs(o.ChartsDir)
	if err != nil {
		return fmt.Errorf("listing charts under %s: %w", o.ChartsDir, err)
	}

	pushed, resigned := 0, 0
	for _, dir := range dirs {
		name, version, err := pcInspect(dir)
		if err != nil {
			return err
		}
		if o.Selected != "all" && o.Selected != name {
			continue
		}
		ociRef := ociDest + "/" + name
		regRef := regDest + "/" + name

		// Immutability guard: skip only if published AND signed. A version pushed
		// but never signed (an earlier failed sign) must be re-signed in place, not
		// skipped — else it stays unverifiable (Kyverno keyless verify rejects it).
		if pcPublished(ociRef, version) {
			if pcSigned(regRef + ":" + version) {
				fmt.Printf("::notice::%s %s already published + signed — skipping (bump version: to release)\n", name, version)
				continue
			}
			fmt.Printf("::warning::%s %s is published but UNSIGNED — re-signing in place (no re-push)\n", name, version)
			if err := retryPC(o, "cosign sign "+name, func() error { return pcSign(regRef + ":" + version) }); err != nil {
				return err
			}
			resigned++
			continue
		}

		fmt.Printf("Packaging %s %s\n", name, version)
		if err := pcPackage(dir, o.DestDir); err != nil {
			return fmt.Errorf("package %s %s: %w", name, version, err)
		}
		tgz := filepath.Join(o.DestDir, name+"-"+version+".tgz")

		fmt.Printf("Pushing %s %s → %s\n", name, version, ociDest)
		var pushOut string
		if err := retryPC(o, "helm push "+name, func() error {
			var e error
			pushOut, e = pcPush(tgz, ociDest)
			return e
		}); err != nil {
			return err
		}
		digest := parseHelmPushDigest(pushOut)
		if digest == "" {
			return fmt.Errorf("no digest returned by helm push for %s %s:\n%s", name, version, pushOut)
		}

		fmt.Printf("Signing %s %s (%s) — keyless cosign\n", name, version, digest)
		if err := retryPC(o, "cosign sign "+name, func() error { return pcSign(regRef + "@" + digest) }); err != nil {
			return err
		}
		pushed++
	}
	fmt.Printf("::notice::Published %d chart(s); re-signed %d already-published unsigned chart(s)\n", pushed, resigned)
	return nil
}
