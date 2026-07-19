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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// renderedArgoApp is the slice of a rendered ArgoCD Application/AppProject this
// gate reads.
type renderedArgoApp struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name        string            `yaml:"name"`
		Annotations map[string]string `yaml:"annotations"`
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
	// collectManifestPaths (shared with the other tree-scanning guards) also picks
	// up *.yml, which this hand-rolled walk ignored.
	files, err := collectManifestPaths([]string{renderDir})
	if err != nil || len(files) == 0 {
		return fmt.Errorf("argocd-rendered-apps: no rendered manifests under %s/ — run 'make render-charts' first", renderDir)
	}

	apps, problems := 0, 0
	for _, f := range files {
		raw, readErr := os.ReadFile(f)
		if readErr != nil {
			return fmt.Errorf("reading %s: %w", f, readErr)
		}
		// Documents that do not parse as an Application shape are skipped without
		// abandoning the rest of the file; schema validation owns malformed-manifest
		// reporting.
		for _, app := range decodeDocs(string(raw), func(a renderedArgoApp) bool {
			return a.Kind == "Application" || a.Kind == "AppProject"
		}) {
			apps++
			name := app.Metadata.Name
			if name == "" {
				name = "<unknown>"
			}
			// Every Application/AppProject must declare a sync-wave. Without one it
			// defaults to wave 0 and races the resources it should be ordered
			// against, which can deadlock a greenfield install.
			if msg := missingSyncWave(app); msg != "" {
				fmt.Fprintf(os.Stderr, "::error file=%s::%s %q %s\n", f, app.Kind, name, msg)
				problems++
			}
			if app.Kind != "Application" {
				continue
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
	fmt.Fprintf(out, "%d rendered ArgoCD Application(s)/AppProject(s) passed semantic validation.\n", apps)
	return nil
}

// missingSyncWave returns a complaint when the object has no usable
// argocd.argoproj.io/sync-wave annotation, or "" when it is fine.
//
// This absorbs the former `sync-wave-lint` Makefile target, which was FILE
// scoped: it grepped the whole file for `^kind: (Application|AppProject)` and
// then for the sync-wave string anywhere in that same file. Helm renders many
// Applications per output file, so ONE annotated Application satisfied the check
// for every other Application beside it. It also matched the annotation name in a
// comment, never checked the value parsed as an integer, and never required it to
// sit under metadata.annotations. Decoding per document fixes all four.
func missingSyncWave(app renderedArgoApp) string {
	v, ok := app.Metadata.Annotations[syncWaveAnnotation]
	if !ok {
		return "has no " + syncWaveAnnotation + " annotation — it would default to wave 0 and race the resources it should be ordered against (deadlocks a greenfield install)"
	}
	if _, err := strconv.Atoi(strings.TrimSpace(v)); err != nil {
		return fmt.Sprintf("has a non-integer %s value %q — Argo CD ignores what it cannot parse, so this silently behaves as wave 0", syncWaveAnnotation, v)
	}
	return ""
}

// syncWaveAnnotation is the Argo CD ordering annotation both this guard and the
// wave-health guards read.
const syncWaveAnnotation = "argocd.argoproj.io/sync-wave"

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
