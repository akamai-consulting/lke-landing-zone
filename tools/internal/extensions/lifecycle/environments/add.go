package environments

// scaffold.go ports `new-deployment.sh` into the llz binary so
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
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/configreadiness"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/render"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cigate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/envdef"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/envtopology"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/instancelayout"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/instanceresolve"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/proc"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/validate"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
)

func Run(dryRun bool, name string, o envdef.Opts) error {
	// Every path below is CWD-relative, so the wrong directory yields a complete
	// but stray spec tree rather than an error. Gate on the CWD first — before the
	// flag checks, so the operator who forgot `cd my-instance` is told THAT, not
	// which flag they also mistyped. See instance_root.go.
	if err := instanceresolve.RequireInstanceRoot("`llz env add`"); err != nil {
		return err
	}
	if o.TemplateEnv == "" {
		o.TemplateEnv = "example"
	}
	if name == "" {
		return fmt.Errorf("missing <env> argument")
	}
	if err := validate.EnvName(name); err != nil {
		return err
	}
	if name == o.TemplateEnv {
		return fmt.Errorf("new env must differ from --template-env (%s)", o.TemplateEnv)
	}
	if err := envtopology.ValidateHAFlags(o.HARole, o.HAGroup); err != nil {
		return err
	}
	// Spec-first must-sets: the spec validates these, so require them up front
	// rather than scaffolding an env that won't render.
	if o.Region == "" {
		return fmt.Errorf("--region is required (the spec's cluster.region)")
	}
	// The control-plane ACL seed. Written into the spec verbatim, so an unparseable
	// entry reaches the LKE ACL API at apply, and an open-world entry quietly
	// publishes the control plane — the one thing the flag help says never to do.
	if err := validate.CIDRList("--runner-ipv4-cidrs", o.RunnerIPv4CIDRs, validate.IPv4); err != nil {
		return err
	}
	if err := validate.CIDRList("--runner-ipv6-cidrs", o.RunnerIPv6CIDRs, validate.IPv6); err != nil {
		return err
	}
	// Ask the account whether the region exists before authoring a spec against it
	// (best-effort — see region_resolve.go). Runs BEFORE the obj-cluster resolution
	// so a swapped --region/--obj-cluster pair is named for what it is.
	if err := instanceresolve.CheckRegion(o.Region); err != nil {
		return err
	}
	// Derive/check obj-cluster against the account rather than making the operator
	// invent it. Best-effort: with no LINODE_TOKEN this is exactly the old
	// shape-only validation. See objcluster_resolve.go for why the id matters.
	resolved, note, err := instanceresolve.ResolveOBJCluster(o.ObjCluster, o.Region)
	if err != nil {
		return err
	}
	o.ObjCluster = resolved
	if note != "" {
		fmt.Printf("  %s\n", note)
	}
	// THE LAYOUT IS DETECTED BEFORE THE ACCOUNT IS ASKED, because the version
	// question needs to name this deployment's CLUSTER and the label is derived from
	// the instance identity on disk. It is pure filesystem detection with no side
	// effects, so hoisting it changes nothing else about the order.
	tfDir, aplDir, relPrefix := instancelayout.Detect()
	specRoot := filepath.Dir(tfDir)

	// Ask the same account which LKE-Enterprise versions it can actually build,
	// rather than seeding the spec from a literal that is stale by construction
	// (availability is per-account and rotates within hours). Best-effort in the
	// same way as the two above: an unanswerable question leaves the choice empty
	// and envdef keeps its offline default. An explicit --k8s-version the catalog
	// DEFINITELY rejects fails here, where it costs a re-run, instead of at
	// `llz doctor` on a pin the operator never chose.
	//
	// AND WHETHER THIS DEPLOYMENT'S CLUSTER ALREADY EXISTS, which is the question
	// nothing on disk can answer. A re-scaffold over a live cluster — spec, env file
	// and overlay deleted together, which is what this repo's own e2e lane does —
	// is byte-for-byte a fresh instance, and every other guard here is keyed off the
	// tree. The label handed over is the one WriteEnvDefinition is about to author
	// (envdef.ClusterLabelFor, one derivation), so the cluster llz looks up now is
	// the cluster `llz ci assert-k8s-version` will look up later.
	k8s, err := instanceresolve.ResolveK8sVersion(o.K8sVersion, instanceresolve.Deployment{
		ClusterLabel: envdef.ClusterLabelFor(envdef.InstanceName(specRoot), name),
		Region:       o.Region,
	})
	if err != nil {
		return err
	}
	// k8s.Pin is the operator's --k8s-version, OR the version this deployment's
	// existing cluster is already running (see K8sVersionChoice.Running). Either way
	// it is per-deployment: it lands in environments/<env>.yaml and never in
	// spec.defaults, which EnsureLandingZone still seeds from k8s.Newest below.
	o.K8sVersion = k8s.Pin
	if k8s.Note != "" {
		fmt.Printf("  %s\n", k8s.Note)
	}
	if k8s.Warning != "" {
		fmt.Fprintln(os.Stderr, cigate.Warning(k8s.Warning))
	}
	dryRun = o.DryRun || dryRun

	overlayDst := filepath.Join(aplDir, name)
	envFile := filepath.Join(specRoot, clusterspec.EnvironmentsDir, name+".yaml")
	lzPath := filepath.Join(specRoot, clusterspec.LandingZoneFile)

	// Whether landingzone.yaml already exists decides where this deployment's
	// version comes from, so it is read ONCE here and both the banner and the write
	// step below key off it. EnsureLandingZone creates the file iff it is absent, so
	// this is the same fact its `created` return reports — just available in time to
	// tell the operator the truth in the banner and under --dry-run.
	_, lzStatErr := os.Stat(lzPath)
	lzExists := lzStatErr == nil

	// A LATER `env add` INHERITS A PIN NOBODY RE-CHECKED. spec.defaults was seeded
	// when the instance was scaffolded; this deployment may be a new region or a DR
	// peer added a quarter later, by which time the shared pin can have rotated out
	// of the account's catalog entirely. Inheriting it silently is how the failure
	// this whole feature removes comes back one deployment along — and the command
	// would have printed "derived" about a version it then discarded.
	//
	// Only a DEFINITE negative overrides; see ReplacementForInheritedPin.
	inheritedFix := ""
	if o.K8sVersion == "" && lzExists {
		inheritedFix = k8s.ReplacementForInheritedPin(sharedK8sVersion(lzPath))
	}

	// THE OTHER WAY THIS COMMAND CAN MOVE A RUNNING DEPLOYMENT'S VERSION, and it
	// bypasses every guard above. landingzone.yaml ABSENT is read as "fresh
	// instance" and re-seeded from today's catalog — but if environments/ still
	// holds deployments, the file was DELETED (this workflow's own e2e lane does
	// exactly that, and add.go's start-over hint invites it), and those deployments
	// inherit the new default. On the HA-pair-complete path below that re-renders
	// EVERY env's tfvars, planning a control-plane upgrade on clusters that are
	// already running — the precise invariant this feature's own gate asserts
	// against for the inherit path.
	//
	// IT WARNS RATHER THAN REFUSING, because the old shared pin died with the file:
	// nothing on disk records what these deployments used to inherit (the rendered
	// tfvars are gitignored build artifacts), so there is no value to restore and
	// no honest way to choose one. What llz can do is refuse to be quiet about it.
	//
	// WHAT IT DOES NOT COVER, stated because the boundary is easy to misread: it
	// needs ANOTHER deployment to still be defined. A re-scaffold that removes
	// landingzone.yaml, environments/<env>.yaml AND the overlay together — which is
	// exactly what e2e-instantiate.yml does, and what add.go's own start-over hint
	// leads to for a single-deployment instance — is indistinguishable ON DISK from
	// a first run, so nothing here fires.
	//
	// THAT CASE IS COVERED ELSEWHERE NOW, and deliberately not here (#453). The only
	// witness left is the ACCOUNT, so the resolver asks it: a cluster matching this
	// deployment's label+region makes the run a re-scaffold whatever the tree looks
	// like, and K8sVersionChoice.Running carries both the fact and the version. What
	// remains this warning's job is the OTHER deployments — the ones that inherit a
	// re-seeded spec.defaults, which llz does not pin for them and cannot restore.
	orphanedEnvs := existingDeployments(specRoot)
	reseeding := !lzExists && len(orphanedEnvs) > 0

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
				envFile, overlayDst, envFile, lzPath, color.Cyan("llz render "+name), color.Cyan("rm "+envFile))
		}
		return fmt.Errorf("%s already exists — refusing to overwrite", envFile)
	}

	field := func(label, val string) { fmt.Printf("    %s%s\n", color.Dim(label), val) }
	fmt.Println(color.Bold("llz env add") + color.Dim(" — spec-first scaffold"))
	field("env:            ", name)
	// NO --cluster-domain warning here: cobra's MarkDeprecated (main.go) already
	// emits one at parse time, before this banner. Printing a second warning mid-
	// banner said the same thing twice and split the field list in half.
	field("Linode Region:  ", o.Region)
	field("OBJ cluster:    ", o.ObjCluster)
	field("k8sVersion:     ", k8sVersionBanner(k8s, lzExists, inheritedFix))
	field("dry-run:        ", fmt.Sprintf("%v", dryRun))
	fmt.Println()

	if dryRun {
		fmt.Println(color.Bold("Spec that would be authored, then `llz render`:"))
		if _, err := os.Stat(lzPath); err != nil {
			fmt.Printf("  %s  %s  %s\n", color.Cyan("would-create"), lzPath, color.Dim("(instance identity + shared defaults)"))
		} else {
			fmt.Printf("  %s        %s  %s\n", color.Dim("exists"), lzPath, color.Dim("(left as-is)"))
		}
		fmt.Printf("  %s  %s  %s\n", color.Cyan("would-create"), envFile, color.Dim("(ClusterDefinition from the flags)"))
		fmt.Printf("  %s     %s  %s\n", color.Cyan("would-run"), "llz render "+name, color.Dim(fmt.Sprintf("(→ tfvars + the thin apl-values/%s overlay)", name)))
		// BOTH VERSION CONSEQUENCES, HERE TOO. lzExists and inheritedFix are read
		// before this branch precisely so --dry-run can show them, and then only the
		// banner did — so the two states worth previewing (this deployment diverging
		// from the shared pin; a re-seed moving every other deployment's) were visible
		// only by doing the thing. That is the opposite of what a preview is for.
		printK8sVersionConsequences(lzPath, name, k8s, inheritedFix, reseeding, orphanedEnvs)
		fmt.Println("\n" + color.Yellow("DRY RUN") + color.Dim(" — nothing written. Re-run without --dry-run to create the files."))
		return nil
	}

	// ── 1. landingzone.yaml (created on the first env, else left as-is) ───────
	// k8s.Newest, NOT o.K8sVersion: the shared default is the account's answer, and
	// --k8s-version is per-deployment like every other flag here (see
	// K8sVersionChoice.Pin). "" leaves envdef its offline fallback.
	instanceName, created, err := envdef.EnsureLandingZone(specRoot, k8s.Newest)
	if err != nil {
		return fmt.Errorf("write landingzone.yaml: %w", err)
	}
	if created {
		fmt.Printf("  %s  %s  %s\n", color.Green("created"), lzPath, color.Dim("(instance identity + shared defaults)"))
		if k8s.Newest != "" {
			fmt.Printf("            %s\n", color.Dim("k8sVersion "+k8s.Newest+" — the newest LKE-Enterprise version this account offers"))
		}
	}
	printK8sVersionConsequences(lzPath, name, k8s, inheritedFix, reseeding, orphanedEnvs)
	if inheritedFix != "" {
		o.K8sVersion = inheritedFix
	}

	// ── 2. environments/<env>.yaml (the ClusterDefinition from the flags) ─────
	// Through `add`'s OWN binding, not `definition`'s. The two both write
	// environments/<env>.yaml, and borrowing the other's grant is exactly the
	// union this extension split into four bindings to avoid.
	if err := envdef.WriteEnvDefinitionVia(
		capability.RepoWriterAt(specWriteBinding("add"), "."), envFile, name, o, instanceName); err != nil {
		return fmt.Errorf("write %s: %w", envFile, err)
	}
	fmt.Printf("  %s  %s\n", color.Green("created"), envFile)

	// ── 3. render → tfvars + the THIN apl-values/<env>/ overlay ──────────────
	// Nothing to clone: the manifests live ONCE in platform-apl/manifest/ +
	// platform-apl/components/; render writes only the per-env overlay (a thin
	// kustomization referencing the shared base + the enabled component dirs, the
	// volume-labeler REGION_SHORT patch, env-revision) and values.yaml.
	// An HA member can't render until BOTH peers exist (the spec requires one
	// active + one standby per group), so adding the first peer defers the render
	// with guidance instead of failing; completing the pair renders both.
	renderEnv, deferred := name, false
	if o.HAGroup != "" {
		if missing := envdef.HAGroupMissingRole(o.HAGroup); missing != "" {
			deferred = true
			fmt.Printf("\n%s deployment %q authored; HA group %q still needs its %s peer.\n", color.Cyan("○"), name, o.HAGroup, missing)
			fmt.Printf("  add it, then both render:  llz env add <peer> --ha-role %s --ha-group %s --region <r> --obj-cluster <o> --subnet-cidr <distinct/14>\n", missing, o.HAGroup)
			fmt.Printf("  %s\n", color.Dim("HA peers need DISTINCT cluster.network.subnetCIDR (e.g. 10.0.0.0/14 + 10.4.0.0/14) — pass --subnet-cidr on each."))
		} else {
			renderEnv = "" // pair complete — render every env so both peers render
		}
	}
	if !deferred {
		fmt.Printf("\n%s %s\n", color.Bold("Reconciling the spec"), color.Dim("(`llz render "+envdef.OrElse(renderEnv, "(all)")+"`):"))
		if err := render.Run(dryRun, renderEnv, false, false, false); err != nil {
			// The rejected field is not always in the env file — spec.teams (the
			// copier openbao_team answer) lives in landingzone.yaml — so name both,
			// and name the way OUT. Without that last line this state was a loop:
			// the env file now exists, so `llz env add` refuses to overwrite it, and
			// `llz doctor` refuses because the overlay was never created and tells
			// you to run the `llz env add` that just refused.
			fmt.Fprintf(os.Stderr, "\n%s the spec was authored but `llz render` rejected it. Fix the field named above —\n", color.Yellow("!"))
			fmt.Fprintf(os.Stderr, "  it is in %s or %s — then run %s.\n", envFile, lzPath, color.Cyan("llz render "+name))
			fmt.Fprintf(os.Stderr, "  Or start this deployment over: %s\n", color.Cyan("rm "+envFile))
			return err
		}
	}

	// ── 5. promotion pipeline (best-effort) ─────────────────────────────────
	// No provenance stamp is written: copier's .copier-answers.yml already records
	// it (see stamp.go), and a second copy only churned and drifted.
	if _, err := SyncPromoteWorkflow(tfDir, relPrefix, false); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not regenerate promote.yml (%v) — run `llz env pipeline` once the pin is resolvable\n", err)
	}

	if deferred {
		fmt.Printf("\n%s commit the spec (%s + %s), add the peer above, then `llz render` reconciles both.\n",
			color.Dim("→"), lzPath, envFile)
		return nil
	}
	PrintNextSteps(name, envFile, o)
	PrintPlaceholderChecklist(aplDir, name)
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
			color.Green("✓"), color.Cyan("git push"))
	} else {
		fmt.Printf("\n%s commit + push the spec and overlay (CI renders tfvars + builds from the pushed tree):\n", color.Dim("→"))
		fmt.Printf("    %s\n", color.Cyan("git add "+strings.Join(gen, " ")))
		fmt.Printf("    %s\n", color.Cyan(`git commit -m "llz env add `+name+`" && git push`))
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
	if err := proc.Run(append([]string{"git", "add", "--"}, paths...), ""); err != nil {
		return false
	}
	return proc.Run([]string{"git", "commit", "-q", "--no-verify", "-m", msg}, "") == nil
}

func PrintNextSteps(name, envFile string, o envdef.Opts) {
	fmt.Printf("\n%s %s\n", color.Green("✓"), color.Bold(fmt.Sprintf("Deployment %q scaffolded", name)))
	fmt.Println(color.Dim(fmt.Sprintf("  landingzone.yaml + %s are the source; `llz render` reconciled them into", envFile)))
	fmt.Println(color.Dim(fmt.Sprintf("  the tfvars + apl-values/%s overlay. To change the cluster, edit %s", name, envFile)))
	fmt.Println(color.Dim(fmt.Sprintf("  and re-run `llz render %s` (CI re-renders on every build).", name)))
	// The "what is left to fill" half is PrintPlaceholderChecklist's, and ONLY
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
		color.Bold("Next:"), color.Cyan("llz doctor --env "+name), color.Cyan("llz tokens --env "+name+" --yes")+color.Dim("."))
}

// PrintPlaceholderChecklist scans the freshly-scaffolded apl-values overlay for the
// REPLACE_* sentinels still to be filled and prints them as an exact file:line
// checklist. The tfvars are now spec-rendered (transient, regenerated by `llz
// render`), so only the overlay payload — the manifests the spec doesn't carry —
// has anything left to hand-fill. (`llz doctor --env` re-checks before a build.)
//
// This is the SOLE reporter of outstanding placeholders — see the note in
// PrintNextSteps for why.
func PrintPlaceholderChecklist(aplDir, env string) {
	var todo []configreadiness.Finding
	for _, f := range configreadiness.OverlayScanFiles(filepath.Join(aplDir, env)) {
		// false: `env add` scans the freshly-rendered OVERLAY, never tfvars, so the
		// ACL branch has nothing to say here — doctor owns that check.
		fs, _ := configreadiness.ScanForSentinels(f, false)
		for _, fd := range fs {
			if fd.Blocking {
				todo = append(todo, fd)
			}
		}
	}
	if len(todo) == 0 {
		fmt.Printf("\n%s no placeholders left to fill — run %s to confirm readiness.\n",
			color.Green("✓"), color.Cyan("llz doctor --env "+env))
		return
	}
	groups := GroupFindings(todo)
	fmt.Printf("\n%s %s\n", color.Yellow(fmt.Sprintf("Placeholders still to fill (%d in %d file(s))", len(groups), CountFiles(todo))),
		color.Dim("— then `llz doctor --env "+env+"`:"))
	for _, g := range groups {
		where := color.Cyan(g.first.Loc())
		if g.files > 1 {
			where = color.Cyan(g.first.Loc()) + color.Dim(fmt.Sprintf(" (+%d more file(s))", g.files-1))
		}
		fmt.Printf("  %s %s  %s %s\n", color.Dim("•"), where, g.first.Token, color.Dim("— "+g.first.Hint))
	}
}

// findingGroup is one distinct placeholder+remedy, with how many DISTINCT files
// carry it — not how many times it occurs, which is a different number whenever
// one file mentions the placeholder twice (instance-custom.yaml does).
type findingGroup struct {
	first configreadiness.Finding
	files int
}

// GroupFindings collapses findings that share a token AND a remedy, preserving
// first-seen order.
//
// This is not cosmetic. The instance-repo placeholder lands in nine rendered
// files and is fixed by ONE `llz spec set` (hintFor explains why), so printing
// nine identical lines under the heading "edit these" told the operator to do
// the exact thing the hint tells them not to. One line per fix, with the file
// count, makes the size of the job honest.
func GroupFindings(in []configreadiness.Finding) []findingGroup {
	var out []findingGroup
	idx := map[string]int{}
	seen := map[string]bool{} // group key + file — so a repeat in one file adds nothing
	for _, f := range in {
		key := f.Token + "\x00" + f.Hint
		if seen[key+"\x00"+f.File] {
			continue
		}
		seen[key+"\x00"+f.File] = true
		if i, ok := idx[key]; ok {
			out[i].files++
			continue
		}
		idx[key] = len(out)
		out = append(out, findingGroup{first: f, files: 1})
	}
	return out
}

// CountFiles is the number of distinct files carrying any finding — what the
// summary line claims to print.
func CountFiles(in []configreadiness.Finding) int {
	seen := map[string]bool{}
	for _, f := range in {
		seen[f.File] = true
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

// sharedK8sVersion reads spec.defaults.cluster.k8sVersion out of an EXISTING
// landingzone.yaml.
//
// Best-effort by design: an unreadable or malformed spec is `llz validate`'s
// problem and has its own diagnostics, and all this decides is whether to write a
// per-deployment override. "" simply inherits, which is the previous behaviour.
func sharedK8sVersion(lzPath string) string {
	lz, err := clusterspec.Load(lzPath)
	if err != nil || lz == nil {
		return ""
	}
	return strings.TrimSpace(lz.Spec.Defaults.Cluster.K8sVersion)
}

// k8sVersionBanner renders the version field of the `llz env add` banner.
//
// IT DISTINGUISHES THE THREE CASES BECAUSE THEY ARE GENUINELY DIFFERENT, and
// collapsing them is what let the second `env add` announce a derived version it
// did not use. What the operator needs to read off this line is WHICH FILE decides
// their cluster's version.
func k8sVersionBanner(k8s instanceresolve.K8sVersionChoice, lzExists bool, inheritedFix string) string {
	switch {
	case k8s.Pin != "" && k8s.Pin == k8s.Running:
		// ADOPTED OR EXEMPTED, NOT CHOSEN — and the operator needs to read that off
		// this line, because it is the one case where llz wrote a version the account
		// may not even offer any more. Rendering it identically to an explicit
		// --k8s-version made the most surprising value in the banner the most
		// ordinary-looking one. The note/warning printed above says the rest.
		return k8s.Pin + color.Dim(" (this deployment only — its cluster already runs it)")
	case k8s.Pin != "":
		return k8s.Pin
	case inheritedFix != "":
		return inheritedFix + color.Dim(" (this deployment only — the shared default is unbuildable)")
	case !lzExists && k8s.Newest != "":
		return k8s.Newest
	case !lzExists && len(k8s.Offered) > 0:
		// THE ACCOUNT ANSWERED; its catalog just holds nothing that could be sent to
		// the create API. Reporting that as "could not be asked" contradicted the
		// stderr line printed moments earlier, which names the catalog it returned —
		// two messages about one event, disagreeing on whether the request happened.
		return color.Dim("(scaffold default — the account's catalog names no build id)")
	case !lzExists:
		return color.Dim("(scaffold default — the account could not be asked)")
	default:
		return color.Dim("(inherited from landingzone.yaml spec.defaults)")
	}
}

// existingDeployments returns the deployment names environments/ already defines,
// sorted. The deployment being added is not among them — Run refuses to overwrite
// an existing environments/<env>.yaml long before this is called.
//
// Best-effort: an unreadable directory yields nothing and the caller simply does
// not warn, which is the behaviour that shipped before the warning existed.
func existingDeployments(specRoot string) []string {
	entries, err := os.ReadDir(filepath.Join(specRoot, clusterspec.EnvironmentsDir))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		// `.yaml` only, so the template tree's `*.yaml.example` starter files are not
		// mistaken for deployments an instance actually has.
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".yaml") {
			out = append(out, strings.TrimSuffix(e.Name(), ".yaml"))
		}
	}
	sort.Strings(out)
	return out
}

// printK8sVersionConsequences reports the two ways this command can change which
// LKE-Enterprise version something ends up on.
//
// ONE FUNCTION, CALLED FROM BOTH THE DRY-RUN AND THE REAL PATH. It started as two
// inline blocks after the writes, so `--dry-run` — the flag whose entire job is
// "show me what this would do" — printed neither, and the two states most worth
// previewing were visible only by performing them.
func printK8sVersionConsequences(lzPath, env string, k8s instanceresolve.K8sVersionChoice,
	inheritedFix string, reseeding bool, orphanedEnvs []string,
) {
	if inheritedFix != "" {
		// LOUD, because it makes this deployment DIVERGE from the others and the
		// operator has to know which file now holds the difference.
		fmt.Fprintln(os.Stderr, cigate.Warning(fmt.Sprintf(
			"spec.defaults pins k8sVersion %s, which this Linode account can no longer build.\n"+
				"  Pinning %s for %q alone so it can be created.\n"+
				// "UNAFFECTED" WAS TOO STRONG, and the exception is the expensive one. A
				// deployment whose cluster is already BUILT plans no change to k8s_version
				// and is genuinely untouched. One scaffolded and never applied still has the
				// old pin in its spec, and its FIRST apply sends it — dying on the same
				// `[400] k8s_version is not valid` this feature exists to prevent. Saying
				// "unaffected" about both is how an operator learns which one they had
				// fifteen minutes into an apply.
				"  Deployments whose clusters already RUN the old version are untouched — terraform plans no\n"+
				"  change to k8s_version for them. Any deployment NOT yet applied still carries the old pin\n"+
				"  and will fail its first apply on it.\n"+
				"  To move every deployment, set spec.defaults.cluster.k8sVersion in %s and re-render.",
			sharedK8sVersion(lzPath), inheritedFix, env, lzPath)))
	}
	if reseeding {
		// NOT GUARDED ON k8s.Newest. It reads as "only warn when we actually derived
		// something", but the re-seed happens either way: EnsureLandingZone("") falls
		// straight through to llz's compiled default, so the no-LINODE_TOKEN operator
		// — the one most likely to be mid-recovery, since add.go's own start-over hint
		// and the e2e lane both delete this file — got the silent version of the exact
		// upgrade this warning exists to announce. Name the version that WILL be
		// seeded, and say where it came from, because "the newest this account offers"
		// and "a literal compiled months ago" earn very different reactions.
		seeded, source := k8s.Newest, "the newest version this Linode account offers"
		if seeded == "" {
			seeded, source = envdef.SeedK8sVersion(""), "llz's compiled default — this account was never asked"
		}
		fmt.Fprintln(os.Stderr, cigate.Warning(fmt.Sprintf(
			"%s is missing, so a RE-SEEDED spec.defaults sets cluster.k8sVersion to %s (%s)\n"+
				"  for every deployment that inherits it — including ones that already exist: %s.\n"+
				"  The pin they used to inherit died with the file (the rendered tfvars are gitignored), so llz\n"+
				"  cannot restore it. If any of those clusters is RUNNING a different version, pin it in that\n"+
				"  deployment's environments/<env>.yaml BEFORE the next apply — otherwise terraform plans a\n"+
				"  control-plane upgrade nobody asked for.",
			lzPath, seeded, source, strings.Join(orphanedEnvs, ", "))))
	}
}
