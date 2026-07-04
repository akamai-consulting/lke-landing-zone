package main

// ci_chart_publish_check.go implements `llz ci chart-publish-check` — a runtime
// companion to chart-pin-guard. Where chart-pin-guard asserts a pinned first-party
// chart version MATCHES the local Chart.yaml (offline, PR-time), THIS asserts the
// pinned version actually EXISTS in the OCI registry ArgoCD will pull it from.
//
// Why it exists: publish-charts.yml pushes charts only on merge to main, but
// chart-version-guard forces a version bump the moment a chart changes on a branch.
// So a feature-branch e2e pins e.g. llz-cluster-foundation:0.1.6 that GHCR does not
// have yet; ArgoCD 404s the OCI pull, the support-plane app never syncs, the
// llz-openbao namespace is never created, and the OpenBao bootstrap dies deep in on
// `namespaces "llz-openbao" not found` — a cryptic failure ~15 minutes into the run.
// As a preflight this turns that into an immediate, explicit "publish these charts
// first"; with --publish-if-missing (used by release-e2e's instantiate) it instead
// dispatches publish-charts.yml on the branch and waits for the pins to land — the
// chart analog of `pin-instance-images --build-if-missing`, so a branch e2e
// self-heals instead of forcing a manual publish + re-run.
//
// The scan + registry-ref parsing are pure and unit-tested; the registry HTTP call,
// the workflow dispatch, and the wait are reached only through package-var seams.

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var (
	pubChartRe = regexp.MustCompile(`^(\s*)chart:\s*(\S+)\s*$`)
	// A repoURL still carrying a copier placeholder (e.g. `<@ upstream_org @>`) is
	// an unrendered template — skip it rather than fail a registry lookup on it.
	pubPlaceholderRe = regexp.MustCompile(`<@|<%|{{`)
)

// publishPin is a first-party chart version pin found in an Argo Application source.
type publishPin struct {
	RepoURL string
	Chart   string
	Version string
	File    string
	Line    int // 1-based line of the `chart:` line
}

// Seams (package vars) so tests drive the flow without a registry or gh.
var (
	// chartPublishedFn reports whether host/repoPath:version resolves to a manifest.
	chartPublishedFn = ghcrChartPublished
	// chartDispatchPublish kicks off the publish-charts workflow on ref (needs an
	// actions:write token) so an unpublished pin self-heals instead of failing.
	chartDispatchPublish = func(token, templateRepo, ref string) error {
		cmd := exec.Command("gh", "workflow", "run", "publish-charts.yml",
			"--repo", templateRepo, "--ref", ref, "-f", "chart=all")
		cmd.Env = append(os.Environ(), "GH_TOKEN="+token)
		return cmd.Run()
	}
	chartPublishSleep = func(d time.Duration) { time.Sleep(d) }
)

// chartPublishOpts carries the check + optional self-heal configuration.
type chartPublishOpts struct {
	root                     string
	publishIfMissing         bool
	ref, templateRepo, token string
	interval                 time.Duration
	retries                  int
	published                func(host, repoPath, version string) (bool, error)
	dispatch                 func(token, templateRepo, ref string) error
	sleep                    func(time.Duration)
}

func ciChartPublishCheckCmd() *cobra.Command {
	var root, ref, templateRepo string
	var publishIfMissing bool
	var interval, timeout int
	c := &cobra.Command{
		Use:   "chart-publish-check",
		Short: "verify (or publish + wait for) the pinned first-party (llz-*) chart versions in GHCR",
		Long: "Scans the apl-values Argo Application manifests for first-party (llz-*) chart\n" +
			"pins (repoURL + chart + targetRevision/version) and fails if any pinned version\n" +
			"is not present in its OCI registry. A pin the registry never received 404s at\n" +
			"Argo sync time — on a cold bootstrap that silently strands the support-plane app\n" +
			"and times out the OpenBao bootstrap on `namespaces \"llz-openbao\" not found`.\n" +
			"As a preflight, an unpublished chart fails fast, not 15 minutes in. With\n" +
			"--publish-if-missing it instead dispatches publish-charts.yml on --ref and waits\n" +
			"for the pins to land (the chart analog of `pin-instance-images --build-if-missing`).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runChartPublishCheck(chartPublishOpts{
				root: root, publishIfMissing: publishIfMissing, ref: ref, templateRepo: templateRepo,
				// GHCR reads use whatever ghcrChartPublished finds in env; the DISPATCH
				// needs actions:write, so prefer the workflow token over a read-only PAT.
				token:     firstNonEmptyEnv("GH_TOKEN", "GITHUB_TOKEN", "GHCR_READ_TOKEN"),
				interval:  time.Duration(interval) * time.Second,
				retries:   timeout / cpMax1(interval),
				published: chartPublishedFn, dispatch: chartDispatchPublish, sleep: chartPublishSleep,
			})
		},
	}
	c.Flags().StringVar(&root, "root", ".", "repository root to scan for apl-values chart pins")
	c.Flags().BoolVar(&publishIfMissing, "publish-if-missing", false, "if a pinned chart is unpublished, dispatch publish-charts.yml on --ref and wait — instead of failing")
	c.Flags().StringVar(&ref, "ref", "", "branch/tag to dispatch publish-charts.yml on (required with --publish-if-missing)")
	c.Flags().StringVar(&templateRepo, "template-repo", "", "owner/name of the repo hosting publish-charts.yml (required with --publish-if-missing)")
	c.Flags().IntVar(&interval, "interval", 20, "seconds between registry re-checks while waiting for a publish")
	c.Flags().IntVar(&timeout, "timeout", 600, "max seconds to wait for the dispatched charts to publish")
	return c
}

func cpMax1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// collectMissingPins returns the de-duplicated first-party pins whose version is
// absent from the registry (skipping non-ghcr hosts + unparseable refs) and the
// number actually checked.
func collectMissingPins(pins []publishPin, published func(host, repoPath, version string) (bool, error)) (missing []publishPin, checked int, err error) {
	seen := map[string]publishPin{}
	for _, p := range pins {
		seen[p.RepoURL+"|"+p.Chart+"|"+p.Version] = p
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		p := seen[k]
		host, repoPath, perr := parseOCIRef(p.RepoURL, p.Chart)
		if perr != nil {
			fmt.Fprintf(os.Stderr, "skip %s (%s): %v\n", p.Chart, p.RepoURL, perr)
			continue
		}
		if host != "ghcr.io" {
			continue // only GHCR publication is checked here; other hosts are out of scope
		}
		ok, cerr := published(host, repoPath, p.Version)
		if cerr != nil {
			return nil, 0, fmt.Errorf("checking %s:%s in %s: %w", p.Chart, p.Version, host, cerr)
		}
		checked++
		if !ok {
			missing = append(missing, p)
		}
	}
	return missing, checked, nil
}

func printMissingChart(m publishPin) {
	fmt.Fprintf(os.Stderr,
		"::error file=%s,line=%d::%s:%s is pinned in apl-values but not published to %s — "+
			"ArgoCD will 404 the OCI pull, the support-plane app will never sync, and the "+
			"llz-openbao namespace will never be created. Publish it first: run publish-charts.yml "+
			"(workflow_dispatch) on this branch.\n",
		m.File, m.Line, m.Chart, m.Version, m.RepoURL)
}

func runChartPublishCheck(o chartPublishOpts) error {
	pins, err := scanPublishPins(o.root)
	if err != nil {
		return fmt.Errorf("scanning apl-values chart pins: %w", err)
	}
	missing, checked, err := collectMissingPins(pins, o.published)
	if err != nil {
		return err
	}
	if len(missing) == 0 {
		fmt.Printf("chart-publish-check: %d pinned first-party chart(s) are published.\n", checked)
		return nil
	}

	// Preflight mode: report and fail (the operator publishes + re-runs).
	if !o.publishIfMissing {
		for _, m := range missing {
			printMissingChart(m)
		}
		return fmt.Errorf("chart-publish-check: %d pinned first-party chart(s) not in the registry", len(missing))
	}

	// Self-heal mode: dispatch publish-charts on the branch and wait for the pins.
	if o.ref == "" || o.templateRepo == "" {
		return fmt.Errorf("--publish-if-missing requires --ref and --template-repo")
	}
	names := make([]string, len(missing))
	for i, m := range missing {
		names[i] = m.Chart + ":" + m.Version
	}
	fmt.Printf("chart-publish-check: %d chart(s) unpublished (%s) — dispatching publish-charts.yml on %s and waiting...\n",
		len(missing), strings.Join(names, ", "), o.ref)
	if err := o.dispatch(o.token, o.templateRepo, o.ref); err != nil {
		return fmt.Errorf("dispatching publish-charts.yml on %s: %w", o.ref, err)
	}
	for i := 0; i < cpMax1(o.retries); i++ {
		o.sleep(o.interval)
		still, _, cerr := collectMissingPins(missing, o.published)
		if cerr != nil {
			return cerr
		}
		if len(still) == 0 {
			fmt.Printf("chart-publish-check: all %d chart(s) published after dispatch.\n", len(missing))
			return nil
		}
		missing = still
	}
	for _, m := range missing {
		printMissingChart(m)
	}
	return fmt.Errorf("chart-publish-check: %d chart(s) still unpublished after waiting for publish-charts.yml", len(missing))
}

// scanPublishPins walks root for apl-values YAML and returns every first-party
// (llz-*) chart pin whose repoURL is rendered (not a copier placeholder).
func scanPublishPins(root string) ([]publishPin, error) {
	var pins []publishPin
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "templates", "node_modules":
				return fs.SkipDir
			}
			return nil
		}
		// Only Argo Application manifests live under an apl-values/ tree.
		if !strings.Contains(filepath.ToSlash(path), "apl-values/") {
			return nil
		}
		if ext := filepath.Ext(path); ext != ".yaml" && ext != ".yml" {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for _, p := range extractPublishPins(string(b)) {
			if !strings.HasPrefix(p.Chart, "llz-") {
				continue // only first-party charts are published to our registry
			}
			if pubPlaceholderRe.MatchString(p.RepoURL) {
				continue // unrendered template placeholder
			}
			rel, _ := filepath.Rel(root, path)
			p.File = filepath.ToSlash(rel)
			pins = append(pins, p)
		}
		return nil
	})
	return pins, err
}

// extractPublishPins pairs each `chart: <name>` line with its sibling `repoURL:`
// and `targetRevision:`/`version:` keys in the same source block (same indent).
func extractPublishPins(content string) []publishPin {
	lines := strings.Split(content, "\n")
	var pins []publishPin
	for i, line := range lines {
		m := pubChartRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		indent, name := m[1], strings.Trim(m[2], `"'`)
		repoURL := siblingValue(lines, i, indent, "repoURL")
		version := siblingValue(lines, i, indent, "targetRevision")
		if version == "" {
			version = siblingValue(lines, i, indent, "version")
		}
		if repoURL != "" && version != "" {
			pins = append(pins, publishPin{RepoURL: repoURL, Chart: name, Version: version, Line: i + 1})
		}
	}
	return pins
}

// siblingValue returns the value of `<indent>key: <value>` in the contiguous block
// around idx at exactly the given indentation, scanning both directions and
// stopping at the first line that dedents below indent.
func siblingValue(lines []string, idx int, indent, key string) string {
	want := len(indent)
	prefix := indent + key + ":"
	scan := func(step int) string {
		for j := idx + step; j >= 0 && j < len(lines); j += step {
			ln := lines[j]
			if strings.TrimSpace(ln) == "" {
				continue // blank lines don't break a block
			}
			if leadingIndent(ln) < want {
				return "" // dedented out of the source block
			}
			if leadingIndent(ln) == want && strings.HasPrefix(ln, prefix) {
				return strings.Trim(strings.TrimSpace(strings.TrimPrefix(ln, prefix)), `"'`)
			}
		}
		return ""
	}
	if v := scan(-1); v != "" {
		return v
	}
	return scan(1)
}

// leadingIndent counts the leading spaces/tabs of a line.
func leadingIndent(s string) int {
	return len(s) - len(strings.TrimLeft(s, " \t"))
}

// parseOCIRef splits a chart repoURL + chart name into a registry host and the
// v2 repository path. e.g. ("ghcr.io/acme/charts", "llz-foo") ->
// ("ghcr.io", "acme/charts/llz-foo").
func parseOCIRef(repoURL, chart string) (host, repoPath string, err error) {
	r := repoURL
	for _, s := range []string{"oci://", "https://", "http://"} {
		r = strings.TrimPrefix(r, s)
	}
	r = strings.Trim(r, "/")
	parts := strings.SplitN(r, "/", 2)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("repoURL %q has no registry host + path", repoURL)
	}
	return parts[0], parts[1] + "/" + chart, nil
}

// ghcrChartPublished reports whether host/repoPath:version resolves to a manifest,
// using an anonymous pull token (GITHUB_TOKEN/GH_TOKEN upgrades it for private
// packages / rate limits). A 404 means unpublished; any other non-2xx is an error.
func ghcrChartPublished(host, repoPath, version string) (bool, error) {
	client := &http.Client{Timeout: 20 * time.Second}

	tokURL := fmt.Sprintf("https://%s/token?service=%s&scope=repository:%s:pull", host, host, repoPath)
	treq, _ := http.NewRequest(http.MethodGet, tokURL, nil)
	// The first-party charts may be a PRIVATE GHCR repo (ArgoCD reads them with a
	// read:packages PAT). Use the same credential when present; fall back to an
	// anonymous pull token (works when the charts are public). Username is ignored
	// by the GHCR token endpoint, so any non-empty value works.
	if t := firstNonEmptyEnv("GHCR_READ_TOKEN", "GHCR_TOKEN", "GITHUB_TOKEN", "GH_TOKEN"); t != "" {
		user := firstNonEmptyEnv("GHCR_USERNAME")
		if user == "" {
			user = "x"
		}
		treq.SetBasicAuth(user, t)
	}
	tresp, err := client.Do(treq)
	if err != nil {
		return false, err
	}
	defer tresp.Body.Close()
	if tresp.StatusCode/100 != 2 {
		return false, fmt.Errorf("token endpoint returned %d", tresp.StatusCode)
	}
	var tok struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(tresp.Body).Decode(&tok); err != nil {
		return false, fmt.Errorf("decoding pull token: %w", err)
	}

	manURL := fmt.Sprintf("https://%s/v2/%s/manifests/%s", host, repoPath, version)
	mreq, _ := http.NewRequest(http.MethodHead, manURL, nil)
	mreq.Header.Set("Authorization", "Bearer "+tok.Token)
	mreq.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, "+
		"application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json")
	mresp, err := client.Do(mreq)
	if err != nil {
		return false, err
	}
	defer mresp.Body.Close()
	switch {
	case mresp.StatusCode/100 == 2:
		return true, nil
	case mresp.StatusCode == http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("manifest HEAD returned %d", mresp.StatusCode)
	}
}

func firstNonEmptyEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}
