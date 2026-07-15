package main

// ci_check_prom_rules.go implements `llz ci check-prom-rules` — the native port
// of the former template-scripts/linting-and-validation/
// check-prometheus-rule-crds.py (the Makefile's prom-rules-check target, and
// the last first-party Python script in the repo).
//
// Apl-core's kube-prometheus-stack consumes PrometheusRule CRDs (the wrapped
// `kind: PrometheusRule` / `spec.groups` form via its ruleSelector), but
// `promtool check rules` only understands the bare `groups:` document. So for
// each CRD this extracts spec.groups, writes the bare-groups form to a
// tempfile, and runs promtool against it. The extraction is pure (see
// extractBareGroups) so the CRD-shape handling is unit-tested without promtool.
//
// Exit semantics match the Python: exit 0 when every file parses and promtool
// reports SUCCESS, non-zero when one or more files fail. The default
// --rules-dir is the observability component's prometheus-rules/ tree (the
// template DOES ship PrometheusRules there — openbao-alerts,
// support-plane-alerts); an instance overlay that removes the component skips
// cleanly on the absent directory.

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// defaultPromRulesDir is where the template ships its PrometheusRule CRDs
// (matched by kube-prometheus-stack's ruleSelector). It was
// platform-apl/manifest/observability/prometheus-rules-crd until the
// rules moved into the observability component — the stale default made the
// gate skip-clean on every run, so nothing promtool-validated the live rules.
const defaultPromRulesDir = "platform-apl/components/observability/prometheus-rules"

// extractBareGroups parses a PrometheusRule CRD and returns the bare-groups
// YAML document (`groups: …`) promtool expects. Pure and faithful to the Python
// helper: it errors on a non-PrometheusRule kind or an empty/absent spec.groups,
// and preserves the groups subtree verbatim (yaml.Node) so promtool sees exactly
// the rules the cluster runs.
func extractBareGroups(data []byte) ([]byte, error) {
	var doc struct {
		Kind string `yaml:"kind"`
		Spec struct {
			Groups yaml.Node `yaml:"groups"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}
	if doc.Kind != "PrometheusRule" {
		kind := doc.Kind
		if kind == "" {
			kind = "<none>"
		}
		return nil, fmt.Errorf("not a PrometheusRule CRD (kind=%s)", kind)
	}
	// A missing key leaves a zero Node (Kind 0); `groups: []` / `groups:` /
	// `groups: ""` parse to a node with no content — both are "no rules" like
	// the Python `if not groups`.
	if doc.Spec.Groups.Kind == 0 || len(doc.Spec.Groups.Content) == 0 {
		return nil, fmt.Errorf("PrometheusRule has no spec.groups")
	}
	return yaml.Marshal(map[string]yaml.Node{"groups": doc.Spec.Groups})
}

// promtoolCheckRules runs `promtool check rules <path>`, streaming promtool's
// own diagnostics to the terminal. A package var so tests stub it (and so they
// don't require promtool on PATH).
var promtoolCheckRules = func(path string) error {
	cmd := exec.Command("promtool", "check", "rules", path)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// checkRuleCRD validates one PrometheusRule CRD: extract spec.groups, write the
// bare form to a tempfile, and run promtool against it.
func checkRuleCRD(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	bare, err := extractBareGroups(data)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "*.rules.yml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(bare); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := promtoolCheckRules(tmp.Name()); err != nil {
		return fmt.Errorf("promtool rejected rules: %w", err)
	}
	return nil
}

// walkPromRuleFiles returns every *.yaml under dir (sorted by WalkDir's lexical
// order), the set promtool validates.
func walkPromRuleFiles(dir string) []string {
	var files []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() {
			return nil
		}
		if filepath.Ext(p) == ".yaml" {
			files = append(files, p)
		}
		return nil
	})
	return files
}

// runCICheckPromRules validates the explicit file args, or — when none are
// given — every *.yaml under rulesDir, skipping cleanly if that directory is
// absent (an instance overlay that removed the observability component).
// rulesDir tolerates both repo layouts via esRepoPath: apl-values/ at the root
// (an instance) or under instance-template/ (this template repo).
func runCICheckPromRules(rulesDir string, files []string, w io.Writer) error {
	if len(files) == 0 {
		if !filepath.IsAbs(rulesDir) {
			rulesDir = esRepoPath(".", rulesDir)
		}
		if info, err := os.Stat(rulesDir); err != nil || !info.IsDir() {
			fmt.Fprintf(w, "check-prom-rules: no PrometheusRule manifests (%s absent) — skipping\n", rulesDir)
			return nil
		}
		files = walkPromRuleFiles(rulesDir)
		if len(files) == 0 {
			fmt.Fprintf(w, "check-prom-rules: no *.yaml under %s — skipping\n", rulesDir)
			return nil
		}
	}
	failed := 0
	for _, f := range files {
		if err := checkRuleCRD(f); err != nil {
			fmt.Fprintf(w, "::error file=%s::%v\n", f, err)
			failed++
			continue
		}
		fmt.Fprintf(w, "ok: %s\n", f)
	}
	if failed > 0 {
		return fmt.Errorf("%d PrometheusRule file(s) failed validation", failed)
	}
	return nil
}

func ciCheckPromRulesCmd() *cobra.Command {
	var rulesDir string
	c := &cobra.Command{
		Use:   "check-prom-rules [file ...]",
		Short: "promtool check rules over PrometheusRule CRDs (extracts spec.groups first)",
		Long: "Native port of the former template-scripts/linting-and-validation/\n" +
			"check-prometheus-rule-crds.py (the Makefile's prom-rules-check). For each\n" +
			"PrometheusRule CRD it extracts spec.groups into the bare-groups document\n" +
			"`promtool check rules` understands, then runs promtool against it. With no\n" +
			"file args it validates every *.yaml under --rules-dir (tolerating both the\n" +
			"instance layout and the template's instance-template/ nesting), skipping\n" +
			"cleanly when that directory is absent.",
		RunE: func(_ *cobra.Command, args []string) error {
			return runCICheckPromRules(rulesDir, args, os.Stdout)
		},
	}
	c.Flags().StringVar(&rulesDir, "rules-dir", defaultPromRulesDir, "directory walked for *.yaml when no file args are given")
	return c
}
