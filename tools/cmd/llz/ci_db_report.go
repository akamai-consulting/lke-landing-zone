package main

// ci_db_report.go — the two workflow-facing helpers around the `databases` root,
// kept in Go rather than inline `run:` bash for the reason .untestable-budget.yaml
// exists: both encode a decision (is a cluster declared? did the apply provision
// anything?) and a decision in CI shell is a decision nothing tests.
//
//   llz ci db-declared  → does this deployment declare any database clusters?
//   llz ci db-summary   → the $GITHUB_STEP_SUMMARY block for apply / destroy-plan

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/tofudriver"
	"github.com/spf13/cobra"
)

// dbDeclaredAssignRe matches the `databases = …` assignment in a rendered tfvars.
// The gate is exact rather than heuristic: DatabasesTFVars OMITS the assignment
// entirely when the map is empty, so the line is present if and only if the spec
// declared at least one cluster.
var dbDeclaredAssignRe = regexp.MustCompile(`(?m)^[ \t]*databases[ \t]*=`)

func ciDBDeclaredCmd() *cobra.Command {
	var region string
	c := &cobra.Command{
		Use:   "db-declared",
		Short: "report whether a deployment declares any database clusters",
		Long: "Writes `declared=true|false` to $GITHUB_OUTPUT so the bootstrap workflow can\n" +
			"skip the databases terraform-init + admin seed on the majority of instances\n" +
			"that declare no databases — initializing that root unconditionally would put\n" +
			"a new failure mode on the critical bootstrap path for no benefit.\n\n" +
			"Reads the RENDERED terraform-iac-bootstrap/databases/<region>.tfvars. Reading\n" +
			"the per-env file is sound because `databases` is deliberately NOT inherited\n" +
			"from spec.defaults.cluster (llz validate rejects it there), so the env's own\n" +
			"tfvars is the whole truth.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIDBDeclared(region) },
	}
	c.Flags().StringVar(&region, "region", "", "deployment (spec env name) to check (required)")
	return c
}

func runCIDBDeclared(region string) error {
	if region == "" {
		return fmt.Errorf("--region is required")
	}
	path := filepath.Join("terraform-iac-bootstrap", "databases", region+".tfvars")
	body, err := os.ReadFile(path)
	if err != nil {
		// A missing tfvars is a deployment that never rendered the databases root
		// (or predates it) — that is "none declared", not a failure. Anything that
		// genuinely matters fails later, loudly, in terraform itself.
		fmt.Printf("db-declared: %s not present — no database clusters declared for %s.\n", path, region)
		return appendGHAFile("GITHUB_OUTPUT", "declared=false")
	}
	declared := dbDeclaredAssignRe.Match(body)
	if declared {
		fmt.Printf("db-declared: %s declares database clusters for %s — the admin seed will run.\n", path, region)
	} else {
		fmt.Printf("db-declared: %s declares no database clusters for %s — skipping the admin seed.\n", path, region)
	}
	return appendGHAFile("GITHUB_OUTPUT", fmt.Sprintf("declared=%t", declared))
}

func ciDBSummaryCmd() *cobra.Command {
	var region, phase string
	c := &cobra.Command{
		Use:   "db-summary",
		Short: "render the databases step summary (apply | destroy-plan)",
		Long: "Appends the $GITHUB_STEP_SUMMARY block for the databases root.\n\n" +
			"--phase apply: reads the `labels` output and lists what was provisioned, or\n" +
			"says plainly that nothing was — a deployment declaring no clusters applies\n" +
			"cleanly and must not look like a silent failure. Points at the bootstrap\n" +
			"workflow, which owns the admin seed.\n\n" +
			"--phase destroy-plan: the data-loss caution. Unlike a bucket, a destroyed\n" +
			"Managed Postgres takes its data with it and Linode keeps no snapshot.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIDBSummary(region, phase) },
	}
	c.Flags().StringVar(&region, "region", "", "deployment (spec env name) being reported (required)")
	c.Flags().StringVar(&phase, "phase", "", "apply | destroy-plan (required)")
	return c
}

func runCIDBSummary(region, phase string) error {
	if region == "" {
		return fmt.Errorf("--region is required")
	}
	switch phase {
	case "destroy-plan":
		return appendGHAFile("GITHUB_STEP_SUMMARY", dbDestroyWarning()...)
	case "apply":
		raw, err := tofudriver.OutputRunFn()
		if err != nil {
			return fmt.Errorf("db-summary: terraform output -json: %w", err)
		}
		// allowMissing: a state predating the databases root has no `labels`
		// output, which is the same "nothing provisioned" as an empty map.
		labels, err := tofudriver.OutputValue(raw, "labels", true, true)
		if err != nil {
			return err
		}
		return appendGHAFile("GITHUB_STEP_SUMMARY", dbApplySummary(region, labels)...)
	default:
		return fmt.Errorf("--phase must be apply or destroy-plan, got %q", phase)
	}
}

// dbApplySummary renders the post-apply block. labelsJSON is the root's `labels`
// output as compact JSON ("", "null" or "{}" all meaning nothing provisioned).
func dbApplySummary(region, labelsJSON string) []string {
	trimmed := strings.TrimSpace(labelsJSON)
	if trimmed == "" || trimmed == "null" || trimmed == "{}" {
		return []string{
			fmt.Sprintf("### Databases (%s)", region),
			"",
			"No clusters declared in `spec.cluster.databases` — nothing provisioned.",
		}
	}
	return []string{
		fmt.Sprintf("### Managed Postgres clusters provisioned (%s)", region),
		"",
		"```json",
		trimmed,
		"```",
		"",
		fmt.Sprintf("**Next step:** run `bootstrap-openbao.yml` → `%s` — its seed-db-admin step", region),
		"writes each cluster's admin connection to `secret/infra/db-admin/<name>`.",
	}
}

// dbDestroyWarning is the caution shown to whoever approves the destroy job.
func dbDestroyWarning() []string {
	return []string{
		"",
		"> [!CAUTION]",
		"> Destroying a Managed Postgres cluster **deletes its data irreversibly** —",
		"> Linode retains no snapshot after the delete. Take a `pg_dump` first if",
		"> anything in these clusters still matters.",
		">",
		"> The admin credential at `secret/infra/db-admin/<name>` is NOT removed",
		"> by this destroy; reap it separately once the cluster is gone.",
	}
}
