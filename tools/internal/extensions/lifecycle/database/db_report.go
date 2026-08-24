package database

// db_report.go — the two workflow-facing helpers around the `databases` root,
// kept in Go rather than inline `run:` bash for the reason .untestable-budget.yaml
// exists: both encode a decision (is a cluster declared? did the apply provision
// anything?) and a decision in CI shell is a decision nothing tests.
//
//   llz ci db-declared  → does this deployment declare any database clusters?
//   llz ci db-summary   → the $GITHUB_STEP_SUMMARY block for apply / destroy-plan

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/tofudriver"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghaout"
)

// dbDeclaredAssignRe finds the `databases = …` assignment in a rendered tfvars.
// It locates the assignment; declaresDatabases below decides whether the VALUE
// declares anything.
//
// THE ASSIGNMENT'S PRESENCE IS NOT THE ANSWER, and believing it was broke every
// deployment that declares no databases. The reasoning went: DatabasesTFVars
// omits the assignment when the map is empty, so the line is present if and only
// if at least one cluster was declared. The first half is true and the
// conclusion does not follow — `llz render` builds every tfvars by applying
// assignments ON TOP OF the root's terraform.tfvars.example, and that example
// ships a literal, uncommented
//
//	databases = {}
//
// So omitting the assignment leaves the example's own empty map in place, and a
// presence test matches it. Declared came back true for a deployment with zero
// databases, seed-db-admin refused to exit 0, and the bootstrap died telling the
// operator to "run the databases apply first" — advice that cannot help, because
// the apply had already run in the same pipeline and correctly created nothing
// (`Apply complete! Resources: 0 added, 0 changed, 0 destroyed`).
//
// That is the DEFAULT configuration: a deployment with no Managed Postgres. It
// shipped on 2026-08-22 and was caught by the first e2e run after it.
var dbDeclaredAssignRe = regexp.MustCompile(`(?m)^[ \t]*databases[ \t]*=[ \t]*`)

// declaresDatabases reports whether the tfvars assign a NON-EMPTY databases map.
//
// A commented-out example block cannot match: the regex above anchors the
// assignment at line start, so a leading `#` excludes it — which is what lets the
// databases example carry a fully worked illustration above its real assignment.
//
// UNBALANCED BRACES COUNT AS DECLARED, deliberately. A truncated or unparseable
// value means this cannot tell, and the two errors are not symmetric: reporting
// "declared" makes seed-db-admin fail loudly and stop, while reporting "none"
// makes it exit 0 — leaving OpenBao with no db-admin credential while the
// PROVISIONING password stays live in Terraform state. That is the outcome the
// whole guard exists to prevent, so uncertainty resolves toward the loud side.
func declaresDatabases(body []byte) bool {
	loc := dbDeclaredAssignRe.FindIndex(body)
	if loc == nil {
		return false
	}
	rest := body[loc[1]:]
	open := bytes.IndexByte(rest, '{')
	if open < 0 {
		// Not a map literal at all — a variable reference, or a value this does not
		// model. Cannot confirm it is empty, so treat it as declared.
		return true
	}
	depth, close := 0, -1
	for i := open; i < len(rest); i++ {
		switch rest[i] {
		case '{':
			depth++
		case '}':
			if depth--; depth == 0 {
				close = i
			}
		}
		if close >= 0 {
			break
		}
	}
	if close < 0 {
		return true // unbalanced — see the header
	}
	for _, line := range bytes.Split(rest[open+1:close], []byte("\n")) {
		t := bytes.TrimSpace(line)
		if len(t) == 0 || bytes.HasPrefix(t, []byte("#")) || bytes.HasPrefix(t, []byte("//")) {
			continue
		}
		return true
	}
	return false
}

// dbTFVarsCandidates are the two places <region>.tfvars can be, because the
// delivered pipeline runs these commands from two different directories:
// `llz ci db-declared` from the repo root, `llz ci seed-db-admin` with the
// databases root as its working directory (it has to — it reads that root's
// terraform state). One path would have made whichever command moved silently
// answer "no tfvars, nothing declared", which is the exact wrong answer for the
// half that fails closed on it.
func dbTFVarsCandidates(region string) []string {
	return []string{
		filepath.Join("terraform-iac-bootstrap", "databases", region+".tfvars"),
		region + ".tfvars",
	}
}

// dbDeclaration is what the rendered tfvars says about a deployment's databases,
// and — separately — whether that could be ANSWERED at all. The two callers want
// different things from an indefinite answer: db-declared treats it as "none" so
// a skip decision cannot fail a bootstrap, while seed-db-admin refuses to call it
// "none" when nothing was seeded.
type dbDeclaration struct {
	Declared bool   // the tfvars carries a `databases = …` assignment
	Present  bool   // a tfvars was found at one of the candidate paths
	Answered bool   // false only when a tfvars existed and could not be READ
	Path     string // the path consulted, for the message
}

// dbDeclares reads the deployment's rendered databases tfvars. An ABSENT file is
// a definite "none declared": DatabasesTFVars omits the assignment for an empty
// map, and `llz render` writes no databases tfvars for a deployment that declares
// none. An UNREADABLE one (a permission error, a directory in its place) is
// indefinite and says so.
func dbDeclares(region string) dbDeclaration {
	first := ""
	for _, p := range dbTFVarsCandidates(region) {
		if first == "" {
			first = p
		}
		body, err := os.ReadFile(p)
		if err == nil {
			return dbDeclaration{Declared: declaresDatabases(body), Present: true, Answered: true, Path: p}
		}
		if !os.IsNotExist(err) {
			return dbDeclaration{Answered: false, Path: p}
		}
	}
	return dbDeclaration{Answered: true, Path: first}
}

func RunDBDeclared(region string) error {
	if region == "" {
		return fmt.Errorf("--region is required")
	}
	d := dbDeclares(region)
	switch {
	case !d.Answered:
		// Unreadable is not the same as absent, but the SKIP decision is the wrong
		// place to fail over the difference: anything that genuinely matters fails
		// later, loudly, in terraform itself. seed-db-admin is the half that fails
		// closed, and it re-reads this for exactly that reason.
		fmt.Printf("db-declared: %s could not be read — treating as no database clusters declared for %s.\n", d.Path, region)
	case !d.Present:
		fmt.Printf("db-declared: %s not present — no database clusters declared for %s.\n", d.Path, region)
	case d.Declared:
		fmt.Printf("db-declared: %s declares database clusters for %s — the admin seed will run.\n", d.Path, region)
	default:
		fmt.Printf("db-declared: %s declares no database clusters for %s — skipping the admin seed.\n", d.Path, region)
	}
	return ghaout.Append("GITHUB_OUTPUT", fmt.Sprintf("declared=%t", d.Declared))
}

func RunDBSummary(region, phase string) error {
	if region == "" {
		return fmt.Errorf("--region is required")
	}
	switch phase {
	case "destroy-plan":
		return ghaout.Append("GITHUB_STEP_SUMMARY", dbDestroyWarning()...)
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
		return ghaout.Append("GITHUB_STEP_SUMMARY", dbApplySummary(region, labels)...)
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
