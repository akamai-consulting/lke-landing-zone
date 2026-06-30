package main

// import_plan.go is the data-migration half of the flow: `llz import plan` reads
// an import-report.yaml and emits a runnable MIGRATION-PLAN.md with concrete
// commands to move the Object Storage buckets (rclone) and the databases (CNPG,
// per owning app) from the source account/cluster to the target LLZ cluster.
//
// The plan is generated from the inventory but can't know the target's
// endpoints/credentials/bucket names — those are clearly-marked ${PLACEHOLDER}
// env vars the operator fills. The command generation is a pure function so it's
// unit-tested; the RunE only reads the report and writes the file.

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

const migrationPlanFile = "MIGRATION-PLAN.md"

type importPlanOpts struct {
	report string
	output string
}

func importPlanCmd() *cobra.Command {
	var o importPlanOpts
	c := &cobra.Command{
		Use:   "plan",
		Short: "emit a data-migration runbook (OBJ buckets + databases) from an import-report.yaml",
		Long: "Reads the report from `llz import scan` and writes MIGRATION-PLAN.md: concrete,\n" +
			"copy-pasteable commands to move the Object Storage buckets (rclone) and the\n" +
			"databases (CNPG, per owning app) from the source account/cluster to the target\n" +
			"LLZ cluster. Target endpoints/credentials/bucket names are ${PLACEHOLDER} env\n" +
			"vars you fill. Read-only; generates a plan, runs nothing.",
		Example: "  llz import plan --report import-report.yaml -o MIGRATION-PLAN.md\n" +
			"  llz import plan --report import-report.yaml -o -   # print to stdout",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Flags().NFlag() == 0 {
				return cmd.Help()
			}
			return runImportPlan(o)
		},
	}
	c.Flags().StringVar(&o.report, "report", defaultImportReport, "the import-report.yaml to plan from")
	c.Flags().StringVarP(&o.output, "output", "o", migrationPlanFile, `path to write the plan ("-" for stdout)`)
	return c
}

func runImportPlan(o importPlanOpts) error {
	rep, err := loadImportReport(o.report)
	if err != nil {
		return err
	}
	plan := buildMigrationPlan(rep)
	if o.output == "-" {
		fmt.Print(plan)
		return nil
	}
	if err := os.WriteFile(o.output, []byte(plan), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", o.output, err)
	}
	fmt.Printf("%s wrote %s — review, fill the ${...} target values, then run the steps.\n", green("✓"), o.output)
	return nil
}

// ── plan generation (pure) ───────────────────────────────────────────────────

// dbAppNativeHint suggests the preferred, version-tolerant migration for a known
// platform DB (keyed by namespace), since a raw cross-version pg_restore fights
// the new app's schema migrations.
var dbAppNativeHint = map[string]string{
	"keycloak": "Preferred: Keycloak **realm export/import** (`kc.sh export`/`import`) — version-tolerant; carries realms, clients, users.",
	"harbor":   "Preferred: recreate **projects/robots/replication** via Harbor config/IaC. Image blobs live in the OBJ bucket, not this DB.",
	"gitea":    "Tied to the **Gitea → BYO-Git** move: mirror repos to the external Git host. This DB is only repo/issue/user metadata.",
}

// buildMigrationPlan renders the data-migration runbook from the report.
func buildMigrationPlan(rep importReport) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }

	srcEndpoint := "https://${SRC_OBJ_CLUSTER}.linodeobjects.com"
	if oc := reportObjCluster(rep); oc != "" {
		srcEndpoint = "https://" + oc + ".linodeobjects.com"
	}

	w("# Data migration plan\n\n")
	w("Generated from the scan report. Moves Object Storage buckets and databases from\n")
	w("the **source** account/cluster to the **target** LLZ cluster. Fill every `${...}`\n")
	w("placeholder, then run the steps. Re-run the syncs/dumps as a FINAL pass after you\n")
	w("freeze writes at cutover.\n\n")

	w("## Prerequisites (export first)\n\n")
	w("```bash\n")
	w("export SRC_CONTEXT=...            # kubeconfig context of the OLD cluster\n")
	w("export DST_CONTEXT=...            # kubeconfig context of the NEW LLZ cluster\n")
	w("# Source Object Storage (from the report; keys are in the APL platform-values file: obj.provider.linode):\n")
	w("export SRC_OBJ_ENDPOINT=%s\n", srcEndpoint)
	w("export SRC_OBJ_KEY=...  SRC_OBJ_SECRET=...\n")
	w("# Target Object Storage (new account/cluster):\n")
	w("export DST_OBJ_ENDPOINT=https://${DST_OBJ_CLUSTER}.linodeobjects.com\n")
	w("export DST_OBJ_KEY=...  DST_OBJ_SECRET=...\n")
	w("```\n")
	w("> Take a maintenance window and freeze writes before the FINAL pass.\n\n")

	planObjectStorage(&b, rep)
	planDatabases(&b, rep)
	return b.String()
}

func planObjectStorage(b *strings.Builder, rep importReport) {
	w := func(format string, a ...any) { fmt.Fprintf(b, format, a...) }
	buckets := reportBuckets(rep)
	w("## Object Storage — %d bucket(s)\n\n", len(buckets))
	if len(buckets) == 0 {
		w("_No buckets in the report._\n\n")
		return
	}
	w("rclone moves S3-compatible objects incrementally (run now to bulk-copy, re-run as\n")
	w("the final pass). Immutable stores (loki/thanos/harbor blobs) sync cheaply; mutable\n")
	w("ones need the write freeze first.\n\n")
	w("```bash\n")
	w("rclone config create src s3 provider=Ceph endpoint=$SRC_OBJ_ENDPOINT access_key_id=$SRC_OBJ_KEY secret_access_key=$SRC_OBJ_SECRET\n")
	w("rclone config create dst s3 provider=Ceph endpoint=$DST_OBJ_ENDPOINT access_key_id=$DST_OBJ_KEY secret_access_key=$DST_OBJ_SECRET\n\n")
	for _, bk := range buckets {
		w("rclone sync src:%s dst:${DST_BUCKET_%s:?set target bucket} --checksum --transfers=16 --fast-list --progress\n",
			bk, sanitizeEnvKey(bk))
	}
	w("```\n\n")
}

func planDatabases(b *strings.Builder, rep importReport) {
	w := func(format string, a ...any) { fmt.Fprintf(b, format, a...) }
	var cnpg, caches []dbInfo
	for _, d := range rep.Storage.Databases {
		if d.Kind == "CNPG" {
			cnpg = append(cnpg, d)
		} else {
			caches = append(caches, d)
		}
	}

	w("## Databases — %d CNPG cluster(s)\n\n", len(cnpg))
	w("Each is one Postgres written by a single app. This is a v4→v5/6 jump, so **prefer\n")
	w("the app's own export/import** below; raw `pg_dump`/`pg_restore` is the fallback and\n")
	w("only safe when the source and target app versions match.\n\n")

	for _, d := range cnpg {
		client := "unknown"
		if len(d.Clients) > 0 {
			client = strings.Join(d.Clients, ", ")
		}
		w("### %s/%s — client: %s\n\n", d.Namespace, d.Name, client)
		if hint := dbAppNativeHint[d.Namespace]; hint != "" {
			w("%s\n\n", hint)
		}
		w("```bash\n")
		w("# Fallback — raw dump/restore (same-version only):\n")
		w("SRC_NS=%s; SRC_CLUSTER=%s\n", d.Namespace, d.Name)
		w("DB=$(kubectl --context $SRC_CONTEXT get secret ${SRC_CLUSTER}-app -n $SRC_NS -o jsonpath='{.data.dbname}' | base64 -d)\n")
		w("SRC_POD=$(kubectl --context $SRC_CONTEXT get pod -n $SRC_NS -l cnpg.io/cluster=$SRC_CLUSTER,cnpg.io/instanceRole=primary -o name)\n")
		w("kubectl --context $SRC_CONTEXT exec -n $SRC_NS $SRC_POD -c postgres -- pg_dump -Fc -U postgres -d \"$DB\" > %s.dump\n", d.Name)
		w("# restore into the target (after apl-core provisions the new DB):\n")
		w("DST_NS=%s; DST_CLUSTER=${DST_CLUSTER_%s:?set target CNPG cluster}\n", d.Namespace, sanitizeEnvKey(d.Name))
		w("DST_POD=$(kubectl --context $DST_CONTEXT get pod -n $DST_NS -l cnpg.io/cluster=$DST_CLUSTER,cnpg.io/instanceRole=primary -o name)\n")
		w("kubectl --context $DST_CONTEXT exec -i -n $DST_NS $DST_POD -c postgres -- pg_restore -U postgres -d \"$DB\" --clean --if-exists < %s.dump\n", d.Name)
		w("```\n\n")
	}

	if len(caches) > 0 {
		w("## Caches — rebuild, do NOT migrate (%d)\n\n", len(caches))
		for _, d := range caches {
			w("- %s/%s (%s) — ephemeral; the new cluster provisions a fresh instance.\n", d.Namespace, d.Name, d.Engine)
		}
		w("\n")
	}
}

// reportBuckets returns the source bucket names — Linode's authoritative list,
// else the APL values' bucket map.
func reportBuckets(rep importReport) []string {
	var out []string
	if rep.Linode != nil {
		for _, b := range rep.Linode.ObjectStorage {
			if b.Label != "" {
				out = append(out, b.Label)
			}
		}
	}
	if len(out) == 0 {
		if apl := firstAplSignals(rep.Repos); apl != nil {
			for _, name := range apl.ObjectBuckets {
				out = append(out, name)
			}
		}
	}
	return dedupeSorted(out)
}

// sanitizeEnvKey makes a bucket/cluster name safe as a shell env-var suffix
// (lke579582-loki → lke579582_loki).
func sanitizeEnvKey(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}
