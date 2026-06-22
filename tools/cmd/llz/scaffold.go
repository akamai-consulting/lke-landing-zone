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
	"strconv"
	"strings"

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

var tfRoots = []string{"cluster", "cluster-bootstrap", "object-storage"}

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
	if err := validateOBJCluster(o.objCluster); err != nil {
		return err
	}
	dryRun := o.dryRun || g.dryRun

	tfDir, aplDir, relPrefix := instanceLayout()
	clusterDomain := o.clusterDomain
	if clusterDomain == "" {
		clusterDomain = name + ".internal"
	}
	regionShort := o.regionShort
	if regionShort == "" {
		regionShort = first3(name)
	}
	templateShort := first3(o.templateEnv)

	overlaySrc := filepath.Join(aplDir, o.templateEnv)
	overlayDst := filepath.Join(aplDir, name)

	// ── pre-flight ───────────────────────────────────────────────────────────
	if fi, err := os.Stat(overlaySrc); err != nil || !fi.IsDir() {
		return fmt.Errorf("template overlay not found: %s (run from an instance or template checkout)", overlaySrc)
	}
	if _, err := os.Stat(overlayDst); err == nil {
		return fmt.Errorf("%s already exists — refusing to overwrite", overlayDst)
	}
	for _, root := range tfRoots {
		if _, err := os.Stat(filepath.Join(tfDir, root, tplTfvars)); err != nil {
			return fmt.Errorf("missing template tfvars: %s/%s", root, tplTfvars)
		}
		if _, err := os.Stat(filepath.Join(tfDir, root, name+".tfvars")); err == nil {
			return fmt.Errorf("%s/%s.tfvars already exists — refusing to overwrite", root, name)
		}
	}

	fmt.Println("=== llz env add — scaffold ===")
	fmt.Printf("    env:            %s\n", name)
	fmt.Printf("    template-env:   %s\n", o.templateEnv)
	fmt.Printf("    domainSuffix:   %s\n", clusterDomain)
	fmt.Printf("    REGION_SHORT:   %s -> %s\n", templateShort, regionShort)
	fmt.Printf("    Linode region:  %s\n", orUnset(o.region, "cluster/"+name+".tfvars"))
	fmt.Printf("    OBJ cluster:    %s\n", orUnset(o.objCluster, "object-storage/"+name+".tfvars"))
	fmt.Printf("    dry-run:        %v\n\n", dryRun)

	// ── 1. Terraform tfvars ──────────────────────────────────────────────────
	fmt.Println("Terraform tfvars:")
	for _, root := range tfRoots {
		dst := filepath.Join(tfDir, root, name+".tfvars")
		if dryRun {
			fmt.Printf("  would-create  %s\n", dst)
			continue
		}
		if err := copyFile(filepath.Join(tfDir, root, tplTfvars), dst); err != nil {
			return err
		}
		if err := editFile(dst, func(s string) string { return strings.ReplaceAll(s, o.templateEnv, name) }); err != nil {
			return err
		}
		fmt.Printf("  created  %s\n", dst)
	}

	if !dryRun {
		clusterTfvars := filepath.Join(tfDir, "cluster", name+".tfvars")
		objTfvars := filepath.Join(tfDir, "object-storage", name+".tfvars")
		cbTfvars := filepath.Join(tfDir, "cluster-bootstrap", name+".tfvars")

		if err := editFile(clusterTfvars, func(s string) string {
			if o.region != "" {
				s = setHCLField(s, "region", quote(o.region))
			}
			if o.k8sVersion != "" {
				s = setHCLField(s, "k8s_version", quote(o.k8sVersion))
			}
			if o.nodeType != "" {
				s = setHCLField(s, "node_type", quote(o.nodeType))
			}
			if o.nodeCount != "" {
				s = setHCLField(s, "node_count", o.nodeCount) // numeric — no quotes
			}
			if o.runnerIPv4CIDRs != "" {
				s = setHCLField(s, "github_runner_ipv4_cidrs", hclList(o.runnerIPv4CIDRs))
			}
			if o.runnerIPv6CIDRs != "" {
				s = setHCLField(s, "github_runner_ipv6_cidrs", hclList(o.runnerIPv6CIDRs))
			}
			if o.haRole != "" {
				s = setHCLField(s, "ha_role", quote(o.haRole))
			}
			if o.haGroup != "" {
				s = setHCLField(s, "ha_group", quote(o.haGroup))
			}
			if o.promotionRank > 0 {
				// Unquoted: promotion_rank is an HCL number.
				s = setHCLField(s, "promotion_rank", strconv.Itoa(o.promotionRank))
			}
			return s
		}); err != nil {
			return err
		}

		if err := editFile(objTfvars, func(s string) string {
			if o.objCluster != "" {
				s = setHCLField(s, "obj_cluster", quote(o.objCluster))
			}
			// region_suffix is the deployment discriminator — always the env name.
			return setHCLField(s, "region_suffix", quote(name))
		}); err != nil {
			return err
		}

		// cluster-bootstrap carries "your-env" sentinels the example→env swap does
		// not touch: region (workspace key), apl_values_env, cluster_name,
		// cluster_domain. Fill them, then apply the domain + must-set overrides.
		if err := editFile(cbTfvars, func(s string) string {
			s = strings.ReplaceAll(s, "your-env", name)
			if clusterDomain != name+".internal" {
				s = strings.ReplaceAll(s, name+".internal", clusterDomain)
			}
			if o.aplChartVersion != "" {
				s = setHCLField(s, "apl_chart_version", quote(o.aplChartVersion))
			}
			if o.aplValuesRepoURL != "" {
				s = setHCLField(s, "apl_values_repo_url", quote(o.aplValuesRepoURL))
			}
			return s
		}); err != nil {
			return err
		}
	}

	// ── 2. apl-values overlay ────────────────────────────────────────────────
	fmt.Println("\napl-values overlay:")
	if dryRun {
		for _, rel := range walkFilesRel(overlaySrc) {
			fmt.Printf("  would-create  %s\n", filepath.Join(overlayDst, rel))
		}
	} else {
		if err := copyTree(overlaySrc, overlayDst); err != nil {
			return err
		}
		for _, p := range walkFiles(overlayDst) {
			if err := editFile(p, func(s string) string {
				s = strings.ReplaceAll(s, o.templateEnv, name)
				if clusterDomain != name+".internal" {
					s = strings.ReplaceAll(s, name+".internal", clusterDomain)
				}
				return s
			}); err != nil {
				return err
			}
		}
		// REGION_SHORT in the volume-labeler patch, if present.
		labeler := filepath.Join(overlayDst, "manifest", "linode-volume-labeler-region-patch.yaml")
		if _, err := os.Stat(labeler); err == nil {
			_ = editFile(labeler, func(s string) string {
				return strings.ReplaceAll(s, quote(templateShort), quote(regionShort))
			})
		}
		fmt.Printf("  created  %s/ (%d files)\n", overlayDst, len(walkFiles(overlayDst)))
	}

	if dryRun {
		fmt.Println("\nDRY RUN — nothing written. Re-run without --dry-run to create the files.")
		return nil
	}

	// ── 3. residual-token review ─────────────────────────────────────────────
	if hits := grepToken(o.templateEnv, overlayDst, tfvarsPaths(tfDir, name)); len(hits) > 0 {
		fmt.Printf("\nLeftover %q references to review (some may be intentional, e.g. shared comments):\n", o.templateEnv)
		for _, h := range hits {
			fmt.Printf("  %s\n", h)
		}
	}

	// ── 4. provenance stamp (best-effort) ────────────────────────────────────
	if err := stampTemplateVersion(name); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write .template-version (%v)\n", err)
	}

	// ── 5. regenerate the promotion pipeline from the ranks (best-effort) ─────
	// promotion_rank is the source of truth; promote.yml is the native
	// needs:-chain rendered from it (docs/environments-and-promotion.md). A
	// failure here must not abort an otherwise-complete scaffold.
	if _, err := syncPromoteWorkflow(tfDir, relPrefix, false); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not regenerate promote.yml (%v) — run `llz env pipeline` once the pin is resolvable\n", err)
	}

	printEnvAddNextSteps(name, relPrefix, o)
	printPlaceholderChecklist(tfDir, aplDir, name)
	return nil
}

func printEnvAddNextSteps(name, relPrefix string, o envAddOpts) {
	set := func(v, label string) string {
		if v != "" {
			return " ✓ " + label + "=" + v
		}
		return ""
	}
	fmt.Printf(`
Scaffold created for env %q. Still to fill in before `+"`llz build`"+`:
  • %sterraform-iac-bootstrap/cluster/%s.tfvars
      - region%s, k8s_version%s, node sizing (node_type/node_count — or pass
        --node-type/--node-count to env add)
      - github_runner_ipv4_cidrs / *_ipv6_cidrs%s  → static operator/CI egress CIDRs
        seeding the bootstrap control-plane ACL (never 0.0.0.0/0). github.com-hosted
        runners use `+"`llz ci runner-acl open`"+` instead — no static CIDR needed.
  • %sterraform-iac-bootstrap/cluster-bootstrap/%s.tfvars
      - apl_chart_version%s, apl_values_repo_url (defaults from instance_repo)
  • %sterraform-iac-bootstrap/object-storage/%s.tfvars
      - obj_cluster%s
  • %sapl-values/%s/values.yaml + manifest/ — fill the REPLACE_PER_ENV / REPLACE_ME
      placeholders (ACME email, GitOps repoUrl/branch/path, DNS domain).

Next: `+"`llz doctor --env %s`"+` to catch unfilled values, then `+"`llz tokens --env %s --yes`"+`.
`,
		name,
		relPrefix, name, set(o.region, "region"), set(o.k8sVersion, "k8s_version"),
		set(o.runnerIPv4CIDRs, "ipv4_cidrs"),
		relPrefix, name, set(o.aplChartVersion, "apl_chart_version"),
		relPrefix, name, set(o.objCluster, "obj_cluster"),
		relPrefix, name, name, name)
}

// printPlaceholderChecklist scans the freshly-scaffolded tfvars + overlay for the
// REPLACE_* / your-* sentinels still to be filled and prints them as an exact
// file:line checklist — so the operator edits a concrete list instead of hunting
// through the overlay tree. (`llz doctor --env` re-checks these before a build.)
func printPlaceholderChecklist(tfDir, aplDir, env string) {
	overlay := filepath.Join(aplDir, env)
	files := tfvarsPaths(tfDir, env)
	files = append(files, overlayScanFiles(overlay)...)

	var todo []finding
	for _, f := range files {
		fs, _ := scanForSentinels(f)
		for _, fd := range fs {
			if fd.blocking {
				todo = append(todo, fd)
			}
		}
	}
	if len(todo) == 0 {
		fmt.Printf("\n✓ no placeholders left to fill — run `llz doctor --env %s` to confirm readiness.\n", env)
		return
	}
	fmt.Printf("\nPlaceholders still to fill (%d) — edit these, then `llz doctor --env %s`:\n", len(todo), env)
	for _, f := range todo {
		fmt.Printf("  • %s:%d  %s — %s\n", f.file, f.line, f.token, f.hint)
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
