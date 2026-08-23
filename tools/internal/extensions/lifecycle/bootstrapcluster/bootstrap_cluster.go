package bootstrapcluster

// ci_bootstrap_cluster.go implements `llz ci bootstrap-cluster` for a Linode
// MANAGED App Platform cluster (linode_lke_cluster.apl_enabled=true). Terraform
// owns day-0 infra (vpc, cluster, object-storage) AND — via apl_enabled — the
// apl-core install itself: Linode installs+manages apl-core and provisions the
// lke<id>.akamai-apl.net domain/DNS/wildcard-cert. LLZ never self-installs
// apl-core (see ADR 0005). All LLZ does here is LAYER its extras (OpenBao,
// harbor-robot, reconciler, team-creds) onto the managed cluster as SEPARATE
// Argo Applications — the "Argo bridge" — plus the two cluster-scoped pieces
// managed apl-core doesn't provide for the extras (the named non-default
// block-storage-retain StorageClass the OpenBao PVC references, and the
// llz-openbao namespace).
//
// CONVERGENCE CONTRACT — see docs/architecture/convergence-contract.md. This
// command returning 0 means the Argo bridge was placed onto a managed ArgoCD
// that can admit Applications; Argo CD/apl-core own everything day-2. The deep-
// convergence verdict remains `llz ci converge` at the tail of
// llz-bootstrap-openbao.yml, unchanged.
//
// The kubectl/apply/clock seams are injected (bootstrapDeps) so the whole flow
// is unit-tested without a cluster.

import (
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/answers"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cigate"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/platform"
	"sigs.k8s.io/yaml"
)

//go:embed manifests/block-storage-class.yaml
var blockStorageClassYAML []byte

// The clone/snapshot-PVC deny ValidatingAdmissionPolicy (+ its binding) — the
// in-apiserver twin of the Kyverno pvc-deny-untaggable-clone ClusterPolicy.
// In-tree admissionregistration API (no CRD wait), so it enforces from bootstrap,
// before apl-core installs Kyverno — and through any later webhook outage.
//
//go:embed manifests/vap-pvc-deny-untaggable-clone.yaml
var vapPVCDenyCloneYAML []byte

//go:embed manifests/vap-pvc-deny-untaggable-clone-binding.yaml
var vapPVCDenyCloneBindingYAML []byte

// The apl-core baseline THE ALIAS IS GONE. `defaultAplChartVersion` was
// `= clusterspec.BaselineAplChartVersion`, a second name for one fact, kept
// because package main could not reach the constant conveniently. Callers now say
// `clusterspec.BaselineAplChartVersion` and there is one name again — which is
// what the alias's own comment had been asking for.

// The secrets-only placeholder set moved to internal/manifestguard.
//
// IT LIVES WITH THE GUARD THAT VALIDATES IT, not with the code that substitutes
// it, and that is the same resolution `deliver-docs` reached for its keep-set: the
// check is meaningless if it runs against a different set than the producer ships,
// so the set is defined once, next to the check, and the producer imports it. Two
// copies is exactly the drift this constant exists to prevent.

// BootstrapFlags are the raw CLI inputs (identity via flags, secrets via env).
type BootstrapFlags struct {
	Kubeconfig       string
	Env              string
	ClusterID        string
	AppsRepoRevision string
	InstanceRepo     string
	UpstreamOrg      string
	TemplateRef      string
	DryRun           bool
}

// bootstrapClusterOpts is the resolved config the bridge apply consumes. Managed
// apl-core owns the install, so LLZ needs none of the self-install secrets — only
// the repo/env/ref identity the bridge Applications point at, plus the optional
// GHCR credentials for a private fork's first-party OCI charts/images.
type bootstrapClusterOpts struct {
	env string
	// clusterID is the numeric LKE cluster id, rendered into the block-storage-retain
	// StorageClass's `lke<id>` volumeTag — the ownership signal `llz reap`'s
	// cluster-liveness gate keys on.
	clusterID        string
	appsRepoRevision string
	instanceRepo     string
	upstreamOrg      string
	templateRef      string

	ghcrUsername string // GHCR_USERNAME (optional)
	ghcrToken    string // GHCR_TOKEN (optional)
	// instanceRepoToken authenticates ArgoCD to a PRIVATE instance repo (the
	// platform-bootstrap Application's source). APL_VALUES_REPO_TOKEN; empty = public
	// repo, no repository Secret applied.
	instanceRepoToken string
	// managedApps are the optional apl-core apps LLZ enables in apl-core at bootstrap
	// (ADR 0006); from spec.cluster.bootstrap.managedApps (defaulted to the LLZ set).
	managedApps []string
}

// bootstrapDeps are the seams the flow drives. kubectl runs one read/wait/patch/logs
// invocation (KUBECONFIG wired) returning combined output + exit-0; apply pipes a
// manifest to `kubectl apply --server-side` (used for the bridge + the in-cluster
// apl-values migration Job); now/sleep make the deadline loops testable.
type bootstrapDeps struct {
	kubectl func(args ...string) (string, bool)
	apply   func(stdinYAML, fieldManager string, force bool) (string, bool)
	now     func() time.Time
	sleep   func(time.Duration)
}

// RunBootstrapCluster is the single (managed App Platform) bootstrap path. LLZ
// only LAYERS its extras onto the Linode-managed apl-core via the Argo bridge; it
// needs none of the self-install secrets — no value rendering, no otomi.git seed,
// no Linode-DNS token — only the repo/env/ref identity the bridge points at.
func RunBootstrapCluster(f BootstrapFlags) error {
	if f.Env == "" {
		return fmt.Errorf("--env is required (the apl-values subdir, e.g. primary)")
	}
	if f.InstanceRepo == "" {
		return fmt.Errorf("--instance-repo is required (owner/name of the instance repo the bootstrap Application syncs from)")
	}

	o := bootstrapClusterOpts{
		env:              f.Env,
		clusterID:        firstNonEmpty(f.ClusterID, os.Getenv("LKE_CLUSTER_ID")),
		instanceRepo:     f.InstanceRepo,
		upstreamOrg:      firstNonEmpty(f.UpstreamOrg, "akamai-consulting"),
		templateRef:      firstNonEmpty(f.TemplateRef, answers.PinnedTemplateRef(), "main"),
		appsRepoRevision: f.AppsRepoRevision,
		// A PRIVATE fork keeping its first-party OCI charts/images private needs the
		// GHCR repo + pull secrets in the argocd namespace (empty token = public path,
		// skipped). Without these the layered extras ComparisonError / ImagePullBackOff
		// with no error from bootstrap.
		ghcrUsername: os.Getenv("GHCR_USERNAME"),
		ghcrToken:    os.Getenv("GHCR_TOKEN"),
		// The instance repo is private in the normal case; ArgoCD needs its own
		// repository credential to pull it (apl-core's otomi.git cred points at
		// Linode's gitea on managed, not this repo).
		instanceRepoToken: os.Getenv("APL_VALUES_REPO_TOKEN"),
	}
	reportDNSTokenPlaceholder(os.Getenv("LINODE_DNS_TOKEN"))
	// apps-repo-revision + managedApps come from the spec (Defaults() populates
	// managedApps to the LLZ set on managed).
	if lz, present, err := clusterspec.Detected(); present && err == nil {
		if e, ok := lz.Env(o.env); ok {
			if o.appsRepoRevision == "" {
				o.appsRepoRevision = e.Cluster.Bootstrap.AppsRepoRevision
			}
			o.managedApps = e.Cluster.Bootstrap.ManagedApps
		}
	} else if err != nil {
		return fmt.Errorf("load spec for apps-repo-revision/managedApps: %w", err)
	}
	o.appsRepoRevision = firstNonEmpty(o.appsRepoRevision, "main")

	kubeconfigPath, cleanup, err := ResolveKubeconfig(f.Kubeconfig)
	if err != nil {
		return err
	}
	defer cleanup()

	if f.DryRun {
		return dryRunBootstrap(o, kubeconfigPath)
	}
	return bootstrapCluster(o, NewBootstrapDeps(kubeconfigPath))
}

// NewBootstrapDeps wires the kubectl/apply/clock seams the bridge apply drives,
// all pointed at the resolved kubeconfig.
func NewBootstrapDeps(kubeconfigPath string) bootstrapDeps {
	return bootstrapDeps{
		kubectl: func(args ...string) (string, bool) {
			cmd := exec.Command("kubectl", args...)
			if kubeconfigPath != "" {
				cmd.Env = cigate.EnvWithKubeconfig(kubeconfigPath)
			}
			return cigate.RunCombined(cmd)
		},
		apply: func(stdinYAML, fieldManager string, force bool) (string, bool) {
			args := []string{"apply", "--server-side", "--field-manager=" + fieldManager}
			if force {
				args = append(args, "--force-conflicts")
			}
			args = append(args, "-f", "-")
			cmd := exec.Command("kubectl", args...)
			if kubeconfigPath != "" {
				cmd.Env = cigate.EnvWithKubeconfig(kubeconfigPath)
			}
			cmd.Stdin = strings.NewReader(stdinYAML)
			return cigate.RunCombined(cmd)
		},
		now:   time.Now,
		sleep: time.Sleep,
	}
}

// managedArgoReadyBudget / managedArgoReadyInterval bound the wait for Linode's
// managed apl-core to stand up an ArgoCD that can admit Applications. On a fresh
// apl_enabled cluster the apl-operator installs ArgoCD a few minutes after the
// nodes are Ready, so the budget is generous.
const (
	managedArgoReadyBudget   = 15 * time.Minute
	managedArgoReadyInterval = 10 * time.Second
)

// bootstrapCluster applies the Argo bridge onto a Linode-managed apl-core: it
// applies the named non-default StorageClass + the llz-openbao namespace the
// extras need, waits for the managed ArgoCD to be able to admit Applications, then
// server-side-applies the three bridge manifests. Everything else (apl-core
// install, DNS, cert, Kyverno) is Linode's. Validated live: managed ArgoCD
// reconciles the externally-applied AppProject + Application without disturbing
// its own apps (ADR 0005 option-A findings).
func bootstrapCluster(o bootstrapClusterOpts, d bootstrapDeps) error {
	fmt.Fprintln(os.Stderr, "→ managed App Platform (apl_enabled): Linode owns apl-core; layering LLZ's extras as separate Argo Applications.")

	// block-storage-retain StorageClass. The extras (OpenBao raft PVC, etc.) reference
	// it BY NAME, and managed apl-core doesn't create it. Apply LLZ's class (same
	// Linode-CSI provisioner, encryption + Retain) as the cluster DEFAULT — same as
	// self-install. Managed leaves NO default of its own (apl-core relies on LKE's
	// `linode-block-storage-retain`, which the always-on llzReconciler sc-demote pass
	// then demotes), so without this a managed cluster ends up with NO default
	// StorageClass and PVCs without an explicit class stay Pending. Making this the
	// default also lands new apl-core app PVCs on encrypted+Retain storage directly.
	// force=false: LLZ is the sole owner of this class and re-applies it under the
	// same field manager, so there's no cross-manager conflict to force through.
	//
	// The class's volumeTags carry this cluster's `lke<id>` ownership tag AT
	// CreateVolume time — the tag `llz reap`'s cluster-liveness gate keys on, so a
	// Volume is never observable untagged (no reconcile-lag window). The id is taken
	// explicitly (--cluster-id / $LKE_CLUSTER_ID, threaded from the `cluster`
	// workspace's cluster_id output) and substituted into the manifest's
	// ${volume_tags} slot; an empty/malformed id hard-fails, because SC `parameters`
	// are immutable and a silently-untagged class can never be fixed in place.
	scYAML, err := renderBlockStorageClass(o.clusterID)
	if err != nil {
		return err
	}
	if out, ok := d.apply(scYAML, "llz-managed-bridge", false); !ok {
		fmt.Fprint(os.Stderr, out)
		return fmt.Errorf("apply managed block-storage-retain StorageClass")
	}

	// SPIKE — encrypt the LKE stock StorageClasses IN PLACE (by delete+recreate).
	// See encryptStockStorageClasses for why this, and not an admission policy.
	if err := encryptStockStorageClasses(o, d); err != nil {
		return err
	}

	// clone/snapshot-PVC deny ValidatingAdmissionPolicy + binding. In-apiserver CEL
	// (no webhook, no CRD wait) blocking clone/snapshot-sourced PVCs, whose Linode
	// CloneVolume path cannot carry the lke<id> tag and would mint an un-reapable
	// Volume. Applied here so it enforces from bootstrap, ahead of the Kyverno twin
	// apl-core installs later, and through any later webhook outage.
	for _, m := range []struct{ name, body string }{
		{"pvc-deny-untaggable-clone VAP", string(vapPVCDenyCloneYAML)},
		{"pvc-deny-untaggable-clone VAP binding", string(vapPVCDenyCloneBindingYAML)},
	} {
		if out, ok := d.apply(m.body, "llz-managed-bridge", false); !ok {
			fmt.Fprint(os.Stderr, out)
			return fmt.Errorf("apply %s failed", m.name)
		}
	}

	// LLZ-owned namespaces (llz-openbao, llz-observability) that cluster-foundation
	// creates on self-install but managed does not. Their carved apps are
	// CreateNamespace=false, so without this the apps never sync (OpenBao seal-key seed
	// times out; observability's dashboards + loki-object-store ExternalSecret + otel
	// stay OutOfSync).
	for _, ns := range managedLLZNamespaces {
		if err := applyManifest(d, llzNamespaceManifest(ns), "llz-managed-bridge", true); err != nil {
			return err
		}
	}

	if err := waitManagedArgoReady(d); err != nil {
		return err
	}

	// apl-core 6.1.0's documented pre-upgrade prerequisite. Linode owns WHEN the
	// managed apl-core upgrade rolls, so LLZ has no upgrade hook to hang this on —
	// assert it eagerly, every apply, while we already hold a kubeconfig. Idempotent,
	// and self-retiring: 6.1.x's own chart sets the same annotation.
	PrepareAplUpgradeBestEffort(d)

	// Point the managed apl-core at LLZ's github values branch (BYO Git) and enable the
	// default apps (harbor/loki/grafana/kyverno) so apl-core INSTALLS them and LLZ's
	// extras layer on apl-core's own installs. See ADR 0006. Best-effort: on failure it
	// warns rather than aborting the bridge (the extras that need a default app degrade
	// instead of wedging; kyverno-dependent imageSignature stays render-gated as backup).
	// HARD FAIL. This used to warn, on the theory that the extras "degrade instead of
	// wedging". They do not: apl-core is repointed at the apl-<env> branch, so if the
	// migration does not push it, apl-operator logs `couldn't find remote ref` every
	// 15s forever, every gitops-* Application sits in ComparisonError, and CONVERGE
	// hard-fails ~20 minutes later naming none of it. Cluster 637367 spent three runs
	// that way while this step reported success.
	//
	// Nothing downstream can proceed without the values branch, so a failure here is
	// terminal — and saying so at the point of failure is worth far more than a
	// color.Green step and an unexplained collapse two phases later.
	// TERMINAL, including "apl-core hasn't published its coordinates yet". That state
	// used to warn-and-skip on the theory that it is a normal early state on a fresh
	// cluster and the extras would merely degrade. The first half is true and is now
	// handled by WAITING (waitAplGitConfig); the second half is not — skipping the
	// migration means the apl-<env> branch is never seeded, so apl-operator has no
	// values branch, every gitops-* Application sits in ComparisonError, and converge
	// hard-fails ~20 minutes later naming none of it.
	//
	// Failing here cannot turn a passing run color.Red: a managed cluster that never
	// publishes apl-git-config has no apl-core, so converge was going to fail
	// regardless. It just fails earlier, and says why.
	if err := configureManagedApl(o, d); err != nil {
		return fmt.Errorf("managed apl-core BYO-Git + default-apps configuration failed, so apl-core has no "+
			"values branch to reconcile: %w", err)
	}

	// Instance-repo credential (private-repo path; empty token = public, skip). ArgoCD
	// needs its OWN repository Secret to pull the private instance repo the
	// platform-bootstrap Application sources — apl-core's otomi.git credential points
	// at Linode's in-cluster gitea on managed, not this repo, so without this
	// platform-bootstrap dead-ends on "authentication required" and never deploys its
	// OpenBao child. Applied BEFORE the bridge Applications so the credential is
	// present on their first reconcile.
	if o.instanceRepoToken != "" {
		if err := applyManifest(d, instanceRepoArgoSecretManifest(o), "llz-managed-bridge", true); err != nil {
			return err
		}
	}

	// Optional GHCR secrets (private-fork path; empty token = public, skip) — the OCI
	// repo secret + image pull secret in the argocd namespace, so the layered extras'
	// first-party charts/images can be pulled.
	if o.ghcrToken != "" {
		if err := applyManifest(d, ghcrOCIRepoSecretManifest(o), "llz-managed-bridge", true); err != nil {
			return err
		}
		if err := applyManifest(d, ghcrPullSecretManifest(o), "llz-managed-bridge", true); err != nil {
			return err
		}
	}

	// force=true: llz-secret-store's targetRevision (the template ref) changes every
	// push, and a re-bootstrapped cluster may carry a different field manager on these
	// specs. We are the sole intended owner of the bridge (apl-core creates none of it).
	if err := applyManifest(d, platformBootstrapAppProjectManifest(o), "llz-managed-bridge", true); err != nil {
		return err
	}
	if err := applyManifest(d, platformBootstrapApplicationManifest(o), "llz-managed-bridge", true); err != nil {
		return err
	}
	if err := applyManifest(d, secretStoreApplicationManifest(o), "llz-managed-bridge", true); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "✓ Argo bridge applied — Argo CD will converge the LLZ extras onto the managed cluster.")
	return nil
}

// waitManagedArgoReady blocks until the managed apl-core's ArgoCD can admit our
// Applications: the Application CRD is registered AND argocd-server reports at least
// one available replica. Applying the bridge before the CRD exists would hard-fail
// ("no matches for kind Application"), so this gate is load-bearing on a fresh cluster.
func waitManagedArgoReady(d bootstrapDeps) error {
	deadline := d.now().Add(managedArgoReadyBudget)
	announced := false
	for {
		_, crdOK := d.kubectl("get", "crd", "applications.argoproj.io")
		avail, srvOK := d.kubectl("-n", "argocd", "get", "deploy", "argocd-server",
			"-o", "jsonpath={.status.availableReplicas}")
		if crdOK && srvOK && strings.TrimSpace(avail) != "" && strings.TrimSpace(avail) != "0" {
			if announced {
				fmt.Fprintln(os.Stderr, "managed ArgoCD is ready.")
			}
			return nil
		}
		if !announced {
			fmt.Fprintln(os.Stderr, "Waiting for managed apl-core's ArgoCD (Application CRD + argocd-server)...")
			announced = true
		}
		if !d.now().Before(deadline) {
			return fmt.Errorf("managed apl-core's ArgoCD not ready within %s (Application CRD present=%t, argocd-server availableReplicas=%q) — is apl_enabled fully converged?", managedArgoReadyBudget, crdOK, strings.TrimSpace(avail))
		}
		d.sleep(managedArgoReadyInterval)
	}
}

// dryRunBootstrap prints the intended actions without touching the cluster — the
// operator-facing replacement for `terraform plan` on this layer.
func dryRunBootstrap(o bootstrapClusterOpts, kubeconfigPath string) error {
	fmt.Printf("→ (dry-run) bootstrap-cluster (managed App Platform / apl_enabled) env=%s kubeconfig=%s\n", o.env, kubeconfigPath)
	fmt.Printf("  0. kubectl apply --server-side block-storage-retain StorageClass (cluster DEFAULT; volumeTags lke<id> rendered from --cluster-id; llzReconciler sc-demote keeps LKE's linode-block-storage-retain non-default)\n")
	fmt.Printf("  0a. SPIKE: delete+recreate LKE's stock StorageClasses (%s) with encryption + lke<id> volumeTags — parameters are immutable, so this is the only way to fix them, and the only lever that lands BEFORE apl-core creates its PVCs\n",
		strings.Join(lkeStockStorageClasses, ", "))
	fmt.Printf("  0b. kubectl apply --server-side pvc-deny-untaggable-clone ValidatingAdmissionPolicy + binding\n")
	fmt.Printf("  0b. kubectl apply --server-side llz-openbao Namespace (OpenBao is CreateNamespace=false; managed apl-core does not create it)\n")
	fmt.Printf("  1. wait for managed ArgoCD (Application CRD + argocd-server available)\n")
	if o.instanceRepoToken != "" {
		fmt.Printf("  1b. kubectl apply --server-side ArgoCD repository Secret for the private instance repo %s (apl-core's otomi.git cred is gitea on managed, not this repo)\n", o.instanceRepo)
	}
	fmt.Printf("  2. kubectl apply --server-side platform-bootstrap AppProject (sourceRepos: %s + %s)\n", o.instanceRepo, o.upstreamOrg+"/lke-landing-zone")
	fmt.Printf("  3. kubectl apply --server-side platform-bootstrap Application (source: %s @ %s, path apl-values/%s/manifest)\n", o.instanceRepo, o.appsRepoRevision, o.env)
	fmt.Printf("  4. kubectl apply --server-side llz-secret-store Application (source: %s @ %s)\n", o.upstreamOrg+"/lke-landing-zone", o.templateRef)
	fmt.Printf("  (skipped — Linode owns it on managed: apl-core helm install, otomi.git seed, values render, DNS/cert, Kyverno)\n")
	return nil
}

// ResolveKubeconfig returns a filesystem path the KUBECONFIG env can point at,
// in priority order:
//
//  1. an explicit --kubeconfig. NAMING ONE IS AN INSTRUCTION, so a path that is
//     missing or zero-byte is an ERROR here, not a reason to look elsewhere —
//     this command deletes and recreates StorageClasses, and doing that against
//     a cluster the operator did not name is worse than stopping. It used to
//     fall through, silently, to whatever the machine was already pointed at.
//  2. KUBECONFIG_RAW spilled to a 0600 tempfile.
//  3. the ambient environment — an empty return, leaving kubectl/helm to resolve
//     $KUBECONFIG (if it points at a NON-EMPTY file) or the default
//     ~/.kube/config themselves.
//
// The non-empty check is load-bearing: fetch-kubeconfig writes the real kubeconfig
// to ~/.kube/config and most steps rely on that default, while $KUBECONFIG is
// often an EMPTY placeholder file. Using $KUBECONFIG blindly made kubectl read an
// empty config and every read returned empty (the e2e bootstrap failure). Only the
// tempfile path needs cleanup; the rest are no-ops.
func ResolveKubeconfig(path string) (string, func(), error) {
	noop := func() {}
	// 1. Explicit --kubeconfig (non-empty file) → override the child env with it.
	//
	// AN EXPLICIT PATH THAT CANNOT BE USED IS AN ERROR, NOT A FALLBACK. This used
	// to fall through silently to KUBECONFIG_RAW and then to the ambient
	// ~/.kube/config, so an operator who named the wrong file — or a workflow whose
	// fetch step produced a zero-byte one — ran the whole bootstrap against
	// WHATEVER CLUSTER THE MACHINE WAS ALREADY POINTED AT, and it reported success.
	// This command deletes and recreates StorageClasses; landing that on a
	// different cluster is not a case worth being lenient about. Naming a
	// kubeconfig is an instruction, so honour it or say why not.
	if path != "" {
		st, err := os.Stat(path)
		switch {
		case err != nil:
			return "", noop, fmt.Errorf("--kubeconfig %s: %w (refusing to fall back to $KUBECONFIG_RAW or the "+
				"ambient ~/.kube/config — this command deletes and recreates StorageClasses, and doing that "+
				"against a cluster you did not name is worse than stopping)", path, err)
		case st.Size() == 0:
			return "", noop, fmt.Errorf("--kubeconfig %s is empty (0 bytes) — refusing to fall back to "+
				"$KUBECONFIG_RAW or the ambient ~/.kube/config. A zero-byte file is usually a fetch step "+
				"that failed without failing the job", path)
		}
		return path, noop, nil
	}
	// 2. KUBECONFIG_RAW → spill to a 0600 tempfile → override.
	if raw := os.Getenv("KUBECONFIG_RAW"); raw != "" {
		path, cleanup, err := cigate.WriteTempKubeconfig("llz-bootstrap-kubeconfig-*", []byte(raw))
		if err != nil {
			return "", noop, err
		}
		return path, cleanup, nil
	}
	// 3. Otherwise INHERIT the ambient environment — let kubectl/helm resolve
	//    $KUBECONFIG / ~/.kube/config THEMSELVES, exactly like wait-cluster-ready +
	//    diagnose-argocd, which read the cluster fine. An empty return path signals
	//    "do not touch the child env". Fail loudly only when nothing is resolvable.
	if kubectlprobe.EffectiveKubeconfig() == "" {
		return "", noop, fmt.Errorf("no usable kubeconfig: pass --kubeconfig, set a non-empty $KUBECONFIG or ~/.kube/config, or set KUBECONFIG_RAW")
	}
	return "", noop, nil
}

// blockStorageBaseVolumeTags are the static tags every platform Volume carries;
// `lke<id>` is appended when the cluster id resolves. Kept in sync with the
// manifest header comment in manifests/block-storage-class.yaml.
const blockStorageBaseVolumeTags = "block-storage,platform-support-services"

// csiEncryptedParam / csiVolumeTagsParam are the Linode CSI StorageClass
// parameters that decide, at CreateVolume, whether a Volume is encrypted at rest
// and which tags it carries. Both are read ONLY at provision time.
const (
	csiEncryptedParam    = "linodebs.csi.linode.com/encrypted"
	csiVolumeTagsParam   = "linodebs.csi.linode.com/volumeTags"
	linodeCSIProvisioner = "linodebs.csi.linode.com"
)

// lkeStockStorageClasses are the two classes LKE's own `workload` Helm release
// installs into every cluster, unencrypted and untagged.
var lkeStockStorageClasses = []string{"linode-block-storage", "linode-block-storage-retain"}

// encryptStockStorageClasses rewrites LKE's stock StorageClasses so they encrypt
// and ownership-tag at CreateVolume, by DELETING and RECREATING each one with the
// same identity and the CSI parameters added.
//
// WHY THIS, AND NOT AN ADMISSION POLICY. On a managed cluster Linode owns
// apl-values, and its `cluster.defaultStorageClass` is `linode-block-storage` — so
// apl-core's PVCs name the unencrypted stock class EXPLICITLY. Two levers do not
// work on those:
//
//   - promoting block-storage-retain to cluster default does nothing, because an
//     explicit storageClassName never consults the default;
//   - a Kyverno mutation cannot win the race. apl-core installs Kyverno AND creates
//     the PVCs, and it creates most of them FIRST. Measured live:
//     kyverno-admission-controller went Available roughly three minutes after the
//     first PVC, by which point 11 of the 13 already existed. A policy applied the
//     instant Kyverno can admit one still arrives too late for 11 of 13.
//
// What DOES have a head start is this command. bootstrap-cluster applied its
// StorageClass at 15:56:03 — 67 seconds before the first PVC — and needs nothing
// from apl-core to do it. Fixing the class itself is therefore the only lever that
// lands before the PVCs exist.
//
// WHY DELETE+RECREATE. StorageClass `parameters` are immutable; there is no patch
// that adds encryption to an existing class. Deleting a StorageClass does NOT
// affect existing PVs or PVCs — binding does not re-consult the class — so the only
// exposure is a PVC created in the sub-second gap, which would land Pending and be
// retried by the external-provisioner once the class is back.
//
// KNOWN OPEN RISK (this is a spike). The stock classes belong to LKE's `workload`
// Helm release. If LKE's control plane re-reconciles that release it will restore
// the unencrypted definitions, and any PVC created after that point regresses. This
// change deliberately does NOT add a fight-loop for that yet — the point of running
// it against a live e2e cluster is to find out whether, and how fast, LKE
// re-promotes. If it does, the durable form is a reconciler lane alongside
// sc-demote, which already watches StorageClasses for exactly this shape of drift.
func encryptStockStorageClasses(o bootstrapClusterOpts, d bootstrapDeps) error {
	id, err := normalizeLKEClusterID(o.clusterID)
	if err != nil {
		return err
	}
	wantTags := blockStorageBaseVolumeTags + ",lke" + id

	for _, name := range lkeStockStorageClasses {
		raw, ok := d.kubectl("get", "storageclass", name, "-o", "json")
		// Absent is fine and expected on some cluster shapes — nothing to fix. An
		// EMPTY body on a successful get counts as absent too: never delete a class
		// whose definition could not actually be read back.
		if !ok || strings.TrimSpace(raw) == "" {
			fmt.Fprintf(os.Stderr, "  stock StorageClass %s not present — skipping.\n", name)
			continue
		}
		live, err := parseStorageClass([]byte(raw))
		if err != nil {
			return fmt.Errorf("parse StorageClass %s: %w", name, err)
		}
		if live.Provisioner != linodeCSIProvisioner {
			// Not a Linode-CSI class; encryption parameters would be meaningless.
			fmt.Fprintf(os.Stderr, "  %s is provisioned by %q, not %s — leaving alone.\n", name, live.Provisioner, linodeCSIProvisioner)
			continue
		}
		if live.Parameters[csiEncryptedParam] == "true" && live.Parameters[csiVolumeTagsParam] == wantTags {
			fmt.Fprintf(os.Stderr, "  %s already encrypted + tagged %q — no change.\n", name, wantTags)
			continue
		}

		// Build and validate the replacement BEFORE deleting anything: a render bug
		// here must not be able to leave the cluster with no stock class at all.
		replacement := live.withEncryption(wantTags)
		body, err := yaml.Marshal(replacement.manifest())
		if err != nil {
			return fmt.Errorf("render replacement StorageClass %s: %w", name, err)
		}

		fmt.Fprintf(os.Stderr, "→ %s: encrypted=%q tags=%q → encrypted=\"true\" tags=%q (delete+recreate; parameters are immutable)\n",
			name, live.Parameters[csiEncryptedParam], live.Parameters[csiVolumeTagsParam], wantTags)

		if out, ok := d.kubectl("delete", "storageclass", name, "--ignore-not-found"); !ok {
			fmt.Fprint(os.Stderr, out)
			return fmt.Errorf("delete stock StorageClass %s (needed because parameters are immutable)", name)
		}
		if out, ok := d.apply(string(body), "llz-managed-bridge", true); !ok {
			fmt.Fprint(os.Stderr, out)
			// The class is GONE at this point and we could not put it back. Say so
			// unmistakably: any PVC naming it will sit Pending until an operator
			// recreates it.
			return fmt.Errorf("recreate stock StorageClass %s FAILED after it was deleted — the cluster now has NO %s class and any PVC naming it will stay Pending. Recreate it by hand or re-run bootstrap-cluster", name, name)
		}
		fmt.Fprintf(os.Stderr, "  %s recreated encrypted + tagged.\n", name)
	}
	return nil
}

// stockStorageClass is the subset of a StorageClass that survives the delete+recreate.
// Anything not listed here is intentionally dropped: the recreated object is LLZ's,
// so Helm/Flux ownership metadata must NOT be carried over (copying it would make
// the class look like it still belongs to LKE's release).
type stockStorageClass struct {
	Name                 string
	Provisioner          string
	ReclaimPolicy        string
	VolumeBindingMode    string
	AllowVolumeExpansion *bool
	Parameters           map[string]string
	IsDefault            bool
}

func parseStorageClass(raw []byte) (stockStorageClass, error) {
	var doc struct {
		Metadata struct {
			Name        string            `json:"name"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Provisioner          string            `json:"provisioner"`
		ReclaimPolicy        string            `json:"reclaimPolicy"`
		VolumeBindingMode    string            `json:"volumeBindingMode"`
		AllowVolumeExpansion *bool             `json:"allowVolumeExpansion"`
		Parameters           map[string]string `json:"parameters"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return stockStorageClass{}, err
	}
	if doc.Metadata.Name == "" {
		return stockStorageClass{}, fmt.Errorf("StorageClass JSON carried no metadata.name")
	}
	return stockStorageClass{
		Name:                 doc.Metadata.Name,
		Provisioner:          doc.Provisioner,
		ReclaimPolicy:        doc.ReclaimPolicy,
		VolumeBindingMode:    doc.VolumeBindingMode,
		AllowVolumeExpansion: doc.AllowVolumeExpansion,
		Parameters:           doc.Parameters,
		IsDefault:            doc.Metadata.Annotations[platform.SCDefaultAnnotation] == "true",
	}, nil
}

// withEncryption returns a copy carrying the CSI encryption + volumeTags
// parameters. Every other parameter is preserved — the class may carry Linode
// options this code does not know about, and dropping one silently would change
// provisioning behaviour beyond encryption.
//
// IsDefault is deliberately NOT preserved as-is: block-storage-retain is the
// cluster default and the sc-demote reconciler actively keeps LKE's retain class
// non-default. Re-creating a stock class WITH the default annotation would
// reintroduce the two-default-StorageClass wedge that sc-demote exists to prevent.
func (s stockStorageClass) withEncryption(tags string) stockStorageClass {
	params := make(map[string]string, len(s.Parameters)+2)
	for k, v := range s.Parameters {
		params[k] = v
	}
	params[csiEncryptedParam] = "true"
	params[csiVolumeTagsParam] = tags
	s.Parameters = params
	s.IsDefault = false
	return s
}

func (s stockStorageClass) manifest() map[string]any {
	meta := map[string]any{"name": s.Name}
	m := map[string]any{
		"apiVersion":  "storage.k8s.io/v1",
		"kind":        "StorageClass",
		"metadata":    meta,
		"provisioner": s.Provisioner,
		"parameters":  s.Parameters,
	}
	if s.ReclaimPolicy != "" {
		m["reclaimPolicy"] = s.ReclaimPolicy
	}
	if s.VolumeBindingMode != "" {
		m["volumeBindingMode"] = s.VolumeBindingMode
	}
	if s.AllowVolumeExpansion != nil {
		m["allowVolumeExpansion"] = *s.AllowVolumeExpansion
	}
	return m
}

// renderBlockStorageClass fills the ${volume_tags} slot in the embedded
// block-storage-retain StorageClass with the static tags plus this cluster's
// `lke<id>` ownership tag, from the numeric cluster id passed in (the `cluster`
// workspace's cluster_id output, threaded through --cluster-id / $LKE_CLUSTER_ID).
//
// HARD-FAIL on an empty/malformed id. StorageClass `parameters` are immutable, so a
// class applied without the lke<id> tag can never be fixed in place, and every
// Volume it provisions is un-attributable — `llz reap` could then never clean them
// up. Failing the bootstrap loudly is safer than silently shipping an un-reapable
// fleet.
func renderBlockStorageClass(clusterID string) (string, error) {
	id, err := normalizeLKEClusterID(clusterID)
	if err != nil {
		return "", err
	}
	volumeTags := blockStorageBaseVolumeTags + ",lke" + id
	rendered := strings.ReplaceAll(string(blockStorageClassYAML), "${volume_tags}", volumeTags)
	if strings.Contains(rendered, "${") {
		return "", fmt.Errorf("block-storage StorageClass still has an unrendered ${...} placeholder after substitution")
	}
	return rendered, nil
}

// normalizeLKEClusterID validates the cluster id renders a reap-attributable
// `lke<id>` tag: it strips an optional `lke` prefix and requires the remainder be
// all digits (the shape linode.LKEIDFromTags — `^lke-?[0-9]+$` — parses). Anything
// else is rejected so a typo can't produce a silently-unattributable tag.
func normalizeLKEClusterID(raw string) (string, error) {
	id := strings.TrimSpace(raw)
	id = strings.TrimPrefix(id, "lke")
	if id == "" {
		return "", fmt.Errorf("bootstrap-cluster: --cluster-id (or $LKE_CLUSTER_ID) is empty — the block-storage-retain StorageClass would render WITHOUT its lke<id> ownership tag, so every Volume it provisions would be un-attributable and `llz reap` could never clean them up (StorageClass parameters are immutable — a silently-untagged class can't be fixed in place). Pass the `cluster` workspace's cluster_id output")
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return "", fmt.Errorf("bootstrap-cluster: --cluster-id %q is not a numeric LKE cluster id — it would render a malformed lke<id> tag that `llz reap` cannot attribute; pass the `cluster` workspace's cluster_id output", raw)
		}
	}
	return id, nil
}

// applyManifest marshals a manifest map to YAML and server-side-applies it,
// mirroring the module's `kubectl_manifest` (yamlencode + server_side_apply).
func applyManifest(d bootstrapDeps, obj map[string]any, fieldManager string, force bool) error {
	y, err := yaml.Marshal(obj)
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	out, ok := d.apply(string(y), fieldManager, force)
	if !ok {
		fmt.Fprint(os.Stderr, out)
		return fmt.Errorf("kubectl apply failed for %s/%s", manifestKind(obj), manifestName(obj))
	}
	return nil
}

func manifestKind(obj map[string]any) string { return fmt.Sprint(obj["kind"]) }
func manifestName(obj map[string]any) string {
	if md, ok := obj["metadata"].(map[string]any); ok {
		return fmt.Sprint(md["name"])
	}
	return ""
}

// aplGitConfig is App Platform's "BYO Git" config, stored (BARE keys, not otomi.git.*)
// in the Secret apl-secrets/apl-git-config. apl-operator re-reads this Secret on EVERY
// reconcile poll (reloadGitCredentials → git remote set-url), so patching it repoints
// apl-core's values repo at runtime with no pod restart and no helm.
type aplGitConfig struct {
	repoURL, branch, username, password string
	// cloneCmd is apl-core's ORIGINAL clone URL for its own Gitea values tree,
	// credentials included. Despite the "Cmd" in the key name it is a bare URL, not
	// a shell command, so it can be handed to `git clone` as-is. It survives
	// untouched when patchAplGitConfig repoints repoURL at github, which makes it
	// the only remaining pointer to the true SOURCE tree on a re-bootstrap — see
	// migrateAplValuesToGitHub.
	//
	// Note the host is apl-core's PUBLIC Gitea name (git.<domainSuffix>), not a
	// cluster-internal Service DNS name, so an in-cluster clone hairpins out to the
	// LB and back. That pattern is what broke the Keycloak JWKS fetch (see
	// keycloakJWKSURL) — but here it is verified to work: a Job cloning this exact
	// URL from inside the cluster succeeded on 637367. Recorded so the resemblance
	// doesn't get "fixed" on suspicion.
	cloneCmd string
}

const (
	aplGitSecretName = "apl-git-config"
	aplGitSecretNS   = "apl-secrets"
)

// errAplNotBYOGitReady means apl-core has not yet written its values-repo
// coordinates. TRANSIENT and EXPECTED on a fresh managed cluster — it must stay a
// warning, unlike a migration that actually ran and failed. Conflating the two
// would make every fresh bootstrap fatal.
var errAplNotBYOGitReady = errors.New("apl-core not yet BYO-Git-configured")

// configureManagedApl points the Linode-installed apl-core at LLZ's github values
// branch (App Platform "BYO Git") and enables the default apps — the SUPPORTED way,
// NOT via helm. A managed cluster has no customer-`helm`-upgradeable `apl` release
// (helm upgrade → "apl has no deployed releases"), and app-enablement flows through
// the git VALUES REPO the operator reconciles, not chart values (which are read only
// at first-install bootstrap). See docs/adr/0006-managed-default-apps.md.
//
// Mechanism (all source-verified against linode/apl-core):
//  1. Read apl-core's CURRENT git remote from apl-secrets/apl-git-config (Gitea).
//  2. Migrate: clone that tree, drop an `env/apps/<name>.yaml` AplApp enable file for
//     each managed app, and push the WHOLE tree to the github apl-<env> branch. The
//     complete tree matters: the operator does `git reset --hard origin/<branch>` each
//     poll, so a partial branch (toggles only) would WIPE apl-core's config. This
//     replicates the Console's "push existing values history" BYO-Git migration step.
//  3. Patch apl-secrets/apl-git-config → the github coords. The operator reloads it,
//     `git remote set-url`s to github, hard-resets to our complete+toggled tree, and
//     reconciles the apps on.
//
// Needs APL_VALUES_REPO_TOKEN (Contents:write on the instance repo). Empty token →
// skip. Best-effort at the call site (a failure warns; the Argo bridge still applies).
func configureManagedApl(o bootstrapClusterOpts, d bootstrapDeps) error {
	if o.instanceRepoToken == "" {
		fmt.Fprintln(os.Stderr, "APL_VALUES_REPO_TOKEN unset — skipping the managed apl-core BYO-Git + default-apps configuration.")
		return nil
	}
	githubURL := "https://github.com/" + o.instanceRepo + ".git"
	branch := "apl-" + o.env
	apps := o.managedApps
	if len(apps) == 0 {
		apps = clusterspec.DefaultManagedApps
	}
	tok := o.instanceRepoToken

	// 1. Read apl-core's current values-repo coordinates (the Gitea default on managed),
	//    WAITING for them if apl-core has not published them yet.
	cur, err := waitAplGitConfig(d)
	if err != nil {
		return err
	}

	// 2. Migrate the current tree → github apl-<env>, layering the app-enable files on.
	if err := migrateAplValuesToGitHub(d, cur, githubURL, branch, apps, tok); err != nil {
		return err
	}

	// 3. Repoint apl-core at the github branch by patching the BYO-Git Secret.
	if err := patchAplGitConfig(d, githubURL, branch, tok); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "✓ managed apl-core repointed at %s (branch %s) + apps enabled (%s).\n", githubURL, branch, strings.Join(apps, ", "))
	return nil
}

// readAplGitConfig reads the current apl-secrets/apl-git-config Secret (base64 .data,
// BARE keys) so the migration can clone apl-core's existing (Gitea) values tree.
// aplGitConfigWait / aplGitConfigPoll bound the wait for apl-core to publish its
// BYO-Git coordinates.
var (
	aplGitConfigWait = 10 * time.Minute
	aplGitConfigPoll = 10 * time.Second
)

// waitAplGitConfig is readAplGitConfig with a bounded wait for the Secret to exist.
//
// WHY A WAIT AND NOT A SKIP. readAplGitConfig is one shot, and a fresh managed
// cluster races it: Linode installs apl-core during cluster provisioning, and
// apl-secrets/apl-git-config appears somewhere in that window. Treating "not there
// yet" as a skip means the migration never runs, so the app-enable files are never
// committed and the apl-<env> branch is never seeded — and then apl-operator has no
// values branch, every gitops-* Application sits in ComparisonError, and converge
// hard-fails ~20 minutes later naming none of it. That is the SAME shape as the
// re-bootstrap bug (a color.Green step, a late unexplained collapse) reached by a
// different route, and the skip made it look intentional.
//
// Exhausting the budget is therefore TERMINAL. If apl-core never publishes its git
// config on a managed cluster then apl-core is not installed, and nothing this
// bootstrap does afterwards can work. Saying so here beats discovering it at
// converge.

// aplGitConfigAttempts is waitAplGitConfig's budget, expressed as a COUNT.
//
// Bounded by ATTEMPTS, not by a deadline read off d.now(). The deps' clock and
// sleep are independent seams, and every existing fake pairs a real time.Now with a
// no-op sleep — under which a `for d.now().Before(deadline)` loop spins at full
// speed for a real ten minutes. An attempt cap cannot be defeated by a clock that
// does not advance, which is the property a poll loop actually wants.
func aplGitConfigAttempts() int {
	if n := int(aplGitConfigWait / aplGitConfigPoll); n > 0 {
		return n
	}
	return 1
}

// waitAplGitConfig polls for apl-core's published git config. The paragraphs above
// aplGitConfigAttempts are about THIS function — why exhausting the budget is
// terminal rather than a skip — and were separated from it when the budget helper
// was split out.
func waitAplGitConfig(d bootstrapDeps) (aplGitConfig, error) {
	attempts := aplGitConfigAttempts()
	var cur aplGitConfig
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		cur, err = readAplGitConfig(d)
		if err == nil {
			if attempt > 1 {
				fmt.Fprintf(os.Stderr, "apl-core published its git config after %d attempt(s).\n", attempt)
			}
			return cur, nil
		}
		// Only the not-yet-published state is worth waiting on. A decode failure or a
		// genuinely broken Secret will not fix itself.
		if !errors.Is(err, errAplNotBYOGitReady) {
			return cur, err
		}
		if attempt == attempts {
			break
		}
		fmt.Fprintf(os.Stderr, "apl-core has not published %s/%s yet (attempt %d/%d) — waiting %s...\n",
			aplGitSecretNS, aplGitSecretName, attempt, attempts, aplGitConfigPoll)
		d.sleep(aplGitConfigPoll)
	}
	return cur, fmt.Errorf("apl-core never published %s/%s within %s (%d attempts; last: %v) — on a managed "+
		"cluster Linode installs apl-core, so this means apl-core is not up. Nothing downstream (values branch, "+
		"app enables, gitops-* Applications) can work without it",
		aplGitSecretNS, aplGitSecretName, aplGitConfigWait, attempts, err)
}

func readAplGitConfig(d bootstrapDeps) (aplGitConfig, error) {
	get := func(key string) (string, error) {
		out, ok := d.kubectl("-n", aplGitSecretNS, "get", "secret", aplGitSecretName,
			"-o", "jsonpath={.data."+key+"}")
		if !ok {
			// WRAPPED IN errAplNotBYOGitReady, WHICH IS THE WHOLE POINT OF THE WAIT.
			// waitAplGitConfig returns immediately on any error that is NOT this one,
			// and an ABSENT Secret returned a plain error — so on a fresh managed
			// cluster, where Linode installs apl-core and the Secret does not exist
			// yet, the ten-minute wait gave up on attempt 1 and failed the bootstrap
			// terminally. The wait only ever worked for the "Secret exists, repoUrl
			// empty" case, which is the only case its tests exercised.
			//
			// A permanently unreadable Secret is not silently tolerated: it exhausts
			// the budget and produces waitAplGitConfig's loud never-published error,
			// which is the right answer for both. What stays terminal is a decode
			// failure — that cannot fix itself.
			return "", fmt.Errorf("read %s/%s (%s): %w", aplGitSecretNS, aplGitSecretName,
				strings.TrimSpace(out), errAplNotBYOGitReady)
		}
		if strings.TrimSpace(out) == "" {
			return "", nil
		}
		dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out))
		if err != nil {
			return "", fmt.Errorf("decode %s.%s: %w", aplGitSecretName, key, err)
		}
		return string(dec), nil
	}
	var c aplGitConfig
	var err error
	if c.repoURL, err = get("repoUrl"); err != nil {
		return c, err
	}
	if c.branch, err = get("branch"); err != nil {
		return c, err
	}
	if c.username, err = get("username"); err != nil {
		return c, err
	}
	if c.password, err = get("password"); err != nil {
		return c, err
	}
	// Best-effort: absent on an apl-core build that does not publish it, in which
	// case the re-bootstrap fallback below simply has nothing to fall back to.
	c.cloneCmd, _ = get("gitCloneCmd")
	if c.repoURL == "" {
		return c, fmt.Errorf("%s/%s has no repoUrl: %w", aplGitSecretNS, aplGitSecretName, errAplNotBYOGitReady)
	}
	if c.branch == "" {
		c.branch = "main"
	}
	return c, nil
}

// migration Job coordinates. The apl-core values repo (git-server.<ns>.svc) is only
// reachable from INSIDE the cluster and is SOPS-encrypted at rest, so the clone/push
// must run in-cluster (a kubectl-cp of the operator pod's ENV_DIR would leak the
// decrypted tree). alpine/git carries git + /bin/sh (busybox).
const (
	aplMigrateJobNS   = "apl-operator"
	aplMigrateJobName = "llz-apl-values-migrate"
	aplMigrateImage   = "alpine/git:latest"
	// Generous budget: the Job waits (in-script) for apl-core to finish initializing its
	// values repo and push the branch before it can clone (a fresh managed cluster races).
	aplMigrateBudget = 13 * time.Minute
	aplMigrateTick   = 10 * time.Second
)

// migrateAplValuesToGitHub runs the values-repo migration as an in-cluster Job: clone
// apl-core's current (encrypted, in-cluster) tree, layer an env/apps/<name>.yaml AplApp
// enable file per app, and FORCE-PUSH the COMPLETE tree to the github apl-<env> branch.
// The full tree, pushed BEFORE the caller repoints the Secret, is what makes the switch
// wipe-safe: the operator's per-poll `git reset --hard origin/<branch>` then adopts a
// complete+toggled tree instead of deleting tracked files (a partial branch would wipe).
// aplMigrationSource picks the SOURCE the migration Job should clone, and is the
// fix for a re-bootstrap that could not recover.
//
// THE BUG. The first migration force-pushes apl-core's Gitea tree to the github
// apl-<env> branch and then PATCHES apl-git-config to point at it. From then on
// `cur.repoURL` IS the github URL and `cur.branch` IS apl-<env> — so a second
// bootstrap set SRC = DST and tried to clone the destination in order to rebuild
// the destination. That works only while the destination already exists, and can
// never recreate it: with apl-e2e missing, the Job waited 48x10s for a branch it
// was itself supposed to create, exited 1, and — because the caller treats this as
// best-effort — reported "Bootstrap cluster: success". apl-operator then logged
// `couldn't find remote ref apl-e2e` every 15s, apl-core's gitops-* Applications
// sat in ComparisonError, and converge hard-failed ~20 minutes later with no
// mention of the actual cause. Observed on cluster 637367.
//
// Resolution order:
//   - not yet migrated  -> clone the current (Gitea) repo, as before.
//   - migrated AND the destination branch exists -> nothing to rebuild; skip.
//   - migrated BUT the destination is GONE -> re-seed from apl-core's original
//     in-cluster tree via cloneCmd. That is the only source that still holds the
//     real values, and re-pushing the COMPLETE tree is what keeps the switch
//     wipe-safe (a partial branch would delete apl-core's tracked files).
//   - migrated, destination gone, and no cloneCmd -> refuse, loudly. Guessing a
//     source here risks pushing an incomplete tree, which WIPES config.
//
// Returns an ALREADY-AUTHENTICATED source URL: the two candidate sources carry
// credentials differently — cur.repoURL needs username/password injected, while
// cloneCmd already embeds them — and conflating the two yields an unauthenticated
// clone that fails with a bare "Authentication failed".
//
// skipIfDstExists is true ONLY on the re-seed path, and is what makes a
// re-bootstrap safe rather than merely non-fatal. See migrateAplValuesToGitHub.
func aplMigrationSource(cur aplGitConfig, githubURL, dstBranch string) (srcAuthedURL, srcBranch string, skipIfDstExists bool, err error) {
	if cur.repoURL != githubURL || cur.branch != dstBranch {
		// Not yet migrated: the current repo IS the source, as originally designed.
		// Unchanged behaviour, including force-pushing over a partial branch left by
		// an earlier failed attempt.
		return basicAuthGitURL(cur.repoURL, cur.username, cur.password), cur.branch, false, nil
	}
	// Already migrated, so this is a REPAIR, and it must not overwrite a healthy
	// destination. apl-core has been reconciling the github branch since the first
	// migration and its Gitea tree has been abandoned since; re-pushing that stale
	// tree would revert every values change made after the switch (including any made
	// through the Console). So the Job skips entirely when the branch is intact and
	// only rebuilds when it is genuinely missing — the half-migrated state that broke
	// 637367.
	// A MISSING SOURCE IS NOT AN ERROR HERE. The overwhelmingly common re-bootstrap is
	// "destination is fine, nothing to do", and that needs no source at all — so
	// demanding one up front would fail the normal case to prepare for the rare one.
	// It also would have been a real regression: patchAplGitConfig overwrites
	// username/password with the github coords, so gitCloneCmd is the ONLY surviving
	// Gitea credential, and any cluster whose apl-core does not publish that field
	// would now fail its bootstrap outright.
	//
	// So resolve it best-effort and let the Job decide. It checks the destination
	// FIRST; an empty SRC_URL is only reached when the branch is genuinely missing,
	// and then the Job fails saying exactly that.
	src, err := giteaSourceFromCloneCmd(cur.cloneCmd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "::warning::apl-core is already pointed at %s branch %s, and no Gitea source "+
			"could be recovered to rebuild it from (%v). If that branch is intact this is harmless — the "+
			"migration has nothing to do. If it is MISSING, the Job will fail and the branch must be recreated "+
			"from apl-core's values tree by hand.\n", githubURL, dstBranch, err)
		src = ""
	}
	// apl-core creates its Gitea values repo on `main`.
	return src, "main", true, nil
}

// aplGiteaInClusterURL is apl-core's values repo at the address that is PROVEN
// reachable from a pod: the git-server Service DNS name. Confirmed by the e2e —
// the migration Job's clone works against exactly this URL, and apl-git-config's
// repoUrl holds it verbatim on a fresh managed cluster.
const aplGiteaInClusterURL = "http://git-server.git-server.svc.cluster.local/otomi/values.git"

// giteaSourceFromCloneCmd recovers a usable Gitea source from the gitCloneCmd
// field, which after migration is the only place apl-core's Gitea PASSWORD still
// exists (patchAplGitConfig overwrites username/password with the github coords).
//
// It keeps the CREDENTIALS from gitCloneCmd but discards its HOST. gitCloneCmd
// carries the PUBLIC name (git.<domainSuffix>), and apl-core deploys git-server
// with httproute.enabled=false — no public route — so cloning it from inside the
// cluster is not known to work. The clone that HAS been observed to succeed is of
// the in-cluster Service DNS URL, so this rebuilds against aplGiteaInClusterURL —
// the address with evidence behind it.
//
// NOT verified live: this path only runs on a re-bootstrap whose destination branch
// has gone missing, which no run has reproduced since the fix. It fails loudly if
// wrong.
func giteaSourceFromCloneCmd(cloneCmd string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(cloneCmd))
	if err != nil {
		return "", fmt.Errorf("apl-git-config gitCloneCmd is not a URL (%v) — cannot recover the Gitea credentials "+
			"needed to re-seed the values branch", err)
	}
	user := u.User.Username()
	pass, _ := u.User.Password()
	if user == "" || pass == "" {
		return "", fmt.Errorf("apl-git-config gitCloneCmd carries no embedded credentials, and username/password " +
			"now hold the github coords — there is no way to authenticate to apl-core's Gitea repo. Recreate the " +
			"values branch from apl-core's tree, or reset apl-git-config back to the in-cluster repo and re-run")
	}
	return basicAuthGitURL(aplGiteaInClusterURL, user, pass), nil
}

func migrateAplValuesToGitHub(d bootstrapDeps, cur aplGitConfig, githubURL, branch string, apps []string, tok string) error {
	srcAuth, srcBranch, skipIfDstExists, err := aplMigrationSource(cur, githubURL, branch)
	if err != nil {
		return err
	}
	if skipIfDstExists {
		fmt.Fprintf(os.Stderr, "apl-core is already pointed at %s branch %s — the Job will verify that branch "+
			"exists and rebuild it from apl-core's in-cluster values repo (branch %s) ONLY if it is missing.\n",
			githubURL, branch, srcBranch)
	}
	dstAuth := basicAuthGitURL(githubURL, "x-access-token", tok)
	// Redact the raw creds AND the full authed URLs (git echoes the percent-encoded URL
	// on error, where the raw password substring won't match).
	secrets := []string{tok, cur.password, srcAuth, dstAuth}
	del := func() {
		d.kubectl("-n", aplMigrateJobNS, "delete", "job", aplMigrateJobName, "--ignore-not-found")
		d.kubectl("-n", aplMigrateJobNS, "delete", "secret", aplMigrateJobName, "--ignore-not-found")
	}
	del() // clear any prior run so the Job template is not immutable-conflicted
	if out, ok := d.apply(aplMigrateSecretManifest(srcAuth, dstAuth), "llz-managed-bridge", false); !ok {
		return fmt.Errorf("apply apl-values migration Secret: %s", redactSecrets(out, secrets))
	}
	if out, ok := d.apply(aplMigrateJobManifest(srcBranch, branch, apps, skipIfDstExists), "llz-managed-bridge", false); !ok {
		del()
		return fmt.Errorf("apply apl-values migration Job: %s", redactSecrets(out, secrets))
	}
	defer del()

	// Attempt-bounded, not deadline-bounded — see waitAplGitConfig for why. This loop
	// was previously `for d.now().Before(deadline)` and was safe only because the test
	// fake reports success on the first poll; one behaviour change away from spinning
	// for the full real budget under `now: time.Now, sleep: func(time.Duration){}`.
	attempts := int(aplMigrateBudget / aplMigrateTick)
	if attempts < 1 {
		attempts = 1
	}
	for attempt := 1; attempt <= attempts; attempt++ {
		if s, _ := d.kubectl("-n", aplMigrateJobNS, "get", "job", aplMigrateJobName, "-o", "jsonpath={.status.succeeded}"); strings.TrimSpace(s) == "1" {
			return nil
		}
		// THE Failed CONDITION, NOT `.status.failed`. The Job sets backoffLimit: 1, so
		// Kubernetes runs the pod TWICE — and the moment the first pod fails,
		// `.status.failed` becomes 1 while the retry is still pending. Reading that as
		// terminal returned an error here, and the caller's deferred del() then deleted
		// the Job, killing the retry Kubernetes was about to run. The one attempt the
		// backoffLimit exists to provide could never happen.
		//
		// `.status.conditions[?(@.type=="Failed")].status == "True"` is the signal that
		// means the Job is done retrying — and it also covers activeDeadlineSeconds
		// being exceeded, which `.status.failed` alone does not distinguish.
		if c, _ := d.kubectl("-n", aplMigrateJobNS, "get", "job", aplMigrateJobName,
			"-o", `jsonpath={.status.conditions[?(@.type=="Failed")].status}`); strings.TrimSpace(c) == "True" {
			logs, _ := d.kubectl("-n", aplMigrateJobNS, "logs", "job/"+aplMigrateJobName, "--tail=40")
			return fmt.Errorf("apl-values migration Job failed: %s", redactSecrets(logs, secrets))
		}
		if attempt < attempts {
			d.sleep(aplMigrateTick)
		}
	}
	// Include the Job's own logs: "did not complete within 6m" alone says nothing
	// about whether it was waiting on a branch, failing to auth, or never scheduled.
	logs, _ := d.kubectl("-n", aplMigrateJobNS, "logs", "job/"+aplMigrateJobName, "--tail=40")
	return fmt.Errorf("apl-values migration Job did not complete within %s: %s",
		aplMigrateBudget, redactSecrets(logs, secrets))
}

// aplMigrateSecretManifest holds the credential-bearing clone/push URLs (kept out of
// the Job spec / logs — the Job pulls them via envFrom).
func aplMigrateSecretManifest(srcAuth, dstAuth string) string {
	return "apiVersion: v1\nkind: Secret\n" +
		"metadata:\n  name: " + aplMigrateJobName + "\n  namespace: " + aplMigrateJobNS + "\n" +
		"type: Opaque\nstringData:\n" +
		"  SRC_URL: " + yamlSingleQuote(srcAuth) + "\n" +
		"  DST_URL: " + yamlSingleQuote(dstAuth) + "\n"
}

// AplBranchStateScript is the shell function the migration Job uses to ask whether
// a branch exists on a remote. It is a NAMED CONSTANT rather than inline text so a
// test can run the exact same bytes the Job runs — see
// TestAplBranchStateSeparatesAbsentFromUnreachable. Extracting it by regex from the
// rendered manifest would test a copy of the thing under test.
//
// Rendered into the Job at ten spaces of indent; every line here is at column 0.
const AplBranchStateScript = `# Returns: 0 present, 1 absent, 2 could-not-ask.
branch_state() {
  if ! out=$(git ls-remote --heads "$1" "$2" 2>&1); then
    echo "::error::git ls-remote failed — cannot tell whether $2 exists: $out" >&2
    return 2
  fi
  printf '%s\n' "$out" | awk -v want="refs/heads/$2" '$2 == want { found = 1 } END { exit !found }'
}`

// indentScript re-indents a column-0 script block for embedding in a YAML literal
// scalar. Blank lines stay blank — trailing whitespace inside a `|` block is
// preserved by YAML and shows up as diff noise.
func indentScript(script, indent string) string {
	lines := strings.Split(script, "\n")
	for i, ln := range lines {
		if strings.TrimSpace(ln) != "" {
			lines[i] = indent + ln
		}
	}
	return strings.Join(lines, "\n")
}

// aplMigrateJobManifest builds the migration Job. The inline script's env/apps/<app>.yaml
// format MUST match aplAppEnableManifest (kind AplApp, spec.enabled:true).
func aplMigrateJobManifest(srcBranch, dstBranch string, apps []string, skipIfDstExists bool) string {
	return fmt.Sprintf(`apiVersion: batch/v1
kind: Job
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  backoffLimit: 1
  activeDeadlineSeconds: 660
  ttlSecondsAfterFinished: 180
  template:
    metadata:
      annotations:
        # Batch pod — keep it out of the istio mesh (apl-operator may have injection on;
        # an injected proxy never exits, so the Job would never complete).
        sidecar.istio.io/inject: "false"
    spec:
      restartPolicy: Never
      containers:
      - name: migrate
        image: %[3]s
        command: ["/bin/sh", "-c"]
        args:
        - |
          set -e
          # branch_state answers TWO questions that a bare "ls-remote | grep -q"
          # collapsed into one. ls-remote EXIT STATUS says whether the remote could be
          # ASKED; its OUTPUT says whether the branch is THERE. Piped straight into
          # grep, an ls-remote that failed on auth, DNS, a proxy or a rate limit
          # produces no output — indistinguishable from "the branch does not exist",
          # which is the answer that makes this Job FORCE-PUSH apl-core abandoned
          # in-cluster tree over a healthy $DST_BRANCH and revert every values change
          # made since the switch. That is the exact regression SKIP_IF_DST_EXISTS was
          # added to prevent.
          #
          # And the match is now on the WHOLE ref. A grep for "refs/heads/$B" is a
          # SUBSTRING test: "main" matches refs/heads/maintenance and "apl-primary"
          # matches refs/heads/apl-primary-backup, so a similarly-named branch could
          # answer for one that does not exist. awk compares the field exactly, which
          # also sidesteps a branch name containing regex metacharacters.
          #
%[8]s
          # RE-BOOTSTRAP REPAIR ONLY. apl-core has been reconciling $DST_BRANCH since the
          # first migration, and its Gitea tree has been abandoned since — so pushing that
          # stale tree over a healthy branch would revert every values change made after
          # the switch. Rebuild only when the branch is genuinely gone.
          if [ "$SKIP_IF_DST_EXISTS" = "true" ]; then
            branch_state "$DST_URL" "$DST_BRANCH" && dst=0 || dst=$?
            if [ "$dst" -eq 2 ]; then
              echo "refusing to rebuild $DST_BRANCH: the remote could not be queried, so a verdict of gone is a GUESS. Rebuilding on that guess force-pushes apl-core abandoned in-cluster tree over a branch that may be perfectly healthy." >&2
              exit 1
            fi
            if [ "$dst" -eq 0 ]; then
              echo "$DST_BRANCH already exists and apl-core is pointed at it — nothing to migrate."
              exit 0
            fi
            echo "::warning::$DST_BRANCH is MISSING while apl-core is pointed at it (half-migrated) — rebuilding it from apl-core in-cluster values repo."
            if [ -z "$SRC_URL" ]; then
              echo "cannot rebuild $DST_BRANCH: apl-core is pointed at it, the branch is gone, and apl-git-config carried no gitCloneCmd to recover the Gitea credentials from. Recreate the branch from apl-core values tree, or reset apl-git-config back to the in-cluster repo and re-run." >&2
              exit 1
            fi
          fi
          echo "waiting for apl-core values repo branch $SRC_BRANCH (apl-core may still be initializing)..."
          i=0
          # A could-not-ask here is RETRIED rather than fatal: apl-core in-cluster Gitea
          # is genuinely not up yet on a cold bootstrap, which is what this wait is for.
          # Same 48-attempt bound, and the failure names which of the two it ran out on.
          while :; do
            branch_state "$SRC_URL" "$SRC_BRANCH" && src=0 || src=$?
            [ "$src" -eq 0 ] && break
            i=$((i+1))
            if [ "$i" -gt 48 ]; then
              if [ "$src" -eq 2 ]; then echo "values repo $SRC_URL was never reachable (apl-core in-cluster Gitea not up, or the credentials in apl-git-config are wrong)" >&2; else echo "values repo branch $SRC_BRANCH never appeared (apl-core not ready); heads present:" >&2; git ls-remote --heads "$SRC_URL" >&2 || true; fi
              exit 1
            fi
            echo "  not ready (attempt $i, state $src); sleeping 10s..."
            sleep 10
          done
          echo "cloning apl-core values repo (branch $SRC_BRANCH)..."
          git clone --depth 1 --branch "$SRC_BRANCH" "$SRC_URL" /w
          mkdir -p /w/env/apps
          for a in $APPS; do
            printf 'kind: AplApp\nmetadata:\n  name: %%s\nspec:\n  enabled: true\n' "$a" > "/w/env/apps/$a.yaml"
          done
          cd /w
          git config user.email pipeline@cluster.local
          git config user.name llz-bootstrap
          # Orphan commit: a single self-contained snapshot of the full tree. Avoids the
          # shallow-clone push error ("did not receive expected object") and matches the
          # LLZ-owned-branch-superseded-each-bootstrap semantics (history is irrelevant —
          # apl-operator reset --hard's to the tip).
          git checkout --orphan llz-managed
          git add -A
          git commit -m "llz: enable managed apps + point apl-core at github"
          echo "force-pushing full values tree to github branch $DST_BRANCH..."
          git push --force "$DST_URL" "HEAD:refs/heads/$DST_BRANCH"
          echo "apl-values migration complete."
        env:
        - name: GIT_TERMINAL_PROMPT
          value: "0"
        - name: SRC_BRANCH
          value: %[4]s
        - name: DST_BRANCH
          value: %[5]s
        - name: APPS
          value: %[6]s
        - name: SKIP_IF_DST_EXISTS
          value: "%[7]t"
        envFrom:
        - secretRef:
            name: %[1]s
`, aplMigrateJobName, aplMigrateJobNS, aplMigrateImage,
		yamlSingleQuote(srcBranch), yamlSingleQuote(dstBranch), yamlSingleQuote(strings.Join(apps, " ")), skipIfDstExists,
		indentScript(AplBranchStateScript, "          "))
}

// yamlSingleQuote wraps a scalar in single quotes (doubling any embedded quote) so
// URL/branch values with :/@ can't be mis-parsed as YAML.
func yamlSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// patchAplGitConfig repoints apl-core at the github branch by merge-patching the
// BYO-Git Secret's stringData (preserving unrelated keys). username=x-access-token +
// the PAT is the same basic-auth pair the migration push used.
func patchAplGitConfig(d bootstrapDeps, githubURL, branch, tok string) error {
	patch := fmt.Sprintf(`{"stringData":{"repoUrl":%q,"branch":%q,"username":"x-access-token","password":%q}}`,
		githubURL, branch, tok)
	if out, ok := d.kubectl("-n", aplGitSecretNS, "patch", "secret", aplGitSecretName,
		"--type=merge", "-p", patch); !ok {
		return fmt.Errorf("patch %s/%s to github coords: %s", aplGitSecretNS, aplGitSecretName, redactSecret(strings.TrimSpace(out), tok))
	}
	return nil
}

// aplAppEnableManifest is the minimal apl-core values-repo file that enables an app:
// an AplApp manifest whose spec becomes apps.<name> in the merged values. Schema-valid
// for the LLZ default set (harbor has no app-level required; loki's adminPassword is an
// x-secret and, from apl-core 6.2.0, no longer `required` either).
func aplAppEnableManifest(app string) string {
	return "kind: AplApp\nmetadata:\n  name: " + app + "\nspec:\n  enabled: true\n"
}

// basicAuthGitURL injects a user:secret credential into an http(s) git URL so clone/push
// reach a private remote. The in-cluster values repo (git-server.<ns>.svc) is HTTP, the
// github instance repo HTTPS — both need the credential embedded (git has no tty to
// prompt in a Job/CI). The userinfo is percent-encoded via net/url so a generated
// password with reserved chars (&, /, @, :) can't corrupt the URL ("Bad hostname").
// An empty secret or a non-http(s)/unparseable URL is returned unchanged. The result
// carries a secret and must never be logged.
func basicAuthGitURL(rawURL, user, secret string) string {
	if secret == "" {
		return rawURL
	}
	if user == "" {
		user = "x-access-token"
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return rawURL
	}
	// Percent-encode the userinfo like encodeURIComponent (Go's url.UserPassword leaves
	// sub-delims such as & unescaped, which curl/git reject as a "Bad hostname"). Fix
	// QueryEscape's space→+ to %20 for correct userinfo.
	enc := func(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "+", "%20") }
	rest := u.Host + u.EscapedPath()
	if u.RawQuery != "" {
		rest += "?" + u.RawQuery
	}
	return u.Scheme + "://" + enc(user) + ":" + enc(secret) + "@" + rest
}

// redactSecrets masks each non-empty secret wherever it appears in command output.
func redactSecrets(s string, secrets []string) string {
	s = strings.TrimSpace(s)
	for _, sec := range secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "***")
		}
	}
	return s
}

// redactSecret masks a token wherever it appears in command output (git can echo a
// credential-bearing URL in errors), so nothing prints it to the CI log.
func redactSecret(s, secret string) string {
	s = strings.TrimSpace(s)
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "***")
}
