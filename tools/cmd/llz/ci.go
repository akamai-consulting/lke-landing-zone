package main

// ci.go implements `llz ci` — pipeline-only plumbing invoked by the project's
// GitHub Actions workflows, NOT a hand-run operator command. It is grouped (and
// documented) separately from the operator porcelain so `llz --help` stays
// legible and nobody fat-fingers a CI-only step. Each subcommand is the native
// port of a former instance-scripts/terraform/*.sh step: the decision logic
// lives in internal/terraform (+ internal/linode) behind unit tests, and this
// file is the thin terraform/Linode orchestration around it.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/cli"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/linode"
	tf "github.com/akamai-consulting/lke-landing-zone/tools/internal/terraform"
	"github.com/spf13/cobra"
)

func ciCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ci",
		Short: "pipeline plumbing run by .github/workflows (a few also serve manual incident cleanup)",
		Long: "Plumbing subcommands the project's GitHub Actions workflows call in place of\n" +
			"the former instance-scripts/*.sh CI steps. The terraform ones (tf-import,\n" +
			"tf-apply) expect to run inside a job (a terraform working directory,\n" +
			"step-injected env); the orphan sweeps (reap-volumes, reap-nodebalancers) also\n" +
			"double as the manual fallback operators run by hand. The reusable logic lives\n" +
			"in internal/terraform + internal/linode behind unit tests; these commands are\n" +
			"the thin orchestration over it.",
	}
	c.AddCommand(ciTFImportCmd(), ciTFApplyCmd(), ciTFPlanCmd(), ciReapVolumesCmd(), ciReapNodeBalancersCmd(), ciReapObjKeysCmd(),
		ciPreflightCmd(), ciVerifyObjectStorageCmd(), ciHealthCmd(), ciHealthInClusterCmd(), ciConvergeCmd(),
		ciBaoStatusCmd(),
		ciBaoInitCmd(), ciBaoRegenRootCmd(), ciBaoConfigureCmd(), ciBaoEnsureReadyCmd(),
		ciExtractOpenbaoCACmd(), ciNudgeArgoCmd(), ciProvisionPeerCACmd())
	// Cluster readiness gates (assert-loki-bootstrapped.sh / wait-for-harbor.sh).
	// (wait-harbor was registered here. Its post-S3-seed registry rollout wait was
	// continue-on-error and always paid in series ahead of converge — which already
	// adjudicates the Harbor Argo apps, concurrently with everything else. Retired
	// with its only call site.)
	c.AddCommand(ciAssertLokiCmd(), ciAssertHealthWorkflowCmd(), ciValidateTokensCmd())
	// Generic wait primitives (formerly inline kubectl polling loops in the
	// bootstrap / rotation workflows).
	c.AddCommand(ciWaitPodsCmd(), ciWaitClusterReadyCmd())
	// Fail-fast gate ahead of wait-pods: a missing Argo Application means the
	// platform-bootstrap sync is wedged waves earlier — fail in ~4 min WITH the
	// operationState message instead of burning the 600s pod wait blind (PR #142).
	c.AddCommand(ciAssertArgoAppCmd())
	// Destroy-path teardown sweeps (formerly inline curl+jq in llz-terraform.yml).
	c.AddCommand(ciTeardownCaptureCmd(), ciTeardownForceDeleteCmd(), ciTeardownDeleteVPCCmd(), ciAssertNoOrphansCmd())
	// Rotation routing + the in-cluster narrow-PAT rotation (formerly inline in
	// llz-secret-rotation.yml; rotate-incluster-pat replaced propagate-pat —
	// the broad PAT is CI/Terraform-only and no longer pushed into clusters).
	c.AddCommand(ciRotationPlanCmd(), ciRotateInclusterPATCmd())
	// Harbor API steps (formerly inline curl in llz-bootstrap-openbao.yml).
	// Harbor: the active-path provisioning (project + robots + OpenBao seed +
	// repo-secret publication + smoke) runs IN-CLUSTER via harbor-provisioner
	// (the harbor-robot-provisioner CronJob); seed-standby-harbor-robots is the
	// CI-side standby half. port-forward/ensure-project/smoke were retired with
	// the workflow's harbor job.
	// kick-harbor-provisioner force-ticks that CronJob at bootstrap so the
	// converge tail is event-paced instead of waiting out the */5 schedule.
	c.AddCommand(ciHarborProvisionerCmd(), ciSeedStandbyHarborRobotsCmd(), ciKickHarborProvisionerCmd())
	// Pre-flight guards (require-secret.sh / assert-destroy-confirm.sh).
	c.AddCommand(ciRequireSecretCmd(), ciAssertDestroyConfirmCmd())
	// Bootstrap seeding (bootstrap-cloud-firewall.sh / provision-harbor-robots.sh).
	// (gen-bootstrap-tls was retired: the OTel collector serving cert is now issued
	// by the otel-bootstrap-ca cert-manager chain in the observability component.)
	// bootstrap-cloud-firewall is the manual/recovery fallback; the cidrFirewall
	// component's CronJob (discover-firewall-config) is the steady-state owner.
	c.AddCommand(ciBootstrapCloudFirewallCmd(), ciDiscoverFirewallConfigCmd())
	// Cluster access plumbing (lke-runner-acl action / fetch-kubeconfig action).
	c.AddCommand(ciRunnerACLCmd(), ciFetchKubeconfigCmd(), ciFetchKubeconfigStateCmd())
	// (gh-pat-expiry + cred-audit were registered here. They were the per-provider
	// expiry probes, superseded by the credential single-pane flow below, and were
	// retired once they had zero callers. Their measurement primitives live on in
	// credentials_probe.go.)
	// Credential single-pane-of-glass writer: measure CI-token expiry and emit the
	// ConfigMap the in-cluster reconciler re-exposes as metrics (llz-scheduled-checks.yml).
	c.AddCommand(ciTokenInventoryCmd())
	// Scheduled rotation-SLA + cluster-readiness checks (llz-scheduled-checks.yml).
	c.AddCommand(ciHealthLKEAdminRotationCmd(), ciHealthLokiObjkeyRotationCmd(),
		ciHealthOpenbaoCmd(), ciHealthCertManagerCmd(), ciHealthPromRulesCmd())
	// Apply-time failure diagnostics (llz-terraform.yml). (The former
	// stash-env-secret / ensure-env-secret siblings were retired with the S3-stash
	// hop and the loki-admin-password step — see docs/designs/linode-credential-rotator.md
	// + apl-core-v6-migration.md — so their commands are gone too.)
	c.AddCommand(ciDiagnoseArgoCDCmd())
	// E2E timing instrumentation (docs/designs/e2e-instrumentation.md): a phase
	// timeline (phase-mark/phase-report → step summary + JSON artifact) and the
	// image-pull collector that answers whether a bring-up phase is pull-bound.
	c.AddCommand(ciPhaseMarkCmd(), ciPhaseReportCmd(), ciCollectImagePullsCmd(), ciCollectTimingCmd())
	// Release-e2e instantiate: pin the instance's TF_IMAGE/KUBE_IMAGE to this
	// commit's ci images so the baked llz can't drift from the rendered workflow.
	c.AddCommand(ciPinInstanceImagesCmd())
	// OpenBao KV seed steps (formerly ~15 inline-bash blocks in
	// llz-bootstrap-openbao.yml): the generic bao-seed plus the derive-their-
	// material specials in ci_bao_seed.go / ci_bao_seed_seal_key.go /
	// ci_seed_special.go.
	c.AddCommand(ciBaoSeedCmd(), ciBaoSeedAllCmd(), ciBaoSeedSealKeyCmd(),
		ciResolveHarborURLCmd(), ciAuditPVCStorageClassCmd())
	// Object-storage key lifecycle, one owner end to end: mint-bootstrap-objkeys
	// mints the FIRST Loki/Harbor keys at bootstrap and seeds OpenBao (replacing
	// the TF-minted keys + LOKI_S3_*/HARBOR_REGISTRY_S3_* GitHub relay +
	// seed-harbor-registry-s3); the in-cluster rotator (linodeCredRotator
	// CronJob, slim llz image) owns rotation after first boot.
	c.AddCommand(ciMintBootstrapObjkeysCmd(), ciRotateLinodeCredsCmd(), ciTempObjkeyCmd())
	// In-cluster rotation of the broad account:read_write Linode PAT (LINODE_API_TOKEN):
	// mint -> seed OpenBao -> publish to each deployment's GitHub env secret (sealed box)
	// -> revoke old. Runs in a dedicated CronJob, not the reconciler.
	c.AddCommand(ciRotateBroadPATCmd())
	// Bootstrap seed for the broad-PAT rotator's minting credential — gated on the
	// component being enabled (the account-wide broad PAT lands in exactly one cluster).
	c.AddCommand(ciSeedBroadPATCmd())
	// e2e: force one rotation Job from the CronJob + assert it rotated end-to-end.
	c.AddCommand(ciAssertBroadPATRotationCmd())
	// e2e: prove the operator escape hatch works end to end — the release-e2e seed
	// drops a trivial manifest under kubernetes-custom/namespaces/<ns>/, and this
	// asserts the instance-custom ApplicationSet generated instance-custom-<ns> and
	// it reached Synced+Healthy (a silently-empty hatch leaves platform apps green).
	c.AddCommand(ciAssertInstanceCustomCmd())
	// Linode Volume relabeler — the Go port of the linode-volume-labeler
	// relabel.sh CronJob (also runnable in-cluster by the volume-labels reconciler).
	c.AddCommand(ciRelabelVolumesCmd())
	// The narrow in-cluster PAT, same one-owner shape: mint-bootstrap-pat seeds
	// the first token at bootstrap; rotate-incluster-pat (registered with the
	// rotation commands above) re-mints it monthly.
	c.AddCommand(ciMintBootstrapPATCmd())
	// Secretless day-2 auth: exchange a GitHub OIDC token for an OpenBao token
	// (jwt auth) over a direct API call, for in-cluster runners — the primitive
	// behind the cross-org thin-caller pattern (docs/designs/cross-org-reuse-pattern.md).
	c.AddCommand(ciOpenBaoLoginCmd())
	// Copier render-time slimming: prune docs/ to the operator set + reference
	// the rest at the template repo. (The former strip-comments verb is gone:
	// the vendored llz-*.yml bodies ship verbatim so an instance copy matches
	// the copier render — see copier.yml's _tasks note.)
	c.AddCommand(ciDeliverDocsCmd())
	// Repo-scan gate (former template-scripts python: validate-externalsecret-paths.py
	// via the Makefile).
	c.AddCommand(ciExternalSecretPathsCmd())
	// Static guard for the PR #142 wedge class: negative-sync-wave kinds that
	// could health-wedge the platform-bootstrap sync (Makefile wave-health-guard).
	c.AddCommand(ciWaveHealthGuardCmd())
	// Static guard for the #163 wedge class: a workload that hard-depends on a
	// Secret produced by a LATER-wave ExternalSecret can never go Healthy and
	// wedges the sync (Makefile wave-dependency-guard).
	c.AddCommand(ciWaveDependencyGuardCmd())
	// Live fault-injection game-day: break one platform ExternalSecret and assert
	// the wedge is contained to its own carved Application (blast-radius
	// decomposition proof). Run on a warm e2e cluster.
	c.AddCommand(ciWedgeGamedayCmd())
	// Runtime counterpart to wave-health-guard: assert the VAP is bound + enforcing
	// (negative canary), which is what makes the static guard's verdict hold live.
	c.AddCommand(ciAssertWaveHealthVAPCmd())
	// Cluster diagnostic: list in-cluster Prometheus metric names matching a regex
	// (metric-name discovery for writing error-rate/saturation alerts).
	c.AddCommand(ciPromMetricsCmd())
	// Cluster diagnostic: evaluate deployed PrometheusRule alert exprs against the
	// live Prometheus (catch never-fire / false-positive rules promtool can't).
	c.AddCommand(ciAlertEvalCmd())
	// E2E gate: assert every landing-zone ServiceMonitor has an `up` scrape target
	// and every PrometheusRule group is loaded — the observability-pipeline wiring
	// converge/health/assert-loki all stay green on when a label/port/selector
	// regression silently un-scrapes/un-loads it.
	c.AddCommand(ciAssertScrapeTargetsCmd())
	// E2E gate: assert the reconciler is FUNCTIONALLY healthy (llz_reconcile_up=1 +
	// llz_reconcile_leader=1) — the silently-broken-loop class (pod Running yet
	// failing on dropped RBAC/OpenBao access) that converge and alert-eval --strict
	// both miss.
	c.AddCommand(ciAssertReconcilerCmd())
	// Static guard for the harbor-reconciler mesh class: a NetworkPolicy egress to
	// a STRICT-mesh namespace (harbor) from outside it describes traffic Istio
	// silently drops (Makefile mesh-egress-guard).
	c.AddCommand(ciMeshEgressGuardCmd())
	// Static guard for the #175 day-2-blind class: every ServiceMonitor/PodMonitor/
	// PrometheusRule must carry `prometheus: system` or apl-core's Prometheus
	// silently ignores it (metrics unscraped / rules unloaded) — Makefile
	// monitoring-label-guard.
	c.AddCommand(ciMonitoringLabelGuardCmd())
	// Offline apl-core schema validation (helm template) — the check
	// helm_release.apl runs at apply time, shifted left into scaffold-check.
	c.AddCommand(ciAplSchemaValidateCmd())
	// PrometheusRule promtool gate (former template-scripts python:
	// check-prometheus-rule-crds.py via the Makefile's prom-rules-check) — the
	// last first-party Python script in the repo.
	c.AddCommand(ciCheckPromRulesCmd())
	// Render/coverage lint gates ported from template-scripts (the Makefile's
	// helm-dep-lock-check, argocd-rendered-apps-check, and the per-package
	// coverage floor in `make coverage`).
	c.AddCommand(ciChartLockDriftCmd(), ciArgoCDRenderedAppsCmd(), ciCheckCoverageCmd())
	// Design-principle gate: budget on inline-bash / shell / python logic that
	// should instead live in unit-tested Go (lint.yml). Ratchets DOWN over time.
	c.AddCommand(ciUntestableLOCCmd())
	// Release-hygiene gate: a chart change must bump its Chart.yaml version, or
	// publish-charts.yml never publishes it and clusters keep the stale artifact.
	c.AddCommand(ciChartVersionGuardCmd())
	// Companion gate: every Argo CD chart pin (apl-values targetRevision +
	// llz-argo-bootstrap-apps component version) must match the chart's local
	// Chart.yaml version, or Argo pulls a tag the registry never received and the
	// support-plane app silently never syncs (llz-openbao namespace never created).
	c.AddCommand(ciChartPinGuardCmd())
	// Runtime companion: a pinned first-party chart version must actually EXIST in
	// the OCI registry, or Argo 404s the pull on a feature-branch e2e (bumped-but-
	// unpublished chart) and the OpenBao bootstrap dies on the missing llz-openbao ns.
	c.AddCommand(ciChartPublishCheckCmd())
	// Package + push + keyless-sign first-party charts to GHCR (immutable; re-signs a
	// pushed-but-unsigned version). Replaces publish-charts.yml's inline bash.
	c.AddCommand(ciPublishChartsCmd())
	// Cluster-bootstrap native command + its former local-exec bodies. bootstrap-
	// cluster is the whole in-cluster bootstrap (apl-core install + Argo bridge +
	// the race-ahead Kyverno policies) that used to be the cluster-bootstrap
	// Terraform workspace; wait-apl-pipeline + apply-kyverno-policy remain
	// separately runnable (bootstrap-cluster calls them in-process), and
	// destroy-unwedge / clear-cluster-secrets are the destroy-path cleanups.
	c.AddCommand(ciBootstrapClusterCmd(), ciWaitAplPipelineCmd(), ciApplyKyvernoPolicyCmd(),
		ciDestroyUnwedgeCmd(), ciClearClusterSecretsCmd())
	// Image/source skew guard: fail fast when the baked llz is older than the
	// workflow's template-ref (the independent TF_IMAGE vs template-ref pins drift).
	c.AddCommand(ciAssertImageFreshCmd())
	// CI guard: a container job whose run-steps lack a bash default falls back to
	// dash and breaks `set -o pipefail` (the discover-workflow regression).
	c.AddCommand(ciCheckWorkflowShellsCmd())
	// Scaffold update-class manifest gate (former template-scripts/check-template-manifest.sh).
	c.AddCommand(ciTemplateManifestCmd())
	// Template provenance stamp (former template-scripts/stamp-template-version.sh).
	c.AddCommand(ciStampTemplateVersionCmd())
	return c
}

func ciVerifyObjectStorageCmd() *cobra.Command {
	var region string
	c := &cobra.Command{
		Use:   "verify-object-storage",
		Short: "verify a region's Loki/Harbor object-storage BUCKETS exist before the key mint + seeds",
		Long: "Lists Linode object-storage buckets and checks the region's four exist\n" +
			"(platform-loki-{chunks,ruler,admin}-<region>, platform-harbor-registry-\n" +
			"<region>) — i.e. terraform.yml's apply-object-storage ran. Buckets only:\n" +
			"the scoped KEYS are no longer Terraform-minted — `llz ci\n" +
			"mint-bootstrap-objkeys` mints them AFTER this preflight and the in-cluster\n" +
			"rotator owns them after first boot, so key absence here is normal on a\n" +
			"fresh bootstrap. Non-fatal on a Linode API hiccup (warn + succeed); fails\n" +
			"only when the API responds and a bucket is genuinely absent. Reads\n" +
			"LINODE_API_TOKEN.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIVerifyObjectStorage(region) },
	}
	c.Flags().StringVar(&region, "region", "", "region whose buckets to verify (required)")
	return c
}

func runCIVerifyObjectStorage(region string) error {
	if region == "" {
		return fmt.Errorf("--region is required")
	}
	token, err := ciToken()
	if err != nil {
		return err
	}
	buckets, err := linode.NewClient(token, 30*time.Second).ListObjectStorageBuckets(context.Background())
	if err != nil {
		// Transient API hiccup / auth page / non-JSON body — don't block the
		// bootstrap on a parsing edge case; the mint + seed steps below still run.
		fmt.Fprintf(os.Stderr, "::warning::could not verify object-storage buckets for %s (%v) — skipping this preflight (non-fatal).\n", region, err)
		return nil
	}
	have := map[string]bool{}
	for _, b := range buckets {
		have[cli.AsString(b["label"])] = true
	}
	want := []string{
		"platform-loki-chunks-" + region,
		"platform-loki-ruler-" + region,
		"platform-loki-admin-" + region,
		"platform-harbor-registry-" + region,
	}
	var missing []string
	for _, label := range want {
		if !have[label] {
			missing = append(missing, label)
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "::error::object-storage bucket(s) not found on Linode for %s: %s\n", region, strings.Join(missing, " "))
		fmt.Fprintln(os.Stderr, "The Loki/Harbor buckets are absent — the key mint below would fail and Loki/Harbor would have no object store. Remediate:")
		fmt.Fprintf(os.Stderr, "  gh workflow run terraform.yml -f action=apply -f module=object-storage -f region=%s\n", region)
		fmt.Fprintf(os.Stderr, "  gh workflow run bootstrap-openbao.yml -f region=%s\n", region)
		return fmt.Errorf("object-storage preflight failed: %d bucket(s) missing for %s", len(missing), region)
	}
	fmt.Printf("object-storage preflight OK for %s: all four buckets exist on Linode.\n", region)
	return nil
}

func ciTFImportCmd() *cobra.Command {
	var region string
	var nonfatal bool
	c := &cobra.Command{
		Use:   "tf-import",
		Short: "idempotently import existing Linode cluster resources into TF state",
		Long: "Native port of terraform-linode-import.sh. Run from the cluster terraform\n" +
			"working directory: for each cluster resource (VPC, subnet, LKE cluster, node\n" +
			"pool, node firewall) not already in state, it finds the live resource by label\n" +
			"via the Linode API (fully paginated) and `terraform import`s it. Also seeds a\n" +
			"kubeconfig (real or stub) so the kubernetes/helm/kubectl providers can init.\n" +
			"Reads LINODE_TOKEN (or LINODE_API_TOKEN). --nonfatal logs+skips import failures\n" +
			"instead of aborting (destroy workflows only).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCITFImport(gopts, region, nonfatal) },
	}
	c.Flags().StringVar(&region, "region", "", "tfvars prefix, e.g. primary (required)")
	c.Flags().BoolVar(&nonfatal, "nonfatal", false, "log+skip import failures instead of aborting (destroy only)")
	return c
}

func ciTFApplyCmd() *cobra.Command {
	var varFile, plan string
	c := &cobra.Command{
		Use:   "tf-apply",
		Short: "terraform apply with self-heal for two known idempotent failure modes",
		Long: "Native port of terraform-apply-with-heal.sh. Runs `terraform apply` once; on\n" +
			"failure it matches two known patterns, applies a targeted heal, re-plans, and\n" +
			"retries ONCE. Heal A: a phantom helm_release in state (cluster lost the release)\n" +
			"→ `terraform state rm`. Heal B: a duplicate Cloud Firewall label → find the\n" +
			"existing firewall by label (paginated) and `terraform import` it so the retry\n" +
			"adopts it. Any other error passes through. Reads LINODE_TOKEN (or\n" +
			"TF_VAR_linode_token) for Heal B.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCITFApply(gopts, plan, varFile) },
	}
	c.Flags().StringVar(&plan, "plan", "", "saved terraform plan file to apply (required)")
	c.Flags().StringVar(&varFile, "var-file", "", "tfvars file for re-plan/import (required)")
	return c
}

// cluster-resource terraform addresses (stable; match the bootstrap modules).
const (
	addrVPC      = "module.cluster.linode_vpc.this"
	addrSubnet   = "module.cluster.linode_vpc_subnet.nodes"
	addrCluster  = "module.cluster.linode_lke_cluster.this"
	addrNodePool = "module.node_pool.linode_lke_node_pool.this"
	addrFirewall = "module.cluster.module.node_firewall.linode_firewall.this"
)

func runCITFImport(g globalOpts, region string, nonfatal bool) error {
	if region == "" {
		return fmt.Errorf("--region is required (the tfvars prefix, e.g. primary)")
	}
	token := firstNonEmpty(os.Getenv("LINODE_TOKEN"), os.Getenv("LINODE_API_TOKEN"))
	if token == "" {
		return fmt.Errorf("set LINODE_TOKEN (or LINODE_API_TOKEN) to a Linode PAT")
	}

	// tfvars file: prefer <region>.tfvars, fall back to the .example (mirrors the
	// script + the plan step's own resolution).
	varFile := region + ".tfvars"
	if _, err := os.Stat(varFile); err != nil {
		varFile = region + ".tfvars.example"
	}
	content, err := os.ReadFile(varFile)
	if err != nil {
		return fmt.Errorf("read %s: %w", varFile, err)
	}
	labels := tf.DeriveLabels(tf.ParseTFVars(string(content)))
	if labels.Cluster == "" {
		return fmt.Errorf("%s has no cluster_label", varFile)
	}

	client := linode.NewClient(token, 60*time.Second)
	ctx := context.Background()

	if err := ensureKubeconfig(ctx, g, client, labels.Cluster); err != nil {
		return err
	}

	// ── VPC (always fatal — fast, no cluster dependency, even under --nonfatal) ──
	// linode_vpc.this is a COUNTED resource (llz-cluster module:
	// `count = local.create_vpc ? 1 : 0`, create_vpc = vpc_id == ""), so for a
	// dedicated VPC its real state address is this[0]. A shared-VPC deployment
	// (vpc_network set) has no such resource — nothing to import here — but the
	// subnet below still needs the VPC id, so we resolve it either way. Importing
	// the un-indexed `.this` fails with "Configuration for import target does not
	// exist", which silently orphaned the VPC/subnet (they could not be re-adopted
	// into state) and surfaced as label-collisions on the next apply.
	dedicatedVPC := tf.ParseTFVars(string(content)).VPCNetwork == ""
	addrVPCEff := addrVPC + "[0]"
	var vpcID string
	if dedicatedVPC {
		vpcID = tfStateID(addrVPCEff)
	}
	if vpcID != "" {
		fmt.Printf("%s already in state — skipping\n", addrVPCEff)
	} else {
		vpcs, err := client.ListVPCs(ctx)
		if err != nil {
			return fmt.Errorf("list VPCs: %w", err)
		}
		if id, ok := linode.FindIDByLabel(vpcs, labels.VPC); ok {
			vpcID = strconv.FormatUint(id, 10)
			// Only a dedicated VPC is managed (and thus imported) by this root; a
			// shared VPC is owned by the vpc/<network> root — we just reuse its id
			// for the subnet import.
			if dedicatedVPC {
				if _, err := tfImport(g, varFile, addrVPCEff, vpcID, false); err != nil {
					return err
				}
			}
		} else {
			fmt.Printf("VPC %q not found in Linode — skipping import\n", labels.VPC)
		}
	}

	// ── VPC subnet (always fatal; needs the VPC id) ──
	if subnetInState := tfStateID(addrSubnet); subnetInState != "" {
		fmt.Printf("%s already in state — skipping\n", addrSubnet)
	} else if vpcID == "" {
		fmt.Println("No VPC id available — skipping subnet import")
	} else {
		vpcNum, _ := strconv.ParseUint(vpcID, 10, 64)
		subs, err := client.ListVPCSubnets(ctx, vpcNum)
		if err != nil {
			return fmt.Errorf("list subnets of vpc %s: %w", vpcID, err)
		}
		if sid, ok := linode.FindIDByLabel(subs, labels.Subnet); ok {
			if _, err := tfImport(g, varFile, addrSubnet, vpcID+","+strconv.FormatUint(sid, 10), false); err != nil {
				return err
			}
		} else {
			fmt.Printf("Subnet %q not found in VPC %s — skipping import\n", labels.Subnet, vpcID)
		}
	}

	// ── LKE cluster (nonfatal-aware; a failed import clears the id so the pool
	//    import is skipped too) ──
	clusterID := tfStateID(addrCluster)
	if clusterID == "" {
		ids, err := client.ClustersWithLabel(ctx, labels.Cluster)
		if err != nil {
			return fmt.Errorf("list clusters: %w", err)
		}
		if len(ids) > 0 {
			clusterID = strconv.FormatUint(ids[0], 10)
			ok, err := tfImport(g, varFile, addrCluster, clusterID, !nonfatal)
			if err != nil {
				return err
			}
			if !ok {
				clusterID = ""
			}
		} else {
			fmt.Printf("Cluster %q not found in Linode — skipping import\n", labels.Cluster)
		}
	} else {
		fmt.Printf("%s already in state — skipping\n", addrCluster)
	}

	// ── LKE node pool (nonfatal-aware; needs the cluster id) ──
	if poolInState := tfStateID(addrNodePool); poolInState != "" {
		fmt.Printf("%s already in state — skipping\n", addrNodePool)
	} else if clusterID == "" {
		fmt.Println("No cluster id available — skipping node pool import")
	} else {
		cNum, _ := strconv.ParseUint(clusterID, 10, 64)
		pools, err := client.ListNodePools(ctx, cNum)
		if err != nil {
			return fmt.Errorf("list node pools of cluster %s: %w", clusterID, err)
		}
		if pid, ok := tf.SelectNodePoolID(pools, labels.NodePool); ok {
			if _, err := tfImport(g, varFile, addrNodePool, clusterID+","+strconv.FormatUint(pid, 10), !nonfatal); err != nil {
				return err
			}
		} else {
			fmt.Printf("Node pool %q not found by label or tag — skipping import\n", labels.NodePool)
		}
	}

	// ── Node firewall (nonfatal-aware; account-unique label) ──
	if fwInState := tfStateID(addrFirewall); fwInState != "" {
		fmt.Printf("%s already in state — skipping\n", addrFirewall)
	} else {
		fws, err := client.ListFirewalls(ctx)
		if err != nil {
			return fmt.Errorf("list firewalls: %w", err)
		}
		if fid, ok := linode.FindIDByLabel(fws, labels.Firewall); ok {
			if _, err := tfImport(g, varFile, addrFirewall, strconv.FormatUint(fid, 10), !nonfatal); err != nil {
				return err
			}
		} else {
			fmt.Printf("Firewall %q not found — skipping import\n", labels.Firewall)
		}
	}

	return nil
}

// ensureKubeconfig writes generated/<cluster>-kubeconfig.yaml if absent so the
// kubernetes/helm/kubectl providers can initialise: the real kubeconfig when the
// cluster exists and the API serves it, otherwise a stub.
func ensureKubeconfig(ctx context.Context, g globalOpts, client *linode.Client, clusterLabel string) error {
	path := filepath.Join("generated", clusterLabel+"-kubeconfig.yaml")
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if g.dryRun {
		fmt.Fprintln(os.Stderr, "→ (dry-run) ensure kubeconfig "+path)
		return nil
	}
	if err := os.MkdirAll("generated", 0o755); err != nil {
		return fmt.Errorf("mkdir generated: %w", err)
	}
	var b64 string
	if ids, err := client.ClustersWithLabel(ctx, clusterLabel); err == nil && len(ids) > 0 {
		if kc, err := client.GetKubeconfig(ctx, ids[0]); err == nil {
			b64 = kc
		}
	}
	content, stub := tf.KubeconfigContent(b64)
	if stub {
		fmt.Printf("Kubeconfig unavailable for %q — writing stub for provider init\n", clusterLabel)
	} else {
		fmt.Printf("Kubeconfig written to %s\n", path)
	}
	return os.WriteFile(path, content, 0o600)
}

// tfImport runs `terraform import` for a resource, with the script's timeouts
// (300s fatal / 120s non-fatal). When fatal it returns the error; when non-fatal
// it logs a warning and returns (false, nil) so the caller can skip dependents.
// Honors --dry-run (prints, imports nothing). ok reports whether the resource is
// now in state.
func tfImport(g globalOpts, varFile, addr, id string, fatal bool) (ok bool, err error) {
	fmt.Printf("Importing %s (id=%s)\n", addr, id)
	if g.dryRun {
		fmt.Fprintf(os.Stderr, "→ (dry-run) terraform import -var-file=%s %s %s\n", varFile, addr, id)
		return true, nil
	}
	timeout := 300 * time.Second
	if !fatal {
		timeout = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "terraform", "import", "-var-file="+varFile, addr, id)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if runErr := cmd.Run(); runErr != nil {
		if fatal {
			return false, fmt.Errorf("import %s: %w", addr, runErr)
		}
		fmt.Printf("WARNING: import of %s timed out or failed — skipping (post-destroy API cleanup will delete it)\n", addr)
		return false, nil
	}
	return true, nil
}

// tfStateID returns the id of an in-state resource via `terraform state show`,
// or "" if the resource is not in state: `state show` of an absent address
// exits non-zero, so the error path covers the not-in-state case.
func tfStateID(addr string) string {
	out, err := exec.Command("terraform", "state", "show", addr).Output()
	if err != nil {
		return ""
	}
	return tf.ParseStateID(string(out))
}

// clusterUnreachableSettle is how long Heal C waits for the LKE-E control plane
// to settle before re-planning after a transient "Kubernetes cluster
// unreachable" apply failure. A package var so tests can zero it.
var clusterUnreachableSettle = 30 * time.Second

func runCITFApply(g globalOpts, plan, varFile string) error {
	if plan == "" || varFile == "" {
		return fmt.Errorf("--plan and --var-file are required")
	}
	if g.dryRun {
		fmt.Fprintf(os.Stderr, "→ (dry-run) terraform apply -auto-approve %s (with self-heal + one retry)\n", plan)
		return nil
	}

	// First attempt — the happy path. -no-color is load-bearing: the heal
	// parsers anchor on the plain "  with <addr>," diagnostic lines.
	applyLog, code, err := runTeed("terraform", "apply", "-no-color", "-auto-approve", plan)
	if err != nil {
		return fmt.Errorf("could not run terraform apply: %w", err)
	}
	if code == 0 {
		return nil
	}

	healed := false

	// ── Heal A: phantom helm_release in state ──
	if addr := tf.ParseHelmPhantom(applyLog); addr != "" {
		fmt.Fprintf(os.Stderr, "::warning::Detected stale TF state for %s (cluster lacks the underlying release). Self-healing.\n", addr)
		if err := runTF("state", "rm", addr); err != nil {
			return fmt.Errorf("terraform state rm %s failed — original apply error stands (exit %d): %w", addr, code, err)
		}
		healed = true
	}

	// ── Heal B: duplicate Cloud Firewall label ──
	if !healed && tf.FirewallCollision(applyLog) {
		if err := healFirewallCollision(g, applyLog, varFile, code); err != nil {
			return err
		}
		healed = true
	}

	// ── Heal C: transient "Kubernetes cluster unreachable" ──
	// No state to repair: the apiserver flaked on a TLS handshake mid-apply
	// (the LKE-E HA control plane can drop an individual replica seconds after
	// wait-cluster-ready passed). Let it settle, then fall through to the shared
	// re-plan + re-apply — the re-plan is load-bearing here, since the failed
	// apply already created earlier resources and staled the saved plan.
	if !healed && tf.TransientAPIFlake(applyLog) {
		fmt.Fprintf(os.Stderr, "::warning::Apply hit a transient control-plane API flake (TLS handshake/timeout against :6443 after readiness passed). Waiting %s for the control plane to settle, then retrying.\n", clusterUnreachableSettle)
		time.Sleep(clusterUnreachableSettle)
		healed = true
	}

	// ── Heal D: transient Cloud Firewall device-read flake ──
	// No state to repair: the node firewall was created but the provider's
	// immediate read-back of its attached devices failed on Linode read-after-
	// write consistency ("Failed to Get Devices for Firewall <id>", usually with
	// terraform's generic "Provider returned invalid result object after apply").
	// A settle + shared re-plan + re-apply re-reads the now-consistent firewall
	// and succeeds. This class of flake burned a whole cold e2e create (run
	// 29655607246) that "no self-heal pattern detected" refused to retry.
	if !healed && tf.FirewallDeviceReadFlake(applyLog) {
		fmt.Fprintf(os.Stderr, "::warning::Apply hit a transient Cloud Firewall device-read flake (Linode read-after-write consistency). Waiting %s to settle, then retrying.\n", clusterUnreachableSettle)
		time.Sleep(clusterUnreachableSettle)
		healed = true
	}

	if !healed {
		return fmt.Errorf("terraform apply failed (exit %d); no self-heal pattern detected, not retrying", code)
	}

	fmt.Fprintln(os.Stderr, "::notice::Re-planning after state heal.")
	if err := runTF("plan", "-no-color", "-out="+plan, "-var-file="+varFile); err != nil {
		return fmt.Errorf("re-plan failed after state heal: %w", err)
	}
	fmt.Fprintln(os.Stderr, "::notice::Retrying apply after state heal.")
	return runTF("apply", "-no-color", "-auto-approve", plan)
}

// healFirewallCollision resolves the colliding firewall by label (paginated) and
// imports it into the resource address terraform tried to create, so the retry
// adopts it instead of recreating.
func healFirewallCollision(g globalOpts, applyLog, varFile string, applyExit int) error {
	fwAddr := tf.ParseFirewallAddress(applyLog)
	if fwAddr == "" {
		return fmt.Errorf("firewall label collision detected but could not parse the resource address — original error stands (exit %d)", applyExit)
	}
	token := firstNonEmpty(os.Getenv("LINODE_TOKEN"), os.Getenv("TF_VAR_linode_token"))
	if token == "" {
		return fmt.Errorf("firewall collision but LINODE_TOKEN / TF_VAR_linode_token is unset — cannot look up the existing firewall (exit %d)", applyExit)
	}
	content, err := os.ReadFile(varFile)
	if err != nil {
		return fmt.Errorf("read %s: %w", varFile, err)
	}
	label := tf.ResolveFirewallLabel(tf.ParseTFVars(string(content)))

	client := linode.NewClient(token, 60*time.Second)
	fws, err := client.ListFirewalls(context.Background())
	if err != nil {
		return fmt.Errorf("list firewalls: %w", err)
	}
	id, ok := linode.FindIDByLabel(fws, label)
	if !ok {
		return fmt.Errorf("firewall %q collided on create but was not found by label in the account — cannot import (exit %d)", label, applyExit)
	}
	fmt.Fprintf(os.Stderr, "::warning::Firewall label %q already exists (id=%d); importing it into %s so the retry adopts it.\n", label, id, fwAddr)
	if err := runTF("import", "-var-file="+varFile, fwAddr, strconv.FormatUint(id, 10)); err != nil {
		return fmt.Errorf("terraform import %s %d failed — original apply error stands (exit %d): %w", fwAddr, id, applyExit, err)
	}
	return nil
}

// runTF runs a terraform subcommand with inherited stdio.
func runTF(args ...string) error {
	cmd := exec.Command("terraform", args...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	return cmd.Run()
}

// runTeed runs a command streaming combined stdout+stderr to the terminal while
// also capturing it, and returns (output, exitCode, startErr). startErr is non-nil
// only when the process could not be started/observed; a non-zero terraform exit
// is reported via exitCode, not startErr.
func runTeed(name string, args ...string) (string, int, error) {
	var buf bytes.Buffer
	w := io.MultiWriter(os.Stdout, &buf)
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = w, w
	err := cmd.Run()
	if err == nil {
		return buf.String(), 0, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return buf.String(), ee.ExitCode(), nil
	}
	return buf.String(), -1, err
}

// ── orphan-resource sweeps (ports of cleanup-orphan-{volumes,nodebalancers}.sh) ──
// Both reuse the orphan-identity heuristics + list/delete primitives in
// internal/linode (the same ones `llz reap` drives); this is just the
// CI-scoped orchestration. Dry-run by default; deletes only with --yes.

func ciReapVolumesCmd() *cobra.Command {
	var region, volumeIDs, tagMustInclude string
	var waitDetach, attempts, retryDelay int
	var requireEmpty bool
	c := &cobra.Command{
		Use:   "reap-volumes",
		Short: "delete orphaned pvc-* Block Storage Volumes (--yes to delete)",
		Long: "Native port of cleanup-orphan-volumes.sh. Deletes unattached CSI Volumes\n" +
			"(label pvc-*, linode_id null) scoped by --volume-ids and/or --region, with an\n" +
			"optional --tag-must-include constraint — the same orphan predicate as `llz\n" +
			"reap`. At least one scope is required (never an unscoped sweep).\n" +
			"--wait-detach polls until every --volume-ids Volume is unattached before\n" +
			"sweeping (cluster delete detaches them asynchronously as the LKE Linodes\n" +
			"tear down).\n" +
			"--require-empty (needs --volume-ids) re-lists after the sweep and, if any\n" +
			"tracked Volume is still present, retries up to --attempts (sleeping\n" +
			"--retry-delay s between tries) and finally EXITS NON-ZERO when orphans\n" +
			"remain — so a destroy doesn't go green leaving Volumes that block the next\n" +
			"apply's preflight. Without it the sweep is single-pass and best-effort.\n" +
			"Reads LINODE_TOKEN; dry-run by default, deletes only with --yes.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runCIReapVolumes(gopts, region, volumeIDs, tagMustInclude, waitDetach, attempts, retryDelay, requireEmpty)
		},
	}
	f := c.Flags()
	f.StringVar(&region, "region", "", "scope to one Linode region (e.g. us-ord)")
	f.StringVar(&volumeIDs, "volume-ids", "", "space-separated Volume id allowlist (the precise CI scope)")
	f.StringVar(&tagMustInclude, "tag-must-include", "", "only delete Volumes whose tags include this (e.g. block-storage)")
	f.IntVar(&waitDetach, "wait-detach", 0, "seconds to wait for the --volume-ids Volumes to detach before sweeping (0 = no wait)")
	f.BoolVar(&requireEmpty, "require-empty", false, "verify every --volume-ids Volume is gone; retry then fail if orphans remain")
	f.IntVar(&attempts, "attempts", 1, "sweep+verify attempts before failing (only with --require-empty)")
	f.IntVar(&retryDelay, "retry-delay", 30, "seconds between --require-empty retries")
	return c
}

func ciReapNodeBalancersCmd() *cobra.Command {
	var clusterID, region string
	var attempts, retryDelay int
	var requireEmpty bool
	c := &cobra.Command{
		Use:   "reap-nodebalancers",
		Short: "delete orphaned NodeBalancers (--cluster-id for the CI-scoped sweep; --yes to delete)",
		Long: "Native port of cleanup-orphan-nodebalancers.sh. With --cluster-id it deletes\n" +
			"only NodeBalancers carrying that cluster's CCM tag (lke<id>) — the\n" +
			"co-located-peer-safe mode the destroy path uses. Without it, an account-wide\n" +
			"orphan sweep (CCM tag points to a gone cluster, or CCM-identified with 0\n" +
			"backends), optionally narrowed by --region. Dry-run by default; --yes to delete.\n" +
			"--require-empty (needs --cluster-id) re-lists after the sweep and, if any\n" +
			"NodeBalancer still carries the cluster's CCM tag, retries up to --attempts\n" +
			"(sleeping --retry-delay s between tries) and finally EXITS NON-ZERO when\n" +
			"orphans remain — so a destroy doesn't go green leaving a NodeBalancer that\n" +
			"blocks the next apply's preflight.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runCIReapNodeBalancers(gopts, clusterID, region, attempts, retryDelay, requireEmpty)
		},
	}
	f := c.Flags()
	f.StringVar(&clusterID, "cluster-id", "", "scope to one cluster's CCM-tagged NodeBalancers (numeric LKE id)")
	f.StringVar(&region, "region", "", "narrow the account-wide sweep to one region (ignored with --cluster-id)")
	f.BoolVar(&requireEmpty, "require-empty", false, "verify the cluster's NodeBalancers are gone; retry then fail if orphans remain")
	f.IntVar(&attempts, "attempts", 1, "sweep+verify attempts before failing (only with --require-empty)")
	f.IntVar(&retryDelay, "retry-delay", 30, "seconds between --require-empty retries")
	return c
}

func ciReapObjKeysCmd() *cobra.Command {
	var env string
	c := &cobra.Command{
		Use:   "reap-objkeys",
		Short: "delete a destroyed deployment's minted Linode obj-storage keys + in-cluster PAT (--yes to delete)",
		Long: "Teardown hygiene for the ACCOUNT-scoped Linode credentials a deployment mints\n" +
			"at bootstrap/rotation: the loki + harbor-registry Object Storage keys\n" +
			"(platform-loki-<env> / platform-harbor-registry-<env>) and the narrow in-cluster\n" +
			"PAT (llz-incluster-<env>). These carry no cluster tag, so the cluster-liveness\n" +
			"sweeps (reap-volumes / reap-nodebalancers / `llz reap`) can't see them; a leaked\n" +
			"mint (failed run, failed grace-window revoke) accretes toward the account's\n" +
			"100-key / 100-PAT caps until a fresh mint 400s. Run on the destroy path with the\n" +
			"env being torn down. Exact-label match — never another env's creds, and never the\n" +
			"broad token this runs under (a different label). Reads LINODE_TOKEN; dry-run by\n" +
			"default, --yes to delete.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIReapObjKeys(gopts, env) },
	}
	c.Flags().StringVar(&env, "env", "", "deployment whose minted keys + PAT to reap (required)")
	return c
}

func runCIReapObjKeys(g globalOpts, env string) error {
	if env == "" {
		return fmt.Errorf("--env is required")
	}
	token, err := ciToken()
	if err != nil {
		return err
	}
	client := linode.NewClient(token, 60*time.Second)
	ctx := context.Background()
	del, fin := ciDeleter(ctx, g, client)
	if err := reapEnvObjKeys(ctx, client, env, del); err != nil {
		return err
	}
	if err := reapEnvInclusterPAT(ctx, client, env, del); err != nil {
		return err
	}
	return fin()
}

// ciToken reads the Linode PAT the CI sweeps run under.
func ciToken() (string, error) {
	t := firstNonEmpty(os.Getenv("LINODE_TOKEN"), os.Getenv("LINODE_API_TOKEN"))
	if t == "" {
		return "", fmt.Errorf("set LINODE_TOKEN (or LINODE_API_TOKEN) to a Linode PAT")
	}
	return t, nil
}

// ciDeleter returns a delete closure that honors --yes/--dry-run and tallies
// outcomes, plus a finalize func that prints the summary and errors if any delete
// failed. Mirrors the del/summary scaffolding in runReap.
func ciDeleter(ctx context.Context, g globalOpts, client *linode.Client) (func(path, desc string), func() error) {
	confirm := g.yes && !g.dryRun
	if !confirm {
		fmt.Println("DRY-RUN — nothing will be deleted. Re-run with --yes to delete.")
	}
	deleted, failed := 0, 0
	del := func(path, desc string) {
		if !confirm {
			fmt.Printf("  would DELETE %s\n", desc)
			return
		}
		if err := client.DeleteResourcePath(ctx, path); err != nil {
			fmt.Fprintf(os.Stderr, "  DELETE %s FAILED: %v\n", desc, err)
			failed++
			return
		}
		fmt.Printf("  DELETE %s\n", desc)
		deleted++
	}
	fin := func() error {
		fmt.Printf("summary: deleted=%d failed=%d\n", deleted, failed)
		if failed > 0 {
			return fmt.Errorf("%d delete(s) failed", failed)
		}
		return nil
	}
	return del, fin
}

func runCIReapVolumes(g globalOpts, region, volumeIDs, tagMustInclude string, waitDetach, attempts, retryDelay int, requireEmpty bool) error {
	if region == "" && volumeIDs == "" {
		return fmt.Errorf("--region and/or --volume-ids is required (refusing an unscoped Volume sweep)")
	}
	if requireEmpty && volumeIDs == "" {
		return fmt.Errorf("--require-empty needs --volume-ids (the precise set whose disappearance is verified)")
	}
	token, err := ciToken()
	if err != nil {
		return err
	}
	client := linode.NewClient(token, 60*time.Second)
	ctx := context.Background()
	confirm := g.yes && !g.dryRun
	if attempts < 1 {
		attempts = 1
	}

	var lastErr error
	remaining := -1
	for attempt := 1; attempt <= attempts; attempt++ {
		if waitDetach > 0 && volumeIDs != "" {
			waitVolumesDetached(ctx, client, volumeIDs, waitDetach)
		}
		del, fin := ciDeleter(ctx, g, client)
		fmt.Printf("=== orphan Volumes (region=%q volume-ids=%q tag=%q, label prefix pvc-, unattached) [attempt %d/%d] ===\n",
			region, volumeIDs, tagMustInclude, attempt, attempts)
		if err := reapVolumes(ctx, client, reapOpts{region: region, volumeIDs: volumeIDs, tagMustInclude: tagMustInclude}, del); err != nil {
			return err
		}
		lastErr = fin()

		// Without --require-empty (or in dry-run, where nothing was deleted)
		// keep the historical single-pass best-effort behavior.
		if !requireEmpty || !confirm {
			return lastErr
		}

		var verr error
		if remaining, verr = countVolumesPresent(ctx, client, volumeIDs); verr != nil {
			fmt.Fprintf(os.Stderr, "verify Volumes: %v\n", verr)
			remaining = -1
		} else if remaining == 0 {
			fmt.Println("verified: all tracked Volumes are gone.")
			return lastErr
		} else {
			fmt.Printf("verify: %d tracked Volume(s) still present after attempt %d/%d.\n", remaining, attempt, attempts)
		}
		if attempt < attempts {
			fmt.Printf("retrying the Volume sweep in %ds...\n", retryDelay)
			time.Sleep(time.Duration(retryDelay) * time.Second)
		}
	}
	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("reap-volumes: %s tracked Volume(s) still present after %d attempt(s) — orphans remain; failing the destroy so they don't block the next apply's preflight",
		firstNonEmpty(itoaOrUnknown(remaining), "some"), attempts)
}

// countVolumesPresent reports how many of the tracked Volume ids still exist in
// the account (attached or not) — the post-sweep convergence check for
// --require-empty. A surviving id is a genuine orphan (or a delete still
// settling), so the caller retries and ultimately fails on a non-zero count.
func countVolumesPresent(ctx context.Context, client interface {
	ListVolumes(context.Context) ([]map[string]any, error)
}, volumeIDs string) (int, error) {
	tracked := map[string]bool{}
	for _, id := range strings.Fields(volumeIDs) {
		tracked[id] = true
	}
	vols, err := client.ListVolumes(ctx)
	if err != nil {
		return -1, err
	}
	n := 0
	for _, v := range vols {
		if tracked[linode.MapIDString(v)] {
			n++
		}
	}
	return n, nil
}

// itoaOrUnknown renders a count, mapping the -1 "list failed" sentinel to "".
func itoaOrUnknown(n int) string {
	if n < 0 {
		return ""
	}
	return strconv.Itoa(n)
}

// volumeDetachPollInterval is the pause between detach re-checks. The Volumes
// detach asynchronously as the LKE nodes tear down, so a tighter poll catches
// "all detached" sooner (a ListVolumes read is cheap) — 10s trims up to ~20s off
// teardown vs the former 30s without meaningfully more API load. A package var so
// tests can zero it.
var volumeDetachPollInterval = 10 * time.Second

// waitVolumesDetached polls until none of the tracked Volume ids is still
// attached (linode_id non-null), bounded by waitSec. Best-effort: a list error
// or timeout just falls through to the sweep — VolumeIsCandidate skips anything
// still attached, so it is left for the next run rather than mis-deleted.
func waitVolumesDetached(ctx context.Context, client interface {
	ListVolumes(context.Context) ([]map[string]any, error)
}, volumeIDs string, waitSec int) {
	tracked := map[string]bool{}
	for _, id := range strings.Fields(volumeIDs) {
		tracked[id] = true
	}
	deadline := time.Now().Add(time.Duration(waitSec) * time.Second)
	for attempt := 1; ; attempt++ {
		still := -1 // unknown on a list error
		if vols, err := client.ListVolumes(ctx); err == nil {
			still = 0
			for _, v := range vols {
				if tracked[linode.MapIDString(v)] && !linode.VolumeLinodeIDNull(v) {
					still++
				}
			}
		}
		if still == 0 {
			fmt.Println("all tracked Volumes are detached.")
			return
		}
		if time.Now().After(deadline) {
			fmt.Printf("tracked Volumes still attached after %ds — sweeping what is detached; the rest is left for the next run.\n", waitSec)
			return
		}
		if still < 0 {
			fmt.Printf("tracked Volumes still attached: unknown (list error, attempt %d)\n", attempt)
		} else {
			fmt.Printf("tracked Volumes still attached: %d (attempt %d)\n", still, attempt)
		}
		time.Sleep(volumeDetachPollInterval)
	}
}

func runCIReapNodeBalancers(g globalOpts, clusterID, region string, attempts, retryDelay int, requireEmpty bool) error {
	if clusterID != "" {
		if _, perr := strconv.ParseUint(clusterID, 10, 64); perr != nil {
			return fmt.Errorf("--cluster-id must be a numeric LKE cluster id (got %q)", clusterID)
		}
	}
	if requireEmpty && clusterID == "" {
		return fmt.Errorf("--require-empty needs --cluster-id (the scoped set whose disappearance is verified)")
	}
	token, err := ciToken()
	if err != nil {
		return err
	}
	client := linode.NewClient(token, 60*time.Second)
	ctx := context.Background()
	confirm := g.yes && !g.dryRun
	if attempts < 1 {
		attempts = 1
	}

	// Account-wide orphan sweep (cluster gone / 0-backend) — reuse reap's logic.
	// There's no precise scoped set to converge on, so this stays single-pass.
	if clusterID == "" {
		del, fin := ciDeleter(ctx, g, client)
		fmt.Printf("=== orphan NodeBalancers — account-wide (region=%q) ===\n", region)
		if err := reapNodeBalancers(ctx, client, reapOpts{region: region}, del); err != nil {
			return err
		}
		return fin()
	}

	// Scoped sweep: only NodeBalancers carrying THIS cluster's CCM tag (lke<id>).
	var lastErr error
	remaining := -1
	for attempt := 1; attempt <= attempts; attempt++ {
		del, fin := ciDeleter(ctx, g, client)
		fmt.Printf("=== orphan NodeBalancers — scoped to cluster %s (lke_cluster.id or CCM tag lke%s) [attempt %d/%d] ===\n",
			clusterID, clusterID, attempt, attempts)
		nbs, err := client.ListNodeBalancers(ctx)
		if err != nil {
			return fmt.Errorf("list NodeBalancers: %w", err)
		}
		matched := false
		for _, nb := range nbs {
			if !nbBelongsToCluster(nb, clusterID) {
				continue
			}
			id := linode.MapUint(nb, "id")
			del(fmt.Sprintf("/v4/nodebalancers/%d", id),
				fmt.Sprintf("nodebalancer %d (%s)", id, linode.MapString(nb, "label")))
			matched = true
		}
		if !matched {
			fmt.Println("  none matched")
		}
		lastErr = fin()

		if !requireEmpty || !confirm {
			return lastErr
		}

		var verr error
		if remaining, verr = countClusterNodeBalancersPresent(ctx, client, clusterID); verr != nil {
			fmt.Fprintf(os.Stderr, "verify NodeBalancers: %v\n", verr)
			remaining = -1
		} else if remaining == 0 {
			fmt.Println("verified: the cluster's NodeBalancers are gone.")
			return lastErr
		} else {
			fmt.Printf("verify: %d cluster NodeBalancer(s) still present after attempt %d/%d.\n", remaining, attempt, attempts)
		}
		if attempt < attempts {
			fmt.Printf("retrying the NodeBalancer sweep in %ds...\n", retryDelay)
			time.Sleep(time.Duration(retryDelay) * time.Second)
		}
	}
	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("reap-nodebalancers: %s cluster NodeBalancer(s) still present after %d attempt(s) — orphans remain; failing the destroy so they don't block the next apply's preflight",
		firstNonEmpty(itoaOrUnknown(remaining), "some"), attempts)
}

// countClusterNodeBalancersPresent reports how many NodeBalancers still carry
// the cluster's CCM tag (lke<id>) — the post-sweep convergence check for
// reap-nodebalancers --require-empty.
func countClusterNodeBalancersPresent(ctx context.Context, client interface {
	ListNodeBalancers(context.Context) ([]map[string]any, error)
}, clusterID string) (int, error) {
	nbs, err := client.ListNodeBalancers(ctx)
	if err != nil {
		return -1, err
	}
	n := 0
	for _, nb := range nbs {
		if nbBelongsToCluster(nb, clusterID) {
			n++
		}
	}
	return n, nil
}

// nbBelongsToCluster reports whether a NodeBalancer is owned by the given LKE
// cluster id — by its lke_cluster.id (LKE-E's reliable owner link), else its CCM
// `lke<id>` tag (older CCMs). Matching only the tag missed LKE-E CCM
// NodeBalancers, which carry just the `kubernetes` tag, so the destroy's scoped
// sweep deleted nothing and they (and the VPC they parked in) leaked.
func nbBelongsToCluster(nb map[string]any, clusterID string) bool {
	return linode.LKEClusterIDFromNB(nb) == clusterID || linode.LKEIDFromTags(linode.MapTags(nb)) == clusterID
}
