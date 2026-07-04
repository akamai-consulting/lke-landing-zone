package main

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

	"github.com/spf13/cobra"
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
		return chartName(string(out)), chartVersion(string(out)), nil
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

type publishChartsOpts struct {
	chartsDir, selected       string
	registry, owner, repoPath string
	destDir                   string
	retries                   int
	interval                  time.Duration
}

func ciPublishChartsCmd() *cobra.Command {
	var o publishChartsOpts
	var interval int
	c := &cobra.Command{
		Use:   "publish-charts",
		Short: "package, push, and keyless-sign first-party charts to an OCI registry (immutable + re-sign)",
		Long: "Packages every chart under --dir and pushes + cosign-signs it to\n" +
			"oci://<registry>/<owner>/<repo-path>/<chart>. Immutable: a version already\n" +
			"published AND signed is skipped; a version pushed but UNSIGNED (an earlier\n" +
			"run whose sign failed) is re-signed in place. Transient helm/cosign failures\n" +
			"retry. Replaces the publish-charts workflow's inline bash with tested Go.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			o.interval = time.Duration(interval) * time.Second
			return runPublishCharts(o)
		},
	}
	c.Flags().StringVar(&o.chartsDir, "dir", "kubernetes-charts", "directory holding the chart subdirectories")
	c.Flags().StringVar(&o.selected, "selected", "all", "chart name to publish, or \"all\"")
	c.Flags().StringVar(&o.registry, "registry", "ghcr.io", "OCI registry host")
	c.Flags().StringVar(&o.owner, "owner", "", "registry namespace owner (lowercased org)")
	c.Flags().StringVar(&o.repoPath, "repo-path", "charts", "repository path prefix under the owner")
	c.Flags().StringVar(&o.destDir, "dest", "/tmp/charts", "directory for packaged .tgz files")
	c.Flags().IntVar(&o.retries, "retries", 5, "attempts for each flaky helm push / cosign step")
	c.Flags().IntVar(&interval, "interval", 10, "seconds between retries")
	return c
}

// runCapture runs a command and, on failure, folds its combined output into the
// error — so a cosign/helm "exit status 1" is actually debuggable in CI logs
// (exec.Command(...).Run() otherwise discards stderr).
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

// retryPC runs fn up to o.retries times, sleeping o.interval between attempts.
func retryPC(o publishChartsOpts, what string, fn func() error) error {
	var err error
	for n := 1; n <= max1(o.retries); n++ {
		if err = fn(); err == nil {
			return nil
		}
		if n < max1(o.retries) {
			fmt.Fprintf(os.Stderr, "::warning::%s failed (attempt %d/%d): %v — retrying in %s\n", what, n, o.retries, err, o.interval)
			pcSleep(o.interval)
		}
	}
	return fmt.Errorf("%s failed after %d attempts: %w", what, max1(o.retries), err)
}

func runPublishCharts(o publishChartsOpts) error {
	if o.owner == "" {
		return fmt.Errorf("publish-charts: --owner is required")
	}
	ociDest := "oci://" + o.registry + "/" + o.owner + "/" + o.repoPath // helm push (needs oci://)
	regDest := o.registry + "/" + o.owner + "/" + o.repoPath            // cosign (bare ref)

	if o.destDir != "" {
		if err := os.MkdirAll(o.destDir, 0o755); err != nil {
			return fmt.Errorf("creating package dir %s: %w", o.destDir, err)
		}
	}

	dirs, err := chartDirs(o.chartsDir)
	if err != nil {
		return fmt.Errorf("listing charts under %s: %w", o.chartsDir, err)
	}

	pushed, resigned := 0, 0
	for _, dir := range dirs {
		name, version, err := pcInspect(dir)
		if err != nil {
			return err
		}
		if o.selected != "all" && o.selected != name {
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
		if err := pcPackage(dir, o.destDir); err != nil {
			return fmt.Errorf("package %s %s: %w", name, version, err)
		}
		tgz := filepath.Join(o.destDir, name+"-"+version+".tgz")

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
