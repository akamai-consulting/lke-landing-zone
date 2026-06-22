package main

// render.go reconciles the declarative LandingZone spec (landingzone.yaml +
// environments/<env>.yaml, see internal/clusterspec) into the files the rest of
// the toolchain consumes. Two targets:
//   - the three <env>.tfvars (transient, working-tree) from the env's cluster
//     definition, which `terraform -var-file=<env>.tfvars` picks up at build time;
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
	"path/filepath"
	"regexp"
	"sort"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
	"github.com/spf13/cobra"
)

// envVPCCmd prints the shared VPC (spec.networks name) a deployment attaches to,
// or an empty line for a dedicated VPC. It reads the rendered cluster/<env>.tfvars
// (vpc_network), so the apply-vpc workflow step can decide whether — and which —
// shared VPC to apply before the cluster.
func envVPCCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "vpc <deployment>",
		Short: "print the shared VPC a deployment attaches to (spec.networks name); empty for a dedicated VPC",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := validateEnvName(args[0]); err != nil {
				return err
			}
			tfDir, _, _ := instanceLayout()
			p := filepath.Join(tfDir, "cluster", args[0]+".tfvars")
			b, err := os.ReadFile(p)
			if err != nil {
				return fmt.Errorf("read %s (for spec-driven instances run `llz render %s` first): %w", p, args[0], err)
			}
			fmt.Println(tfvarsValue(string(b), "vpc_network"))
			return nil
		},
	}
}

func renderCmd() *cobra.Command {
	var tfvarsOnly, check bool
	c := &cobra.Command{
		Use:   "render [env]",
		Short: "reconcile the LandingZone spec into <env>.tfvars (spec-driven instances)",
		Long: "Reads the LandingZone spec (landingzone.yaml + environments/<env>.yaml) and\n" +
			"renders each deployment's cluster definition into the three\n" +
			"terraform-iac-bootstrap/*/<env>.tfvars files the terraform plan/apply consume.\n" +
			"With no [env], renders every environment in the spec. --check validates the\n" +
			"spec without writing. A no-op contract: callers gate on the presence of a spec\n" +
			"(CI does), so instances that have not adopted it are unaffected.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			env := ""
			if len(args) == 1 {
				env = args[0]
			}
			return runRender(gopts, env, tfvarsOnly, check)
		},
	}
	c.Flags().BoolVar(&tfvarsOnly, "tfvars-only", false, "render only the tfvars (skip the committed manifest kustomizations)")
	c.Flags().BoolVar(&check, "check", false, "validate the spec and exit non-zero on any error; write nothing")
	return c
}

func runRender(g globalOpts, env string, tfvarsOnly, check bool) error {
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
		fmt.Printf("✓ LandingZone spec valid (%d environment(s)); committed manifests in sync\n", len(lz.Spec.Environments))
		return nil
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
			if err := renderManifest(name, e.Components, lz.ValuesIdentity(name), aplDir, relPrefix, dryRun); err != nil {
				return fmt.Errorf("render %s manifests: %w", name, err)
			}
		}
	}
	return nil
}

// committedTargets returns every committed apl-values/<env>/ file a deployment's
// component toggles render to, as {path → content}: the two manifest kustomizations
// (the llz Argo backend) and — when an apl-values/example/values.yaml template is
// present — the values.yaml with apps.<key>.enabled patched (the apl-core backend).
func committedTargets(env string, components map[string]clusterspec.ComponentToggle, id clusterspec.ValuesIdentity, aplDir string) (map[string]string, error) {
	manifest := filepath.Join(aplDir, env, "manifest")
	targets := map[string]string{
		filepath.Join(manifest, "kustomization.yaml"):           clusterspec.RenderManifestKustomization(components),
		filepath.Join(manifest, "argocd", "kustomization.yaml"): clusterspec.RenderArgoKustomization(components),
	}
	// apl-core backend: patch apps.<key>.enabled + the spec-owned identity/platform
	// keys into the shared values.yaml template. Skipped (not an error) for
	// instances without the example overlay.
	if base, err := os.ReadFile(filepath.Join(aplDir, "example", "values.yaml")); err == nil {
		rendered, err := clusterspec.RenderValues(base, components, id)
		if err != nil {
			return nil, fmt.Errorf("render values.yaml: %w", err)
		}
		targets[filepath.Join(aplDir, env, "values.yaml")] = string(rendered)
	}
	return targets, nil
}

// renderManifest writes a deployment's committed apl-values/<env>/ artifacts (the
// manifest kustomizations + the apps-toggled values.yaml) from its components.
func renderManifest(env string, components map[string]clusterspec.ComponentToggle, id clusterspec.ValuesIdentity, aplDir, relPrefix string, dryRun bool) error {
	targets, err := committedTargets(env, components, id, aplDir)
	if err != nil {
		return err
	}
	for dst, content := range targets {
		if dryRun {
			fmt.Printf("  would-render  %s%s\n", relPrefix, filepathRel(aplDir, dst))
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, []byte(content), 0o644); err != nil {
			return err
		}
		fmt.Printf("  rendered  %s%s\n", relPrefix, filepathRel(aplDir, dst))
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
		targets, err := committedTargets(name, e.Components, lz.ValuesIdentity(name), aplDir)
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
		out := applyAssigns(string(base), assigns)
		if dryRun {
			fmt.Printf("  would-render  %s%s (%d assignments)\n", relPrefix, filepathRel(tfDir, dst), len(assigns))
			continue
		}
		if err := os.WriteFile(dst, []byte(out), 0o644); err != nil {
			return err
		}
		fmt.Printf("  rendered  %s%s\n", relPrefix, filepathRel(tfDir, dst))
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
		out := applyAssigns(string(base), roots[root])
		if dryRun {
			fmt.Printf("  would-render  %s%s (%d assignments)\n", relPrefix, filepathRel(tfDir, dst), len(roots[root]))
			continue
		}
		if err := os.WriteFile(dst, []byte(out), 0o644); err != nil {
			return err
		}
		fmt.Printf("  rendered  %s%s\n", relPrefix, filepathRel(tfDir, dst))
	}
	return nil
}

// applyAssigns sets each `key = value` in content, replacing an existing
// assignment line (setHCLField) or appending the key when it is absent — so a
// field the example commented out (e.g. obj_key_rotation_days) is still honored.
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
