package brownfield

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

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
)

const MigrationPlanFile = "MIGRATION-PLAN.md"

type PlanOpts struct {
	Report string
	Output string
}

func RunPlan(o PlanOpts) error {
	rep, err := loadImportReport(o.Report)
	if err != nil {
		return err
	}
	plan := buildMigrationPlan(rep)
	if o.Output == "-" {
		fmt.Print(plan)
		return nil
	}
	if err := os.WriteFile(o.Output, []byte(plan), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", o.Output, err)
	}
	fmt.Printf("%s wrote %s — review, fill the ${...} target values, then run the steps.\n", color.Green("✓"), o.Output)
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

// cacheEngines are the engines whose DEFAULT deployment is a cache — safe to
// rebuild rather than migrate, subject to the operator confirming it.
//
// SPLITTING ON ENGINE, NOT ON Kind, AND THE OLD SPLIT COULD COST AN OPERATOR
// THEIR DATABASE. planDatabases bucketed everything that was not `Kind: "CNPG"`
// into "Caches — rebuild, do NOT migrate", each line reading "ephemeral; the new
// cluster provisions a fresh instance". detectDBWorkloads sets Kind: "workload"
// for every self-managed database it finds — so a `postgres:15` StatefulSet
// holding production data was handed to the operator as throwaway, in a document
// whose whole purpose is to be followed literally. The engine was recorded
// correctly the entire time and simply not consulted.
//
// Redis is here because a Redis in a platform namespace usually IS a cache — but
// it is stated as a default to VERIFY, not as a fact, because Redis with AOF/RDB
// persistence is a primary store and this scan cannot tell which it is looking at.
// Anything not listed, INCLUDING an engine this scanner could not identify, is
// treated as durable: the failure that matters here is one-directional.
var cacheEngines = map[string]bool{"redis": true, "valkey": true, "memcached": true}

func planDatabases(b *strings.Builder, rep importReport) {
	w := func(format string, a ...any) { fmt.Fprintf(b, format, a...) }
	var cnpg, selfManaged, caches []dbInfo
	for _, d := range rep.Storage.Databases {
		switch {
		case d.Kind == "CNPG":
			cnpg = append(cnpg, d)
		case cacheEngines[d.Engine]:
			caches = append(caches, d)
		default:
			selfManaged = append(selfManaged, d)
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

	if len(selfManaged) > 0 {
		w("## Self-managed databases — MIGRATE (%d)\n\n", len(selfManaged))
		w("These are databases running as ordinary workloads, not CNPG clusters, so nothing\n")
		w("above covers them and **this plan cannot write the commands for you** — it does\n")
		w("not know their credentials, their topology, or whether the target is CNPG, a\n")
		w("Linode Managed Database, or another self-managed StatefulSet.\n\n")
		w("**Treat every one as holding data that matters.** An earlier version of this\n")
		w("plan listed them under \"Caches — rebuild, do NOT migrate\" and called them\n")
		w("ephemeral, on the strength of their not being CNPG.\n\n")
		for _, d := range selfManaged {
			engine := d.Engine
			if engine == "" {
				engine = "engine not identified — inspect it before deciding anything"
			}
			client := "no client detected"
			if len(d.Clients) > 0 {
				client = "clients: " + strings.Join(d.Clients, ", ")
			}
			w("- `%s/%s` (%s) — %s\n", d.Namespace, d.Name, engine, client)
		}
		w("\nFor each: freeze writes, dump with the engine's own tool from inside the pod,\n")
		w("restore into the target, then verify row counts before cutting traffic over.\n\n")
	}

	if len(caches) > 0 {
		w("## Likely caches — VERIFY, then rebuild (%d)\n\n", len(caches))
		w("These engines are usually deployed as caches, in which case the new cluster\n")
		w("provisions a fresh instance and there is nothing to migrate. **Confirm that\n")
		w("before you skip them**: Redis with AOF or RDB persistence enabled is a primary\n")
		w("store, and this scan reads the image name, not the configuration.\n\n")
		for _, d := range caches {
			w("- `%s/%s` (%s) — check for a persistence volume and for clients that treat it\n", d.Namespace, d.Name, d.Engine)
			w("  as a system of record; if either holds, migrate it like a database.\n")
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
