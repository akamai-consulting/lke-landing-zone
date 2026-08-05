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
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/validate"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
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

var tfRoots = []string{"cluster", "object-storage", "databases"}

// optionalTFRoots are roots an instance may legitimately never apply. `llz render`
// still writes a tfvars stub for them (render is per-root, not per-opt-in), but a
// MISSING one is not a defect — so readiness must not report it as such.
//
// This matters for legacy (pre-spec) instances: they never run `llz render`, their
// hand-authored <env>.tfvars are the tracked source of truth, and readiness does
// flag a genuinely missing one for them. Without this set, adding `databases` to
// tfRoots would have every such instance reporting a blocking "missing
// databases/<env>.tfvars — run llz env add" for a database they never asked for.
var optionalTFRoots = map[string]bool{"databases": true}

// optionalTFVars reports whether path is a tfvars belonging to an optional root.
func optionalTFVars(path string) bool {
	return optionalTFRoots[filepath.Base(filepath.Dir(path))]
}

// validateOBJCluster catches a value that isn't shaped like a Linode OBJ cluster
// id. The shape rule lives in internal/validate (OBJClusterID) so the LandingZone
// spec validator reuses it.
func validateOBJCluster(v string) error { return validate.OBJClusterID(v) }

func runEnvAdd(g globalOpts, name string, o envAddOpts) error {
	// Every path below is CWD-relative, so the wrong directory yields a complete
	// but stray spec tree rather than an error. Gate on the CWD first — before the
	// flag checks, so the operator who forgot `cd my-instance` is told THAT, not
	// which flag they also mistyped. See instance_root.go.
	if err := requireInstanceRoot("`llz env add`"); err != nil {
		return err
	}
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
	// The control-plane ACL seed. Written into the spec verbatim, so an unparseable
	// entry reaches the LKE ACL API at apply, and an open-world entry quietly
	// publishes the control plane — the one thing the flag help says never to do.
	if err := validate.CIDRList("--runner-ipv4-cidrs", o.runnerIPv4CIDRs, validate.IPv4); err != nil {
		return err
	}
	if err := validate.CIDRList("--runner-ipv6-cidrs", o.runnerIPv6CIDRs, validate.IPv6); err != nil {
		return err
	}
	// Ask the account whether the region exists before authoring a spec against it
	// (best-effort — see region_resolve.go). Runs BEFORE the obj-cluster resolution
	// so a swapped --region/--obj-cluster pair is named for what it is.
	if err := checkRegion(o.region); err != nil {
		return err
	}
	// Derive/check obj-cluster against the account rather than making the operator
	// invent it. Best-effort: with no LINODE_TOKEN this is exactly the old
	// shape-only validation. See objcluster_resolve.go for why the id matters.
	resolved, note, err := resolveOBJCluster(o.objCluster, o.region)
	if err != nil {
		return err
	}
	o.objCluster = resolved
	if note != "" {
		fmt.Printf("  %s\n", note)
	}
	dryRun := o.dryRun || g.dryRun

	tfDir, aplDir, relPrefix := instanceLayout()
	specRoot := filepath.Dir(tfDir)
	overlayDst := filepath.Join(aplDir, name)
	envFile := filepath.Join(specRoot, clusterspec.EnvironmentsDir, name+".yaml")
	lzPath := filepath.Join(specRoot, clusterspec.LandingZoneFile)

	// ── pre-flight ───────────────────────────────────────────────────────────
	if _, err := os.Stat(overlayDst); err == nil {
		return fmt.Errorf("%s already exists — refusing to overwrite", overlayDst)
	}
	if _, err := os.Stat(envFile); err == nil {
		// Distinguish "already scaffolded" from "a previous run authored the spec and
		// then render rejected it", which leaves the env file WITHOUT an overlay. That
		// second state used to dead-end: this refused, and `llz doctor` sent you back
		// here because apl-values/<env>/ was missing.
		if _, oerr := os.Stat(overlayDst); oerr != nil {
			return fmt.Errorf("%s exists but %s does not — a previous `llz env add` authored the spec and `llz render` then rejected it.\n"+
				"  Fix the spec (%s or %s) and run %s,\n"+
				"  or discard it and start over: %s",
				envFile, overlayDst, envFile, lzPath, cyan("llz render "+name), cyan("rm "+envFile))
		}
		return fmt.Errorf("%s already exists — refusing to overwrite", envFile)
	}

	field := func(label, val string) { fmt.Printf("    %s%s\n", dim(label), val) }
	fmt.Println(bold("llz env add") + dim(" — spec-first scaffold"))
	field("env:            ", name)
	// NO --cluster-domain warning here: cobra's MarkDeprecated (main.go) already
	// emits one at parse time, before this banner. Printing a second warning mid-
	// banner said the same thing twice and split the field list in half.
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
	if err := writeEnvDefinition(envFile, name, o, instanceName); err != nil {
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
			// The rejected field is not always in the env file — spec.teams (the
			// copier openbao_team answer) lives in landingzone.yaml — so name both,
			// and name the way OUT. Without that last line this state was a loop:
			// the env file now exists, so `llz env add` refuses to overwrite it, and
			// `llz doctor` refuses because the overlay was never created and tells
			// you to run the `llz env add` that just refused.
			fmt.Fprintf(os.Stderr, "\n%s the spec was authored but `llz render` rejected it. Fix the field named above —\n", yellow("!"))
			fmt.Fprintf(os.Stderr, "  it is in %s or %s — then run %s.\n", envFile, lzPath, cyan("llz render "+name))
			fmt.Fprintf(os.Stderr, "  Or start this deployment over: %s\n", cyan("rm "+envFile))
			return err
		}
	}

	// ── 5. promotion pipeline (best-effort) ─────────────────────────────────
	// No provenance stamp is written: copier's .copier-answers.yml already records
	// it (see stamp.go), and a second copy only churned and drifted.
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
	printNextCommand(name)

	// Land the generated files in git for the operator. The source of truth is the
	// spec (landingzone.yaml + environments/<env>.yaml) plus the committed
	// apl-values overlay; CI builds from the COMMITTED + pushed tree. The per-env
	// <env>.tfvars are gitignored build artifacts (regenerated from the spec on
	// every render — locally and in CI), so they are deliberately NOT committed.
	// `env add` produces all of this as UNTRACKED files, so a "remember to commit"
	// reminder routinely left them behind and the GitHub repo empty — commit them
	// here, in a real instance only (the in-template dev layout commits nothing).
	gen := existingPaths([]string{lzPath, envFile, filepath.Join(aplDir, name)})
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
	// The "what is left to fill" half is printPlaceholderChecklist's, and ONLY
	// its: this used to print an unconditional "Still to fill … the
	// REPLACE_PER_ENV / REPLACE_ME placeholders" block that the checklist then
	// contradicted two lines later with "✓ no placeholders left to fill". A
	// reader going top-to-bottom went hunting for placeholders that were not
	// there.
}

// printNextCommand prints the follow-on command. Called AFTER the placeholder
// checklist so the run reads in the order the operator acts: what was created,
// what (if anything) is left to fill, then what to run next.
func printNextCommand(name string) {
	fmt.Printf("\n%s %s catch unfilled values, then %s\n",
		bold("Next:"), cyan("llz doctor --env "+name), cyan("llz tokens --env "+name+" --yes")+dim("."))
}

// printPlaceholderChecklist scans the freshly-scaffolded apl-values overlay for the
// REPLACE_* sentinels still to be filled and prints them as an exact file:line
// checklist. The tfvars are now spec-rendered (transient, regenerated by `llz
// render`), so only the overlay payload — the manifests the spec doesn't carry —
// has anything left to hand-fill. (`llz doctor --env` re-checks before a build.)
//
// This is the SOLE reporter of outstanding placeholders — see the note in
// printEnvAddNextSteps for why.
func printPlaceholderChecklist(aplDir, env string) {
	var todo []finding
	for _, f := range overlayScanFiles(filepath.Join(aplDir, env)) {
		// false: `env add` scans the freshly-rendered OVERLAY, never tfvars, so the
		// ACL branch has nothing to say here — doctor owns that check.
		fs, _ := scanForSentinels(f, false)
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
	groups := groupFindings(todo)
	fmt.Printf("\n%s %s\n", yellow(fmt.Sprintf("Placeholders still to fill (%d in %d file(s))", len(groups), countFiles(todo))),
		dim("— then `llz doctor --env "+env+"`:"))
	for _, g := range groups {
		where := cyan(g.first.loc())
		if g.files > 1 {
			where = cyan(g.first.loc()) + dim(fmt.Sprintf(" (+%d more file(s))", g.files-1))
		}
		fmt.Printf("  %s %s  %s %s\n", dim("•"), where, g.first.token, dim("— "+g.first.hint))
	}
}

// findingGroup is one distinct placeholder+remedy, with how many DISTINCT files
// carry it — not how many times it occurs, which is a different number whenever
// one file mentions the placeholder twice (instance-custom.yaml does).
type findingGroup struct {
	first finding
	files int
}

// groupFindings collapses findings that share a token AND a remedy, preserving
// first-seen order.
//
// This is not cosmetic. The instance-repo placeholder lands in nine rendered
// files and is fixed by ONE `llz spec set` (hintFor explains why), so printing
// nine identical lines under the heading "edit these" told the operator to do
// the exact thing the hint tells them not to. One line per fix, with the file
// count, makes the size of the job honest.
func groupFindings(in []finding) []findingGroup {
	var out []findingGroup
	idx := map[string]int{}
	seen := map[string]bool{} // group key + file — so a repeat in one file adds nothing
	for _, f := range in {
		key := f.token + "\x00" + f.hint
		if seen[key+"\x00"+f.file] {
			continue
		}
		seen[key+"\x00"+f.file] = true
		if i, ok := idx[key]; ok {
			out[i].files++
			continue
		}
		idx[key] = len(out)
		out = append(out, findingGroup{first: f, files: 1})
	}
	return out
}

// countFiles is the number of distinct files carrying any finding — what the
// summary line claims to print.
func countFiles(in []finding) int {
	seen := map[string]bool{}
	for _, f := range in {
		seen[f.file] = true
	}
	return len(seen)
}

// ── helpers ──────────────────────────────────────────────────────────────────

// first3 is the REGION_SHORT derivation `llz render` stamps into the reconciler's
// env patch. It delegates to linode.RegionShort so the label the volume-labels
// reconciler WRITES and the prefix `llz reap` ACCEPTS come from one definition —
// they were derived independently once, and the sweep went blind on every
// deployment whose name is longer than three characters.
func first3(s string) string { return linode.RegionShort(s) }

func quote(s string) string { return `"` + s + `"` }

// setHCLField replaces EVERY `^<key> ... = ...` line with `<key> = <value>` (the
// doc said "the first" for years; the call has always been a ReplaceAll). Harmless
// today — no embedded terraform.tfvars.example declares the same key twice at
// column 0, which a test asserts — but two would both be rewritten into a
// duplicate assignment, which HCL rejects as a redefined attribute.
// Matches the bash `replace_in_file "^<key> .*=.*"` line-rewrite.
//
// ReplaceAllLiteral, NOT ReplaceAllString: the replacement is a rendered HCL value,
// and ReplaceAllString EXPANDS `$name`/`${name}` in it. Every value here comes from
// the spec, so a `$` in one was silently eaten rather than written — a Linode tag
// `cost$1center` rendered as `"cost"`, and `owner${team}` as `"owner"`, tagging real
// infrastructure wrong with no error (spec.cluster.tags is free-form, so nothing
// upstream rejects it). No caller wants expansion; these are literals.
func setHCLField(content, key, value string) string {
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(key) + `\s*=.*$`)
	return re.ReplaceAllLiteralString(content, key+" = "+value)
}

func tfvarsPaths(tfDir, env string) []string {
	var out []string
	for _, root := range tfRoots {
		out = append(out, filepath.Join(tfDir, root, env+".tfvars"))
	}
	return out
}
