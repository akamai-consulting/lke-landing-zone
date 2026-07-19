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
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// defaultPromRulesDirs are the roots that ship PrometheusRule CRDs (matched by
// kube-prometheus-stack's ruleSelector). It was a single dir,
// platform-apl/manifest/observability/prometheus-rules-crd, until the rules moved
// into the observability component — the stale default made the gate skip-clean
// on every run, so nothing promtool-validated the live rules.
//
// The llzReconciler component was the same hole one directory over: its
// prometheusrule.yaml holds the entire reconciler alert surface (and now the
// label-joined LLZReconcilerStale, exactly the class promtool catches) and sat
// outside the only root this gate walked. That dir is a mixed component tree —
// Deployment, ServiceMonitor, RBAC — so the walk selects PrometheusRules by kind
// rather than assuming every manifest is one. Explicit file args keep the strict
// behavior: naming a non-PrometheusRule is still an error, not a silent skip.
var defaultPromRulesDirs = []string{
	"platform-apl/components/observability/prometheus-rules",
	"platform-apl/components/llzReconciler/llz-reconciler",
}

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

// isPrometheusRule reports whether a manifest is a PrometheusRule CRD. Used only
// on the walked (directory) path, so a mixed component tree contributes its rules
// without its Deployment/ServiceMonitor tripping the kind check.
func isPrometheusRule(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var doc struct {
		Kind string `yaml:"kind"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		// Unparseable YAML is not this gate's business to adjudicate — the manifest
		// guards cover it — but it is certainly not a rule file to validate.
		return false, nil
	}
	return doc.Kind == "PrometheusRule", nil
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

// walkPromRuleFiles returns every YAML manifest under dir (sorted), the set
// promtool validates. It shares collectManifestPaths with the other tree-scanning
// guards, which also picks up *.yml — a PrometheusRule saved with that extension
// used to be skipped silently, i.e. never promtool-validated.
//
// A walk error is REPORTED, not swallowed. The caller reads "no files" as the
// skip-clean "nothing to validate" case, so an unreadable subtree that aborted
// the walk would be indistinguishable from an absent rules dir — a real
// PrometheusRule would go unvalidated and the guard would still print success.
func walkPromRuleFiles(dir string) ([]string, error) {
	return collectManifestPaths([]string{dir})
}

// runCICheckPromRules validates the explicit file args, or — when none are
// given — every *.yaml under rulesDir, skipping cleanly if that directory is
// absent (an instance overlay that removed the observability component).
// rulesDir tolerates both repo layouts via esRepoPath: apl-values/ at the root
// (an instance) or under instance-template/ (this template repo).
func runCICheckPromRules(rulesDirs []string, files []string, w io.Writer) error {
	if len(files) == 0 {
		present := 0
		for _, dir := range rulesDirs {
			if !filepath.IsAbs(dir) {
				dir = esRepoPath(".", dir)
			}
			if info, err := os.Stat(dir); err != nil || !info.IsDir() {
				fmt.Fprintf(w, "check-prom-rules: %s absent — skipping that root\n", dir)
				continue
			}
			present++
			walked, walkErr := walkPromRuleFiles(dir)
			if walkErr != nil {
				// Not the skip case: the dir exists and the walk broke partway, so an
				// empty or short list means "could not read", not "nothing to check".
				return fmt.Errorf("check-prom-rules: scanning %s: %w", dir, walkErr)
			}
			for _, f := range walked {
				isRule, err := isPrometheusRule(f)
				if err != nil {
					return fmt.Errorf("check-prom-rules: reading %s: %w", f, err)
				}
				if isRule {
					files = append(files, f)
				}
			}
		}
		if present == 0 {
			fmt.Fprintf(w, "check-prom-rules: no PrometheusRule roots present — skipping\n")
			return nil
		}
		if len(files) == 0 {
			// A root exists but holds no PrometheusRule. Same posture as
			// requireCorpus (guard_corpus.go): a gate that validated nothing prints
			// the same success as one that validated every rule, so it fails instead.
			return fmt.Errorf("check-prom-rules: found 0 PrometheusRule CRDs under %s — "+
				"refusing to pass on an empty corpus. Update --rules-dir if the rules moved",
				strings.Join(rulesDirs, ", "))
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
	var rulesDirs []string
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
			return runCICheckPromRules(rulesDirs, args, os.Stdout)
		},
	}
	c.Flags().StringSliceVar(&rulesDirs, "rules-dir", defaultPromRulesDirs,
		"directories walked for PrometheusRule CRDs when no file args are given (repeatable)")
	return c
}
