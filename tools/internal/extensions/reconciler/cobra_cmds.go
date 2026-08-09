package reconciler

// cobra_cmds.go — the five cobra constructors, AND THEY STAYED HERE.
//
// Every previous extraction in this campaign sent flag sets back to package main
// on the rule that a flag set is not a capability. This one does not, and the
// reason is `reconcileOpts`: TWENTY-FIVE FIELDS, of which twenty are a
// `--reconcile-<lane>` bool paired with its resync interval. That struct is not
// incidental wiring around a capability — it IS the runtime's configuration
// surface, one field per lane the manager can be asked to run.
//
// Sending the constructor to main would mean exporting all twenty-five fields so
// main could set them, which trades a private struct for a public one and buys
// nothing: main would still know every lane by name. The rule's purpose is to keep
// package main from holding capability logic, and a `c.Flags().BoolVar` for a lane
// this package defines is not main's business either.
//
// The four `llz ci` constructors follow the same lane home for consistency: their
// verbs (discover-firewall, relabel-volumes, reconcile-volume-tags,
// assert-volume-encryption) are the one-shot forms of reconcilers this package
// runs continuously, and the two spellings should not drift apart.

import (
	"context"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/reconcilelanes"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/volumes"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kube"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/platform"
	"github.com/spf13/cobra"
)

// The file this was split from held NOTHING ELSE, so its header came with the
// commands rather than being left behind on an empty file.

func DiscoverFirewallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "discover-firewall-config",
		Short: "self-discover the firewall-controller config from this pod's node and reconcile the ConfigMap",
		Long: "In-cluster replacement for the bootstrap-cloud-firewall CI seed. Resolves the\n" +
			"node-pool firewall ID, LKE cluster ID and VPC subnet CIDR from this pod's own\n" +
			"node via the Linode API (providerID → instance → attached firewall / VPC\n" +
			"interface), reconciles them into the " + platform.FirewallConfigMapName + " ConfigMap,\n" +
			"and rolls the controller Deployment only when a value changed.\n\n" +
			"Env: NODE_NAME (downward API), LINODE_TOKEN (ESO-synced rotating token).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runCIDiscoverFirewallConfig(context.Background())
		},
	}
}
func Cmd() *cobra.Command {
	var o reconcileFlags
	c := &cobra.Command{
		Use:   "reconcile",
		Short: "run the in-cluster reconciler + Prometheus metrics surface",
		Long: "Long-lived in-cluster process that runs a set of reconcilers and serves a\n" +
			"Prometheus metrics surface at --metrics-addr/metrics. The observe reconciler\n" +
			"(always on) samples cluster convergence signals and drives nothing. The Phase 2\n" +
			"timed reconcilers are folded off their CronJobs and stay OFF by default — enable\n" +
			"one with its flag once it is ready to own the work per-env (it needs the same\n" +
			"env/secrets its CronJob had):\n" +
			"  --reconcile-argo-nudge     re-trigger terminally-failed Argo Applications, watch-\n" +
			"                             driven (was the argo-resync-nudger CronJob)\n" +
			"  --reconcile-cidr-firewall  reconcile the CIDR-firewall ConfigMap on Node change\n" +
			"                             (was the cidrFirewall CronJob; needs NODE_NAME,\n" +
			"                             LINODE_TOKEN)\n" +
			"  --reconcile-volume-labels  rename Linode Volumes for bound PVs, watch-driven (was\n" +
			"                             the linode-volume-labeler CronJob; needs REGION_SHORT,\n" +
			"                             LINODE_TOKEN)\n" +
			"  --reconcile-volume-tags    heal the block-storage-retain StorageClass's volumeTags\n" +
			"                             (incl. this cluster's lke<id> ownership tag) onto\n" +
			"                             Volumes born untagged, watch-driven (needs LINODE_TOKEN)\n" +
			"  --reconcile-sc-demote      keep LKE's Flux-promoted retain StorageClass non-default,\n" +
			"                             watch + resync floor (was the sc-default-patcher CronJob)\n" +
			"  --reconcile-linode-creds   rotate in-cluster Linode object-storage keys (was\n" +
			"                             the linodeCredRotator CronJob; needs REGION,\n" +
			"                             OBJ_CLUSTER, LINODE_TOKEN, OPENBAO_*)\n" +
			"  --reconcile-openbao-gauges seal + credential-age gauges (read-only; needs\n" +
			"                             OpenBao egress + the reconciler k8s-auth role)\n" +
			"  --reconcile-es-store-recovery force-sync ExternalSecrets/PushSecrets when\n" +
			"                             the openbao ClusterSecretStore goes Ready\n" +
			"                             (supersedes the CI nudge's force-sync half)\n" +
			"  --reconcile-apl-overlay    git-sync the apl-overlay (obj storage + app toggles)\n" +
			"                             onto apl-<env> with ff-retry (needs GH_REPO,\n" +
			"                             APL_VALUES_REPO_TOKEN, REGION, OpenBao)\n" +
			"Runs from the slim distroless image with an in-pod ServiceAccount. Terminates\n" +
			"gracefully on SIGTERM.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := kube.NewInCluster()
			if err != nil {
				return err
			}
			return runReconcile(cmd.Context(), client, reconcileOpts{
				metricsAddr:         o.metricsAddr,
				healthAddr:          o.healthAddr,
				metricsCertFile:     o.metricsCertFile,
				metricsKeyFile:      o.metricsKeyFile,
				metricsClientCAFile: o.metricsClientCAFile,
				sampleInterval:      time.Duration(o.sampleInterval) * time.Second,
				leaderElection:      o.leaderElection,
				reconcileArgoNudge:  o.reconcileArgoNudge,
				argoNudgeResync:     time.Duration(o.argoNudgeResync) * time.Second,
				reconcileCidrFW:     o.reconcileCidrFW,
				cidrFWResync:        time.Duration(o.cidrFWResync) * time.Second,
				reconcileVolLabels:  o.reconcileVolLabels,
				volLabelsResync:     time.Duration(o.volLabelsResync) * time.Second,
				reconcileVolTags:    o.reconcileVolTags,
				volTagsResync:       time.Duration(o.volTagsResync) * time.Second,
				reconcileLinodeCred: o.reconcileLinodeCred,
				linodeCredInterval:  time.Duration(o.linodeCredInterval) * time.Second,
				reconcileOpenBao:    o.reconcileOpenBao,
				openbaoInterval:     time.Duration(o.openbaoInterval) * time.Second,
				reconcileTokens:     o.reconcileTokens,
				tokensInterval:      time.Duration(o.tokensInterval) * time.Second,
				reconcileSCDemote:   o.reconcileSCDemote,
				scDemoteResync:      time.Duration(o.scDemoteResync) * time.Second,
				scDemoteName:        o.scDemoteName,
				reconcileESRecovery: o.reconcileESRecovery,
				esRecoveryResync:    time.Duration(o.esRecoveryResync) * time.Second,
				reconcileAplOverlay: o.reconcileAplOverlay,
				aplOverlayInterval:  time.Duration(o.aplOverlayInterval) * time.Second,
			})
		},
	}
	f := c.Flags()
	f.StringVar(&o.metricsAddr, "metrics-addr", ":8080", "address to serve the Prometheus /metrics endpoint on (mTLS when --metrics-tls-* are set)")
	// /healthz is on its OWN port because the kubelet probes it and cannot
	// present a client certificate; see the two-listener note in runReconcile.
	f.StringVar(&o.healthAddr, "health-addr", ":8081", "address to serve the plaintext /healthz probe endpoint on")
	f.StringVar(&o.metricsCertFile, "metrics-tls-cert-file", "", "serving certificate for /metrics (llz-serving-ca leaf); enables mTLS with the two flags below")
	f.StringVar(&o.metricsKeyFile, "metrics-tls-key-file", "", "private key for --metrics-tls-cert-file")
	f.StringVar(&o.metricsClientCAFile, "metrics-tls-client-ca-file", "", "CA bundle scrapers' client certificates must chain to (llz-client-ca)")
	f.IntVar(&o.sampleInterval, "sample-interval", 30, "seconds between observe-reconciler cluster samples")
	f.BoolVar(&o.leaderElection, "leader-election", true, "gate the driving reconcilers on a coordination.k8s.io Lease (single writer)")
	f.BoolVar(&o.reconcileArgoNudge, "reconcile-argo-nudge", false, "enable the argo-resync-nudger watch reconciler (default off; enabled in the llz-reconciler Deployment — the former CronJob is RETIRED, so this lane is the sole owner)")
	f.IntVar(&o.argoNudgeResync, "argo-nudge-resync", 300, "resync-floor seconds for the argo-nudge reconciler (watch drives the immediacy)")
	f.BoolVar(&o.reconcileCidrFW, "reconcile-cidr-firewall", false, "enable the CIDR-firewall discovery watch reconciler (default off; enabled in the llz-reconciler Deployment — the former CronJob is RETIRED, so this lane is the sole owner)")
	f.IntVar(&o.cidrFWResync, "cidr-firewall-resync", 600, "resync-floor seconds for the cidr-firewall reconciler (Node watch drives immediacy)")
	f.BoolVar(&o.reconcileVolLabels, "reconcile-volume-labels", false, "enable the Linode Volume relabeler watch reconciler (default off; enabled in the llz-reconciler Deployment — the former CronJob is RETIRED, so this lane is the sole owner)")
	f.IntVar(&o.volLabelsResync, "volume-labels-resync", 3600, "resync-floor seconds for the volume-labels reconciler (PV watch drives immediacy)")
	f.BoolVar(&o.reconcileVolTags, "reconcile-volume-tags", false, "enable the Linode Volume tag-heal watch reconciler (default off; the StorageClass tags at provision — this lane only heals Volumes born untagged)")
	f.IntVar(&o.volTagsResync, "volume-tags-resync", 3600, "resync-floor seconds for the volume-tags reconciler (PV watch drives immediacy)")
	f.BoolVar(&o.reconcileLinodeCred, "reconcile-linode-creds", false, "enable the Linode credential-rotation reconciler (default off; enabled in the llz-reconciler Deployment — the former CronJob is RETIRED, so this lane is the sole owner)")
	f.IntVar(&o.linodeCredInterval, "linode-creds-interval", 3600, "seconds between Linode credential-rotation resync passes")
	f.BoolVar(&o.reconcileOpenBao, "reconcile-openbao-gauges", false, "enable the OpenBao seal + credential-age gauges (read-only; needs OpenBao egress + the reconciler k8s-auth role)")
	f.IntVar(&o.openbaoInterval, "openbao-gauges-interval", 60, "seconds between OpenBao gauge samples")
	f.BoolVar(&o.reconcileTokens, "reconcile-token-inventory", false, "enable the CI-token expiry gauges (read-only; re-exposes the llz-token-inventory ConfigMap the token-inventory job writes)")
	f.IntVar(&o.tokensInterval, "token-inventory-interval", 60, "seconds between token-inventory ConfigMap samples")
	f.BoolVar(&o.reconcileSCDemote, "reconcile-sc-demote", false, "enable the StorageClass default-demote watch reconciler (default off; enabled in the llz-reconciler Deployment — the former CronJob is RETIRED, so this lane is the sole owner)")
	f.IntVar(&o.scDemoteResync, "sc-demote-resync", 120, "resync-floor seconds for the sc-demote reconciler (defeats the admission-policy starvation case)")
	f.StringVar(&o.scDemoteName, "sc-demote-name", reconcilelanes.DefaultDemoteSC, "the StorageClass to keep non-default (LKE's Flux-promoted retain class)")
	f.BoolVar(&o.reconcileESRecovery, "reconcile-es-store-recovery", false, "enable the ES store-recovery watch reconciler: force-sync ExternalSecrets/PushSecrets when the openbao ClusterSecretStore goes Ready (default off)")
	f.IntVar(&o.esRecoveryResync, "es-store-recovery-resync", 300, "resync-floor seconds for the es-store-recovery reconciler (the store watch drives immediacy)")
	f.BoolVar(&o.reconcileAplOverlay, "reconcile-apl-overlay", false, "enable the apl-overlay git-sync reconciler (obj storage + app toggles → apl-<env>, ff-retry; needs GH_REPO, APL_VALUES_REPO_TOKEN, REGION, OpenBao)")
	f.IntVar(&o.aplOverlayInterval, "apl-overlay-interval", 300, "seconds between apl-overlay git-sync passes")
	return c
}
func AssertVolumeEncryptionCmd() *cobra.Command {
	var scName string
	c := &cobra.Command{
		Use:   "assert-volume-encryption",
		Short: "FAIL if any PV-backed Linode Volume is unencrypted, untagged, or still named pvc-<uuid>",
		Long: "E2E gate for the storage invariant. Lists every Linode-CSI PV in the cluster,\n" +
			"GETs its backing Volume from the Linode API, and fails unless EVERY one is\n" +
			"encrypted, carries the tag set the StorageClass defines, and has been renamed\n" +
			"off the CSI default pvc-<uuid> to a readable <region>-<ns>-<pvc>.\n" +
			"\n" +
			"Tags and labels are applied by reconciler lanes after CreateVolume, so those two\n" +
			"get a bounded wait before failing. Encryption never does — it cannot change.\n" +
			"\n" +
			"Checks the Linode API rather than the PVC's storageClassName on purpose: the\n" +
			"class name is a proxy for encryption, and it was a envreq.Satisfied proxy the whole\n" +
			"time a managed cluster was provisioning unencrypted Volumes. `encryption` on\n" +
			"the Volume itself is the fact.\n" +
			"\n" +
			"Fails closed on every ambiguity, INCLUDING a cluster with no Linode-CSI PVs —\n" +
			"a gate that passes having examined nothing is worse than no gate.\n" +
			"\n" +
			"A color.Red run is not re-runnable: encryption is set inside CreateVolume and\n" +
			"storageClassName is immutable once bound, so the fix is to re-roll the workload\n" +
			"onto a class that encrypts (which destroys that volume's data).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return volumes.AssertEncryption(cmd.Context(), ciVolumeDeps(), scName)
		},
	}
	c.Flags().StringVar(&scName, "storage-class", volumes.DefaultTagsSC,
		"StorageClass whose volumeTags parameter defines the required tag set")
	return c
}
func ReconcileVolumeTagsCmd() *cobra.Command {
	var scName string
	c := &cobra.Command{
		Use:   "reconcile-volume-tags",
		Short: "heal StorageClass volumeTags onto every PV-backed Linode Volume",
		Long: "One-shot tag reconciler (also the llz-reconciler's `volume-tags` lane): reads\n" +
			"the desired tag set from the StorageClass's linodebs.csi.linode.com/volumeTags\n" +
			"parameter, lists the cluster's Linode-CSI PVs, and PUTs any missing tags onto\n" +
			"their backing Volumes (labels untouched — the volume-labels lane owns those).\n" +
			"Exists for Volumes born untagged — e.g. a clone/snapshot PVC admitted while\n" +
			"admission control was degraded (the Linode clone API takes no tags). Also\n" +
			"reports (never deletes) this cluster's abandoned Retain Volumes: tagged\n" +
			"lke<id> but referenced by no PV. Reads LINODE_TOKEN (env or the optional\n" +
			"linode-api-token Secret volume) and the cluster's PVs + StorageClass through\n" +
			"the in-pod ServiceAccount. Idempotent.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCIReconcileVolumeTags(cmd.Context(), scName)
		},
	}
	c.Flags().StringVar(&scName, "storage-class", volumes.DefaultTagsSC, "StorageClass whose volumeTags parameter defines the desired tag set")
	return c
}
func RelabelVolumesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "relabel-volumes",
		Short: "rename Linode Volumes to <region>-<ns>-<pvc> for every bound Linode-CSI PV",
		Long: "In-cluster Linode Volume relabeler — the Go port of the linode-volume-labeler\n" +
			"relabel.sh CronJob. Lists cluster PVs and, for each bound Linode-CSI volume,\n" +
			"rewrites its Linode UI label from the CSI default pvc-<uuid> to a readable\n" +
			"<REGION_SHORT>-<namespace>-<pvc-name> (sanitized to Linode's charset, truncated\n" +
			"to 32 chars). Idempotent — already-correct labels are skipped; a volume deleted\n" +
			"out-of-band (absent from the account list) is skipped. Env: REGION_SHORT,\n" +
			"LINODE_TOKEN.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runRelabelVolumes(context.Background()) },
	}
}
