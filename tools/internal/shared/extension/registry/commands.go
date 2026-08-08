package registry

// commands.go — which cobra constructor belongs to which extension.
//
// IT EXISTS FOR ONE CHECK: every command an extension exposes must be reachable
// in the tree package main builds. Nothing caught that before. An extension could
// be declared, validate, appear in `llz extension list`, and still have a verb
// nobody could run, because wiring it is a hand-written AddCommand in ci.go or
// main.go and forgetting one is silent.
//
// CONSTRUCTORS, NOT NAMES, and that is the whole design. The obvious shape was a
// `Commands []string` field on Extension holding verb names. Authoring it meant
// 214 hand-typed entries, twenty of them ambiguous because verb names are NOT
// unique across groups -- `add`, `set`, `list` and `validate` each appear under
// two parents -- and every entry would duplicate what the tree already says, with
// only a test to catch the drift. A function reference costs one line, the
// COMPILER checks it, and renaming a constructor breaks the build rather than a
// test three weeks later.
//
// This file imports cobra. internal/shared/extension -- the model -- does NOT and
// must not: it is pure data that every extension depends on. The registry already
// imports all of them, so the cost here is zero new coupling.
//
// WHAT IT DELIBERATELY DOES NOT DO is build the tree. A registry-driven tree
// needs group-placement metadata on every extension, and ~26% of the wiring is
// package main's regardless -- the group constructors themselves, plus the
// deps-ABI, composition and guard-exception commands. Verification needs none of
// that, and gets the property that was actually missing.

import (
	"github.com/spf13/cobra"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/argodiag"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertidentity"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertnetwork"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertobjstore"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertobs"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertplatform"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertreconciler"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertregistry"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertsecrets"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/atrest"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/baoca"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/baolifecycle"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/baoseed"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/bootstrapcluster"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/budget"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/chartguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/chartpublish"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/clusteraccess"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/configreadiness"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/converge"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/credcoverage"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/credrotate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/database"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/deliverdocs"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/docsguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/environments"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/gameday"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/harbor"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/healthsla"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/identityconfig"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/kyverno"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lint"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/manifestguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/mutate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/objenc"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/onboard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/openbao"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/phasetiming"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/plaintext"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/reachability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/reconciler"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/releasepublish"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/render"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/seedspecial"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/selfupgrade"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/statepassphrase"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/teardown"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/templatecommit"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/tofudriver"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/tokeninv"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/upgrade"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/wavehealth"
)

// Command pairs an extension name with one constructor it owns.
type Command struct {
	Extension string
	New       func() *cobra.Command
}

// Commands is every command the extensions expose. Order is irrelevant; the
// guard treats it as a set.
func Commands() []Command { return append([]Command(nil), commands...) }

var commands = []Command{
	{"argodiag", argodiag.DiagnoseArgoCDCmd},
	{"assertidentity", assertidentity.CertificatesCmd},
	{"assertidentity", assertidentity.TeamLoginSmokeCmd},
	{"assertnetwork", assertnetwork.AdmissionEnforcementCmd},
	{"assertnetwork", assertnetwork.NetProbeCmd},
	{"assertnetwork", assertnetwork.NetworkEnforcementCmd},
	{"assertnetwork", assertnetwork.WaveHealthVAPCmd},
	{"assertobjstore", assertobjstore.AssertObjRoundTripCmd},
	{"assertobjstore", assertobjstore.VerifyObjectStorageCmd},
	{"assertobs", assertobs.AlertDeliveryCmd},
	{"assertobs", assertobs.AlertEvalCmd},
	{"assertobs", assertobs.AssertLokiCmd},
	{"assertobs", assertobs.CheckPromRulesCmd},
	{"assertobs", assertobs.GrafanaDashboardsCmd},
	{"assertobs", assertobs.HarborTrustObjProxyCACmd},
	{"assertobs", assertobs.HealthPromRulesCmd},
	{"assertobs", assertobs.LogIngestionCmd},
	{"assertobs", assertobs.PromMetricsCmd},
	{"assertobs", assertobs.ScrapeTargetsCmd},
	{"assertobs", assertobs.WaitHarborCmd},
	{"assertplatform", assertplatform.AplVersionCmd},
	{"assertplatform", assertplatform.ArgoAppCmd},
	{"assertplatform", assertplatform.HealthWorkflowCmd},
	{"assertplatform", assertplatform.InstanceCustomCmd},
	{"assertreconciler", assertreconciler.EffectsCmd},
	{"assertreconciler", assertreconciler.ReconcilerCmd},
	{"assertregistry", assertregistry.AssertHarborRoundTripCmd},
	{"assertsecrets", assertsecrets.BroadPATRotationCmd},
	{"assertsecrets", assertsecrets.ESORoundTripCmd},
	{"assertsecrets", assertsecrets.KickHarborProvisionerCmd},
	{"assertsecrets", assertsecrets.OpenbaoAuditCmd},
	{"assertsecrets", assertsecrets.RotationHealthCmd},
	{"atrest", atrest.AtRestGuardCmd},
	{"baoca", baoca.ExtractOpenbaoCACmd},
	{"baoca", baoca.ProvisionPeerCACmd},
	{"baolifecycle", baolifecycle.BaoBreakglassCmd},
	{"baolifecycle", baolifecycle.BaoEnsureReadyCmd},
	{"baolifecycle", baolifecycle.BaoInitCmd},
	{"baolifecycle", baolifecycle.BaoRegenRootCmd},
	{"baolifecycle", baolifecycle.RegenRootCmd},
	{"baoseed", baoseed.BaoSeedAllCmd},
	{"baoseed", baoseed.BaoSeedCmd},
	{"baoseed", baoseed.BaoSeedSealKeyCmd},
	{"baoseed", baoseed.SeedBroadPATCmd},
	{"bootstrapcluster", bootstrapcluster.BootstrapClusterCmd},
	{"bootstrapcluster", bootstrapcluster.PrepareAplUpgradeCmd},
	{"budget", budget.CoreSurfaceCmd},
	{"budget", budget.UntestableLOCCmd},
	{"chartguard", chartguard.ChartLockDriftCmd},
	{"chartguard", chartguard.ChartPinGuardCmd},
	{"chartguard", chartguard.ChartVersionGuardCmd},
	{"chartpublish", chartpublish.ChartPublishCheckCmd},
	{"clusteraccess", clusteraccess.FetchKubeconfigCmd},
	{"clusteraccess", clusteraccess.FetchKubeconfigStateCmd},
	{"clusteraccess", clusteraccess.RunnerACLCmd},
	{"configreadiness", configreadiness.PreflightCmd},
	{"converge", converge.ConvergeCmd},
	{"converge", converge.HealthCmd},
	{"converge", converge.HealthInClusterCmd},
	{"converge", converge.NudgeArgoCmd},
	{"converge", converge.WaitAplPipelineCmd},
	{"converge", converge.WaitClusterReadyCmd},
	{"converge", converge.WaitPodsCmd},
	{"credcoverage", credcoverage.CoverageGuardCmd},
	{"credcoverage", credcoverage.ExternalSecretPathsCmd},
	{"credrotate", credrotate.CredentialsCmd},
	{"credrotate", credrotate.MintBootstrapObjkeysCmd},
	{"credrotate", credrotate.MintBootstrapPATCmd},
	{"credrotate", credrotate.RotateBroadPATCmd},
	{"credrotate", credrotate.RotateInclusterPATCmd},
	{"credrotate", credrotate.RotateLinodeCredsCmd},
	{"credrotate", credrotate.TempObjkeyCmd},
	{"database", database.AssertDatabaseCmd},
	{"database", database.DBDeclaredCmd},
	{"database", database.DBSummaryCmd},
	{"database", database.RotateDBAdminCmd},
	{"database", database.SeedDBAdminCmd},
	{"deliverdocs", deliverdocs.DeliverDocsCmd},
	{"docsguard", docsguard.DocsGuardCmd},
	{"environments", environments.EditCmd},
	{"environments", environments.ListCmd},
	{"environments", environments.NetworkCmd},
	{"environments", environments.PeerCmd},
	{"environments", environments.ResolveCmd},
	{"environments", environments.RoleCmd},
	{"environments", environments.SetCmd},
	{"environments", environments.SpecCmd},
	{"gameday", gameday.WedgeGamedayCmd},
	{"harbor", harbor.HarborProvisionerCmd},
	{"harbor", harbor.SeedStandbyHarborRobotsCmd},
	{"healthsla", healthsla.HealthCertManagerCmd},
	{"healthsla", healthsla.HealthLKEAdminRotationCmd},
	{"healthsla", healthsla.HealthLokiObjkeyRotationCmd},
	{"healthsla", healthsla.HealthOpenbaoCmd},
	{"identityconfig", identityconfig.AplUserCmd},
	{"identityconfig", identityconfig.BaoConfigureCmd},
	{"identityconfig", identityconfig.KeycloakConfigureCmd},
	{"identityconfig", identityconfig.PinKeycloakGatewayAliasCmd},
	{"identityconfig", identityconfig.UsersAddCmd},
	{"kyverno", kyverno.ApplyKyvernoPolicyCmd},
	{"lint", lint.CheckCmd},
	{"lint", lint.FmtCmd},
	{"lint", lint.HooksCmd},
	{"lint", lint.LintCmd},
	{"lint", lint.PrecommitCmd},
	{"lint", lint.ValidateCmd},
	{"manifestguard", manifestguard.AplSchemaValidateCmd},
	{"manifestguard", manifestguard.ArgoCDRenderedAppsCmd},
	{"manifestguard", manifestguard.DroppedAPIVersionsCmd},
	{"manifestguard", manifestguard.PlaceholderGuardCmd},
	{"mutate", mutate.MutateCmd},
	{"objenc", objenc.AssertObjEncryptionCmd},
	{"objenc", objenc.ObjProxyCmd},
	{"objenc", objenc.SeedSSECKeyCmd},
	{"onboard", onboard.DoctorCmd},
	{"onboard", onboard.SecretsCmd},
	{"onboard", onboard.TokensCmd},
	{"openbao", openbao.OpenBaoLoginCmd},
	{"openbao", openbao.OpenbaoCmd},
	{"openbao", openbao.OpenbaoLoginCmd},
	{"phasetiming", phasetiming.CollectImagePullsCmd},
	{"phasetiming", phasetiming.CollectTimingCmd},
	{"phasetiming", phasetiming.PhaseMarkCmd},
	{"phasetiming", phasetiming.PhaseReportCmd},
	{"plaintext", plaintext.PlaintextGuardCmd},
	{"reachability", reachability.VerifyCmd},
	{"reconciler", reconciler.AssertVolumeEncryptionCmd},
	{"reconciler", reconciler.DiscoverFirewallCmd},
	{"reconciler", reconciler.ReconcileVolumeTagsCmd},
	{"reconciler", reconciler.RelabelVolumesCmd},
	{"releasepublish", releasepublish.PinInstanceImagesCmd},
	{"releasepublish", releasepublish.PublishChartsCmd},
	{"render", render.EnvVPCCmd},
	{"render", render.RenderCmd},
	{"seedspecial", seedspecial.AuditPVCStorageClassCmd},
	{"seedspecial", seedspecial.ResolveHarborURLCmd},
	{"selfupgrade", selfupgrade.SelfUpdateCmd},
	{"statepassphrase", statepassphrase.RotateStatePassphraseCmd},
	{"teardown", teardown.DrainObjBucketsCmd},
	{"teardown", teardown.ReapCmd},
	{"teardown", teardown.ReapNodeBalancersCmd},
	{"teardown", teardown.ReapObjKeysCmd},
	{"teardown", teardown.ReapVolumesCmd},
	{"templatecommit", templatecommit.AssertAdopterPinCmd},
	{"templatecommit", templatecommit.AssertImageFreshCmd},
	{"tofudriver", tofudriver.DestroyCmd},
	{"tofudriver", tofudriver.OutputCmd},
	{"tofudriver", tofudriver.PlanCmd},
	{"tofudriver", tofudriver.TFApplyCmd},
	{"tofudriver", tofudriver.TFImportCmd},
	{"tokeninv", tokeninv.RotationPlanCmd},
	{"tokeninv", tokeninv.TokenInventoryCmd},
	{"tokeninv", tokeninv.ValidateTokensCmd},
	{"upgrade", upgrade.UpgradeCmd},
	{"upgrade", upgrade.UpgradeTestCmd},
	{"wavehealth", wavehealth.DependencyGuardCmd},
	{"wavehealth", wavehealth.HealthGuardCmd},
}
