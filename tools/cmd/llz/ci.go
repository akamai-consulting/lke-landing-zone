package main

// ci.go implements `llz ci` — pipeline-only plumbing invoked by the project's
// GitHub Actions workflows, NOT a hand-run operator command. It is grouped (and
// documented) separately from the operator porcelain so `llz --help` stays
// legible and nobody fat-fingers a CI-only step. Each subcommand is the native
// port of a former instance-scripts/terraform/*.sh step: the decision logic
// lives in internal/terraform (+ internal/linode) behind unit tests, and this
// file is the thin terraform/Linode orchestration around it.

import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/assertidentity"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/assertnetwork"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/assertobjstore"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/assertobs"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/assertplatform"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/assertsecrets"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/assertsuite"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/configreadiness"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/manifestguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/seedspecial"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/templatecommit"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/capabilities/assertreconciler"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/capabilities/assertregistry"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/capabilities/database"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/capabilities/gameday"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/capabilities/releasepublish"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/budget"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/chartguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/cosignguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/coverageguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/credcoverage"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/docsguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/meshegress"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/monitoringlabel"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/mtlsguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/plaintext"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/templatemanifest"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/versionpins"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/wavehealth"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/workflowshells"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/atrest"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/bootstrapcluster"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/chartpublish"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/clusteraccess"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/converge"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/credrotate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/deliverdocs"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/firewall"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/harbor"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/healthsla"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/identityconfig"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/kyverno"
	openbaoext "github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/openbao"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/reconciler"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/statepassphrase"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/teardown"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/tofudriver"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/tokeninv"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baoread"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cliopts"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghsecret"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/verbs/argodiag"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/verbs/mutate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/verbs/phasetiming"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/verbs/upgrade"
	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/capabilities/objenc"
)

func ciCmd() *cobra.Command {
	// Install converge's capability set before any of its verbs can run. Here
	// rather than in main() because cliopts.Global is populated by flag parsing, and DryRun
	// is one of the capabilities.
	installConvergeDeps(cliopts.Global)
	installAssertPlatformDeps()
	installAssertReconcilerDeps()
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
	c.AddCommand(tofudriver.TFImportCmd(), tofudriver.TFApplyCmd(), tofudriver.PlanCmd(), tofudriver.OutputCmd(), tofudriver.DestroyCmd(), teardown.ReapVolumesCmd(), teardown.ReapNodeBalancersCmd(), teardown.ReapObjKeysCmd(),
		configreadiness.PreflightCmd(), assertobjstore.VerifyObjectStorageCmd(), converge.HealthCmd(), converge.HealthInClusterCmd(), converge.ConvergeCmd(),
		assertplatform.AplVersionCmd(),
		// BREAK-GLASS: bao-init / bao-regen-root are manual handles for a wedged
		// bao-ensure-ready (still callerless). bao-status + bao-breakglass ARE now
		// invoked — by the operator-dispatched llz-breakglass-openbao.yml workflow.
		baoread.BaoStatusCmd(),
		openbaoext.BaoInitCmd(), openbaoext.BaoRegenRootCmd(), identityconfig.BaoConfigureCmd(), openbaoext.BaoEnsureReadyCmd(),
		openbaoext.BaoBreakglassCmd(),
		openbaoext.ExtractOpenbaoCACmd(), converge.NudgeArgoCmd(), openbaoext.ProvisionPeerCACmd(),
		// keycloak-configure IS workflow-driven (bootstrap-openbao + scheduled-checks
		// ensure the device-flow client); team-login-smoke stays a manual operator check.
		identityconfig.KeycloakConfigureCmd(),
		assertidentity.TeamLoginSmokeCmd())
	// Cluster readiness gates (assert-loki-bootstrapped.sh / wait-for-harbor.sh).
	c.AddCommand(assertobs.AssertLokiCmd(), assertobs.WaitHarborCmd(), assertobs.HarborTrustObjProxyCACmd(), teardown.DrainObjBucketsCmd(), assertplatform.HealthWorkflowCmd(), tokeninv.ValidateTokensCmd())
	// Generic wait primitives (formerly inline kubectl polling loops in the
	// bootstrap / rotation workflows).
	c.AddCommand(converge.WaitPodsCmd(), converge.WaitClusterReadyCmd())
	// Fail-fast gate ahead of wait-pods: a missing Argo Application means the
	// platform-bootstrap sync is wedged waves earlier — fail in ~4 min WITH the
	// operationState message instead of burning the 600s pod wait blind (PR #142).
	c.AddCommand(assertplatform.ArgoAppCmd())
	// Destroy-path teardown sweeps (formerly inline curl+jq in llz-terraform.yml).
	c.AddCommand(ciTeardownCaptureCmd(), ciTeardownForceDeleteCmd(), ciTeardownDeleteVPCCmd(), ciAssertNoOrphansCmd())
	// Rotation routing + the in-cluster narrow-PAT rotation (formerly inline in
	// llz-secret-rotation.yml; rotate-incluster-pat replaced propagate-pat —
	// the broad PAT is CI/Terraform-only and no longer pushed into clusters).
	c.AddCommand(tokeninv.RotationPlanCmd(), credrotate.RotateInclusterPATCmd())
	// Harbor API steps (formerly inline curl in llz-bootstrap-openbao.yml).
	// Harbor: the active-path provisioning (project + robots + OpenBao seed +
	// repo-secret publication + smoke) runs IN-CLUSTER via harbor-provisioner
	// (the harbor-robot-provisioner CronJob); seed-standby-harbor-robots is the
	// CI-side standby half. port-forward/ensure-project/smoke were retired with
	// the workflow's harbor job.
	// kick-harbor-provisioner force-ticks that CronJob at bootstrap so the
	// converge tail is event-paced instead of waiting out the */5 schedule.
	c.AddCommand(harbor.HarborProvisionerCmd(), harbor.SeedStandbyHarborRobotsCmd(), assertsecrets.KickHarborProvisionerCmd())
	// Pre-flight guards (require-secret.sh / assert-destroy-confirm.sh).
	c.AddCommand(ciRequireSecretCmd(), ciAssertDestroyConfirmCmd(), ciBuildFailureSummaryCmd())
	// Bootstrap seeding (bootstrap-cloud-firewall.sh / provision-harbor-robots.sh).
	// (gen-bootstrap-tls was retired: the OTel collector serving cert is now issued
	// by the otel-bootstrap-ca cert-manager chain in the observability component.)
	// bootstrap-cloud-firewall is the manual/recovery fallback; the cidrFirewall
	// component's CronJob (discover-firewall-config) is the steady-state owner.
	c.AddCommand(firewall.Cmd(), reconciler.DiscoverFirewallCmd())
	// Cluster access plumbing (lke-runner-acl action / fetch-kubeconfig action).
	// fetch-kubeconfig (the Linode-API variant) is BREAK-GLASS and deliberately
	// callerless: the workflows use fetch-kubeconfig-state, which reads the TF
	// state output. The API variant exists precisely because it does NOT need
	// terraform init, the S3 backend, or git auth — the things most likely to be
	// broken when an operator needs a kubeconfig by hand.
	c.AddCommand(clusteraccess.RunnerACLCmd(), clusteraccess.FetchKubeconfigCmd(), clusteraccess.FetchKubeconfigStateCmd())
	// ── BREAK-GLASS VERBS ────────────────────────────────────────────────────
	// bao-init, bao-regen-root, fetch-kubeconfig (the API variant) and openbao-login
	// have ZERO workflow callers on purpose — the manual handles an operator reaches
	// for during an incident. (bao-status + bao-breakglass ARE now dispatched by the
	// operator-triggered llz-breakglass-openbao.yml.) Documented in
	// docs/runbooks/bootstrap-openbao.md ("Break-glass handles").
	//
	// They therefore look identical to dead code to any "0 callers" scan, and one
	// such audit proposed deleting ~500 LOC of them. Owner confirmed: intentional.
	// Confirm with the owner before retiring any verb in this group — unlike
	// gh-pat-expiry/cred-audit below, which were genuinely superseded.

	// (gh-pat-expiry + cred-audit were registered here. They were the per-provider
	// expiry probes, superseded by the credential single-pane flow below, and were
	// retired once they had zero callers. Their measurement primitives live on in
	// credentials_probe.go.)
	// Credential single-pane-of-glass writer: measure CI-token expiry and emit the
	// ConfigMap the in-cluster reconciler re-exposes as metrics (llz-scheduled-checks.yml).
	c.AddCommand(tokeninv.TokenInventoryCmd())
	// Mutation testing that validates its own harness before reporting a score
	// (every gremlins failure mode so far surfaced as a flattering 100%).
	c.AddCommand(mutate.MutateCmd())
	// Scheduled rotation-SLA + cluster-readiness checks (llz-scheduled-checks.yml).
	c.AddCommand(healthsla.HealthLKEAdminRotationCmd(), healthsla.HealthLokiObjkeyRotationCmd(),
		healthsla.HealthOpenbaoCmd(), healthsla.HealthCertManagerCmd(), assertobs.HealthPromRulesCmd())
	// Apply-time failure diagnostics (llz-terraform.yml). (The former
	// stash-env-secret / ensure-env-secret siblings were retired with the S3-stash
	// hop and the loki-admin-password step — see docs/designs/linode-credential-rotator.md
	// + apl-core-v6-migration.md — so their commands are gone too.)
	c.AddCommand(argodiag.DiagnoseArgoCDCmd())
	// E2E timing instrumentation (docs/designs/e2e-instrumentation.md): a phase
	// timeline (phase-mark/phase-report → step summary + JSON artifact) and the
	// image-pull collector that answers whether a bring-up phase is pull-bound.
	c.AddCommand(phasetiming.PhaseMarkCmd(), phasetiming.PhaseReportCmd(), phasetiming.CollectImagePullsCmd(), phasetiming.CollectTimingCmd())
	// Release-e2e instantiate: pin the instance's TF_IMAGE/KUBE_IMAGE to this
	// commit's ci images so the baked llz can't drift from the rendered workflow.
	c.AddCommand(releasepublish.PinInstanceImagesCmd())
	// OpenBao KV seed steps (formerly ~15 inline-bash blocks in
	// llz-bootstrap-openbao.yml): the generic bao-seed plus the derive-their-
	// material specials in ci_bao_seed.go / ci_bao_seed_seal_key.go /
	// ci_seed_special.go.
	c.AddCommand(openbaoext.BaoSeedCmd(), openbaoext.BaoSeedAllCmd(), openbaoext.BaoSeedSealKeyCmd(),
		seedspecial.ResolveHarborURLCmd(), seedspecial.AuditPVCStorageClassCmd(),
		// Must run BEFORE the OpenBao pods are waited on: it patches the
		// StatefulSet, so pinning it late would roll a freshly unsealed cluster.
		identityconfig.PinKeycloakGatewayAliasCmd())
	// Object-storage key lifecycle, one owner end to end: mint-bootstrap-objkeys
	// mints the FIRST Loki/Harbor keys at bootstrap and seeds OpenBao (replacing
	// the TF-minted keys + LOKI_S3_*/HARBOR_REGISTRY_S3_* GitHub relay +
	// seed-harbor-registry-s3); the in-cluster rotator (linodeCredRotator
	// CronJob, slim llz image) owns rotation after first boot.
	c.AddCommand(credrotate.MintBootstrapObjkeysCmd(), credrotate.RotateLinodeCredsCmd(), credrotate.TempObjkeyCmd())
	// The databases root's OpenBao half: copy each Managed Postgres cluster's admin
	// connection from TF state to secret/infra/db-admin/<name>. Unlike the
	// object-storage keys above there is nothing to MINT — the credential is the
	// provider's — so this is a copy, and re-running it heals a stale one.
	// …and its rotation half. Unlike every other rotator here this one cannot
	// mint-verify-swap — Linode offers only an in-place credential RESET on the
	// fixed `akmadmin` user — so it is --apply-gated and refreshes TF state after,
	// or seed-db-admin would reconcile the rotation away. See ci_rotate_dbadmin.go.
	c.AddCommand(database.SeedDBAdminCmd(), database.DBDeclaredCmd(), database.DBSummaryCmd(), database.RotateDBAdminCmd())
	// State-encryption key rollover (ADR 0007 (state encryption) / ADR 0009). Dispatch-only.
	c.AddCommand(statepassphrase.RotateStatePassphraseCmd())
	// In-cluster rotation of the broad account:read_write Linode PAT (LINODE_API_TOKEN):
	// mint -> seed OpenBao -> publish to each deployment's GitHub env secret (sealed box)
	// -> revoke old. Runs in a dedicated CronJob, not the reconciler.
	c.AddCommand(credrotate.RotateBroadPATCmd())
	// Bootstrap seed for the broad-PAT rotator's minting credential — gated on the
	// component being enabled (the account-wide broad PAT lands in exactly one cluster).
	c.AddCommand(openbaoext.SeedBroadPATCmd())
	c.AddCommand(objenc.SeedSSECKeyCmd())
	c.AddCommand(objenc.AssertObjEncryptionCmd())
	// e2e: force one rotation Job from the CronJob + assert it rotated end-to-end.
	c.AddCommand(assertsecrets.BroadPATRotationCmd())
	// e2e: prove the operator escape hatch works end to end — the release-e2e seed
	// drops a trivial manifest under kubernetes-custom/namespaces/<ns>/, and this
	// asserts the instance-custom ApplicationSet generated instance-custom-<ns> and
	// it reached Synced+Healthy (a silently-empty hatch leaves platform apps color.Green).
	c.AddCommand(assertplatform.InstanceCustomCmd())
	// Linode Volume relabeler — the Go port of the linode-volume-labeler
	// relabel.sh CronJob (also runnable in-cluster by the volume-labels reconciler).
	c.AddCommand(reconciler.RelabelVolumesCmd())
	// Linode Volume tag-heal backstop for Volumes born without the StorageClass's
	// lke<id> ownership tag (also runnable in-cluster by the volume-tags reconciler).
	c.AddCommand(reconciler.ReconcileVolumeTagsCmd())
	// The e2e GATE for the same invariant the two lanes above maintain: asserts
	// against the Linode API that every PV-backed Volume is encrypted at rest AND
	// carries its lke<id> ownership tag. Fails closed, including on an empty cluster.
	c.AddCommand(reconciler.AssertVolumeEncryptionCmd())
	// The narrow in-cluster PAT, same one-owner shape: mint-bootstrap-pat seeds
	// the first token at bootstrap; rotate-incluster-pat (registered with the
	// rotation commands above) re-mints it monthly.
	c.AddCommand(credrotate.MintBootstrapPATCmd())
	// Secretless day-2 auth: exchange a GitHub OIDC token for an OpenBao token
	// (jwt auth) over a direct API call, for in-cluster runners — the primitive
	// behind the cross-org thin-caller pattern (docs/designs/cross-org-reuse-pattern.md).
	// BREAK-GLASS / forward-looking: deliberately callerless today.
	c.AddCommand(openbaoext.OpenBaoLoginCmd())
	// Copier render-time slimming: prune docs/ to the operator set + reference
	// the rest at the template repo. (The former strip-comments verb is gone:
	// the vendored llz-*.yml bodies ship verbatim so an instance copy matches
	// the copier render — see copier.yml's _tasks note.)
	c.AddCommand(deliverdocs.DeliverDocsCmd())
	// Doc rot, mechanically: llz commands/flags against the live cobra tree,
	// `gh workflow run` inputs against the workflow YAML, and links resolved BOTH
	// in the template and in the post-deliver-docs keep-set. Added after an audit
	// found 30 doc defects, most of them detectable from the repo itself.
	c.AddCommand(docsguard.DocsGuardCmd())
	c.AddCommand(ciGenTOCCmd())
	// Day-2 gate: scaffold at the previous release and `copier update` to HEAD.
	// instance-test.sh covers `copier copy` and stops there, so the upgrade path —
	// same answers, same _tasks, plus a 3-way merge — was run by nobody.
	c.AddCommand(upgrade.UpgradeTestCmd())
	// Repo-scan gate (former template-scripts python: validate-externalsecret-paths.py
	// via the Makefile).
	c.AddCommand(credcoverage.ExternalSecretPathsCmd())
	// Static guard for the PR #142 wedge class: negative-sync-wave kinds that
	// could health-wedge the platform-bootstrap sync (Makefile wave-health-guard).
	c.AddCommand(wavehealth.HealthGuardCmd())
	c.AddCommand(mtlsguard.Cmd())
	c.AddCommand(plaintext.PlaintextGuardCmd())
	// Static guard on credential-OBSERVABILITY drift: a `secrets.NAME` an instance
	// workflow consumes must be measured by one of the single-pane feeds or
	// registered as a reasoned exemption (Makefile credential-coverage-guard).
	c.AddCommand(credcoverage.CoverageGuardCmd())
	// Static guard on ENCRYPTION AT REST for Terraform-declared resources: every
	// root declares an encryption block, every node pool sets disk_encryption
	// (Makefile at-rest-guard).
	c.AddCommand(atrest.AtRestGuardCmd())
	// Static guard for the #163 wedge class: a workload that hard-depends on a
	// Secret produced by a LATER-wave ExternalSecret can never go Healthy and
	// wedges the sync (Makefile wave-dependency-guard).
	c.AddCommand(wavehealth.DependencyGuardCmd())
	// Live fault-injection game-day: break one platform ExternalSecret and assert
	// the wedge is contained to its own carved Application (blast-radius
	// decomposition proof). Run on a warm e2e cluster.
	c.AddCommand(gameday.WedgeGamedayCmd())
	// Runtime counterpart to wave-health-guard: assert the VAP is bound + enforcing
	// (negative canary), which is what makes the static guard's verdict hold live.
	c.AddCommand(assertnetwork.WaveHealthVAPCmd())
	// Cluster diagnostic: list in-cluster Prometheus metric names matching a regex
	// (metric-name discovery for writing error-rate/saturation alerts).
	c.AddCommand(assertobs.PromMetricsCmd())
	// Cluster diagnostic: evaluate deployed PrometheusRule alert exprs against the
	// live Prometheus (catch never-fire / false-positive rules promtool can't).
	c.AddCommand(assertobs.AlertEvalCmd())
	// E2E gate: assert every landing-zone ServiceMonitor has an `up` scrape target
	// and every PrometheusRule group is loaded — the observability-pipeline wiring
	// converge/health/assert-loki all stay color.Green on when a label/port/selector
	// regression silently un-scrapes/un-loads it.
	c.AddCommand(assertobs.ScrapeTargetsCmd())
	// E2E gate: assert the reconciler is FUNCTIONALLY healthy (llz_reconcile_up=1 +
	// llz_reconcile_leader=1) — the silently-broken-loop class (pod Running yet
	// failing on dropped RBAC/OpenBao access) that converge and alert-eval --strict
	// both miss.
	c.AddCommand(assertreconciler.ReconcilerCmd())
	// E2E gate: what the reconciler lanes DO, not that they are running. A lane can
	// report a successful pass every cycle while its effect on the cluster is absent
	// (reconciled onto the wrong object, reverted by Argo, computed from empty
	// input) — assert-reconciler reads the lane's self-report and cannot see that.
	c.AddCommand(assertreconciler.EffectsCmd())
	// ── Tier-2 delivery gates: does the data actually ARRIVE? ────────────────
	// Each covers a pipeline whose failure mode is silence — the producer stays
	// Running, the sink stays Ready, converge stays color.Green, and the thing simply
	// never gets there. assert-openbao-audit proved one such round trip; these are
	// the rest of them.
	//
	// log path (the cluster-wide collector, NOT the OpenBao promtail sidecar).
	c.AddCommand(assertobs.LogIngestionCmd())
	// secret path: ESO is still RE-READING OpenBao, not just holding a Secret it
	// materialized once and can no longer refresh.
	c.AddCommand(assertsecrets.ESORoundTripCmd())
	// alert path: a firing alert has somewhere to go. Prometheus treats "firing
	// with no receivers" as normal, so this is invisible everywhere else.
	c.AddCommand(assertobs.AlertDeliveryCmd())
	// dashboard path: the sidecar is a label selector, so a dropped label leaves a
	// valid ConfigMap holding a dashboard nobody can see.
	c.AddCommand(assertobs.GrafanaDashboardsCmd())
	// ── Tier-4 enforcement negatives ─────────────────────────────────────────
	// The runtime counterpart to the static policy manifests, in the same spirit as
	// assert-wave-health-vap: server-dry-run something each policy MUST act on and
	// require that policy's own response. `kubectl get clusterpolicy` proves the
	// YAML is present; it cannot tell an enforcing policy from a decorative one,
	// and both ship failurePolicy: Ignore, so a downed Kyverno admits silently.
	c.AddCommand(assertnetwork.AdmissionEnforcementCmd())
	// The two enforcement properties that CANNOT be dry-run: admission answers
	// from the API server, but a dropped packet is only knowable by sending one.
	// assert-network-enforcement opens real connections from a real pod (each
	// negative paired with a positive control, so a broken probe reports
	// INCONCLUSIVE rather than passing); net-probe is the dial it runs there.
	c.AddCommand(assertnetwork.NetworkEnforcementCmd(), assertnetwork.NetProbeCmd())
	// The e2e assert battery itself. Was ~40 lines of inline bash implementing a
	// parallel job runner in YAML — untestable, and with the lane list written
	// TWICE (once to run, once to collect) so a lane could run and never be able to
	// fail the step. One tested list now drives both, and it ships with the binary
	// rather than with each instance's vendored workflow.
	// ── Tier-3 credential-lifecycle gates ────────────────────────────────────
	// assert-rotation-health gates the age of every credential credpaths.CredPaths declares:
	// a declared credential publishing NO series is invisible on the single pane
	// AND unalertable, because a rule over an absent series never evaluates.
	// assert-harbor-roundtrip USES a minted robot rather than trusting it was
	// created — the truncation regression left every credential valid and every
	// push and pull 401ing on a malformed host.
	c.AddCommand(assertsecrets.RotationHealthCmd(), assertregistry.AssertHarborRoundTripCmd())
	// ── Delivery/health gates found in the post-review functional pass ───────
	// assert-obj-roundtrip WRITES to Loki's and Harbor's object storage at each
	// consumer's OWN endpoint with its OWN credential. verify-object-storage asks
	// the Linode API whether the buckets exist by label, and every one of those
	// checks passed while both consumers were returning NoSuchBucket — the
	// generations are disjoint namespaces, so the API and the consumer were both
	// telling the truth about different places.
	c.AddCommand(assertobjstore.AssertObjRoundTripCmd())
	// assert-certificates consumes a signal that already existed and nothing read:
	// llz_certificates_not_ready is published and alerted on, but alert-eval is
	// report-only and --strict ignores FIRING, so a stuck Certificate reds nothing.
	c.AddCommand(assertidentity.CertificatesCmd())
	// assert-database proves the seeded admin credential is still ACCEPTED.
	// rotate-db-admin resets the password in place with no overlap window, so the
	// failure is a live endpoint that rejects the credential every consumer holds.
	c.AddCommand(database.AssertDatabaseCmd())
	c.AddCommand(assertsuite.Cmd())
	// E2E gate: assert OpenBao's audit log is ARRIVING in Loki, by reading it back
	// out of Loki. The metrics path has assert-scrape-targets; the log path had
	// nothing, and shipped to a Service that never existed for its entire life —
	// with a NetworkPolicy egress allow pointed at the same empty namespace, so
	// both sides agreed with each other and neither agreed with the cluster. Only
	// the round trip can tell a correct URL from a plausible one.
	c.AddCommand(assertsecrets.OpenbaoAuditCmd())
	// Static guard for the harbor-reconciler mesh class: a NetworkPolicy egress to
	// a STRICT-mesh namespace (harbor) from outside it describes traffic Istio
	// silently drops (Makefile mesh-egress-guard).
	c.AddCommand(meshegress.Cmd())
	c.AddCommand(manifestguard.PlaceholderGuardCmd())
	// Static guard for the #175 day-2-blind class: every ServiceMonitor/PodMonitor/
	// PrometheusRule must carry `prometheus: system` or apl-core's Prometheus
	// silently ignores it (metrics unscraped / rules unloaded) — Makefile
	// monitoring-label-guard.
	c.AddCommand(monitoringlabel.Cmd())
	// No manifest may declare an apiVersion apl-core's bundled operators no longer
	// serve (it cannot apply — opaque Argo SyncFailed). Covers platform-apl/, which
	// the $RENDER_DIR-based dry-run never sees — Makefile dropped-apiversions-check.
	c.AddCommand(manifestguard.DroppedAPIVersionsCmd())
	// Offline apl-core schema validation (helm template) — the check
	// helm_release.apl runs at apply time, shifted left into scaffold-check.
	c.AddCommand(manifestguard.AplSchemaValidateCmd())
	// PrometheusRule promtool gate (former template-scripts python:
	// check-prometheus-rule-crds.py via the Makefile's prom-rules-check) — the
	// last first-party Python script in the repo.
	c.AddCommand(assertobs.CheckPromRulesCmd())
	// Render/coverage lint gates ported from template-scripts (the Makefile's
	// helm-dep-lock-check, argocd-rendered-apps-check, and the per-package
	// coverage floor in `make coverage`).
	c.AddCommand(chartguard.ChartLockDriftCmd(), manifestguard.ArgoCDRenderedAppsCmd(), coverageguard.Cmd())
	// Design-principle gate: budget on inline-bash / shell / python logic that
	// should instead live in unit-tested Go (lint.yml). Ratchets DOWN over time.
	c.AddCommand(budget.UntestableLOCCmd())
	// Its counterweight (ADR 0014): untestable-loc names tools/cmd/llz as the
	// destination for converted logic but no capacity, so package main accretes.
	// This budgets the destination. Ratchets DOWN as code moves to
	// tools/internal/<pkg> or out to an extension.
	c.AddCommand(budget.CoreSurfaceCmd())
	// Release-hygiene gate: a chart change must bump its Chart.yaml version, or
	// publish-charts.yml never publishes it and clusters keep the stale artifact.
	c.AddCommand(chartguard.ChartVersionGuardCmd())
	// Companion gate: every Argo CD chart pin (apl-values targetRevision +
	// llz-argo-bootstrap-apps component version) must match the chart's local
	// Chart.yaml version, or Argo pulls a tag the registry never received and the
	// support-plane app silently never syncs (llz-openbao namespace never created).
	c.AddCommand(chartguard.ChartPinGuardCmd())
	c.AddCommand(cosignguard.Cmd())
	// Runtime companion: a pinned first-party chart version must actually EXIST in
	// the OCI registry, or Argo 404s the pull on a feature-branch e2e (bumped-but-
	// unpublished chart) and the OpenBao bootstrap dies on the missing llz-openbao ns.
	c.AddCommand(chartpublish.ChartPublishCheckCmd())
	// Package + push + keyless-sign first-party charts to GHCR (immutable; re-signs a
	// pushed-but-unsigned version). Replaces publish-charts.yml's inline bash.
	c.AddCommand(releasepublish.PublishChartsCmd())
	// Cluster-bootstrap native command + its former local-exec bodies. bootstrap-
	// cluster is the whole in-cluster bootstrap (apl-core install + Argo bridge +
	// the race-ahead Kyverno policies) that used to be the cluster-bootstrap
	// Terraform workspace; wait-apl-pipeline + apply-kyverno-policy remain
	// separately runnable (bootstrap-cluster calls them in-process), and
	// destroy-unwedge / clear-cluster-secrets are the destroy-path cleanups.
	c.AddCommand(bootstrapcluster.BootstrapClusterCmd(), converge.WaitAplPipelineCmd(), kyverno.ApplyKyvernoPolicyCmd(),
		ciDestroyUnwedgeCmd(), ghsecret.ClearClusterSecretsCmd())
	// apl-core 6.1.0's pre-upgrade prerequisite (the apl-operator sync-options
	// annotation). bootstrap-cluster runs it on every apply; it stays separately
	// runnable so an operator can assert it on a cluster ahead of a managed upgrade
	// without re-running a bootstrap.
	c.AddCommand(bootstrapcluster.PrepareAplUpgradeCmd())
	// Image/source skew guard: fail fast when the baked llz is older than the
	// workflow's template-ref (the independent TF_IMAGE vs template-ref pins drift).
	c.AddCommand(templatecommit.AssertImageFreshCmd())
	// Release gate for the shape e2e structurally cannot produce: an instance pinned
	// at a release TAG (every e2e run pins a sha) whose images come from `llz tokens`
	// (every e2e run uses pin-instance-images). That blind spot shipped a broken
	// first-run to a live adopter with e2e color.Green throughout.
	c.AddCommand(templatecommit.AssertAdopterPinCmd())
	// CI guard: a container job whose run-steps lack a bash default falls back to
	// dash and breaks `set -o pipefail` (the discover-workflow regression).
	c.AddCommand(workflowshells.Cmd())
	// Scaffold update-class manifest gate (former template-scripts/check-template-manifest.sh).
	c.AddCommand(templatemanifest.Cmd())
	// Vendored-CI drift guard: the `managed` .github/ surface is overwritten by
	// `llz upgrade`, so a local edit is silently lost — fail CI instead.
	c.AddCommand(ciManagedFreshCmd())
	// Tool-version pin agreement: the Dockerfile ARG block is the authority and
	// every restatement (build matrix, container fallbacks, Go constants) must
	// match it. This drifted once already — see ci_version_pins.go.
	c.AddCommand(versionpins.Cmd())
	return c
}

// ── orphan-resource sweeps (ports of cleanup-orphan-{volumes,nodebalancers}.sh) ──
// Both reuse the orphan-identity heuristics + list/delete primitives in
// internal/linode (the same ones `llz reap` drives); this is just the
// CI-scoped orchestration. Dry-run by default; deletes only with --yes.
