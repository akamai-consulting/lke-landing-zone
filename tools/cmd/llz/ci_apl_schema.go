package main

// ci_apl_schema.go implements `llz ci validate-apl-values` — the offline,
// no-cloud port of the two apl-values checks that were previously Release-E2E-
// ONLY, each of which burned multiple ~50-min runs in the week of 2026-07-02:
//
//  1. templatefile var-contract. cluster-bootstrap/main.tf renders the per-env
//     values.yaml via templatefile() with a SECRETS-ONLY var map (llz render
//     resolves everything else from the spec first). A ${...} left in the
//     rendered file that is NOT in that map hard-fails `terraform apply`
//     ("vars map does not contain key apl_values_repo_url" — a real 2026-07-02
//     failure). We derive the map keys from main.tf and assert every unescaped
//     ${...} still in the rendered values is one of them — no terraform needed.
//
//  2. apl-core schema. helm_release.apl validates the rendered values against
//     apl-core's bundled values.schema.json at APPLY time; the v6 migration
//     burned runs on `apps.loki: adminPassword is required` surfacing there.
//     `helm template apl/apl` runs the identical JSON-Schema check client-side
//     (the apl chart has no dependencies — no cluster, no `helm dependency
//     build`), pinned to the chart version read from the scaffolded tfvars so
//     it always matches what deploys.
//
// Both the parsing/scan LOGIC and the helm orchestration live here (behind an
// exec seam) so they are unit-tested without terraform, helm, or a network; the
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
// cluster-bootstrap/main.tf helm_release.apl.repository).
const aplChartRepo = "https://linode.github.io/apl-core"

// unescapedPlaceholderRe matches a ${var} that is NOT escaped as $${var}: the
// leading (^|[^$]) rejects a preceding '$'. Group 1 is the var name. Word chars
// include digits (e.g. loki_s3_endpoint).
var unescapedPlaceholderRe = regexp.MustCompile(`(^|[^$])\$\{([a-zA-Z_][a-zA-Z0-9_]*)\}`)

// anyPlaceholderRe matches every ${var} (escaped or not) for stubbing before the
// schema check — the schema cares about structure/required keys, not values.
var anyPlaceholderRe = regexp.MustCompile(`\$\{[a-zA-Z_][a-zA-Z0-9_]*\}`)

// templatefileKeyRe pulls the LHS `key =` map entries out of the
// apl_rendered_values = templatefile( … ) block sliced from main.tf.
var templatefileKeyRe = regexp.MustCompile(`(?m)^\s+([a-zA-Z_][a-zA-Z0-9_]*)\s*=`)

// aplChartVersionRe pulls the pinned version out of an apl_chart_version tfvars
// assignment (e.g. `apl_chart_version = "6.0.0"`).
var aplChartVersionRe = regexp.MustCompile(`apl_chart_version\s*=\s*"([0-9]+\.[0-9]+\.[0-9]+)"`)

// helmRunner runs a helm invocation, returning combined output and success.
// A package var so tests substitute a fake without helm or a network.
var helmRunner = func(args ...string) (string, bool) {
	cmd := exec.Command("helm", args...)
	var buf strings.Builder
	cmd.Stdout, cmd.Stderr = &buf, &buf
	cmd.Env = os.Environ()
	return buf.String(), cmd.Run() == nil
}

func ciAplSchemaValidateCmd() *cobra.Command {
	var valuesPath, tfvarsPath, mainTFPath string
	var skipSchema bool
	cmd := &cobra.Command{
		Use:   "validate-apl-values",
		Short: "check a rendered apl-values file's templatefile var-contract + apl-core schema (no cluster)",
		Long: "Two offline checks on a rendered apl-values values.yaml, shifted left from\n" +
			"Release-E2E: (1) every unescaped ${...} still in the file is a key in\n" +
			"cluster-bootstrap/main.tf's templatefile() map (else terraform apply fails);\n" +
			"(2) the values pass apl-core's chart schema via `helm template apl/apl`, pinned\n" +
			"to the version in the scaffolded tfvars. The schema check self-skips (--skip-schema\n" +
			"or no helm on PATH); the var-contract check always runs.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runValidateAplValues(valuesPath, tfvarsPath, mainTFPath, skipSchema)
		},
	}
	cmd.Flags().StringVar(&valuesPath, "values", "", "path to the rendered apl-values values.yaml (required)")
	cmd.Flags().StringVar(&tfvarsPath, "tfvars", "", "path to the scaffolded cluster-bootstrap <env>.tfvars (apl_chart_version)")
	cmd.Flags().StringVar(&mainTFPath, "main-tf", "", "path to cluster-bootstrap/main.tf (templatefile var map) (required)")
	cmd.Flags().BoolVar(&skipSchema, "skip-schema", false, "skip the helm schema check (var-contract only)")
	_ = cmd.MarkFlagRequired("values")
	_ = cmd.MarkFlagRequired("main-tf")
	return cmd
}

func runValidateAplValues(valuesPath, tfvarsPath, mainTFPath string, skipSchema bool) error {
	valuesRaw, err := os.ReadFile(valuesPath)
	if err != nil {
		return fmt.Errorf("read values %s: %w", valuesPath, err)
	}
	mainTFRaw, err := os.ReadFile(mainTFPath)
	if err != nil {
		return fmt.Errorf("read main.tf %s: %w", mainTFPath, err)
	}

	// ── Check 1: templatefile var-contract ────────────────────────────────────
	keys := templatefileMapKeys(string(mainTFRaw))
	if len(keys) == 0 {
		return fmt.Errorf("no templatefile() var map found in %s — the apl_rendered_values block moved or was reformatted", mainTFPath)
	}
	if unwired := unwiredPlaceholders(string(valuesRaw), keys); len(unwired) > 0 {
		return fmt.Errorf("%s references ${%s} not in cluster-bootstrap/main.tf's templatefile map (keys: %s) — it will fail at terraform apply (the apl_values_repo_url class)",
			valuesPath, strings.Join(unwired, "}, ${"), strings.Join(sortedSetKeys(keys), " "))
	}
	fmt.Printf("templatefile var-contract ok (%d map keys, all leftover placeholders wired)\n", len(keys))

	// ── Check 2: apl-core schema (helm template) ──────────────────────────────
	if skipSchema {
		fmt.Println("schema check skipped (--skip-schema)")
		return nil
	}
	if _, err := exec.LookPath("helm"); err != nil {
		fmt.Println("schema check skipped (no helm on PATH)")
		return nil
	}
	version := parseAplChartVersion(readOrEmpty(tfvarsPath))
	if version == "" {
		return fmt.Errorf("no apl_chart_version in tfvars %q — cannot pin the schema check (pass --skip-schema to run the var-contract only)", tfvarsPath)
	}
	return validateAplSchema(string(valuesRaw), version)
}

// templatefileMapKeys slices the apl_rendered_values = templatefile( … ) block
// and returns its map keys — mirrors what cluster-bootstrap actually passes.
func templatefileMapKeys(mainTF string) map[string]bool {
	start := strings.Index(mainTF, "apl_rendered_values")
	if start < 0 {
		return nil
	}
	tf := mainTF[start:]
	tf = tf[strings.Index(tf, "templatefile(")+len("templatefile("):]
	// The map is { … } up to the matching ) that closes templatefile(.
	end := strings.Index(tf, "\n  )")
	if end < 0 {
		return nil
	}
	block := tf[:end]
	keys := map[string]bool{}
	for _, m := range templatefileKeyRe.FindAllStringSubmatch(block, -1) {
		keys[m[1]] = true
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

// parseAplChartVersion extracts the pinned apl_chart_version ("" if absent).
func parseAplChartVersion(tfvars string) string {
	if m := aplChartVersionRe.FindStringSubmatch(tfvars); m != nil {
		return m[1]
	}
	return ""
}

func readOrEmpty(path string) string {
	if path == "" {
		return ""
	}
	b, _ := os.ReadFile(path)
	return string(b)
}
