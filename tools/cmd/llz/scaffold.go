package main

// scaffold.go ports `template-scripts/new-deployment.sh` into the llz binary so
// `llz env add` works in a rendered instance, which carries NO scripts/ tree (the
// reusable workflows source instance-scripts from a template checkout; copier no
// longer copies any script trees into an instance). The bash version still ships
// for template-repo CI (release-e2e / scaffold-render-check), which runs it from
// a template checkout — this Go port is the same logic for the operator path.
//
// It is layout-aware: in a rendered instance the TF roots + overlays sit at the
// repo root (terraform-iac-bootstrap/, apl-values/); in a template-repo checkout
// they sit under instance-template/. Both share one code path, keyed off the
// detected instanceRoot. Deployments are created dynamically by cloning the
// `example` overlay + each root's terraform.tfvars.example and swapping identity
// tokens — there is no hardcoded env list (mirrors the docs' contract).

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/validate"
)

// envAddOpts mirrors new-deployment.sh's flags, plus the ADOPTER-MUST-SET values
// that used to be "open the file and edit" steps (item 8): supplying them here
// makes `env add → tokens → build` a guided path instead of a hand-edit detour.
type envAddOpts struct {
	templateEnv   string
	region        string
	regionShort   string
	clusterDomain string
	objCluster    string
	// must-set values (empty = leave the scaffold placeholder for the operator)
	k8sVersion       string
	nodeType         string // Linode node type for the pool
	nodeCount        string // pool size (integer; string so empty = leave default)
	runnerIPv4CIDRs  string // comma-separated
	runnerIPv6CIDRs  string // comma-separated
	aplChartVersion  string
	aplValuesRepoURL string
	haRole           string // active | standby | standalone (default: leave example's standalone)
	haGroup          string // HA pair id (required for active/standby)
	network          string // shared VPC name (spec.networks) to attach to; "" = dedicated VPC
	subnetCIDR       string // cluster.network.subnetCIDR (/13 or /14); "" = default
	promotionRank    int    // code-promotion pipeline position; 0 = leave example's 0 (not in a pipeline)
	dryRun           bool
}

// instanceLayout detects where the TF roots + overlays live and returns the
// terraform-iac-bootstrap root, the apl-values root, and the prefix to show in
// operator-facing paths. A template-repo checkout keeps them under
// instance-template/; a rendered instance keeps them at the repo root.
func instanceLayout() (tfDir, aplDir, relPrefix string) {
	if fi, err := os.Stat(filepath.Join("instance-template", "terraform-iac-bootstrap")); err == nil && fi.IsDir() {
		return filepath.Join("instance-template", "terraform-iac-bootstrap"),
			filepath.Join("instance-template", "apl-values"), "instance-template/"
	}
	return "terraform-iac-bootstrap", "apl-values", ""
}

const tplTfvars = "terraform.tfvars.example"

var tfRoots = []string{"cluster", "object-storage"}

// validateOBJCluster catches a value that isn't shaped like a Linode OBJ cluster
// id. The shape rule lives in internal/validate (OBJClusterID) so the LandingZone
// spec validator reuses it.
func validateOBJCluster(v string) error { return validate.OBJClusterID(v) }

func runEnvAdd(g globalOpts, name string, o envAddOpts) error {
	if o.templateEnv == "" {
		o.templateEnv = "example"
	}
	if name == "" {
		return fmt.Errorf("missing <env> argument")
	}
	if err := validateEnvName(name); err != nil {
		return err
	}
	if name == o.templateEnv {
		return fmt.Errorf("new env must differ from --template-env (%s)", o.templateEnv)
	}
	if err := validateHAFlags(o.haRole, o.haGroup); err != nil {
		return err
	}
	// Spec-first must-sets: the spec validates these, so require them up front
	// rather than scaffolding an env that won't render.
	if o.region == "" {
		return fmt.Errorf("--region is required (the spec's cluster.region)")
	}
	if err := validateOBJCluster(o.objCluster); err != nil {
		return fmt.Errorf("--obj-cluster: %w", err)
	}
	dryRun := o.dryRun || g.dryRun

	tfDir, aplDir, relPrefix := instanceLayout()
	specRoot := filepath.Dir(tfDir)
	clusterDomain := orElse(o.clusterDomain, name+".internal")
	overlayDst := filepath.Join(aplDir, name)
	envFile := filepath.Join(specRoot, clusterspec.EnvironmentsDir, name+".yaml")
	lzPath := filepath.Join(specRoot, clusterspec.LandingZoneFile)

	// ── pre-flight ───────────────────────────────────────────────────────────
	if _, err := os.Stat(overlayDst); err == nil {
		return fmt.Errorf("%s already exists — refusing to overwrite", overlayDst)
	}
	if _, err := os.Stat(envFile); err == nil {
		return fmt.Errorf("%s already exists — refusing to overwrite", envFile)
	}

	field := func(label, val string) { fmt.Printf("    %s%s\n", dim(label), val) }
	fmt.Println(bold("llz env add") + dim(" — spec-first scaffold"))
	field("env:            ", name)
	field("domainSuffix:   ", clusterDomain)
	field("Linode region:  ", o.region)
	field("OBJ cluster:    ", o.objCluster)
	field("dry-run:        ", fmt.Sprintf("%v", dryRun))
	fmt.Println()

	if dryRun {
		fmt.Println(bold("Spec that would be authored, then `llz render`:"))
		if _, err := os.Stat(lzPath); err != nil {
			fmt.Printf("  %s  %s  %s\n", cyan("would-create"), lzPath, dim("(instance identity + shared defaults)"))
		} else {
			fmt.Printf("  %s        %s  %s\n", dim("exists"), lzPath, dim("(left as-is)"))
		}
		fmt.Printf("  %s  %s  %s\n", cyan("would-create"), envFile, dim("(ClusterDefinition from the flags)"))
		fmt.Printf("  %s     %s  %s\n", cyan("would-run"), "llz render "+name, dim(fmt.Sprintf("(→ tfvars + the thin apl-values/%s overlay)", name)))
		fmt.Println("\n" + yellow("DRY RUN") + dim(" — nothing written. Re-run without --dry-run to create the files."))
		return nil
	}

	// ── 1. landingzone.yaml (created on the first env, else left as-is) ───────
	instanceName, created, err := ensureLandingZone(specRoot)
	if err != nil {
		return fmt.Errorf("write landingzone.yaml: %w", err)
	}
	if created {
		fmt.Printf("  %s  %s  %s\n", green("created"), lzPath, dim("(instance identity + shared defaults)"))
	}

	// ── 2. environments/<env>.yaml (the ClusterDefinition from the flags) ─────
	if err := writeEnvDefinition(envFile, name, o, instanceName, clusterDomain); err != nil {
		return fmt.Errorf("write %s: %w", envFile, err)
	}
	fmt.Printf("  %s  %s\n", green("created"), envFile)

	// ── 3. render → tfvars + the THIN apl-values/<env>/ overlay ──────────────
	// Nothing to clone: the manifests live ONCE in platform-apl/manifest/ +
	// platform-apl/components/; render writes only the per-env overlay (a thin
	// kustomization referencing the shared base + the enabled component dirs, the
	// volume-labeler REGION_SHORT patch, env-revision) and values.yaml.
	// An HA member can't render until BOTH peers exist (the spec requires one
	// active + one standby per group), so adding the first peer defers the render
	// with guidance instead of failing; completing the pair renders both.
	renderEnv, deferred := name, false
	if o.haGroup != "" {
		if missing := haGroupMissingRole(o.haGroup); missing != "" {
			deferred = true
			fmt.Printf("\n%s deployment %q authored; HA group %q still needs its %s peer.\n", cyan("○"), name, o.haGroup, missing)
			fmt.Printf("  add it, then both render:  llz env add <peer> --ha-role %s --ha-group %s --region <r> --obj-cluster <o> --subnet-cidr <distinct/14>\n", missing, o.haGroup)
			fmt.Printf("  %s\n", dim("HA peers need DISTINCT cluster.network.subnetCIDR (e.g. 10.0.0.0/14 + 10.4.0.0/14) — pass --subnet-cidr on each."))
		} else {
			renderEnv = "" // pair complete — render every env so both peers render
		}
	}
	if !deferred {
		fmt.Printf("\n%s %s\n", bold("Reconciling the spec"), dim("(`llz render "+orElse(renderEnv, "(all)")+"`):"))
		if err := runRender(g, renderEnv, false, false, false); err != nil {
			fmt.Fprintf(os.Stderr, "\nThe spec was authored but `llz render` rejected it — fix %s above, then re-run `llz render %s`.\n", envFile, name)
			return err
		}
	}

	// ── 5. provenance stamp + promotion pipeline (best-effort) ───────────────
	if err := stampTemplateVersion(name); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write .template-version (%v)\n", err)
	}
	if _, err := syncPromoteWorkflow(tfDir, relPrefix, false); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not regenerate promote.yml (%v) — run `llz env pipeline` once the pin is resolvable\n", err)
	}

	if deferred {
		fmt.Printf("\n%s commit the spec (%s + %s), add the peer above, then `llz render` reconciles both.\n",
			dim("→"), lzPath, envFile)
		return nil
	}
	printEnvAddNextSteps(name, envFile, o)
	printPlaceholderChecklist(aplDir, name)

	// Land the generated files in git for the operator. The source of truth is the
	// spec (landingzone.yaml + environments/<env>.yaml) plus the committed
	// apl-values overlay; CI builds from the COMMITTED + pushed tree. The per-env
	// <env>.tfvars are gitignored build artifacts (regenerated from the spec on
	// every render — locally and in CI), so they are deliberately NOT committed.
	// `env add` produces all of this as UNTRACKED files, so a "remember to commit"
	// reminder routinely left them behind and the GitHub repo empty — commit them
	// here, in a real instance only (the in-template dev layout commits nothing).
	gen := existingPaths([]string{lzPath, envFile, ".template-version", filepath.Join(aplDir, name)})
	if relPrefix == "" && commitFiles(gen, "llz env add "+name) {
		fmt.Printf("\n%s committed the spec + overlay — %s to publish (CI renders tfvars + builds from the pushed tree).\n",
			green("✓"), cyan("git push"))
	} else {
		fmt.Printf("\n%s commit + push the spec and overlay (CI renders tfvars + builds from the pushed tree):\n", dim("→"))
		fmt.Printf("    %s\n", cyan("git add "+strings.Join(gen, " ")))
		fmt.Printf("    %s\n", cyan(`git commit -m "llz env add `+name+`" && git push`))
	}
	return nil
}

// existingPaths keeps only the paths that exist on disk (a best-effort stamp may
// have failed, leaving its file absent).
func existingPaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// commitFiles stages exactly `paths` and commits them with msg. Best-effort: a
// failure (no git, not a repo, nothing to commit) returns false so `env add`
// degrades to printing the manual command rather than erroring. --no-verify keeps
// it fast + quiet — the staged set is generated files only (never .llz secrets),
// and CI re-runs lint + `llz render --check` on the pushed tree.
func commitFiles(paths []string, msg string) bool {
	if len(paths) == 0 {
		return false
	}
	if err := execArgv(append([]string{"git", "add", "--"}, paths...), ""); err != nil {
		return false
	}
	return execArgv([]string{"git", "commit", "-q", "--no-verify", "-m", msg}, "") == nil
}

func printEnvAddNextSteps(name, envFile string, o envAddOpts) {
	fmt.Printf("\n%s %s\n", green("✓"), bold(fmt.Sprintf("Deployment %q scaffolded", name)))
	fmt.Println(dim(fmt.Sprintf("  landingzone.yaml + %s are the source; `llz render` reconciled them into", envFile)))
	fmt.Println(dim(fmt.Sprintf("  the tfvars + apl-values/%s overlay. To change the cluster, edit %s", name, envFile)))
	fmt.Println(dim(fmt.Sprintf("  and re-run `llz render %s` (CI re-renders on every build).", name)))

	fmt.Printf("\n%s %s\n", bold("Still to fill"), dim("in the overlay before `llz build`:"))
	fmt.Printf("  %s apl-values/%s/manifest/ %s\n", dim("•"), name, dim("— the REPLACE_PER_ENV / REPLACE_ME placeholders"))
	fmt.Println(dim("      (ACME email, GitOps repoUrl/branch/path, DNS domain) the spec doesn't carry."))

	fmt.Printf("\n%s %s catch unfilled values, then %s\n",
		bold("Next:"), cyan("llz doctor --env "+name), cyan("llz tokens --env "+name+" --yes")+dim("."))
}

// printPlaceholderChecklist scans the freshly-scaffolded apl-values overlay for the
// REPLACE_* sentinels still to be filled and prints them as an exact file:line
// checklist. The tfvars are now spec-rendered (transient, regenerated by `llz
// render`), so only the overlay payload — the manifests the spec doesn't carry —
// has anything left to hand-fill. (`llz doctor --env` re-checks before a build.)
func printPlaceholderChecklist(aplDir, env string) {
	var todo []finding
	for _, f := range overlayScanFiles(filepath.Join(aplDir, env)) {
		fs, _ := scanForSentinels(f)
		for _, fd := range fs {
			if fd.blocking {
				todo = append(todo, fd)
			}
		}
	}
	if len(todo) == 0 {
		fmt.Printf("\n%s no placeholders left to fill — run %s to confirm readiness.\n",
			green("✓"), cyan("llz doctor --env "+env))
		return
	}
	fmt.Printf("\n%s %s\n", yellow(fmt.Sprintf("Placeholders still to fill (%d)", len(todo))),
		dim("— edit these, then `llz doctor --env "+env+"`:"))
	for _, f := range todo {
		fmt.Printf("  %s %s  %s %s\n", dim("•"), cyan(fmt.Sprintf("%s:%d", f.file, f.line)), f.token, dim("— "+f.hint))
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func first3(s string) string {
	if len(s) < 3 {
		return s
	}
	return s[:3]
}

func quote(s string) string { return `"` + s + `"` }

func orUnset(v, where string) string {
	if v == "" {
		return "<unset — fill in " + where + ">"
	}
	return v
}

// hclList renders a comma-separated CIDR string as an HCL list literal.
func hclList(csv string) string {
	parts := strings.Split(csv, ",")
	var out []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, quote(p))
		}
	}
	return "[" + strings.Join(out, ", ") + "]"
}

// setHCLField replaces the first `^<key> ... = ...` line with `<key> = <value>`.
// Matches the bash `replace_in_file "^<key> .*=.*"` line-rewrite.
func setHCLField(content, key, value string) string {
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(key) + `\s*=.*$`)
	return re.ReplaceAllString(content, key+" = "+value)
}

func editFile(path string, transform func(string) string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(transform(string(b))), 0o644)
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if fi.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fi.Mode())
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	})
}

func walkFiles(root string) []string {
	var out []string
	_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			out = append(out, p)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func walkFilesRel(root string) []string {
	var out []string
	for _, p := range walkFiles(root) {
		rel, _ := filepath.Rel(root, p)
		out = append(out, rel)
	}
	return out
}

func tfvarsPaths(tfDir, env string) []string {
	var out []string
	for _, root := range tfRoots {
		out = append(out, filepath.Join(tfDir, root, env+".tfvars"))
	}
	return out
}

// grepToken returns "path:line: text" hits for token across the overlay tree +
// the listed tfvars files (best-effort; missing files are skipped).
func grepToken(token, overlay string, extra []string) []string {
	files := append(walkFiles(overlay), extra...)
	var hits []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for i, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, token) {
				hits = append(hits, fmt.Sprintf("%s:%d: %s", f, i+1, strings.TrimSpace(line)))
			}
		}
	}
	return hits
}
