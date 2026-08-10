package registry

// commands.go — which cobra constructor belongs to which extension.
//
// IT EXISTS FOR ONE CHECK: every command an extension exposes must be reachable
// in the tree internal/cli builds. Nothing caught that before. An extension could
// be declared, validate, appear in `llz extension list`, and still have a verb
// nobody could run, because wiring it is a hand-written AddCommand in that package
// and forgetting one is silent.
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

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/assertidentity"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/assertnetwork"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/assertobs"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/assertplatform"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/assertreconciler"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/assertregistry"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/assertsecrets"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/assertsuite"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/configreadiness"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/manifestguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/reachability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/seedspecial"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/templatecommit"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/tokeninv"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/budget"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/callerperms"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/chartguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/cosignguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/coverageguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/credcoverage"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/docsguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/meshegress"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/monitoringlabel"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/mtlsguard"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/pincoherence"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/plaintext"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/setupgosite"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/sourceref"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/templatemanifest"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/versionpins"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/wavehealth"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/guards/workflowshells"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/assertobjstore"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/atrest"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/bootstrapcluster"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/chartpublish"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/clusteraccess"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/converge"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/credrotate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/database"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/deliverdocs"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/environments"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/firewall"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/gameday"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/harbor"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/healthsla"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/identityconfig"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/kyverno"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/objenc"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/openbao"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/reconciler"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/releasepublish"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/render"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/statepassphrase"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/teardown"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/lifecycle/tofudriver"
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
	{"assertsuite", assertsuite.Cmd},
	{"atrest", atrest.AtRestGuardCmd},
	{"bootstrapcluster", bootstrapcluster.BootstrapClusterCmd},
	{"bootstrapcluster", bootstrapcluster.PrepareAplUpgradeCmd},
	{"budget", budget.CoreSurfaceCmd},
	{"budget", budget.UntestableLOCCmd},
	{"chartguard", chartguard.ChartLockDriftCmd},
	{"chartguard", chartguard.ChartPinGuardCmd},
	{"chartguard", chartguard.ChartVersionGuardCmd},
	{"cosignguard", cosignguard.Cmd},
	{"coverageguard", coverageguard.Cmd},
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
	{"firewall", firewall.Cmd},
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
	{"manifestguard", manifestguard.AplSchemaValidateCmd},
	{"manifestguard", manifestguard.ArgoCDRenderedAppsCmd},
	{"manifestguard", manifestguard.DroppedAPIVersionsCmd},
	{"manifestguard", manifestguard.PlaceholderGuardCmd},
	{"meshegress", meshegress.Cmd},
	{"monitoringlabel", monitoringlabel.Cmd},
	{"mtlsguard", mtlsguard.Cmd},
	{"objenc", objenc.AssertObjEncryptionCmd},
	{"objenc", objenc.ObjProxyCmd},
	{"objenc", objenc.SeedSSECKeyCmd},
	{"openbao", openbao.BaoBreakglassCmd},
	{"openbao", openbao.BaoEnsureReadyCmd},
	{"openbao", openbao.BaoInitCmd},
	{"openbao", openbao.BaoRegenRootCmd},
	{"openbao", openbao.BaoSeedAllCmd},
	{"openbao", openbao.BaoSeedCmd},
	{"openbao", openbao.BaoSeedSealKeyCmd},
	{"openbao", openbao.ExtractOpenbaoCACmd},
	{"openbao", openbao.OpenBaoLoginCmd},
	{"openbao", openbao.OpenbaoCmd},
	{"openbao", openbao.OpenbaoLoginCmd},
	{"openbao", openbao.ProvisionPeerCACmd},
	{"openbao", openbao.RegenRootCmd},
	{"openbao", openbao.SeedBroadPATCmd},
	{"plaintext", plaintext.PlaintextGuardCmd},
	{"pincoherence", pincoherence.Cmd},
	{"reachability", reachability.VerifyCmd},
	{"reconciler", reconciler.AssertVolumeEncryptionCmd},
	{"reconciler", reconciler.DiscoverFirewallCmd},
	{"reconciler", reconciler.ReconcileVolumeTagsCmd},
	{"reconciler", reconciler.RelabelVolumesCmd},
	{"reconciler", reconciler.Cmd},
	{"releasepublish", releasepublish.PinInstanceImagesCmd},
	{"releasepublish", releasepublish.PublishChartsCmd},
	{"render", render.EnvVPCCmd},
	{"render", render.RenderCmd},
	{"seedspecial", seedspecial.AuditPVCStorageClassCmd},
	{"seedspecial", seedspecial.ResolveHarborURLCmd},
	{"statepassphrase", statepassphrase.RotateStatePassphraseCmd},
	{"teardown", teardown.DrainObjBucketsCmd},
	{"teardown", teardown.ReapCmd},
	{"teardown", teardown.ReapNodeBalancersCmd},
	{"teardown", teardown.ReapObjKeysCmd},
	{"teardown", teardown.ReapVolumesCmd},
	{"templatemanifest", templatemanifest.Cmd},
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
	{"versionpins", versionpins.Cmd},
	{"setupgosite", setupgosite.Cmd},
	{"callerperms", callerperms.Cmd},
	{"sourceref", sourceref.Cmd},
	{"sourceref", sourceref.SymbolsCmd},
	{"workflowshells", workflowshells.Cmd},
	{"wavehealth", wavehealth.DependencyGuardCmd},
	{"wavehealth", wavehealth.HealthGuardCmd},
}
