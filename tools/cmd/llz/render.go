package main

// render.go reconciles the declarative LandingZone spec (llz.yaml, see
// internal/clusterspec) into the files the rest of the toolchain already
// consumes. PR1 covers the tfvars half: for each deployment it writes the three
// <env>.tfvars in the working tree from spec.environments.<env>.cluster, which
// `terraform -var-file=<env>.tfvars` then picks up (the CI step runs this before
// plan/apply; the file is transient and not committed). Recipe/manifest rendering
// and copier-answers sync land in later PRs.
//
// The pure spec→tfvars mapping lives in clusterspec (tfvars_map.go); this file is
// the thin apply loop — it reads each root's terraform.tfvars.example and sets
// (or appends) each assignment with setHCLField (shared with `llz env add`).

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
	"github.com/spf13/cobra"
)

func renderCmd() *cobra.Command {
	var tfvarsOnly, check bool
	c := &cobra.Command{
		Use:   "render [env]",
		Short: "reconcile llz.yaml into <env>.tfvars (spec-driven instances)",
		Long: "Reads the repo-root llz.yaml (kind: LandingZone) and renders each\n" +
			"deployment's cluster definition into the three terraform-iac-bootstrap/*/\n" +
			"<env>.tfvars files the terraform plan/apply consume. With no [env], renders\n" +
			"every environment in the spec. --check validates the spec without writing.\n" +
			"A no-op contract: callers gate on `test -f llz.yaml` (CI does), so instances\n" +
			"that have not adopted the spec are unaffected.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			env := ""
			if len(args) == 1 {
				env = args[0]
			}
			return runRender(gopts, env, tfvarsOnly, check)
		},
	}
	c.Flags().BoolVar(&tfvarsOnly, "tfvars-only", false, "render only the tfvars (skip recipe/manifest rendering)")
	c.Flags().BoolVar(&check, "check", false, "validate the spec and exit non-zero on any error; write nothing")
	return c
}

func runRender(g globalOpts, env string, tfvarsOnly, check bool) error {
	specPath := clusterspec.DefaultFile
	if !clusterspec.Exists(specPath) {
		return fmt.Errorf("no %s found in the current directory — `llz render` needs a LandingZone spec", specPath)
	}
	lz, err := clusterspec.Load(specPath)
	if err != nil {
		return err
	}
	if errs := lz.Validate(); len(errs) > 0 {
		fmt.Fprintf(os.Stderr, "%s is invalid (%d problem(s)):\n", specPath, len(errs))
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "  • %v\n", e)
		}
		return fmt.Errorf("invalid LandingZone spec")
	}
	if check {
		fmt.Printf("✓ %s valid (%d environment(s))\n", specPath, len(lz.Spec.Environments))
		return nil
	}

	envs := lz.EnvNames()
	if env != "" {
		if _, ok := lz.Env(env); !ok {
			return fmt.Errorf("env %q not in %s (have: %v)", env, specPath, lz.EnvNames())
		}
		envs = []string{env}
	}

	tfDir, _, relPrefix := instanceLayout()
	dryRun := g.dryRun
	for _, name := range envs {
		e, _ := lz.Env(name)
		if err := renderEnvTfvars(name, e.Cluster, tfDir, relPrefix, dryRun); err != nil {
			return fmt.Errorf("render %s: %w", name, err)
		}
	}
	if !tfvarsOnly {
		// Recipe/manifest rendering arrives in a later PR; the flag is accepted
		// now so the CI step and operators can opt into tfvars-only explicitly.
		fmt.Println("note: recipe/manifest rendering is not yet implemented; only tfvars were rendered")
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
