package database

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

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/tofudriver"
)

// dbDeclaredAssignRe matches the `databases = …` assignment in a rendered tfvars.
// The gate is exact rather than heuristic: DatabasesTFVars OMITS the assignment
// entirely when the map is empty, so the line is present if and only if the spec
// declared at least one cluster.
var dbDeclaredAssignRe = regexp.MustCompile(`(?m)^[ \t]*databases[ \t]*=`)

func RunDBDeclared(region string) error {
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

func RunDBSummary(region, phase string) error {
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
