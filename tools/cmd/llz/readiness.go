package main

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
	"strings"
)

type sentinel struct {
	token    string
	blocking bool
	hint     string
}

var scaffoldSentinels = []sentinel{
	{"REPLACE_PER_ENV", true, "fill in the per-env value (ACME email, GitOps repoUrl/branch/path, DNS domain)"},
	{"REPLACE_ME", true, "replace the placeholder Helm registry URL / value"},
	{"your-org/your-instance-repo", true, "repoint to your fork / instance repo (owner/name)"},
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

type finding struct {
	file     string
	line     int
	token    string
	hint     string
	blocking bool
}

func runEnvReadiness(env string) error {
	if env == "" {
		return fmt.Errorf("--env is required (e.g. --env primary)")
	}
	if err := validateEnvName(env); err != nil {
		return err
	}
	tfDir, aplDir, _ := instanceLayout()

	overlay := filepath.Join(aplDir, env)
	if fi, err := os.Stat(overlay); err != nil || !fi.IsDir() {
		return fmt.Errorf("no scaffold for %q (%s missing) — run `llz env add %s` first", env, overlay, env)
	}

	files := tfvarsPaths(tfDir, env)
	files = append(files, overlayScanFiles(overlay)...)
	for _, cf := range chartValuesFiles {
		if fi, err := os.Stat(cf); err == nil && !fi.IsDir() {
			files = append(files, cf)
		}
	}

	fmt.Printf("%s\n\n", bold(fmt.Sprintf("Deployment %q readiness (%s + %s):", env, tfDir, aplDir)))

	var findings []finding
	missing := 0
	for _, f := range files {
		fs, present := scanForSentinels(f)
		if !present {
			if strings.HasSuffix(f, ".tfvars") {
				fmt.Printf("  %s  %s %s\n", red("✗ missing"), f, dim("— run `llz env add "+env+"`"))
				missing++
			}
			continue
		}
		findings = append(findings, fs...)
	}

	blocking := 0
	if len(findings) == 0 && missing == 0 {
		fmt.Println("  " + green("✓") + " no residual scaffold placeholders in the tfvars or overlay")
	}
	// DNS/cert overlay placeholders are DEFERRABLE, not blocking: they configure
	// cert-manager DNS-01 issuance, which `llz bootstrap dns` provisions AFTER the
	// first build (quickstart §4). Split them out so the build isn't gated on them.
	var deferred []finding
	for _, f := range findings {
		if isDeferrable(f.file) {
			deferred = append(deferred, f)
			continue
		}
		mark := yellow("⚠ warn ")
		if f.blocking {
			mark = red("✗ TODO ")
			blocking++
		}
		fmt.Printf("  %s %s:%d  %s %s\n", mark, f.file, f.line, f.token, dim("— "+f.hint))
	}

	// obj_cluster must be shaped like a Linode OBJ cluster id — catch a malformed
	// value here, before the object-storage apply fails on it.
	objTfv := filepath.Join(tfDir, "object-storage", env+".tfvars")
	if b, err := os.ReadFile(objTfv); err == nil {
		if v := tfvarsValue(string(b), "obj_cluster"); v != "" {
			if err := validateOBJCluster(v); err != nil {
				fmt.Printf("  %s  %s %s\n", red("✗ TODO"), objTfv, dim("— "+err.Error()))
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
			fmt.Printf("  %s  %s  %s\n", red("✗ TODO"), p,
				dim(fmt.Sprintf("%s = %q must equal the deployment name %q (mismatched discriminator → wrong TF state key / overlay)", dc.key, v, env)))
			blocking++
		}
	}

	fmt.Println()
	switch err := renderOverlay(overlay); {
	case err == nil:
		fmt.Printf("  %s kubectl kustomize %s/manifest renders\n", green("✓"), overlay)
	case isMissingBinary(err):
		fmt.Printf("  %s kubectl not found — skipped overlay render (install kubectl to enable)\n", dim("–"))
	default:
		fmt.Printf("  %s kubectl kustomize %s/manifest failed:\n%s\n", red("✗"), overlay, indent(err.Error(), "      "))
		blocking++
	}

	// Deferred (non-blocking) cert/DNS placeholders, surfaced on their own so it's
	// obvious the build can proceed now and DNS-01 is finished later.
	if len(deferred) > 0 {
		fmt.Println("\n" + bold("Deferred — cert/DNS issuance (non-blocking; set up after the build):"))
		for _, f := range deferred {
			fmt.Printf("  %s %s:%d  %s %s\n", cyan("○ later"), f.file, f.line, f.token, dim("— "+f.hint))
		}
		fmt.Println("  " + dim("↳ fine to leave for now — run `llz bootstrap dns "+env+" --yes` once LINODE_DNS_TOKEN exists (quickstart §4)."))
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
			green("✓"), env, env, dim(fmt.Sprintf("— %d cert/DNS item(s) deferred to `llz bootstrap dns`", len(deferred))))
		return nil
	}
	fmt.Printf("%s deployment %q is ready to build (`llz build %s --yes`).\n", green("✓"), env, env)
	return nil
}

// isDeferrable reports whether a readiness finding lives in the cert/DNS overlay
// (apl-values/<env>/manifest/dns/...). Those placeholders configure cert-manager
// DNS-01 issuance, which `llz bootstrap dns` provisions AFTER the first build
// (quickstart §4), so they must not block the apply.
func isDeferrable(file string) bool {
	return strings.Contains(filepath.ToSlash(file), "/manifest/dns/")
}

// scaffoldExists reports whether an apl-values overlay directory exists for env.
// `llz doctor` uses it to decide whether to run the file-level readiness scan for
// its default env without erroring when no such deployment has been scaffolded.
func scaffoldExists(env string) bool {
	if env == "" {
		return false
	}
	_, aplDir, _ := instanceLayout()
	fi, err := os.Stat(filepath.Join(aplDir, env))
	return err == nil && fi.IsDir()
}

// overlayScanFiles returns the overlay's regular files except Markdown docs
// (READMEs legitimately mention the sentinels).
func overlayScanFiles(overlay string) []string {
	var out []string
	_ = filepath.Walk(overlay, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() && !strings.EqualFold(filepath.Ext(p), ".md") {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// scanForSentinels reports whether the file exists and any sentinel / empty-CIDR
// findings. Comment-only lines (trimmed starts with '#') are skipped — the
// sentinels appear there as documentation, not as unfilled values.
func scanForSentinels(path string) ([]finding, bool) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer fh.Close()
	var out []finding
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
				out = append(out, finding{path, ln, s.token, s.hint, s.blocking})
			}
		}
		if isTfvars && isEmptyCIDRList(line) {
			out = append(out, finding{path, ln, strings.TrimSpace(line),
				"empty runner CIDR list — fine for github.com-hosted runners (they open their egress IP at runtime via `llz ci runner-acl open`); fill it for self-hosted runners with a fixed range", false})
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

func indent(s, pad string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := range lines {
		lines[i] = pad + lines[i]
	}
	return strings.Join(lines, "\n")
}
