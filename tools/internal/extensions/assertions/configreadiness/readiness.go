package configreadiness

// readiness.go is the FILE-level readiness scan behind `llz doctor --env <env>`
// (and the legacy `llz validate --env`) — the complement to the code-level
// `llz validate` (terraform validate + checkov).
// After `llz env add` scaffolds a deployment, a handful of values still have to be
// hand-filled (the ADOPTER-MUST-SET tfvars + the apl-values overlay placeholders).
// This catches the ones people forget BEFORE they pay for a cluster build:
//
//   * residual scaffold sentinels that survived the swap — REPLACE_PER_ENV /
//     REPLACE_ME, a `your-env` that escaped substitution;
//   * the same sentinels in the first-party chart values that live OUTSIDE the
//     copier scaffold (adopter-guide §5) and so are NOT token-swapped on render —
//     a missed REPLACE_ME gitRepoURL silently points Argo CD at the wrong repo;
//   * an obj_cluster that isn't shaped like a Linode OBJ cluster id (a CIDR, a
//     bare region with no datacenter ordinal — would fail the object-storage apply);
//   * empty github_runner_*_cidrs (a warning — fine for github.com-hosted runners
//     that open their egress IP at runtime via `llz ci runner-acl open`);
//   * an apl-values/<env>/manifest overlay that does not `kubectl kustomize`.
//
// It is layout-aware (instanceLayout) and returns an error on any blocking
// finding so a pre-build gate can rely on it.

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/instancelayout"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/validate"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
)

// quotedTokens pulls the "…" entries out of an HCL list so each can be parsed on
// its own terms rather than string-matched inside the whole line.
var quotedTokens = regexp.MustCompile(`"([^"]*)"`)

// InstanceRepoPlaceholder is the unrendered instance_repo answer. It reaches two
// very different kinds of file, which need two different fixes — see hintFor.
// InstanceRepoPlaceholder is EXPORTED because the CLI's grouping test asserts
// against the same constant this package scans for — one fact, one definition.
const InstanceRepoPlaceholder = "your-org/your-instance-repo"

type sentinel struct {
	token    string
	blocking bool
	hint     string
}

var scaffoldSentinels = []sentinel{
	{"REPLACE_PER_ENV", true, "fill in the per-env value (ACME email, GitOps repoUrl/branch/path, DNS domain)"},
	{"REPLACE_ME", true, "replace the placeholder Helm registry URL / value"},
	{InstanceRepoPlaceholder, true, "repoint to your fork / instance repo (owner/name)"},
	{"your-env", true, "an env token escaped substitution — set it to the deployment name"},
}

// chartValuesFiles are the first-party chart values that live OUTSIDE the copier
// scaffold (adopter-guide §5) and so are NOT token-swapped on render — they carry
// hand-edit placeholders (REPLACE_ME gitRepoURL, your-org/your-instance-repo) that,
// if missed, silently point Argo CD at the wrong repo and fail the bootstrap
// opaquely. Best-effort: absent files (a copier-only instance that pulls the charts
// from OCI) are skipped.
var chartValuesFiles = []string{
	filepath.Join("kubernetes-charts", "llz-argo-bootstrap-apps", "values.yaml"),
	filepath.Join("kubernetes-charts", "llz-cert-automation", "values.yaml"),
	filepath.Join("kubernetes-charts", "llz-openbao-platform", "values.yaml"),
}

// EXPORTED because the scaffolding path groups these for operator output:
// environments.GroupFindings (lifecycle/environments/add.go) collapses findings
// sharing a token AND a remedy. The grouping is PRESENTATION and lives with the
// command that prints it; the finding itself is this package's model. It used to
// be cmd/llz/scaffold.go, which is where the split was first drawn.
type Finding struct {
	File     string
	Line     int
	Token    string
	Hint     string
	Blocking bool
}

// loc renders a finding's position. A spec-level finding has no line — the value
// may be inherited from another file entirely — and printing "file:0" for it
// reads as a bug in the reporter rather than as "somewhere in this file".
func (f Finding) Loc() string {
	if f.Line <= 0 {
		return f.File
	}
	return fmt.Sprintf("%s:%d", f.File, f.Line)
}

func RunEnvReadiness(env string) error {
	if env == "" {
		return fmt.Errorf("--env is required (e.g. --env primary)")
	}
	if err := validateEnvName(env); err != nil {
		return err
	}
	tfDir, aplDir, _ := instancelayout.Detect()

	overlay := filepath.Join(aplDir, env)
	if fi, err := os.Stat(overlay); err != nil || !fi.IsDir() {
		return fmt.Errorf("no scaffold for %q (%s missing) — run `llz env add %s` first", env, overlay, env)
	}

	// Spec-aware gate. When a LandingZone spec is present it's the source of truth,
	// so validate it AND confirm the committed apl-values match what it renders
	// before the file-level scan below — which reads the RENDERED output and would
	// otherwise pass on stale tfvars after a spec edit that wasn't re-rendered.
	specDriven := false // spec present AND this env defined in it
	var specFindings []Finding
	if lz, present, perr := deps.LoadSpec(); present {
		fmt.Println(color.Bold("LandingZone spec:"))
		if perr != nil {
			fmt.Printf("  %s failed to load: %v\n", color.Red("✗"), perr)
			return perr
		}
		if errs := lz.Validate(); len(errs) > 0 {
			for _, e := range errs {
				fmt.Printf("  %s %v\n", color.Red("✗"), e)
			}
			return fmt.Errorf("%d spec problem(s) — fix landingzone.yaml / environments/<env>.yaml, then re-run", len(errs))
		}
		if e, ok := lz.Env(env); ok {
			specDriven = true
			// The spec is the source of truth here, and it is ALWAYS present —
			// unlike the rendered tfvars, which are gitignored build artifacts
			// absent in a fresh clone. Read the ACL off the merged environment
			// (applyInheritance folds spec.defaults in at load), so an open-world
			// prefix is caught wherever it came from: a flag, a hand edit, or a
			// spec.defaults entry that no per-env file even mentions.
			specFindings = openWorldACLFindings(env, e.Cluster.APIServerAllowCIDRs)
			if err := deps.CheckManifestDrift(lz, aplDir, []string{env}); err != nil {
				// checkManifestDrift already printed the drifted files + the
				// `llz render` hint; surface it as the blocking failure.
				return err
			}
			fmt.Printf("  %s valid; committed apl-values for %q in sync with the spec\n\n", color.Green("✓"), env)
		} else {
			fmt.Printf("  %s valid (%q is not in the spec — scanning its committed files directly)\n\n", color.Green("✓"), env)
		}
	}

	files := instancelayout.TFVarsPaths(tfDir, env)
	files = append(files, OverlayScanFiles(overlay)...)
	// apl-values/values.yaml is GONE (LLZ renders no apl-core values on the managed
	// App Platform — ADR 0005), so there is no shared values base left to scan for
	// unfilled placeholders. The scan is kept as a no-op rather than deleted: an
	// instance that has not yet run `llz upgrade` still carries the file, and a
	// REPLACE_ME left in it is worth reporting even though nothing reads it — the
	// operator needs to know it is there before it disappears. (The ACME email is
	// no longer a placeholder: it renders to a valid `email: ""` by default — the
	// contact is optional. The cert/DNS tree that used to carry the REPLACE_ME
	// webhook repoURL now lives at platform-apl/ and is fetched as a pinned remote
	// ref, so it is no longer part of the instance.)
	files = append(files, OverlayScanFiles(filepath.Join(aplDir, "values.yaml"))...)
	for _, cf := range chartValuesFiles {
		if fi, err := os.Stat(cf); err == nil && !fi.IsDir() {
			files = append(files, cf)
		}
	}

	fmt.Printf("%s\n\n", color.Bold(fmt.Sprintf("Deployment %q readiness (%s + %s):", env, tfDir, aplDir)))

	findings := specFindings
	missing := 0
	for _, f := range files {
		// The rendered-tfvars ACL scan is for LEGACY instances only: a spec-driven
		// one was already read from its spec above, and scanning both would report
		// the same prefix twice, in two files, for one mistake.
		fs, present := ScanForSentinels(f, !specDriven)
		if !present {
			// A spec-driven env renders its tfvars on demand (they are gitignored
			// build artifacts), so an absent <env>.tfvars is normal, not a finding —
			// the spec was already validated above. Legacy/manual instances still
			// flag a genuinely missing tfvars — except for an OPT-IN root, which an
			// instance may deliberately never apply (see optionalTFRoots).
			if strings.HasSuffix(f, ".tfvars") && !specDriven && !instancelayout.Optional(f) {
				fmt.Printf("  %s  %s %s\n", color.Red("✗ missing"), f, color.Dim("— run `llz env add "+env+"`"))
				missing++
			}
			continue
		}
		findings = append(findings, fs...)
	}

	blocking := 0
	if len(findings) == 0 && missing == 0 {
		fmt.Println("  " + color.Green("✓") + " no residual scaffold placeholders in the tfvars or overlay")
	}
	// DNS/cert overlay placeholders are DEFERRABLE, not blocking: they configure
	// cert-manager DNS-01 issuance (the Argo-synced letsencrypt ClusterIssuers),
	// which only needs the ACME email + TF_VAR_linode_dns_token — settable after the
	// first build. Split them out so the build isn't gated on them.
	var deferred []Finding
	for _, f := range findings {
		if isDeferrable(f.File) {
			deferred = append(deferred, f)
			continue
		}
		mark := color.Yellow("⚠ warn ")
		if f.Blocking {
			mark = color.Red("✗ TODO ")
			blocking++
		}
		fmt.Printf("  %s %s  %s %s\n", mark, f.Loc(), f.Token, color.Dim("— "+f.Hint))
	}

	// obj_cluster must be shaped like a Linode OBJ cluster id — catch a malformed
	// value here, before the object-storage apply fails on it.
	objTfv := filepath.Join(tfDir, "object-storage", env+".tfvars")
	if b, err := os.ReadFile(objTfv); err == nil {
		if v := tfvarsValue(string(b), "obj_cluster"); v != "" {
			if err := instancelayout.ValidateOBJCluster(v); err != nil {
				fmt.Printf("  %s  %s %s\n", color.Red("✗ TODO"), objTfv, color.Dim("— "+err.Error()))
				blocking++
			}
		}
	}

	// Cross-file consistency: the deployment discriminator must equal the env name
	// in every root. `llz env add` sets these, but a hand-edit or a tfvars copied
	// from a sibling deployment silently desyncs them — Terraform then deploys under
	// a mismatched state key / reads the wrong apl-values overlay. Catch it here.
	for _, dc := range []struct{ root, key string }{
		{"cluster-bootstrap", "deployment"},
		{"cluster-bootstrap", "apl_values_env"},
		{"object-storage", "region_suffix"},
	} {
		p := filepath.Join(tfDir, dc.root, env+".tfvars")
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if v := tfvarsValue(string(b), dc.key); v != "" && v != env {
			fmt.Printf("  %s  %s  %s\n", color.Red("✗ TODO"), p,
				color.Dim(fmt.Sprintf("%s = %q must equal the deployment name %q (mismatched discriminator → wrong TF state key / overlay)", dc.key, v, env)))
			blocking++
		}
	}

	fmt.Println()
	switch err := renderOverlay(overlay); {
	case err == nil:
		fmt.Printf("  %s kubectl kustomize %s/manifest renders\n", color.Green("✓"), overlay)
	case isMissingBinary(err):
		fmt.Printf("  %s kubectl not found — skipped overlay render (install kubectl to enable)\n", color.Dim("–"))
	default:
		fmt.Printf("  %s kubectl kustomize %s/manifest failed:\n%s\n", color.Red("✗"), overlay, Indent(err.Error(), "      "))
		blocking++
	}

	// Deferred (non-blocking) cert/DNS placeholders, surfaced on their own so it's
	// obvious the build can proceed now and DNS-01 is finished later.
	if len(deferred) > 0 {
		fmt.Println("\n" + color.Bold("Deferred — cert/DNS issuance (non-blocking; set up after the build):"))
		for _, f := range deferred {
			fmt.Printf("  %s %s  %s %s\n", color.Cyan("○ later"), f.Loc(), f.Token, color.Dim("— "+f.Hint))
		}
		fmt.Println("  " + color.Dim("↳ fine to leave for now — set the ACME email + TF_VAR_linode_dns_token; the Argo-synced letsencrypt ClusterIssuers issue certs once both exist (quickstart §4)."))
	}

	fmt.Println()
	if blocking > 0 || missing > 0 {
		tail := ""
		if len(deferred) > 0 {
			tail = fmt.Sprintf(" (the %d cert/DNS item(s) above are deferred — leave them for now)", len(deferred))
		}
		return fmt.Errorf("%d blocking issue(s) — fill the values above, then re-run `llz doctor --env %s`%s", blocking+missing, env, tail)
	}
	if len(deferred) > 0 {
		fmt.Printf("%s deployment %q is ready to build (`llz build %s --yes`) %s\n",
			color.Green("✓"), env, env, color.Dim(fmt.Sprintf("— %d cert/DNS item(s) deferred (set ACME email + TF_VAR_linode_dns_token)", len(deferred))))
		return nil
	}
	fmt.Printf("%s deployment %q is ready to build (`llz build %s --yes`).\n", color.Green("✓"), env, env)
	return nil
}

// isDeferrable reports whether a readiness finding lives in the cert/DNS tree
// (manifest/dns/...). Those placeholders configure cert-manager
// DNS-01 issuance (the Argo-synced letsencrypt ClusterIssuers), which is settable
// after the first build (quickstart §4), so they must not block the apply.
func isDeferrable(file string) bool {
	return strings.Contains(filepath.ToSlash(file), "/manifest/dns/")
}

// ScaffoldExists reports whether an apl-values overlay directory exists for env.
// `llz doctor` uses it to decide whether to run the file-level readiness scan for
// its default env without erroring when no such deployment has been scaffolded.
func ScaffoldExists(env string) bool {
	if env == "" {
		return false
	}
	_, aplDir, _ := instancelayout.Detect()
	fi, err := os.Stat(filepath.Join(aplDir, env))
	return err == nil && fi.IsDir()
}

// OverlayScanFiles returns the overlay's regular files except Markdown docs
// (READMEs legitimately mention the sentinels).
func OverlayScanFiles(overlay string) []string {
	var out []string
	_ = filepath.Walk(overlay, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() && !strings.EqualFold(filepath.Ext(p), ".md") {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// ScanForSentinels reports whether the file exists and any sentinel / empty-CIDR
// findings. Comment-only lines (trimmed starts with '#') are skipped — the
// sentinels appear there as documentation, not as unfilled values.
func ScanForSentinels(path string, tfvarsACL bool) ([]Finding, bool) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer fh.Close()
	var out []Finding
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	ln := 0
	isTfvars := strings.HasSuffix(path, ".tfvars")
	for sc.Scan() {
		ln++
		line := sc.Text()
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		for _, s := range scaffoldSentinels {
			if strings.Contains(line, s.token) {
				out = append(out, Finding{path, ln, s.token, hintFor(s, path), s.blocking})
			}
		}
		if isTfvars && isEmptyCIDRList(line) {
			out = append(out, Finding{path, ln, strings.TrimSpace(line),
				"empty runner CIDR list — fine for github.com-hosted runners (they open their egress IP at runtime via `llz ci runner-acl open`); fill it for self-hosted runners with a fixed range", false})
		}
		// `llz env add` rejects an open-world ACL at the flag, but the spec is a
		// file: `llz env edit`, a hand edit, or an inherited spec.defaults reaches
		// the rendered tfvars without passing that check. Report it here too — as a
		// finding, not a blocker, because an instance that already has one must
		// still be able to render and build while it fixes it.
		if isTfvars && tfvarsACL && isOpenWorldCIDRLine(line) {
			out = append(out, Finding{path, ln, strings.TrimSpace(line), openWorldACLHint, false})
		}
	}
	return out, true
}

// isEmptyCIDRList matches `github_runner_ipv{4,6}_cidrs = []` (ignoring inline comments).
func isEmptyCIDRList(line string) bool {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "github_runner_ipv4_cidrs") && !strings.HasPrefix(t, "github_runner_ipv6_cidrs") {
		return false
	}
	eq := strings.Index(t, "=")
	if eq < 0 {
		return false
	}
	rhs := t[eq+1:]
	if h := strings.Index(rhs, "#"); h >= 0 {
		rhs = rhs[:h]
	}
	return strings.TrimSpace(rhs) == "[]"
}

// openWorldACLHint is the one explanation both ACL paths give, so the spec-level
// check and the legacy tfvars scan cannot drift apart on what the problem is.
const openWorldACLHint = "the control-plane ACL admits every address — LKE-E accepts this and the apply " +
	"succeeds, leaving the Kubernetes API server reachable from the whole internet; replace it with your " +
	"operator/CI egress prefixes (or empty it for github.com-hosted runners)"

// openWorldACLFindings reports every all-addresses prefix in a MERGED
// environment's apiServerAllowCIDRs. The file is named as the spec rather than a
// rendered artifact, because that is where the operator has to fix it — and,
// when the value is inherited, environments/<env>.yaml will not even contain it
// (spec.defaults in landingzone.yaml will).
func openWorldACLFindings(env string, acl clusterspec.AllowCIDRs) []Finding {
	var out []Finding
	file := filepath.Join(clusterspec.EnvironmentsDir, env+".yaml")
	for _, list := range [][]string{acl.IPv4, acl.IPv6} {
		for _, c := range list {
			if validate.IsOpenWorldCIDR(c) {
				out = append(out, Finding{file, 0, c, openWorldACLHint + " (or in spec.defaults, if it is inherited)", false})
			}
		}
	}
	return out
}

// hintFor adapts a sentinel's remediation to WHERE it was found.
//
// The instance-repo placeholder is the case that matters. It appears both in the
// chart values an adopter genuinely hand-edits (adopter-guide §5) AND, until the
// spec is repointed, in every rendered apl-values file — six or more of them.
// The generic "repoint to your fork" hint sends the operator to hand-edit all of
// them, and `llz render` then overwrites the lot: the value is RENDERED from
// spec.instance.repo, so the fix is one command, once, and the checklist should
// say which one.
func hintFor(s sentinel, path string) string {
	if s.token != InstanceRepoPlaceholder {
		return s.hint
	}
	if _, aplDir, _ := instancelayout.Detect(); strings.HasPrefix(filepath.Clean(path), filepath.Clean(aplDir)+string(filepath.Separator)) {
		return "rendered from spec.instance.repo — fix it once with `llz spec set instance.repo=<owner>/<name>` " +
			"(editing this file by hand is undone by the next `llz render`)"
	}
	return s.hint
}

// isOpenWorldCIDRLine matches a `github_runner_ipv{4,6}_cidrs` assignment whose
// list contains an all-addresses prefix. It reads the rendered tfvars rather than
// the spec so a value that arrived by ANY route — flag, `llz env edit`, hand
// edit, inherited spec.defaults — is seen the same way.
func isOpenWorldCIDRLine(line string) bool {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "github_runner_ipv4_cidrs") && !strings.HasPrefix(t, "github_runner_ipv6_cidrs") {
		return false
	}
	eq := strings.Index(t, "=")
	if eq < 0 {
		return false
	}
	rhs := t[eq+1:]
	if h := strings.Index(rhs, "#"); h >= 0 {
		rhs = rhs[:h]
	}
	// Parse each quoted entry rather than string-matching "0.0.0.0/0": a /0 has
	// many spellings ("0::/0", "203.0.113.7/0") that all mask to everything while
	// reading like a specific host. One rule, in internal/validate.
	for _, m := range quotedTokens.FindAllStringSubmatch(rhs, -1) {
		if validate.IsOpenWorldCIDR(m[1]) {
			return true
		}
	}
	return false
}

// renderOverlay runs `kubectl kustomize <overlay>/manifest`, returning a non-nil
// error (with stderr) on failure.
func renderOverlay(overlay string) error {
	cmd := exec.Command("kubectl", "kustomize", filepath.Join(overlay, "manifest"))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if isMissingBinary(err) {
			return err
		}
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%s", msg)
		}
		return err
	}
	return nil
}

func isMissingBinary(err error) bool {
	_, ok := err.(*exec.Error)
	return ok
}

func Indent(s, pad string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := range lines {
		lines[i] = pad + lines[i]
	}
	return strings.Join(lines, "\n")
}
