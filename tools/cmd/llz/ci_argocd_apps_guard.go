package main

// ci_argocd_apps_guard.go implements `llz ci argocd-rendered-apps` — the native
// port of validate-argocd-rendered-apps.py (the Makefile's
// argocd-rendered-apps-check, run after render-charts). It validates semantic
// properties of the rendered ArgoCD Application manifests that schema checks do
// not catch — currently: a Helm `parameters:` list must not name the same
// parameter twice (a duplicate silently shadows the earlier value at sync time).
//
// Unlike the Python original, which scraped `parameters:` blocks with regexes,
// this parses each rendered document as YAML and reads spec.source.helm and
// spec.sources[].helm — duplicate names survive because a YAML sequence keeps
// repeated entries. The duplicate-detection core is pure and unit-tested.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// renderedArgoApp is the slice of a rendered ArgoCD Application this gate reads.
type renderedArgoApp struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Source  *argoSource  `yaml:"source"`
		Sources []argoSource `yaml:"sources"`
	} `yaml:"spec"`
}

type argoSource struct {
	Helm *struct {
		Parameters []struct {
			Name string `yaml:"name"`
		} `yaml:"parameters"`
	} `yaml:"helm"`
}

func ciArgoCDRenderedAppsCmd() *cobra.Command {
	var root, renderDir string
	c := &cobra.Command{
		Use:   "argocd-rendered-apps",
		Short: "reject rendered ArgoCD Applications with duplicate Helm parameters",
		Long: "Native port of validate-argocd-rendered-apps.py (the Makefile's\n" +
			"argocd-rendered-apps-check, run after render-charts). Parses every rendered\n" +
			"manifest under the render dir and fails if any ArgoCD Application names the\n" +
			"same Helm parameter twice — a duplicate silently shadows the earlier value at\n" +
			"sync time, which schema validation does not catch.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if renderDir == "" {
				renderDir = "rendered"
			}
			return runArgoCDRenderedApps(filepath.Join(root, renderDir), os.Stdout)
		},
	}
	c.Flags().StringVar(&root, "root", ".", "repository root")
	c.Flags().StringVar(&renderDir, "render-dir", "rendered", "rendered-manifests directory (relative to --root)")
	return c
}

func runArgoCDRenderedApps(renderDir string, out io.Writer) error {
	var files []string
	err := filepath.WalkDir(renderDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".yaml") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil || len(files) == 0 {
		return fmt.Errorf("argocd-rendered-apps: no rendered manifests under %s/ — run 'make render-charts' first", renderDir)
	}
	sort.Strings(files)

	apps, problems := 0, 0
	for _, f := range files {
		raw, readErr := os.ReadFile(f)
		if readErr != nil {
			return fmt.Errorf("reading %s: %w", f, readErr)
		}
		dec := yaml.NewDecoder(strings.NewReader(string(raw)))
		for {
			var app renderedArgoApp
			if decErr := dec.Decode(&app); decErr != nil {
				if errors.Is(decErr, io.EOF) {
					break
				}
				// Skip a document that does not parse as an Application shape;
				// schema validation owns malformed-manifest reporting.
				continue
			}
			if app.Kind != "Application" {
				continue
			}
			apps++
			name := app.Metadata.Name
			if name == "" {
				name = "<unknown>"
			}
			for _, dup := range duplicateHelmParams(app) {
				fmt.Fprintf(os.Stderr, "::error file=%s::Rendered Application '%s' has duplicate Helm parameter '%s'\n", f, name, dup)
				problems++
			}
		}
	}

	if problems > 0 {
		return fmt.Errorf("argocd-rendered-apps: %d rendered ArgoCD Application validation error(s)", problems)
	}
	fmt.Fprintf(out, "%d rendered ArgoCD Application(s) passed semantic validation.\n", apps)
	return nil
}

// duplicateHelmParams returns the sorted set of Helm parameter names that appear
// more than once across an Application's source and sources[] helm blocks.
func duplicateHelmParams(app renderedArgoApp) []string {
	counts := map[string]int{}
	srcs := app.Spec.Sources
	if app.Spec.Source != nil {
		srcs = append(srcs, *app.Spec.Source)
	}
	for _, s := range srcs {
		if s.Helm == nil {
			continue
		}
		for _, p := range s.Helm.Parameters {
			counts[p.Name]++
		}
	}
	var dups []string
	for name, n := range counts {
		if n > 1 {
			dups = append(dups, name)
		}
	}
	sort.Strings(dups)
	return dups
}
