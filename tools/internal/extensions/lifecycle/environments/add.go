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
	// ── layout + the refuse-to-overwrite preflight, BEFORE any account read ──
	//
	// THE ORDER IS LOAD-BEARING AND IT USED TO BE WRONG. These checks are pure
	// filesystem detection, and one of them exists to break a DEAD-END: an env file
	// with no overlay means a previous run authored the spec and `llz render` then
	// rejected it, and the way out is the guidance below. With the account reads
	// first, re-running `llz env add` with the original flags could die on an
	// unbuildable --k8s-version instead — the version having rotated out in the
	// meantime, which is a matter of hours for LKE-E — and the operator never
	// reached the sentence telling them how to recover.
	//
	// It is also simply cheaper: a run that is going to refuse now makes no Linode
	// requests at all.
	tfDir, aplDir, relPrefix := instancelayout.Detect()
	specRoot := filepath.Dir(tfDir)
	overlayDst := filepath.Join(aplDir, name)
	envFile := filepath.Join(specRoot, clusterspec.EnvironmentsDir, name+".yaml")
	lzPath := filepath.Join(specRoot, clusterspec.LandingZoneFile)

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
	// k8s.Note / k8s.Warning are NOT printed here. They are version consequences like
	// the other two, so they go through printK8sVersionConsequences, which both the
	// dry-run and the real path call — printing them at the resolve site put them
	// above the "would be authored" preview, i.e. above the line that says nothing
	// has happened yet.
	dryRun = o.DryRun || dryRun

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
	//
	// READ ONCE, and by both fixes and the banner: sharedK8sVersion folds "the field
	// is absent" and "the spec did not parse" into the same "", and which of the two
	// this is decides whether llz may write anything at all.
	inherited, lzReadable := "", true
	if lzExists {
		inherited, lzReadable = sharedK8sVersion(lzPath)
	}
	inheritedFix := ""
	if o.K8sVersion == "" && lzExists {
		inheritedFix = k8s.ReplacementForInheritedPin(inherited)
	}
	// NOTHING TO INHERIT IS NOT NOTHING TO DO. A landingzone.yaml that PARSES and
	// names no spec.defaults.cluster.k8sVersion leaves this deployment with no
	// version at all: `llz render` rejects it two steps later with
	// "cluster.k8sVersion is required", landing in the env-file-without-overlay dead
	// end this command has a dedicated error for above — while llz is holding the
	// account's answer and discarding it. That is the failure this whole feature
	// exists to remove, wearing a different hat.
	//
	// PER-DEPLOYMENT, not seeded into spec.defaults: EnsureLandingZone only writes a
	// file it CREATES, and silently editing an existing landingzone.yaml is a much
	// bigger licence than `llz env add` has ever taken.
	//
	// THROUGH envdef.SeedK8sVersion, the same fallback the seed path uses, and NOT
	// k8s.Newest alone. Empty means "the account could not be asked" — the offline
	// or expired-token operator — and keying on it left exactly that operator with
	// no pin at all: the env file authored without a version, `llz render` rejecting
	// it, and the dead end reached in silence. The compiled literal may be stale,
	// but a spec that renders beats one that cannot.
	missingPinFix, missingPinFromSibling := "", false
	if o.K8sVersion == "" && lzExists && lzReadable && inherited == "" {
		chosen := k8s.Newest
		// THE SIBLING-MINOR RULE APPLIES HERE TOO. ReplacementForInheritedPin prefers
		// the family's minor when a SHARED pin rotates out; a shared pin that was never
		// there is not a reason to abandon that. With no spec.defaults every existing
		// deployment carries its own version — the spec does not validate otherwise —
		// so when they agree on a minor, that is this instance's family and the new
		// deployment belongs in it.
		if sib := sharedSiblingMinor(specRoot); sib != "" {
			switch inSib := linode.NewestVersionInMinorOf(k8s.Offered, sib); {
			case inSib != "":
				chosen, missingPinFromSibling = inSib, true
			case k8s.Newest == "":
				// THE FAMILY IS READABLE FROM DISK, AND THE ACCOUNT IS NOT THE ONLY
				// SOURCE. Routing this only through the catalog made the rule silently
				// inert offline: with no token, k8s.Offered is nil, so the lookup found
				// nothing and the pin fell through to llz's compiled literal — possibly
				// two minors from the family sitting right there in environments/. Nothing
				// named the skew either, because the untested-minor warning compares
				// against that same literal and so saw no difference.
				//
				// The siblings' own version is the better answer here: it is what this
				// instance demonstrably runs, and it keeps the new deployment with them.
				// `llz doctor` re-checks it against the account before anything is built.
				//
				// KEYED ON "THE CATALOG CAN SUPPLY NOTHING", not on "it was never read".
				// CatalogAnswered covers a successful EMPTY listing and an all-coarse
				// catalog — in both, Offered holds no parseable build and Newest is "" —
				// so keying on it left this arm inert in two more states than the offline
				// one, with the untested-minor warning silent too because chosen was empty.
				// k8s.Newest == "" is exactly "the account gave me nothing I can write".
				//
				// AND IT PRESERVES THE NEGATIVE ARM: a catalog that DID answer usably and
				// simply lacks that minor leaves Newest set, so this falls through to it —
				// the minor is genuinely gone, and pinning a version the account has
				// stopped offering would be worse than moving.
				chosen, missingPinFromSibling = sib, true
			}
		}
		missingPinFix = envdef.SeedK8sVersion(chosen)
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

	field := func(label, val string) { fmt.Printf("    %s%s\n", color.Dim(label), val) }
	fmt.Println(color.Bold("llz env add") + color.Dim(" — spec-first scaffold"))
	field("env:            ", name)
	// NO --cluster-domain warning here: cobra's MarkDeprecated (main.go) already
	// emits one at parse time, before this banner. Printing a second warning mid-
	// banner said the same thing twice and split the field list in half.
	field("Linode Region:  ", o.Region)
	field("OBJ cluster:    ", o.ObjCluster)
	field("k8sVersion:     ", k8sVersionBanner(k8s, lzExists, lzReadable, inherited, inheritedFix, missingPinFix))
	field("dry-run:        ", fmt.Sprintf("%v", dryRun))
	fmt.Println()

	if dryRun {
		fmt.Println(color.Bold("Spec that would be authored, then `llz render`:"))
		if _, err := os.Stat(lzPath); err != nil {
			fmt.Printf("  %s  %s  %s\n", color.Cyan("would-create"), lzPath, color.Dim("(instance identity + shared defaults)"))
			// AND THE VERSION IT WOULD SEED, which is a DIFFERENT decision from the
			// banner's k8sVersion line and was the one state --dry-run showed nothing
			// about. With an explicit --k8s-version the banner reads back the pin, which
			// is per-deployment; spec.defaults is seeded from the account's newest, and
			// every deployment added afterwards inherits THAT. So the preview said
			// nothing at all about the value with the longest reach.
			//
			// THROUGH envdef.SeedK8sVersion, the function EnsureLandingZone itself calls,
			// so the preview and the write cannot name different versions — including on
			// the no-token path, where both fall through to llz's compiled default.
			fmt.Printf("            %s\n", color.Dim("k8sVersion "+envdef.SeedK8sVersion(k8s.Newest)+" — "+seedSource(k8s)+", inherited by every deployment"))
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
		printK8sVersionConsequences(lzPath, name, k8s, inheritedFix, missingPinFix, lzExists, missingPinFromSibling, reseeding, dryRun, orphanedEnvs)
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
		// UNGUARDED, AND THROUGH THE SAME TWO FUNCTIONS THE PREVIEW USES. This used to
		// be `if k8s.Newest != ""`, so the operator with no LINODE_TOKEN — the one who
		// most needs to know a compiled literal just became every deployment's shared
		// default — got silence, while the operator whose account answered got a line.
		fmt.Printf("            %s\n", color.Dim("k8sVersion "+envdef.SeedK8sVersion(k8s.Newest)+" — "+seedSource(k8s)+", inherited by every deployment"))
	}
	printK8sVersionConsequences(lzPath, name, k8s, inheritedFix, missingPinFix, lzExists, missingPinFromSibling, reseeding, dryRun, orphanedEnvs)
	// Mutually exclusive: inheritedFix needs a non-empty inherited pin and
	// missingPinFix needs an empty one.
	if inheritedFix != "" {
		o.K8sVersion = inheritedFix
	}
	if missingPinFix != "" {
		o.K8sVersion = missingPinFix
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
// per-deployment override.
//
// readable IS RETURNED SEPARATELY because the two failures are not the same thing
// to say out loud, and they were folded into one "". A spec that PARSED and names
// no k8sVersion really does supply none — `llz render` rejects the deployment with
// "cluster.k8sVersion is required" a step later, and llz is holding an answer it
// could have used. A spec that did not parse supplies an UNKNOWN, and the banner
// asserting "(inherited from landingzone.yaml spec.defaults)" about it named the
// wrong fault entirely.
func sharedK8sVersion(lzPath string) (pin string, readable bool) {
	lz, err := clusterspec.Load(lzPath)
	if err != nil || lz == nil {
		return "", false
	}
	return strings.TrimSpace(lz.Spec.Defaults.Cluster.K8sVersion), true
}

// k8sVersionBanner renders the version field of the `llz env add` banner.
//
// IT DISTINGUISHES THE THREE CASES BECAUSE THEY ARE GENUINELY DIFFERENT, and
// collapsing them is what let the second `env add` announce a derived version it
// did not use. What the operator needs to read off this line is WHICH FILE decides
// their cluster's version.
func k8sVersionBanner(k8s instanceresolve.K8sVersionChoice, lzExists, lzReadable bool, inherited, inheritedFix, missingPinFix string) string {
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
	case missingPinFix != "":
		return missingPinFix + color.Dim(" (this deployment only — landingzone.yaml names no shared default)")
	case !lzExists && k8s.Newest != "":
		return k8s.Newest
	case !lzExists && k8s.Catalog == instanceresolve.CatalogAnswered:
		// THE ACCOUNT ANSWERED; its catalog just holds nothing that could be sent to
		// the create API. Reporting that as "could not be asked" contradicted the
		// stderr line printed moments earlier, which names the catalog it returned —
		// two messages about one event, disagreeing on whether the request happened.
		//
		// Catalog, NOT len(k8s.Offered) > 0. A read that SUCCEEDED and returned an
		// empty catalog leaves Offered nil, so keying on it put this line and
		// seedSource on opposite sides of the same fact: one run printed "the account
		// could not be asked" in the banner and "the account's catalog names no full
		// build id" three lines later. The two must read the same field or they will
		// drift again.
		return color.Dim("(scaffold default — the account's catalog names no build id)")
	case !lzExists && k8s.Catalog == instanceresolve.CatalogFailed:
		return color.Dim("(scaffold default — the account did not answer)")
	case !lzExists:
		return color.Dim("(scaffold default — the account could not be asked)")
	case !lzReadable:
		// THE SPEC DID NOT PARSE, which is a different fault from naming no version and
		// wants a different sentence. The inherited-pin re-check ran for neither, but
		// only one of them is "supplies no k8sVersion" — asserting that about an
		// unparseable file sends the operator hunting a missing field.
		return color.Dim("(landingzone.yaml could not be read — `llz render` will say why)")
	// NO `inherited == ""` ARM. It looks like it belongs beside the two above and it
	// is unreachable: reaching here with lzExists and a readable spec naming no
	// version means either --k8s-version was given (the Pin arm wins) or it was not,
	// in which case missingPinFix is set — SeedK8sVersion never returns "" — and that
	// arm wins. An arm only a hand-built fixture can enter is a test of the test.
	default:
		return inherited + color.Dim(" (inherited from landingzone.yaml spec.defaults)")
	}
}

// seedSource names where a re-seeded spec.defaults version came from, because
// "the newest this account offers", "your account named nothing usable" and "a
// literal compiled months ago" earn very different reactions from an operator.
//
// THREE CASES, NOT TWO. Keyed on Newest alone this said "this account was never
// asked" whenever Newest was empty — which also covers the account that ANSWERED
// and simply named no full build id. In that run llz printed the catalog it had
// just read, in a warning, and then claimed a few lines later that it had never
// asked. k8sVersionBanner already distinguishes these three; this did not, and one
// of them is the operator's own credential being blamed for their catalog.
//
// Catalog is the discriminator, and Offered is NOT: a read that succeeded on an
// empty catalog leaves Offered nil, so it cannot tell "answered with nothing" from
// "never answered". k8sVersionBanner reads the same field, deliberately — the two
// sentences are about one event and drifted apart once already.
func seedSource(k8s instanceresolve.K8sVersionChoice) string {
	switch {
	case k8s.Newest != "":
		return "the newest LKE-Enterprise version this account offers"
	case k8s.Catalog == instanceresolve.CatalogAnswered:
		// NOT len(k8s.Offered) > 0. A successful read of an EMPTY catalog leaves Offered
		// nil, so keying on it told an account that had answered it "was never asked" —
		// the same message-lies class the cluster-read arm was fixed for.
		return "llz's compiled default — the account's catalog names no full build id"
	case k8s.Catalog == instanceresolve.CatalogFailed:
		// ASKED AND REFUSED IS NOT NEVER ASKED. A bool could not hold this, so a token
		// whose versions route 401s got "the API did not answer" from the skip notice
		// and "this account was never asked" here, in one run about one request. The
		// remedies differ — fix the token you have, versus export one — which is the
		// entire reason to distinguish them.
		return "llz's compiled default — this account did not answer (see the warning above)"
	default:
		return "llz's compiled default — this account was never asked"
	}
}

// missingPinSource names where the no-shared-pin choice came from, because "the
// newest your account offers", "the minor your other deployments run" and "a
// literal compiled months ago" earn very different reactions — the same reason
// seedSource exists for the seeded default.
//
// IT STOPPED BEING TRUE THE MOMENT missingPinFix ROUTED THROUGH SeedK8sVersion.
// The message said "the newest your account offers" unconditionally, and offline
// that is llz's compiled fallback: a claim about the account on the one path where
// the account was never reached.
func missingPinSource(k8s instanceresolve.K8sVersionChoice, fromSibling bool) string {
	switch {
	case fromSibling:
		return "the minor your other deployments run"
	case k8s.Newest != "":
		return "the newest your account offers"
	default:
		return seedSource(k8s)
	}
}

// sharedSiblingMinor returns a FULL BUILD ID from the deployments this instance
// already has, when every one of them that names a usable version agrees on a
// MAJOR.MINOR — and "" when they disagree, when none names one, or when the specs
// cannot be read.
//
// IT ANSWERS "WHAT FAMILY IS THIS INSTANCE ON" and nothing more, which is why
// disagreement yields "" rather than a vote: if the deployments have already
// diverged there is no family to join, and picking one side would be this command
// taking a position it has no basis for.
func sharedSiblingMinor(specRoot string) string {
	inst, err := clusterspec.LoadInstance(specRoot)
	if err != nil || inst == nil {
		return ""
	}
	// inst.EnvNames(), NOT existingDeployments(). The latter returns FILE BASENAMES
	// and Env() is keyed on metadata.name; a hand-authored spec where the two differ
	// — the shape this whole path exists for — would look up nothing, lose its
	// family silently, and fall back to the account's absolute newest.
	found := ""
	for _, name := range inst.EnvNames() {
		e, ok := inst.Env(name)
		if !ok {
			continue
		}
		// ONLY A FULL BUILD ID COUNTS, and this is the fence every other CHOOSING path
		// here already has. What this returns can become the pin llz writes, and
		// terraform sends it verbatim — so a sibling carrying one of the two
		// misspellings the runbook documents (`v1.33.6`, no `+lke`) would have been
		// copied into the new deployment and killed its first apply on
		// `[400] k8s_version is not valid`. clusterspec only checks the field is
		// non-empty, so nothing downstream would have caught it either.
		//
		// IT ALSO FIXES THE DISAGREEMENT TEST. DifferentMinor answers false when either
		// side names no minor, so an unparseable sibling (`latest`) read as AGREEMENT —
		// and if it sorted first it became the family value itself.
		v := strings.TrimSpace(e.Cluster.K8sVersion)
		if !linode.NamesABuild(v) {
			continue
		}
		if _, _, ok := linode.MinorOf(v); !ok {
			continue
		}
		if found == "" {
			found = v
			continue
		}
		if linode.DifferentMinor(found, v) {
			return ""
		}
	}
	return found
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
	inheritedFix, missingPinFix string, lzExists, missingPinFromSibling, reseeding, dryRun bool, orphanedEnvs []string,
) {
	// THE ADOPTION ARM FIRST, because it is the one that decided this deployment's
	// version — the other two are about deployments that inherit. Both strings are
	// composed by the resolver (instanceresolve.adoptionMessage) so the judgement
	// "is the running version still in the catalog?" has exactly one implementation.
	if k8s.Note != "" {
		fmt.Printf("  %s\n", k8s.Note)
	}
	if k8s.Warning != "" {
		fmt.Fprintln(os.Stderr, cigate.Warning(k8s.Warning))
	}
	// SEEDING A MINOR THIS RELEASE HAS NEVER RUN. `llz doctor` and
	// `llz ci assert-k8s-version` ask only whether the ACCOUNT can build the pin,
	// which is a different question from whether this llz release and the apl-core
	// baseline have been seen working on that minor — so the account offering
	// v1.35.x the week Linode publishes it is enough for every gate to pass.
	// envdef.SeedK8sVersion("") is the honest anchor, and is the SAME expression
	// EnsureLandingZone falls back to rather than a second copy of the literal: it
	// is what an operator with no token gets, and what the e2e lane actually runs.
	//
	// IT DOES NOT REVISIT THE CHOICE, only make it visible. Newest-offered stays
	// (see NewestVersion) — this is written once, at scaffold time, so it cannot
	// move under a live instance, and #455 already adopts a running cluster's
	// version rather than re-seeding over it. What it must not do is move SILENTLY:
	// two instances scaffolded a month apart can differ by a minor, and the operator
	// should learn that at scaffold time rather than in a converge.
	sharedPin, _ := sharedK8sVersion(lzPath)
	// EVERY VERSION LLZ CHOOSES ITSELF, not only the seeded default. missingPinFix
	// pins a brand-new minor per-deployment on the same evidence and was silent —
	// which is the thing this warning exists to prevent. A pin taken from the
	// SIBLINGS' minor is excluded: that choice was made deliberately to keep the
	// deployment with its family, and warning about it would second-guess the rule
	// three lines of this file just applied.
	//
	// AND IN THE TENSE THIS RUN EARNED. chosenField asserts a write, and this
	// function prints from the --dry-run path too, so "is seeded with" / "is pinned
	// to" contradicted the "after a real run (this one wrote nothing)" clause a few
	// lines below in the SAME message.
	seeded, pinnedTo := "is seeded with", "this deployment is pinned to"
	if dryRun {
		seeded, pinnedTo = "would be seeded with", "this deployment would be pinned to"
	}
	chosen, chosenField, chosenPerDeployment := "", "", false
	switch {
	case !lzExists:
		chosen, chosenField = k8s.Newest, "spec.defaults.cluster.k8sVersion "+seeded
	case missingPinFix != "" && !missingPinFromSibling:
		chosen, chosenField, chosenPerDeployment = missingPinFix, pinnedTo, true
	case inheritedFix != "" && linode.DifferentMinor(inheritedFix, sharedPin):
		// THE REPLACEMENT ABANDONED THE FAMILY. ReplacementForInheritedPin keeps a
		// deployment in its own minor when it can, and falls through to the account's
		// newest only when that minor is gone — an unconstrained choice, and the
		// remaining one this block did not cover. DifferentMinor against the pin it
		// REPLACED is what separates the two: a spelling fix and a same-minor
		// replacement both stay put, and warning about those would argue with the rule
		// that produced them.
		chosen, chosenField, chosenPerDeployment = inheritedFix, pinnedTo, true
	}
	// COMPUTED ONCE AND SHARED WITH THE NO-SHARED-PIN BLOCK BELOW, which offers to
	// promote llz's choice to the instance default. When that choice is on an
	// untested minor, promoting it is the opposite of what the warning directly
	// above it asks for — the two blocks would print remedies naming different
	// versions for the same field, and the second would also delete the override
	// holding the line.
	tested := envdef.SeedK8sVersion("")
	promote, offMinor := missingPinFix, linode.DifferentMinor(chosen, tested)
	if offMinor && chosenPerDeployment {
		if inTested := linode.NewestVersionInMinorOf(k8s.Offered, tested); inTested != "" {
			promote = inTested
		}
	}
	if offMinor {
		// NAME THE FIELD, NOT JUST THE VERSION. With an explicit --k8s-version the
		// banner shows the pin and this warns about k8s.Newest — two versions on
		// screen, and nothing said which is which or that the second is the one every
		// LATER deployment inherits.
		msg := fmt.Sprintf(
			"%s %s, a different Kubernetes MINOR from %s — the version this llz release ships as its\n"+
				"  fallback and its e2e lane runs. Your account offers it and `llz doctor` will pass it;\n"+
				"  the pairing with the apl-core baseline is simply unproven at that minor.",
			chosenField, chosen, tested)
		// NAME THE ALTERNATIVE ONLY WHEN IT EXISTS, and name the command that actually
		// moves it: `--k8s-version` pins ONE deployment and never becomes
		// spec.defaults, so re-running with it would leave the shared default on the
		// newer minor and merely split the instance — the opposite of the intent.
		if inTested := linode.NewestVersionInMinorOf(k8s.Offered, tested); inTested != "" {
			// THE ONE CONSEQUENCE MESSAGE THAT IGNORED dryRun. Its remedy edits a spec
			// file, and under --dry-run this run wrote none, so it would fail with
			// "no landingzone.yaml" / "no spec for <env>".
			when := "after this run"
			if dryRun {
				when = "after a real run (this one wrote nothing)"
			}
			// AND IT NAMES THE FILE THAT ACTUALLY GOVERNS. When llz pinned THIS
			// deployment, the per-deployment cluster.k8sVersion this same run writes
			// SHADOWS spec.defaults — so `llz spec set defaults…` would leave the
			// deployment exactly where it is, while the warning printed beside it tells
			// the operator to set defaults to a different version. Two remedies, one
			// file, opposite values.
			what, scope := fmt.Sprintf("`llz spec set defaults.cluster.k8sVersion=%s`", inTested), "this instance"
			if chosenPerDeployment {
				what, scope = fmt.Sprintf("`llz env set %s cluster.k8sVersion=%s`", env, inTested), fmt.Sprintf("%q", env)
			}
			msg += fmt.Sprintf("\n  To put %s on the tested minor, %s:\n    %s", scope, when, what)
		} else {
			msg += fmt.Sprintf("\n  Your account offers no build of that minor. Its catalog: %s.",
				strings.Join(k8s.Offered, ", "))
		}
		fmt.Fprintln(os.Stderr, cigate.Warning(msg))
	}
	if missingPinFix != "" {
		pinning, adds := "Pinning", "the line this run adds to"
		if dryRun {
			pinning, adds = "Would pin", "the line a real run would add to"
		}
		fmt.Fprintln(os.Stderr, cigate.Warning(fmt.Sprintf(
			"%s names no spec.defaults.cluster.k8sVersion, so %q has nothing to inherit and\n"+
				"  `llz render` would reject it with \"cluster.k8sVersion is required\".\n"+
				"  %s %s for this deployment (%s).\n"+
				"  To give every deployment a shared default instead:\n"+
				"    `llz spec set defaults.cluster.k8sVersion=%s`, then delete %s environments/%s.yaml\n"+
				"    (the cluster.k8sVersion line), and re-render.",
			lzPath, env, pinning, missingPinFix, missingPinSource(k8s, missingPinFromSibling), promote, adds, env)))
	}
	if inheritedFix != "" {
		inherited, _ := sharedK8sVersion(lzPath)
		// PAST OR CONDITIONAL, depending on whether this run wrote anything. Both
		// call sites reach here — that is the point of this function — so "Pinning X
		// for "dr" alone so it can be created" was printed by `--dry-run` about a file
		// it never created, and so was the instruction to delete a line from it.
		pinning, adds := "Pinning", "the line this run adds to"
		if dryRun {
			pinning, adds = "Would pin", "the line a real run would add to"
		}
		// A SPELLING SLIP IS NOT A ROTATION, and the account has already said which.
		// docs/runbooks/first-build-failed.md documents both misspellings — a missing
		// leading `v`, a missing `+lke` suffix — and terraform sends the pin VERBATIM,
		// so the account rejects them while building the version the operator meant.
		// Calling that "can no longer build" describes a rotation and sends them
		// hunting a replacement for a version that is right there in the catalog.
		if spelling := k8s.SpellingOf(inherited); spelling != "" {
			fmt.Fprintln(os.Stderr, cigate.Warning(fmt.Sprintf(
				"spec.defaults pins k8sVersion %s, which this Linode account does not offer — but %s is.\n"+
					"  The two are the same version spelled differently, and terraform sends the pin VERBATIM,\n"+
					"  so the catalog's spelling is the one that works.\n"+
					"  %s %s for %q so it can be created. To fix it once, for every deployment:\n"+
					"    `llz spec set defaults.cluster.k8sVersion=%s`, then delete %s environments/%s.yaml\n"+
					"    (the cluster.k8sVersion line) — an override left behind is identical to the shared\n"+
					"    value and therefore invisible, and freezes %q out of every later shared bump.",
				inherited, spelling, pinning, spelling, env, spelling, adds, env, env)))
			return
		}
		// LOUD, because it makes this deployment DIVERGE from the others and the
		// operator has to know which file now holds the difference.
		fmt.Fprintln(os.Stderr, cigate.Warning(fmt.Sprintf(
			"spec.defaults pins k8sVersion %s, which this Linode account can no longer build.\n"+
				"  %s %s for %q alone so it can be created.\n"+
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
				// AND WHAT THE ALTERNATIVE COSTS, because the line above has just promised
				// the running deployments are untouched and this reverses it: moving the
				// shared default moves THEM, on their next apply. It is a legitimate choice
				// and it is why the override exists — an operator has to be able to see they
				// are making it. The second edit is not optional either: leave the override
				// behind and it is identical to the new shared value, therefore invisible,
				// and this deployment silently stops tracking every later bump.
				"  To move every deployment INSTEAD — including the running ones, whose control planes then\n"+
				"  upgrade on their next apply: set spec.defaults.cluster.k8sVersion in %s\n"+
				"  (`llz spec set defaults.cluster.k8sVersion=%s`), then delete %s environments/%s.yaml\n"+
				"  (the cluster.k8sVersion line) — an override left behind freezes %q out of every later\n"+
				"  shared bump — and re-render.",
			inherited, pinning, inheritedFix, env, lzPath, inheritedFix, adds, env, env)))
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
		// THROUGH THE SAME TWO FUNCTIONS THE PREVIEW AND THE WRITE USE. This computed
		// its own version and its own provenance sentence, so it could — and did —
		// disagree with the line `llz env add` printed moments earlier about the same
		// seed, including blaming a missing token for a catalog that had answered.
		seeded, source := envdef.SeedK8sVersion(k8s.Newest), seedSource(k8s)
		// PAST TENSE ON THE ABSENCE, PRESENT ON THE ARTIFACT, because this one string
		// is printed from BOTH paths — and on the write path EnsureLandingZone has
		// already created the file two lines above, so "is missing" contradicted the
		// `created` line directly over it. "Was missing" is what llz actually observed
		// and stays true either way. Same correction adoptionMessage needed.
		fmt.Fprintln(os.Stderr, cigate.Warning(fmt.Sprintf(
			"%s was missing, so a RE-SEEDED spec.defaults gives cluster.k8sVersion %s (%s)\n"+
				"  for every deployment that inherits it — including ones that already exist: %s.\n"+
				"  The pin they used to inherit died with the file (the rendered tfvars are gitignored), so llz\n"+
				"  cannot restore it. If any of those clusters is RUNNING a different version, pin it in that\n"+
				"  deployment's environments/<env>.yaml BEFORE the next apply — otherwise terraform plans a\n"+
				"  control-plane upgrade nobody asked for.",
			lzPath, seeded, source, strings.Join(orphanedEnvs, ", "))))
	}
}
