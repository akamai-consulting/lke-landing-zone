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
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
	"github.com/spf13/cobra"
)

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
// (color.go). `note` is an optional trailing parenthetical (e.g. "(N assignments)").
func renderedPath(prefix, path string) {
	fmt.Printf("  %s  %s%s\n", green("rendered"), prefix, path)
}

func wouldRenderPath(prefix, path, note string) {
	if note != "" {
		note = " " + dim(note)
	}
	fmt.Printf("  %s  %s%s%s\n", cyan("would-render"), prefix, path, note)
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

	// --check is the CI drift guard: the spec is valid AND the committed manifest
	// kustomizations match what the spec renders (they are committed because Argo
	// syncs git; a working-tree-only render would let them silently diverge).
	if check {
		if !tfvarsOnly {
			if err := checkManifestDrift(lz, aplDir, envs); err != nil {
				return err
			}
		}
		fmt.Printf("%s LandingZone spec valid (%d environment(s)); committed manifests in sync\n", green("✓"), len(lz.Spec.Environments))
		return nil
	}

	// --diff previews what a render would create/change, writing nothing.
	if diff {
		return runRenderDiff(lz, envs, tfDir, aplDir, tfvarsOnly)
	}

	dryRun := g.dryRun
	// Shared VPCs (spec.networks) render to vpc/<name>.tfvars and must exist before
	// the clusters that attach to them. No-op when no networks are declared.
	if err := renderNetworks(lz, tfDir, relPrefix, dryRun); err != nil {
		return err
	}
	for _, name := range envs {
		e, _ := lz.Env(name)
		if err := renderEnvTfvars(name, e.Cluster, tfDir, relPrefix, dryRun); err != nil {
			return fmt.Errorf("render %s: %w", name, err)
		}
		if !tfvarsOnly {
			if err := renderManifest(name, e, lz.ValuesIdentity(name), aplDir, relPrefix, dryRun); err != nil {
				return fmt.Errorf("render %s manifests: %w", name, err)
			}
		}
	}
	// The shared DNS tree's ACME email is instance-wide — render it ONCE (not per
	// env) into apl-values/_shared after the per-env loop.
	if !tfvarsOnly {
		if p, content, ok := sharedDNSEmailTarget(lz, aplDir); ok {
			if dryRun {
				wouldRenderPath(relPrefix, filepathRel(aplDir, p), "")
			} else {
				if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
					return fmt.Errorf("render shared dns email: %w", err)
				}
				renderedPath(relPrefix, filepathRel(aplDir, p))
			}
		}
	}
	if !dryRun {
		untrackRenderedTfvars(relPrefix)
	}
	return nil
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
	// tfvars across every root (cluster, cluster-bootstrap, object-storage, vpc).
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
// _shared/manifest base + components: the enabled component dirs), the per-env
// env-revision marker, the volume-labeler REGION_SHORT patch (when enabled), and —
// when an apl-values/_shared/values.yaml base is present — the values.yaml with
// apps.<key>.enabled + identity patched (the apl-core backend).
func committedTargets(env string, e clusterspec.Environment, id clusterspec.ValuesIdentity, aplDir string) (map[string]string, error) {
	manifest := filepath.Join(aplDir, env, "manifest")
	targets := map[string]string{
		// THIN overlay over the shared base + per-component kustomize Components —
		// the resources live ONCE in apl-values/_shared/ + apl-values/components/,
		// never copied per env.
		filepath.Join(manifest, "kustomization.yaml"): clusterspec.RenderManifestKustomization(e.Components),
		// per-env local-config marker the cluster-bootstrap precondition reads.
		filepath.Join(manifest, "env-revision-configmap.yaml"): clusterspec.RenderEnvRevision(orElse(e.Cluster.Bootstrap.AppsRepoRevision, "main")),
	}
	// The one genuine per-env manifest delta: the volume-labeler REGION_SHORT patch
	// the thin overlay references — emitted only when volumeLabeler is enabled.
	if clusterspec.ComponentEnabled(e.Components, "volumeLabeler") {
		targets[filepath.Join(manifest, "linode-volume-labeler-region-patch.yaml")] = clusterspec.RenderRegionPatch(first3(env))
	}
	// The Linode credential rotator's per-env REGION + OBJ_CLUSTER patch — emitted
	// only when linodeCredRotator is enabled (region = deployment name, obj_cluster
	// = the spec's object-storage cluster).
	if clusterspec.ComponentEnabled(e.Components, "linodeCredRotator") {
		targets[filepath.Join(manifest, "linode-cred-rotator-env-patch.yaml")] = clusterspec.RenderRotatorEnvPatch(env, e.Cluster.ObjectStorage.Cluster)
	}
	// apl-core backend: apps.<key>.enabled + the spec-owned identity/platform keys
	// patched into the shared values.yaml base. Skipped (not an error) for instances
	// without the shared overlay.
	if base, err := os.ReadFile(filepath.Join(aplDir, "_shared", "values.yaml")); err == nil {
		rendered, err := clusterspec.RenderValues(base, e.Components, id)
		if err != nil {
			return nil, fmt.Errorf("render values.yaml: %w", err)
		}
		targets[filepath.Join(aplDir, env, "values.yaml")] = string(rendered)
	}
	return targets, nil
}

// sharedDNSEmailTarget returns the instance-wide letsencrypt ClusterIssuer path and
// its email-substituted content when spec.dns.acmeEmail is set (and the shared dns
// tree is present). The ACME email is instance-wide, so it renders ONCE into
// apl-values/_shared/manifest/dns/ — not per env (the whole dns tree is applied from
// _shared by `llz bootstrap dns`). ok=false (no target) when the email is unset: the
// file keeps its REPLACE_PER_ENV placeholder, which `llz doctor` flags as a deferrable
// cert/DNS item and `llz bootstrap dns` finishes after the first build.
func sharedDNSEmailTarget(lz *clusterspec.LandingZone, aplDir string) (string, string, bool) {
	email := lz.Spec.DNS.AcmeEmail
	if email == "" {
		return "", "", false
	}
	p := filepath.Join(aplDir, "_shared", "manifest", "dns", "letsencrypt-clusterissuer.yaml")
	base, err := os.ReadFile(p)
	if err != nil {
		return "", "", false // older layout without the shared dns tree — skip silently
	}
	return p, clusterspec.SetACMEEmail(string(base), email), true
}

// renderManifest writes a deployment's committed apl-values/<env>/ artifacts (the
// manifest kustomizations + the apps-toggled values.yaml) from its components.
func renderManifest(env string, e clusterspec.Environment, id clusterspec.ValuesIdentity, aplDir, relPrefix string, dryRun bool) error {
	targets, err := committedTargets(env, e, id, aplDir)
	if err != nil {
		return err
	}
	for dst, content := range targets {
		if dryRun {
			wouldRenderPath(relPrefix, filepathRel(aplDir, dst), "")
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, []byte(content), 0o644); err != nil {
			return err
		}
		renderedPath(relPrefix, filepathRel(aplDir, dst))
	}
	return nil
}

// checkManifestDrift verifies every env's committed apl-values artifacts match what
// its components render — the CI guard so a spec edit can't silently diverge from
// the committed (Argo-synced) tree. Reports all drifted files at once.
func checkManifestDrift(lz *clusterspec.LandingZone, aplDir string, envs []string) error {
	var drifted []string
	for _, name := range envs {
		e, _ := lz.Env(name)
		targets, err := committedTargets(name, e, lz.ValuesIdentity(name), aplDir)
		if err != nil {
			return err
		}
		for dst, want := range targets {
			got, err := os.ReadFile(dst)
			if err != nil {
				drifted = append(drifted, fmt.Sprintf("%s (%v)", dst, err))
				continue
			}
			if string(got) != want {
				drifted = append(drifted, dst)
			}
		}
	}
	// Instance-wide: the shared DNS tree's rendered ACME email (when spec.dns.acmeEmail
	// is set) must match too, so a spec email edit that wasn't re-rendered is caught.
	if p, want, ok := sharedDNSEmailTarget(lz, aplDir); ok {
		if got, err := os.ReadFile(p); err != nil || string(got) != want {
			drifted = append(drifted, p)
		}
	}
	if len(drifted) > 0 {
		fmt.Fprintln(os.Stderr, "committed apl-values are out of sync with the spec — run `llz render`:")
		for _, d := range drifted {
			fmt.Fprintf(os.Stderr, "  • %s\n", d)
		}
		return fmt.Errorf("%d apl-values file(s) drifted from the spec", len(drifted))
	}
	return nil
}

// renderNetworks writes one vpc/<name>.tfvars per shared VPC in spec.networks
// (vpc_label + region) from the vpc root's terraform.tfvars.example. Each is its
// own apply (state key vpc/<name>). No-op when none are declared, so instances
// that use only dedicated VPCs never touch the vpc root.
func renderNetworks(lz *clusterspec.LandingZone, tfDir, relPrefix string, dryRun bool) error {
	names := make([]string, 0, len(lz.Spec.Networks))
	for n := range lz.Spec.Networks {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		assigns := clusterspec.NetworkTFVars(name, lz.Spec.Networks[name])
		src := filepath.Join(tfDir, "vpc", tplTfvars)
		dst := filepath.Join(tfDir, "vpc", name+".tfvars")
		base, err := os.ReadFile(src)
		if err != nil {
			return fmt.Errorf("read %s (spec.networks needs the terraform-iac-bootstrap/vpc root): %w", src, err)
		}
		out := renderTfvars(string(base), assigns)
		if dryRun {
			wouldRenderPath(relPrefix, filepathRel(tfDir, dst), fmt.Sprintf("(%d assignments)", len(assigns)))
			continue
		}
		if err := os.WriteFile(dst, []byte(out), 0o644); err != nil {
			return err
		}
		renderedPath(relPrefix, filepathRel(tfDir, dst))
	}
	return nil
}

// renderEnvTfvars writes the three <env>.tfvars for one deployment from the
// spec's cluster definition. Each starts from the root's terraform.tfvars.example
// (so unmodeled fields keep their documented defaults) and gets the spec's
// assignments applied.
func renderEnvTfvars(env string, c clusterspec.Cluster, tfDir, relPrefix string, dryRun bool) error {
	roots := map[string][]clusterspec.Assign{
		"cluster":           clusterspec.ClusterTFVars(c),
		"cluster-bootstrap": clusterspec.BootstrapTFVars(env, c),
		"object-storage":    clusterspec.ObjectStorageTFVars(env, c),
	}
	for _, root := range tfRoots {
		src := filepath.Join(tfDir, root, tplTfvars)
		dst := filepath.Join(tfDir, root, env+".tfvars")
		base, err := os.ReadFile(src)
		if err != nil {
			return fmt.Errorf("read %s: %w", src, err)
		}
		out := renderTfvars(string(base), roots[root])
		if dryRun {
			wouldRenderPath(relPrefix, filepathRel(tfDir, dst), fmt.Sprintf("(%d assignments)", len(roots[root])))
			continue
		}
		if err := os.WriteFile(dst, []byte(out), 0o644); err != nil {
			return err
		}
		renderedPath(relPrefix, filepathRel(tfDir, dst))
	}
	return nil
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

// hasHCLKey reports whether content has an uncommented `<key> =` assignment.
func hasHCLKey(content, key string) bool {
	return regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(key) + `\s*=`).MatchString(content)
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
// it (with a compact line diff), writing nothing. It mirrors what runRender emits:
// the shared-VPC tfvars, each env's three tfvars, and — unless tfvarsOnly — the
// committed apl-values artifacts.
func runRenderDiff(lz *clusterspec.LandingZone, envs []string, tfDir, aplDir string, tfvarsOnly bool) error {
	want := map[string]string{} // path -> would-be content
	readExample := func(root string) (string, error) {
		b, err := os.ReadFile(filepath.Join(tfDir, root, tplTfvars))
		return string(b), err
	}

	netNames := make([]string, 0, len(lz.Spec.Networks))
	for n := range lz.Spec.Networks {
		netNames = append(netNames, n)
	}
	sort.Strings(netNames)
	for _, n := range netNames {
		base, err := readExample("vpc")
		if err != nil {
			return fmt.Errorf("read vpc tfvars.example: %w", err)
		}
		want[filepath.Join(tfDir, "vpc", n+".tfvars")] = renderTfvars(base, clusterspec.NetworkTFVars(n, lz.Spec.Networks[n]))
	}

	for _, name := range envs {
		e, _ := lz.Env(name)
		for root, assigns := range map[string][]clusterspec.Assign{
			"cluster":           clusterspec.ClusterTFVars(e.Cluster),
			"cluster-bootstrap": clusterspec.BootstrapTFVars(name, e.Cluster),
			"object-storage":    clusterspec.ObjectStorageTFVars(name, e.Cluster),
		} {
			base, err := readExample(root)
			if err != nil {
				return fmt.Errorf("read %s tfvars.example: %w", root, err)
			}
			want[filepath.Join(tfDir, root, name+".tfvars")] = renderTfvars(base, assigns)
		}
		if !tfvarsOnly {
			ct, err := committedTargets(name, e, lz.ValuesIdentity(name), aplDir)
			if err != nil {
				return err
			}
			for p, c := range ct {
				want[p] = c
			}
		}
	}
	if !tfvarsOnly {
		if p, content, ok := sharedDNSEmailTarget(lz, aplDir); ok {
			want[p] = content
		}
	}

	paths := make([]string, 0, len(want))
	for p := range want {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	changed := 0
	for _, p := range paths {
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
	type dl struct {
		sign byte
		text string
	}
	var seq []dl
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
