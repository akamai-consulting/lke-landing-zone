package chartpublish

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

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/charty"
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

// Opts carries the check + optional self-heal configuration.
type Opts struct {
	Root                     string
	PublishIfMissing         bool
	Ref, TemplateRepo, Token string
	Interval                 time.Duration
	Retries                  int
	Published                func(host, repoPath, version string) (bool, error)
	Dispatch                 func(token, templateRepo, ref string) error
	Sleep                    func(time.Duration)
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

func Run(o Opts) error {
	pins, err := scanPublishPins(o.Root)
	if err != nil {
		return fmt.Errorf("scanning chart pins: %w", err)
	}
	// With the scan trees corrected, finding nothing means the pins moved again —
	// not that everything is published. Refuse to report success having checked
	// none; that vacuous color.Green is what hid this bug on every run.
	if len(pins) == 0 {
		return fmt.Errorf("chart-publish-check: found no first-party chart pins under %s (searched %s) — refusing to report every chart published having checked none",
			o.Root, strings.Join(publishPinTrees, ", "))
	}
	missing, checked, err := collectMissingPins(pins, o.Published)
	if err != nil {
		return err
	}
	if len(missing) == 0 {
		fmt.Printf("chart-publish-check: %d pinned first-party chart(s) are published.\n", checked)
		return nil
	}

	// Preflight mode: report and fail (the operator publishes + re-runs).
	if !o.PublishIfMissing {
		for _, m := range missing {
			printMissingChart(m)
		}
		return fmt.Errorf("chart-publish-check: %d pinned first-party chart(s) not in the registry", len(missing))
	}

	// Self-heal mode: dispatch publish-charts on the branch and wait for the pins.
	if o.Ref == "" || o.TemplateRepo == "" {
		return fmt.Errorf("--publish-if-missing requires --ref and --template-repo")
	}
	names := make([]string, len(missing))
	for i, m := range missing {
		names[i] = m.Chart + ":" + m.Version
	}
	fmt.Printf("chart-publish-check: %d chart(s) unpublished (%s) — dispatching publish-charts.yml on %s and waiting...\n",
		len(missing), strings.Join(names, ", "), o.Ref)
	if err := o.Dispatch(o.Token, o.TemplateRepo, o.Ref); err != nil {
		return fmt.Errorf("dispatching publish-charts.yml on %s: %w", o.Ref, err)
	}
	for i := 0; i < cpMax1(o.Retries); i++ {
		o.Sleep(o.Interval)
		still, _, cerr := collectMissingPins(missing, o.Published)
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
// publishPinTrees are the path markers under which first-party chart pins live.
var publishPinTrees = []string{"platform-apl/", "apl-values/", "kubernetes-charts/"}

// underAny reports whether p sits under one of the given path markers.
func underAny(p string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(p, m) {
			return true
		}
	}
	return false
}

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
		// Scan only the trees that hold Argo Application / app-of-apps chart pins.
		//
		// This used to require "apl-values/" alone, which no longer holds a single
		// chart pin — an instance's apl-values/ is just README.md + values.yaml, and
		// the first-party pins live in platform-apl/ (the platform-bootstrap
		// Applications) and kubernetes-charts/ (the app-of-apps component list). The
		// check therefore found zero pins and reported every chart published while
		// verifying none, on every run including the release gate.
		if !underAny(filepath.ToSlash(path), publishPinTrees) {
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
		repoURL := charty.SiblingValue(lines, i, indent, "repoURL")
		version := charty.SiblingValue(lines, i, indent, "targetRevision")
		if version == "" {
			version = charty.SiblingValue(lines, i, indent, "version")
		}
		if repoURL != "" && version != "" {
			pins = append(pins, publishPin{RepoURL: repoURL, Chart: name, Version: version, Line: i + 1})
		}
	}
	return pins
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

	tokVal, err := ghcrPullToken(client, host, repoPath)
	if err != nil {
		return false, err
	}

	manURL := fmt.Sprintf("https://%s/v2/%s/manifests/%s", host, repoPath, version)
	mreq, _ := http.NewRequest(http.MethodHead, manURL, nil)
	mreq.Header.Set("Authorization", "Bearer "+tokVal)
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

// ghcrShouldRetryAnon reports whether a credentialed GHCR token request that
// returned `code` should be retried ANONYMOUSLY. The first-party charts are
// public, so a present-but-invalid GHCR_READ_TOKEN (a 401/403 at the token
// endpoint — an expired/revoked hand-set PAT) must NOT block the check: anonymous
// access still works. Only retry when creds were actually sent and were rejected.
// Pure (unit-tested).
func ghcrShouldRetryAnon(code int, haveCreds bool) bool {
	return haveCreds && (code == http.StatusUnauthorized || code == http.StatusForbidden)
}

// ghcrPullToken fetches a pull-scoped GHCR token for repoPath. It authenticates
// with GHCR_READ_TOKEN/GHCR_TOKEN/GITHUB_TOKEN/GH_TOKEN when present, but falls
// back to an ANONYMOUS token if the credentialed request is rejected (see
// ghcrShouldRetryAnon) — so an expired/optional GHCR credential can no longer
// 403-block a public-chart check (previously the fallback only fired when NO
// credential was set, not when a present one was rejected). A genuinely private
// chart still fails, because the anonymous retry is then denied too.
func ghcrPullToken(client *http.Client, host, repoPath string) (string, error) {
	tokURL := fmt.Sprintf("https://%s/token?service=%s&scope=repository:%s:pull", host, host, repoPath)
	creds := firstNonEmptyEnv("GHCR_READ_TOKEN", "GHCR_TOKEN", "GITHUB_TOKEN", "GH_TOKEN")

	// do issues the token request, optionally with Basic auth, returning the HTTP
	// status and (on 2xx) the decoded pull token. Username is ignored by the GHCR
	// token endpoint but must be non-empty for Basic auth.
	do := func(withCreds bool) (int, string, error) {
		req, _ := http.NewRequest(http.MethodGet, tokURL, nil)
		if withCreds && creds != "" {
			user := firstNonEmptyEnv("GHCR_USERNAME")
			if user == "" {
				user = "x"
			}
			req.SetBasicAuth(user, creds)
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return resp.StatusCode, "", nil
		}
		var tok struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
			return resp.StatusCode, "", fmt.Errorf("decoding pull Token: %w", err)
		}
		return resp.StatusCode, tok.Token, nil
	}

	code, tok, err := do(creds != "")
	if err != nil {
		return "", err
	}
	if code/100 == 2 {
		return tok, nil
	}
	if ghcrShouldRetryAnon(code, creds != "") {
		fmt.Fprintf(os.Stderr, "::warning::GHCR credential rejected (HTTP %d) at the token endpoint; retrying anonymously (first-party charts are public). Rotate or unset GHCR_READ_TOKEN/GHCR_USERNAME.\n", code)
		code2, tok2, err2 := do(false)
		if err2 != nil {
			return "", err2
		}
		if code2/100 == 2 {
			return tok2, nil
		}
		return "", fmt.Errorf("token endpoint returned %d with credentials, %d anonymously", code, code2)
	}
	return "", fmt.Errorf("token endpoint returned %d", code)
}

func firstNonEmptyEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// Defaults returns an Opts with every seam wired to the real thing, and the
// derived fields computed. Callers set only what the flags gave them.
//
// IT EXISTS SO PACKAGE MAIN DOES NOT HAVE TO KNOW THE SEAMS. The first cut of
// this extraction had the cobra command construct Opts field by field, which
// meant exporting chartPublishedFn, chartDispatchPublish, chartPublishSleep and
// the retry arithmetic purely so the flag set could name them — four exported
// symbols whose only caller was the wiring. A constructor on the owning side
// keeps them unexported and leaves main with the two things it actually has:
// flag values and the environment.
func Defaults(o Opts, intervalSecs, timeoutSecs int) Opts {
	o.Interval = time.Duration(intervalSecs) * time.Second
	o.Retries = timeoutSecs / cpMax1(intervalSecs)
	if o.Token == "" {
		// GHCR reads use whatever ghcrChartPublished finds in env; the DISPATCH
		// needs actions:write, so prefer the workflow token over a read-only PAT.
		o.Token = firstNonEmptyEnv("GH_TOKEN", "GITHUB_TOKEN", "GHCR_READ_TOKEN")
	}
	if o.Published == nil {
		o.Published = chartPublishedFn
	}
	if o.Dispatch == nil {
		o.Dispatch = chartDispatchPublish
	}
	if o.Sleep == nil {
		o.Sleep = chartPublishSleep
	}
	return o
}
