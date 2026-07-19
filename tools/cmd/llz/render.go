package main

// render.go reconciles the declarative LandingZone spec (landingzone.yaml +
// environments/<env>.yaml, see internal/clusterspec) into the files the rest of
// the toolchain consumes. Two targets:
//   - the three <env>.tfvars (gitignored build artifacts — see
//     terraform-iac-bootstrap/.gitignore) from the env's cluster definition, which
//     `terraform -var-file=<env>.tfvars` picks up at build time. They are NOT
//     committed: regenerated here on every render (locally and in CI, before each
//     terraform op), so the spec is the single source of truth. A working `llz` is
//     therefore a hard prerequisite for any terraform op.
//   - the committed apl-values/<env>/ artifacts from the env's component toggles —
//     the manifest kustomizations (llz Argo backend) and values.yaml apps.<key>.
//     enabled (apl-core backend) — committed because Argo syncs git, and
//     drift-guarded by `llz render --check`.
//
// The pure spec→tfvars mapping lives in clusterspec (tfvars_map.go); this file is
// the thin apply loop — it reads each root's terraform.tfvars.example and sets
// (or appends) each assignment with setHCLField (shared with `llz env add`).

import (
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/tfroots"
	"github.com/spf13/cobra"
)

// tfrootTokens resolves the two copier tokens the generated TF roots carry:
// upstream_org is the constant akamai-consulting (no forks — mirrors the kustomize
// hardcoding); ref is the template version the instance tracks (resolveTemplateRef,
// "main" when un-scaffolded).
func tfrootTokens() (upstreamOrg, ref string) {
	return "akamai-consulting", orElse(resolveTemplateRef(), "main")
}

// tfrootExample reads a root's terraform.tfvars.example from the embedded tfroots
// package (it no longer ships in the instance) and substitutes the copier tokens,
// so the base each <env>.tfvars renders from is token-free.
func tfrootExample(root string) (string, error) {
	b, err := tfroots.TfvarsExample(root)
	if err != nil {
		return "", err
	}
	org, ref := tfrootTokens()
	return tfroots.Substitute(string(b), org, ref), nil
}

// envVPCCmd prints the shared VPC (spec.networks name) a deployment attaches to,
// or an empty line for a dedicated VPC, so the apply-vpc workflow step can decide
// whether — and which — shared VPC to apply before the cluster. It reads the spec
// when present (the source of truth), falling back to the rendered
// cluster/<env>.tfvars (vpc_network) for a pre-spec instance.
func envVPCCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "vpc <deployment>",
		Short: "print the shared VPC a deployment attaches to (spec.networks name); empty for a dedicated VPC",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			env := args[0]
			if err := validateEnvName(env); err != nil {
				return err
			}
			// Spec is the source of truth; the committed tfvars can lag a spec edit.
			if lz, present, err := loadSpec(); present {
				if err != nil {
					return err
				}
				e, ok := lz.Env(env)
				if !ok {
					return fmt.Errorf("no such deployment %q in the spec (run `llz env list`)", env)
				}
				fmt.Println(e.Cluster.Network.VPC)
				return nil
			}
			tfDir, _, _ := instanceLayout()
			p := filepath.Join(tfDir, "cluster", env+".tfvars")
			b, err := os.ReadFile(p)
			if err != nil {
				return fmt.Errorf("read %s (for spec-driven instances run `llz render %s` first): %w", p, env, err)
			}
			fmt.Println(tfvarsValue(string(b), "vpc_network"))
			return nil
		},
	}
}

func renderCmd() *cobra.Command {
	var tfvarsOnly, check, diff bool
	c := &cobra.Command{
		Use:   "render [env]",
		Short: "reconcile the LandingZone spec into <env>.tfvars (spec-driven instances)",
		Long: "Reads the LandingZone spec (landingzone.yaml + environments/<env>.yaml) and\n" +
			"renders each deployment's cluster definition into the three\n" +
			"terraform-iac-bootstrap/*/<env>.tfvars files the terraform plan/apply consume.\n" +
			"With no [env], renders every environment in the spec. --check validates the\n" +
			"spec without writing; --diff previews what a render WOULD change (also\n" +
			"writes nothing). A no-op contract: callers gate on the presence of a spec\n" +
			"(CI does), so instances that have not adopted it are unaffected.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			env := ""
			if len(args) == 1 {
				env = args[0]
			}
			return runRender(gopts, env, tfvarsOnly, check, diff)
		},
	}
	c.Flags().BoolVar(&tfvarsOnly, "tfvars-only", false, "render only the tfvars (skip the committed manifest kustomizations)")
	c.Flags().BoolVar(&check, "check", false, "validate the spec and exit non-zero on any error; write nothing")
	c.Flags().BoolVar(&diff, "diff", false, "preview which files a render would create/change (writes nothing)")
	return c
}

// renderedPath / wouldRenderPath print one file-mutation line with a consistent
// colored verb — green for done, cyan for a dry-run plan — so the render/scaffold
// output reads as a scannable action log. Both degrade to plain text off a TTY
// (color.go).
func renderedPath(prefix, path string) {
	fmt.Printf("  %s  %s%s\n", green("rendered"), prefix, path)
}

func wouldRenderPath(prefix, path string) {
	fmt.Printf("  %s  %s%s\n", cyan("would-render"), prefix, path)
}

func runRender(g globalOpts, env string, tfvarsOnly, check, diff bool) error {
	tfDir, aplDir, relPrefix := instanceLayout()
	specRoot := filepath.Dir(tfDir)
	if !clusterspec.InstancePresent(specRoot) {
		return fmt.Errorf("no LandingZone spec (%s) found — `llz render` needs a spec", clusterspec.LandingZoneFile)
	}
	lz, err := clusterspec.LoadInstance(specRoot)
	if err != nil {
		return err
	}
	if errs := lz.Validate(); len(errs) > 0 {
		fmt.Fprintf(os.Stderr, "LandingZone spec is invalid (%d problem(s)):\n", len(errs))
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "  • %v\n", e)
		}
		return fmt.Errorf("invalid LandingZone spec")
	}

	envs := lz.EnvNames()
	if env != "" {
		if _, ok := lz.Env(env); !ok {
			return fmt.Errorf("env %q not in spec (have: %v)", env, lz.EnvNames())
		}
		envs = []string{env}
	}

	// The escape hatch's directory contract, gated BEFORE anything is written. It is an
	// instance-level property, so it is checked once here rather than per env — and only
	// on the paths that emit the manifest tree, since --tfvars-only never touches it.
	// See custom_layout.go.
	if !tfvarsOnly {
		if err := checkCustomLayout(filepath.Join(specRoot, clusterspec.CustomRoot)); err != nil {
			return fmt.Errorf("%s layout — fix these before rendering:\n%w", clusterspec.CustomRoot, err)
		}
	}

	// All three paths below render from the SAME target set (renderTargets) — the
	// write path writes it, --check compares it, --diff diffs it — so they cannot
	// disagree about what a render produces.

	// --diff previews what a render would create/change, writing nothing.
	if diff {
		return runRenderDiff(lz, envs, tfDir, aplDir, tfvarsOnly)
	}

	targets, err := renderTargets(lz, envs, tfDir, aplDir, tfvarsOnly)
	if err != nil {
		return err
	}

	// --check is the CI drift guard: the spec is valid AND the on-disk render targets
	// match what the spec renders (the apl-values artifacts are committed because Argo
	// syncs git; a working-tree-only render would let them silently diverge). It now
	// covers the tfvars/TF-root targets too — they used to be computed only by the
	// write and --diff paths, so a STALE rendered tfvars passed --check while --diff
	// showed it changing.
	//
	// Their ABSENCE, however, is not drift: everything under terraform-iac-bootstrap
	// is a gitignored build artifact (see its .gitignore — `*/*.tfvars`, `*/*.tf`),
	// regenerated before each terraform op, so on a fresh CI checkout none of it
	// exists and demanding it would fail every instance's check for no reason.
	if check {
		if err := reportDrift(targets, func(p string) bool {
			return !strings.HasPrefix(p, tfDir+string(filepath.Separator))
		}); err != nil {
			return err
		}
		fmt.Printf("%s LandingZone spec valid (%d environment(s)); committed manifests in sync\n", green("✓"), len(lz.Spec.Environments))
		return nil
	}

	// The write path. Targets are written in sorted path order so the action log is
	// deterministic. It covers, in one loop:
	//   - the four TF root directories' *.tf, generated from the embedded tfroots copy
	//     (an instance commits ZERO Terraform: the roots are gitignored build artifacts
	//     regenerated on every render, exactly like the per-env tfvars);
	//   - the shared-VPC (spec.networks) and per-env tfvars;
	//   - unless --tfvars-only, the committed apl-values artifacts.
	// filepathRel is relative to the instance root for both tfDir and aplDir targets.
	dryRun := g.dryRun
	if err := writeTargets(targets, tfDir, relPrefix, dryRun); err != nil {
		return err
	}
	// The instance-wide ACME contact is no longer written into the (now remotely-
	// fetched) shared dns tree — it rides as a kustomize patch in each env's manifest
	// overlay (RenderManifestKustomization), emitted above.
	if !dryRun {
		untrackRenderedTfvars(relPrefix)
	}
	return nil
}

// writeTargets writes a renderTargets set to disk (creating parents), in sorted path
// order so the action log is deterministic — or, on --dry-run, just prints what it
// would write.
func writeTargets(targets map[string]string, tfDir, relPrefix string, dryRun bool) error {
	for _, dst := range slices.Sorted(maps.Keys(targets)) {
		if dryRun {
			wouldRenderPath(relPrefix, filepathRel(tfDir, dst))
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, []byte(targets[dst]), 0o644); err != nil {
			return err
		}
		renderedPath(relPrefix, filepathRel(tfDir, dst))
	}
	return nil
}

// renderTargets is THE definition of what a render produces: every file path a
// render writes, mapped to its would-be content. It is the single computation the
// write path, --check and --diff all consume, so a target can never be written by
// one and ignored by another (before this existed, --check validated only the
// committed apl-values and silently skipped every tfvars target that --diff
// covered).
//
// Nothing here touches the filesystem except to READ the committed values.yaml
// base, so it is safe on the write-nothing paths.
func renderTargets(lz *clusterspec.LandingZone, envs []string, tfDir, aplDir string, tfvarsOnly bool) (map[string]string, error) {
	targets := map[string]string{}

	// The generated TF roots (env-identical; all per-env variation lives in tfvars).
	// dst is the instance root, so the files land under tfDir.
	org, ref := tfrootTokens()
	for p, c := range tfroots.Files(filepath.Dir(tfDir), org, ref) {
		targets[p] = c
	}

	// Shared VPCs (spec.networks) render to vpc/<name>.tfvars — one apply each (state
	// key vpc/<name>) — and must exist before the clusters that attach to them. No-op
	// when none are declared, so instances that use only dedicated VPCs never touch
	// the vpc root.
	for _, name := range slices.Sorted(maps.Keys(lz.Spec.Networks)) {
		base, err := tfrootExample("vpc")
		if err != nil {
			return nil, fmt.Errorf("read embedded vpc tfvars.example (spec.networks needs the vpc root): %w", err)
		}
		targets[filepath.Join(tfDir, "vpc", name+".tfvars")] = renderTfvars(base, clusterspec.NetworkTFVars(name, lz.Spec.Networks[name]))
	}

	for _, name := range envs {
		e, _ := lz.Env(name)
		// The per-deployment tfvars. Each starts from the root's
		// terraform.tfvars.example (so unmodeled fields keep their documented
		// defaults) and gets the spec's assignments applied.
		assigns := map[string][]clusterspec.Assign{
			"cluster":        clusterspec.ClusterTFVars(e.Cluster),
			"object-storage": clusterspec.ObjectStorageTFVars(name, e.Cluster),
		}
		for _, root := range tfRoots {
			base, err := tfrootExample(root)
			if err != nil {
				return nil, fmt.Errorf("render %s: read embedded %s tfvars.example: %w", name, root, err)
			}
			targets[filepath.Join(tfDir, root, name+".tfvars")] = renderTfvars(base, assigns[root])
		}
		if tfvarsOnly {
			continue
		}
		ct, err := committedTargets(name, e, lz.ValuesIdentity(name), aplDir, lz.Spec.DNS.AcmeEmail)
		if err != nil {
			return nil, fmt.Errorf("render %s manifests: %w", name, err)
		}
		for p, c := range ct {
			targets[p] = c
		}
	}
	return targets, nil
}

// untrackRenderedTfvars self-heals an instance that committed its per-env tfvars
// before they became gitignored build artifacts: it drops any tracked
// <env>.tfvars from the git index so the operator can commit the removal. The
// terraform-iac-bootstrap/.gitignore (shipped by the template) keeps newly
// rendered files untracked; this only handles the one-time migration of files
// already in the index. Idempotent — a no-op once nothing matches.
//
// Skipped in two cases: the in-template dev layout (relPrefix != "", not a real
// instance repo), and CI (GITHUB_ACTIONS) — there the render is ephemeral and the
// migration is a local, committed action, so CI's index must stay pristine.
func untrackRenderedTfvars(relPrefix string) {
	if relPrefix != "" || os.Getenv("GITHUB_ACTIONS") == "true" {
		return
	}
	// All tracked files under the TF roots, filtered in Go to the rendered per-env
	// tfvars across every root (cluster, object-storage, vpc).
	// terraform.tfvars.example stays tracked — it ends in .example, not .tfvars.
	listed := gitOut("ls-files", "--", "terraform-iac-bootstrap")
	var tracked []string
	for _, p := range strings.Split(strings.TrimSpace(listed), "\n") {
		if p = strings.TrimSpace(p); strings.HasSuffix(p, ".tfvars") {
			tracked = append(tracked, p)
		}
	}
	if len(tracked) == 0 {
		return
	}
	if err := execArgv(append([]string{"git", "rm", "--cached", "-q", "--"}, tracked...), ""); err != nil {
		return // best-effort: no git, not a repo, etc.
	}
	fmt.Fprintf(os.Stderr, "%s untracked %d now-gitignored tfvars (rendered from the spec) — commit the removal:\n  %s\n",
		dim("→"), len(tracked), strings.Join(tracked, "\n  "))
}

// committedTargets returns every committed apl-values/<env>/ file a deployment
// renders to, as {path → content}: the THIN manifest overlay (resources: the shared
// platform-apl/manifest base + the carved Application CRs; components: the enabled plain
// component dirs), the per-env env-revision marker, each enabled carved component's
// App CR + apps/<name>/ source root (kustomization + per-env patches), and — when an
// apl-values/values.yaml base is present — the values.yaml with
// apps.<key>.enabled + identity patched (the apl-core backend).
// resolveTemplateRef returns the ref the shared apl-values tree is fetched at
// (the remote refs RenderManifestKustomization emits). Priority:
//  1. $LLZ_TEMPLATE_REF — set by automation that renders OUTSIDE an instance:
//     release-e2e exports the SHA under test so ArgoCD fetches the shared manifests
//     from that exact commit (the .copier-answers.yml the instance would read is
//     stripped in the throwaway e2e instance);
//  2. .copier-answers.yml llz_version — the release tag a real instance pins to;
//  3. .copier-answers.yml _commit — the exact scaffold SHA, as a fallback;
//  4. "" — no instance context (the template's own render tests / an un-scaffolded
//     tree): keep every reference LOCAL, so behaviour is unchanged and `llz render
//     --check` stays deterministic without reaching for a version.
func resolveTemplateRef() string {
	if r := strings.TrimSpace(os.Getenv("LLZ_TEMPLATE_REF")); r != "" {
		return r
	}
	a, _ := readAnswers(".")
	if a == nil {
		return ""
	}
	if v := strings.TrimSpace(a.Version); v != "" {
		return v
	}
	return strings.TrimSpace(a.Commit)
}

// resolveLLZImageTag returns the tag for the in-cluster llz image the shared
// components run (reconciler / harbor-provisioner). The image NAME is a constant
// (ghcr.io/akamai-consulting/llz — no forks), so only the tag varies; a carved app's
// kustomize `images:` transformer overrides it. Priority mirrors resolveTemplateRef:
//  1. $LLZ_IMAGE_REF — release-e2e exports the signed sha image for the commit under
//     test (ghcr.io/…/llz:sha-<SHA>); take the tag after the last ':';
//  2. .copier-answers.yml llz_version — a real instance pins to its release tag;
//  3. "latest".
func resolveLLZImageTag() string {
	if r := strings.TrimSpace(os.Getenv("LLZ_IMAGE_REF")); r != "" {
		if i := strings.LastIndex(r, ":"); i >= 0 && !strings.Contains(r[i+1:], "/") {
			return r[i+1:]
		}
		return r
	}
	if a, _ := readAnswers("."); a != nil {
		if v := strings.TrimSpace(a.Version); v != "" {
			return v
		}
	}
	return "latest"
}

// repoOwnerName reduces a values-repo URL (https://github.com/<owner>/<name>.git) to
// the <owner>/<name> form the harbor provisioner's GH_REPO env wants.
func repoOwnerName(repoURL string) string {
	s := strings.TrimSuffix(strings.TrimSpace(repoURL), ".git")
	s = strings.TrimPrefix(s, "https://github.com/")
	s = strings.TrimPrefix(s, "git@github.com:")
	return s
}

func committedTargets(env string, e clusterspec.Environment, id clusterspec.ValuesIdentity, aplDir, acmeEmail string) (map[string]string, error) {
	manifest := filepath.Join(aplDir, env, "manifest")
	// Template ref the shared apl-values tree is fetched at (see RenderManifestKustomization):
	// the version this instance tracks, so an instance references the byte-identical manifest
	// layer from the template repo instead of vendoring it. Empty ref → all-local references.
	ref := resolveTemplateRef()
	// The in-cluster llz image tag (reconciler / harbor-provisioner CronJob AND the
	// clusterHealthWorkflow WorkflowTemplate) is the per-instance value a remote,
	// token-free component can't bake — a carved app's images: transformer or the
	// manifest overlay's JSON6902 patch sets it locally. See resolveLLZImageTag.
	imageTag := resolveLLZImageTag()
	// The revision this env's in-repo Argo CD content is pinned to — shared by the
	// platform-bootstrap Application, the carved component Apps, and the env-revision
	// marker. (The instance-custom ApplicationSet deliberately does NOT use it — the
	// escape hatch floats on the default branch; see RenderInstanceCustom.) Resolved
	// via Bootstrap.AppsRevision so the wedge guard in Validate sees the same value.
	revision := e.Cluster.Bootstrap.AppsRevision()
	targets := map[string]string{
		// THIN overlay over the shared base + per-component kustomize Components —
		// the shared, token-free resources are fetched from the template repo at `ref`;
		// only per-env + per-instance pieces are carried locally.
		filepath.Join(manifest, "kustomization.yaml"): clusterspec.RenderManifestKustomization(e.Components, ref, acmeEmail, imageTag),
		// per-env local-config marker the bootstrap-cluster env-revision precondition reads.
		filepath.Join(manifest, "env-revision-configmap.yaml"): clusterspec.RenderEnvRevision(revision),
		// The operator escape-hatch ApplicationSet, render-emitted locally with this
		// instance's repo (its shared base is fetched remotely, so it can't carry it).
		// It deliberately takes NO revision: unlike platform-bootstrap and the carved
		// Apps below, the hatch floats on the values repo's default branch so "drop a
		// file, Argo applies it" holds even on a release-pinned instance. See
		// RenderInstanceCustom.
		filepath.Join(manifest, "instance-custom.yaml"): clusterspec.RenderInstanceCustom(id.RepoURL),
	}
	// Carved components (blast-radius decomposition): each enabled one renders its
	// own health-inert Application CR into the manifest tree PLUS a self-contained
	// per-env source root under apps/<name>/ (the shared Component + this env's
	// patches). A Degraded resource then fails only its own App, never
	// platform-bootstrap. The App CR pins the same repo + apps_repo_revision the
	// platform-bootstrap Application uses. See docs/designs/blast-radius-decomposition.md.
	// This instance's repo slug (harbor GH_REPO) is the other per-instance value a
	// remote token-free component can't bake — the env patch sets it locally.
	ghRepo := repoOwnerName(id.RepoURL)
	for _, c := range clusterspec.Components {
		if c.CarvedApp == nil || !clusterspec.ComponentEnabled(e.Components, c.Name) {
			continue
		}
		appsDir := filepath.Join(aplDir, env, "apps", c.Name)
		targets[filepath.Join(manifest, c.CarvedApp.AppName+".yaml")] = clusterspec.RenderCarvedApp(c, env, id.RepoURL, revision)
		targets[filepath.Join(appsDir, "kustomization.yaml")] = clusterspec.RenderCarvedAppKustomization(c, ref, imageTag)
		for path, content := range carvedPatchTargets(c, appsDir, env, e, ghRepo) {
			targets[path] = content
		}
	}
	// apl-core backend: apps.<key>.enabled + the spec-owned identity/platform keys
	// patched into the shared values.yaml base. Skipped (not an error) for instances
	// without the shared overlay.
	if base, err := os.ReadFile(filepath.Join(aplDir, "values.yaml")); err == nil {
		rendered, err := clusterspec.RenderValues(base, e.Components, id)
		if err != nil {
			return nil, fmt.Errorf("render values.yaml: %w", err)
		}
		targets[filepath.Join(aplDir, env, "values.yaml")] = string(rendered)
	}
	return targets, nil
}

// carvedPatchTargets returns the per-env patch file(s) a carved component writes
// into its apps/<name>/ source root, keyed by absolute path. The content is
// component-specific (the same env-shaped values the patches carried when they
// lived in manifest/); the filename is taken from the registry Patch.Path so it
// stays in lockstep with the reference RenderCarvedAppKustomization emits. Returns
// empty for a carved component with no per-env patch (externalSecrets).
func carvedPatchTargets(c clusterspec.Component, appsDir, env string, e clusterspec.Environment, ghRepo string) map[string]string {
	out := make(map[string]string, len(c.Patches))
	content := map[string]string{}
	switch c.Name {
	case "observability":
		content["otel-collector-tls-san-patch.yaml"] = clusterspec.RenderOtelSANPatch(env)
	case "harbor":
		// Per-env HARBOR_HOST + the instance-repo GH_REPO — the harbor provisioner's
		// two per-instance env values, patched locally so components/harbor stays a
		// token-free remote base.
		content["harbor-provisioner-env-patch.yaml"] = clusterspec.RenderHarborHostPatch(e.Cluster.Bootstrap.DomainSuffix, ghRepo)
	case "broadPatRotator":
		// The account-wide label + deployment list live on the component toggle
		// (Validate guarantees both are set when it's enabled).
		tog := e.Components[c.Name]
		content["broad-pat-rotator-env-patch.yaml"] = clusterspec.RenderBroadPATEnvPatch(tog.BroadPATLabel, tog.BroadPATDeployments, ghRepo)
	case "llzReconciler":
		// REGION_SHORT (volume-labels) + REGION/OBJ_CLUSTER (linode-creds); REGION is
		// the env name and OBJ_CLUSTER the object-storage cluster.
		content["llz-reconciler-env-patch.yaml"] = clusterspec.RenderReconcilerEnvPatch(first3(env), env, e.Cluster.ObjectStorage.Cluster)
	}
	for _, p := range c.Patches {
		if body, ok := content[p.Path]; ok {
			out[filepath.Join(appsDir, p.Path)] = body
		}
	}
	return out
}

// checkManifestDrift verifies every env's committed apl-values artifacts match what
// its components render — the readiness guard so a spec edit can't silently diverge
// from the committed (Argo-synced) tree. Reports all drifted files at once. Scoped
// deliberately to the COMMITTED targets: `llz ready` scans the (gitignored) rendered
// tfvars separately, and they need not exist yet when it runs. `llz render --check`
// checks the full renderTargets set instead.
func checkManifestDrift(lz *clusterspec.LandingZone, aplDir string, envs []string) error {
	targets := map[string]string{}
	for _, name := range envs {
		e, _ := lz.Env(name)
		ct, err := committedTargets(name, e, lz.ValuesIdentity(name), aplDir, lz.Spec.DNS.AcmeEmail)
		if err != nil {
			return err
		}
		for p, c := range ct {
			targets[p] = c
		}
	}
	return reportDrift(targets, func(string) bool { return true })
}

// reportDrift compares a render target set against the tree on disk and reports
// every drifted file at once — the shared body of `llz render --check` and
// checkManifestDrift. A target whose content differs is always drift; a target that
// is ABSENT is drift only when mustExist says so, which is how the committed
// apl-values (absent == drift) are separated from the gitignored build artifacts
// under terraform-iac-bootstrap (absent == simply not rendered yet).
func reportDrift(targets map[string]string, mustExist func(path string) bool) error {
	var drifted []string
	for _, dst := range slices.Sorted(maps.Keys(targets)) {
		got, err := os.ReadFile(dst)
		if err != nil {
			if os.IsNotExist(err) && !mustExist(dst) {
				continue
			}
			drifted = append(drifted, fmt.Sprintf("%s (%v)", dst, err))
			continue
		}
		if string(got) != targets[dst] {
			drifted = append(drifted, dst)
		}
	}
	if len(drifted) == 0 {
		return nil
	}
	fmt.Fprintln(os.Stderr, "rendered files are out of sync with the spec — run `llz render`:")
	for _, d := range drifted {
		fmt.Fprintf(os.Stderr, "  • %s\n", d)
	}
	return fmt.Errorf("%d file(s) drifted from the spec", len(drifted))
}

// applyAssigns sets each `key = value` in content, replacing an existing
// assignment line (setHCLField) or appending the key when it is absent — so a
// field the example commented out (e.g. obj_key_rotation_days) is still honored.
// renderTfvars applies the spec assignments onto a root's terraform.tfvars.example
// and returns the canonically-formatted result. Formatting matters because the
// field setter replaces a value in place without re-aligning the `=` columns
// `tofu fmt` expects — so an unformatted render fails the `tofu fmt -check` in
// `llz lint` (the instance pre-commit hook). Both the write path and the
// `render --check`/`--diff` path go through here, so committed and re-rendered
// tfvars stay byte-identical (no false drift).
func renderTfvars(base string, assigns []clusterspec.Assign) string {
	return fmtHCL(applyAssigns(base, assigns))
}

// fmtHCL pipes HCL through `tofu fmt` (or `terraform fmt`). Best-effort: with
// neither binary present it returns content unchanged — render and render --check
// both call it, so they stay consistent regardless.
func fmtHCL(content string) string {
	bin := "tofu"
	if _, err := execLookPath(bin); err != nil {
		if _, err := execLookPath("terraform"); err != nil {
			return content
		}
		bin = "terraform"
	}
	cmd := exec.Command(bin, "fmt", "-")
	cmd.Stdin = strings.NewReader(content)
	out, err := cmd.Output()
	if err != nil {
		return content
	}
	return string(out)
}

func applyAssigns(content string, assigns []clusterspec.Assign) string {
	for _, a := range assigns {
		if hasHCLKey(content, a.Key) {
			content = setHCLField(content, a.Key, a.Val)
			continue
		}
		if len(content) > 0 && content[len(content)-1] != '\n' {
			content += "\n"
		}
		content += a.Key + " = " + a.Val + "\n"
	}
	return content
}

// hasHCLKey reports whether content has an uncommented `<key> =` assignment: a line
// STARTING with key (so a commented-out `# key = …` does not match) followed by
// optional blanks and `=`. A line scan rather than a regexp — this runs once per
// assignment × per root × per env on both the write and the --diff path, and the
// old `regexp.MustCompile` on every call recompiled the pattern every time.
func hasHCLKey(content, key string) bool {
	for _, line := range strings.Split(content, "\n") {
		rest, ok := strings.CutPrefix(line, key)
		if ok && strings.HasPrefix(strings.TrimLeft(rest, " \t"), "=") {
			return true
		}
	}
	return false
}

// filepathRel renders dst relative to tfDir's parent for tidy operator output;
// falls back to dst on error.
func filepathRel(tfDir, dst string) string {
	if rel, err := filepath.Rel(filepath.Dir(tfDir), dst); err == nil {
		return rel
	}
	return dst
}

// runRenderDiff prints, per target file, whether a render would create or change
// it (with a compact line diff), writing nothing. It previews the SAME renderTargets
// set the write path writes and --check compares, so the preview cannot drift from
// what a render actually does: the generated TF roots, the shared-VPC tfvars, each
// env's tfvars, and — unless tfvarsOnly — the committed apl-values artifacts.
func runRenderDiff(lz *clusterspec.LandingZone, envs []string, tfDir, aplDir string, tfvarsOnly bool) error {
	want, err := renderTargets(lz, envs, tfDir, aplDir, tfvarsOnly)
	if err != nil {
		return err
	}
	changed := 0
	for _, p := range slices.Sorted(maps.Keys(want)) {
		cur, err := os.ReadFile(p)
		switch {
		case err != nil:
			changed++
			fmt.Printf("%s %s\n", green("+ new    "), p)
			fmt.Println(indent(lineDiff("", want[p]), "    "))
		case string(cur) != want[p]:
			changed++
			fmt.Printf("%s %s\n", yellow("~ changed"), p)
			fmt.Println(indent(lineDiff(string(cur), want[p]), "    "))
		}
	}
	if changed == 0 {
		fmt.Printf("%s render is a no-op — all %d target file(s) already match the spec\n", green("✓"), len(want))
		return nil
	}
	fmt.Printf("\n%d file(s) would change. Run `llz render` to apply.\n", changed)
	return nil
}

// lineDiff returns a compact unified diff of old→new via a line-level LCS, so
// scattered changes show as separate small hunks (collapsed unchanged runs become
// "…") rather than one inflated block. Output is capped to keep a preview short.
func lineDiff(oldS, newS string) string {
	var o, n []string
	if oldS != "" {
		o = strings.Split(strings.TrimRight(oldS, "\n"), "\n")
	}
	n = strings.Split(strings.TrimRight(newS, "\n"), "\n")

	type dl struct {
		sign byte
		text string
	}
	var seq []dl

	// Trim the common PREFIX before the DP below, which allocates a full m×k int
	// matrix — a 2000-line tfroot with a late change would otherwise cost ~32MB to
	// preview 20 lines. This is exactly output-preserving: the backtrack's first case
	// is `o[i] == n[j]`, so identical leading lines are always emitted as context in
	// the same order. (Trimming the common SUFFIX is NOT safe here — the backtrack
	// breaks LCS ties toward deletions, so e.g. ["a","x"]→["x","x"] emits `-a  x +x`
	// with the full matrix but `-a +x  x` if the trailing "x" is trimmed off first.)
	for len(o) > 0 && len(n) > 0 && o[0] == n[0] {
		seq = append(seq, dl{' ', n[0]})
		o, n = o[1:], n[1:]
	}

	// LCS-length DP, then backtrack into a ' '/'-'/'+' line sequence.
	m, k := len(o), len(n)
	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, k+1)
	}
	for i := m - 1; i >= 0; i-- {
		for j := k - 1; j >= 0; j-- {
			if o[i] == n[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	i, j := 0, 0
	for i < m && j < k {
		switch {
		case o[i] == n[j]:
			seq = append(seq, dl{' ', n[j]})
			i, j = i+1, j+1
		case dp[i+1][j] >= dp[i][j+1]:
			seq = append(seq, dl{'-', o[i]})
			i++
		default:
			seq = append(seq, dl{'+', n[j]})
			j++
		}
	}
	for ; i < m; i++ {
		seq = append(seq, dl{'-', o[i]})
	}
	for ; j < k; j++ {
		seq = append(seq, dl{'+', n[j]})
	}

	// Keep changed lines + a little context; collapse the rest.
	const ctx, capLines = 2, 20
	keep := make([]bool, len(seq))
	for idx, d := range seq {
		if d.sign != ' ' {
			for w := max(0, idx-ctx); w <= min(len(seq)-1, idx+ctx); w++ {
				keep[w] = true
			}
		}
	}
	var b strings.Builder
	emitted, gap := 0, false
	for idx, d := range seq {
		if !keep[idx] {
			gap = true
			continue
		}
		if gap {
			fmt.Fprintf(&b, "  %s\n", dim("…"))
			gap = false
		}
		if emitted >= capLines {
			fmt.Fprintf(&b, "  %s\n", dim("… (more — run `llz render`, then `git diff`)"))
			break
		}
		switch d.sign {
		case '-':
			fmt.Fprintf(&b, "%s %s\n", red("-"), d.text)
		case '+':
			fmt.Fprintf(&b, "%s %s\n", green("+"), d.text)
		default:
			fmt.Fprintf(&b, "  %s\n", d.text)
		}
		emitted++
	}
	return b.String()
}
