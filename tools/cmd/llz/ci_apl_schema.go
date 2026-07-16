package main

// ci_apl_schema.go implements `llz ci validate-apl-values` — the offline,
// no-cloud port of the two apl-values checks that were previously Release-E2E-
// ONLY, each of which burned multiple ~50-min runs in the week of 2026-07-02:
//
//  1. runtime-placeholder var-contract. `llz ci bootstrap-cluster` fills a
//     SECRETS-ONLY set of ${...} tokens in the per-env values.yaml (llz render
//     resolves everything else from the spec first). A ${...} left in the
//     rendered file that is NOT in that set is a stale template the bootstrap
//     can't fill (the ${apl_values_repo_url} class — a real 2026-07-02 apply
//     failure). We assert every unescaped ${...} still in the rendered values is
//     one of bootstrapValuePlaceholders (the same Go constant bootstrap-cluster
//     fills) — no terraform, no main.tf parsing.
//
//  2. apl-core schema. bootstrap-cluster's `helm upgrade --install apl` validates
//     the rendered values against apl-core's bundled values.schema.json at APPLY
//     time; the v6 migration burned runs on `apps.loki: adminPassword is
//     required` surfacing there. `helm template apl/apl` runs the identical
//     JSON-Schema check client-side (the apl chart has no dependencies — no
//     cluster, no `helm dependency build`), pinned to the chart version passed in
//     (--chart-version, read from the spec by the caller) so it matches deploy.
//
// Both the scan LOGIC and the helm orchestration live here (behind an exec seam)
// so they are unit-tested without terraform, helm, or a network; the
// scaffold-render-check.sh caller is left as thin glue.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// aplChartRepo is the public Helm repo apl-core publishes to (mirrors
// bootstrap-cluster's `helm upgrade --install --repo`).
const aplChartRepo = "https://linode.github.io/apl-core"

// unescapedPlaceholderRe matches a ${var} that is NOT escaped as $${var}: the
// leading (^|[^$]) rejects a preceding '$'. Group 1 is the var name. Word chars
// include digits (e.g. loki_s3_endpoint).
var unescapedPlaceholderRe = regexp.MustCompile(`(^|[^$])\$\{([a-zA-Z_][a-zA-Z0-9_]*)\}`)

// anyPlaceholderRe matches every ${var} (escaped or not) for stubbing before the
// schema check — the schema cares about structure/required keys, not values.
var anyPlaceholderRe = regexp.MustCompile(`\$\{[a-zA-Z_][a-zA-Z0-9_]*\}`)

// helmRunner runs a helm invocation, returning combined output and success.
// A package var so tests substitute a fake without helm or a network.
var helmRunner = func(args ...string) (string, bool) {
	cmd := exec.Command("helm", args...)
	cmd.Env = os.Environ()
	return runCombined(cmd)
}

func ciAplSchemaValidateCmd() *cobra.Command {
	var valuesPath, chartVersion string
	var skipSchema bool
	cmd := &cobra.Command{
		Use:   "validate-apl-values",
		Short: "check a rendered apl-values file's runtime-placeholder contract + apl-core schema (no cluster)",
		Long: "Two offline checks on a rendered apl-values values.yaml, shifted left from\n" +
			"Release-E2E: (1) every unescaped ${...} still in the file is one of the\n" +
			"secrets-only runtime placeholders `llz ci bootstrap-cluster` fills (else the\n" +
			"bootstrap can't fill it — the ${apl_values_repo_url} class); (2) the values\n" +
			"pass apl-core's chart schema via `helm template apl/apl`, pinned to\n" +
			"--chart-version. The schema check self-skips (--skip-schema, no helm on PATH,\n" +
			"or no --chart-version); the var-contract check always runs.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runValidateAplValues(valuesPath, chartVersion, skipSchema)
		},
	}
	cmd.Flags().StringVar(&valuesPath, "values", "", "path to the rendered apl-values values.yaml (required)")
	cmd.Flags().StringVar(&chartVersion, "chart-version", "", "apl-core chart version to pin the schema check (from spec.cluster.bootstrap.aplChartVersion)")
	cmd.Flags().BoolVar(&skipSchema, "skip-schema", false, "skip the helm schema check (var-contract only)")
	_ = cmd.MarkFlagRequired("values")
	return cmd
}

func runValidateAplValues(valuesPath, chartVersion string, skipSchema bool) error {
	valuesRaw, err := os.ReadFile(valuesPath)
	if err != nil {
		return fmt.Errorf("read values %s: %w", valuesPath, err)
	}

	// ── Check 1: runtime-placeholder var-contract ─────────────────────────────
	keys := placeholderSet()
	if unwired := unwiredPlaceholders(string(valuesRaw), keys); len(unwired) > 0 {
		return fmt.Errorf("%s references ${%s} not in the runtime-placeholder set (%s) — bootstrap-cluster cannot fill it (the apl_values_repo_url class)",
			valuesPath, strings.Join(unwired, "}, ${"), strings.Join(sortedSetKeys(keys), " "))
	}
	fmt.Printf("runtime-placeholder var-contract ok (%d placeholders, all leftover placeholders wired)\n", len(keys))

	// ── Check 2: apl-core schema (helm template) ──────────────────────────────
	if skipSchema {
		fmt.Println("schema check skipped (--skip-schema)")
		return nil
	}
	if _, err := exec.LookPath("helm"); err != nil {
		fmt.Println("schema check skipped (no helm on PATH)")
		return nil
	}
	if chartVersion == "" {
		fmt.Println("schema check skipped (no --chart-version — pass spec.cluster.bootstrap.aplChartVersion to enable)")
		return nil
	}
	return validateAplSchema(string(valuesRaw), chartVersion)
}

// placeholderSet is the set form of bootstrapValuePlaceholders (the secrets-only
// ${...} tokens bootstrap-cluster fills) — the single source of truth this guard
// checks a rendered values file against.
func placeholderSet() map[string]bool {
	keys := make(map[string]bool, len(bootstrapValuePlaceholders))
	for _, k := range bootstrapValuePlaceholders {
		keys[k] = true
	}
	return keys
}

// unwiredPlaceholders returns the sorted-unique unescaped ${var} names in values
// that are not in keys.
func unwiredPlaceholders(values string, keys map[string]bool) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range unescapedPlaceholderRe.FindAllStringSubmatch(values, -1) {
		name := m[2]
		if !keys[name] && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// validateAplSchema stubs the placeholders and runs helm template against the
// pinned apl chart version, returning helm's schema error on failure.
func validateAplSchema(values, version string) error {
	stubbed := anyPlaceholderRe.ReplaceAllString(values, "dummy")
	tmp, err := os.MkdirTemp("", "llz-apl-schema-*")
	if err != nil {
		return fmt.Errorf("mktemp: %w", err)
	}
	defer os.RemoveAll(tmp)
	stubPath := filepath.Join(tmp, "values.yaml")
	if err := os.WriteFile(stubPath, []byte(stubbed), 0o644); err != nil {
		return fmt.Errorf("write stubbed values: %w", err)
	}
	// Idempotent repo add/update; ignore output — a real failure resurfaces on
	// the template call with a clearer message.
	helmRunner("repo", "add", "apl", aplChartRepo)
	helmRunner("repo", "update", "apl")
	fmt.Printf("Validating against apl/apl %s schema…\n", version)
	out, ok := helmRunner("template", "apl", "apl/apl", "--version", version, "-f", stubPath)
	if !ok {
		fmt.Fprint(os.Stderr, out)
		return fmt.Errorf("rendered values violate apl-core's schema (apl/apl %s) — fix apl-values before it fails at helm_release.apl in Release-E2E", version)
	}
	fmt.Printf("schema ok (apl/apl %s)\n", version)
	return nil
}
