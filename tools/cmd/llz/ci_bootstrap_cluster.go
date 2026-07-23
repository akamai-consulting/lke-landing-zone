package main

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
	"bytes"
	_ "embed"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"
)

//go:embed manifests/block-storage-class.yaml
var blockStorageClassYAML []byte

// defaultAplChartVersion is the apl-core baseline this LLZ release tracks. On a
// managed cluster Linode owns the apl-core version, so bootstrap does not consume
// it; it survives as the single baseline other tooling asserts against
// (ci_assert_apl_version.go). Bump in lockstep when raising the platform baseline.
const defaultAplChartVersion = "6.0.0"

// bootstrapValuePlaceholders is the SECRETS-ONLY set of ${...} tokens a committed
// apl-values file may still carry after `llz render`. It remains the single source
// of truth for `llz ci validate-apl-values`'s offline var-contract check
// (ci_apl_schema.go), which asserts a rendered file references no OTHER ${...}.
var bootstrapValuePlaceholders = []string{
	"apl_values_repo_password",
	"linode_dns_token",
	"coredns_cluster_ip",
	"loki_admin_password",
}

// bootstrapFlags are the raw CLI inputs (identity via flags, secrets via env).
type bootstrapFlags struct {
	kubeconfig       string
	env              string
	appsRepoRevision string
	instanceRepo     string
	upstreamOrg      string
	templateRef      string
	dryRun           bool
}

// bootstrapClusterOpts is the resolved config the bridge apply consumes. Managed
// apl-core owns the install, so LLZ needs none of the self-install secrets — only
// the repo/env/ref identity the bridge Applications point at, plus the optional
// GHCR credentials for a private fork's first-party OCI charts/images.
type bootstrapClusterOpts struct {
	env              string
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

// bootstrapDeps are the seams the flow drives. kubectl runs one read/wait/patch
// invocation (KUBECONFIG wired) returning combined output + exit-0; apply pipes a
// manifest to `kubectl apply --server-side`; git clones/pushes the apl-core values
// repo (the BYO-Git migration to the apl-<env> github branch); now/sleep make the
// deadline loops testable.
type bootstrapDeps struct {
	kubectl func(args ...string) (string, bool)
	apply   func(stdinYAML, fieldManager string, force bool) (string, bool)
	git     func(args ...string) (string, bool)
	now     func() time.Time
	sleep   func(time.Duration)
}

func ciBootstrapClusterCmd() *cobra.Command {
	var f bootstrapFlags
	c := &cobra.Command{
		Use:   "bootstrap-cluster",
		Short: "layer LLZ's Argo bridge onto a Linode MANAGED App Platform cluster (apl_enabled)",
		Long: "Layers LLZ's extras onto a Linode-managed apl-core (apl_enabled=true).\n" +
			"Linode owns the apl-core install + the lke<id>.akamai-apl.net domain/DNS/cert,\n" +
			"so this command does NOT install apl-core. It waits for the managed ArgoCD to\n" +
			"be able to admit Applications, then server-side-applies the Argo bridge (the\n" +
			"platform-bootstrap AppProject + Application + the llz-secret-store Application),\n" +
			"the named non-default block-storage-retain StorageClass the OpenBao PVC\n" +
			"references, and the llz-openbao namespace. Idempotent (server-side apply), so CI\n" +
			"re-runs it every apply. Reads the kubeconfig from --kubeconfig (or\n" +
			"KUBECONFIG_RAW), and the optional private-fork GHCR creds from\n" +
			"GHCR_USERNAME / GHCR_TOKEN.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runBootstrapCluster(f) },
	}
	fl := c.Flags()
	fl.StringVar(&f.kubeconfig, "kubeconfig", "", "path to the cluster kubeconfig (from the fetch-kubeconfig action); falls back to $KUBECONFIG_RAW")
	fl.StringVar(&f.env, "env", "", "apl-values environment subdir, e.g. primary (required)")
	fl.StringVar(&f.appsRepoRevision, "apps-repo-revision", "", "bootstrap Application targetRevision (default: spec, then main)")
	fl.StringVar(&f.instanceRepo, "instance-repo", "", "owner/name of the instance repo (bootstrap App source) (required)")
	fl.StringVar(&f.upstreamOrg, "upstream-org", "akamai-consulting", "template repo org (llz-secret-store App source + AppProject sourceRepos)")
	fl.StringVar(&f.templateRef, "template-ref", "main", "template repo ref for the llz-secret-store Application")
	fl.BoolVar(&f.dryRun, "dry-run", false, "print the intended actions without touching the cluster")
	return c
}

// runBootstrapCluster is the single (managed App Platform) bootstrap path. LLZ
// only LAYERS its extras onto the Linode-managed apl-core via the Argo bridge; it
// needs none of the self-install secrets — no value rendering, no otomi.git seed,
// no Linode-DNS token — only the repo/env/ref identity the bridge points at.
func runBootstrapCluster(f bootstrapFlags) error {
	if f.env == "" {
		return fmt.Errorf("--env is required (the apl-values subdir, e.g. primary)")
	}
	if f.instanceRepo == "" {
		return fmt.Errorf("--instance-repo is required (owner/name of the instance repo the bootstrap Application syncs from)")
	}

	o := bootstrapClusterOpts{
		env:              f.env,
		instanceRepo:     f.instanceRepo,
		upstreamOrg:      firstNonEmpty(f.upstreamOrg, "akamai-consulting"),
		templateRef:      firstNonEmpty(f.templateRef, "main"),
		appsRepoRevision: f.appsRepoRevision,
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
	// apps-repo-revision + managedApps come from the spec (Defaults() populates
	// managedApps to the LLZ set on managed).
	if lz, present, err := loadSpec(); present && err == nil {
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

	kubeconfigPath, cleanup, err := resolveKubeconfig(f.kubeconfig)
	if err != nil {
		return err
	}
	defer cleanup()

	if f.dryRun {
		return dryRunBootstrap(o, kubeconfigPath)
	}
	return bootstrapCluster(o, newBootstrapDeps(kubeconfigPath))
}

// newBootstrapDeps wires the kubectl/apply/clock seams the bridge apply drives,
// all pointed at the resolved kubeconfig.
func newBootstrapDeps(kubeconfigPath string) bootstrapDeps {
	return bootstrapDeps{
		kubectl: func(args ...string) (string, bool) {
			cmd := exec.Command("kubectl", args...)
			if kubeconfigPath != "" {
				cmd.Env = envWithKubeconfig(kubeconfigPath)
			}
			return runCombined(cmd)
		},
		apply: func(stdinYAML, fieldManager string, force bool) (string, bool) {
			args := []string{"apply", "--server-side", "--field-manager=" + fieldManager}
			if force {
				args = append(args, "--force-conflicts")
			}
			args = append(args, "-f", "-")
			cmd := exec.Command("kubectl", args...)
			if kubeconfigPath != "" {
				cmd.Env = envWithKubeconfig(kubeconfigPath)
			}
			cmd.Stdin = strings.NewReader(stdinYAML)
			return runCombined(cmd)
		},
		git: func(args ...string) (string, bool) {
			// git talks to the remote values repo, NOT the cluster — no kubeconfig.
			return runCombined(exec.Command("git", args...))
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

	// block-storage-retain StorageClass. The extras (OpenBao raft PVC, etc.)
	// reference it BY NAME, but managed apl-core doesn't create it — its default is
	// `linode-block-storage-retain`. Apply LLZ's class (same Linode-CSI provisioner,
	// encryption + Retain) as a NON-default class: the named PVCs bind, and we do
	// NOT create a second cluster-default (that would race managed apl-core's own
	// default).
	scYAML, err := managedBlockStorageClassYAML()
	if err != nil {
		return fmt.Errorf("prepare managed block-storage-retain StorageClass: %w", err)
	}
	// force=false: LLZ is the sole owner of this class and re-applies it under the
	// same field manager, so there's no cross-manager conflict to force through. If a
	// future re-bootstrap ever picks up a different field manager on it, switch to true.
	if out, ok := d.apply(scYAML, "llz-managed-bridge", false); !ok {
		fmt.Fprint(os.Stderr, out)
		return fmt.Errorf("apply managed block-storage-retain StorageClass")
	}

	// llz-openbao namespace. The OpenBao component is CreateNamespace=false and ships
	// no namespace.yaml, and managed apl-core does not create it, so without this the
	// OpenBao Application can never sync and the downstream bootstrap-openbao seal-key
	// seed times out on `namespaces "llz-openbao" not found`.
	if err := applyManifest(d, llzOpenbaoNamespaceManifest(), "llz-managed-bridge", true); err != nil {
		return err
	}

	if err := waitManagedArgoReady(d); err != nil {
		return err
	}

	// Point the managed apl-core at LLZ's github values branch (BYO Git) and enable the
	// default apps (harbor/loki/grafana/kyverno) so apl-core INSTALLS them and LLZ's
	// extras layer on apl-core's own installs. See ADR 0006. Best-effort: on failure it
	// warns rather than aborting the bridge (the extras that need a default app degrade
	// instead of wedging; kyverno-dependent imageSignature stays render-gated as backup).
	if err := configureManagedApl(o, d); err != nil {
		warn(fmt.Sprintf("managed apl-core BYO-Git + default-apps config failed (%v) — the Argo bridge still applies; apps that need apl-core installs (harbor/loki/grafana/kyverno) may not converge until this succeeds.", err))
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

// managedBlockStorageClassYAML returns the embedded block-storage-retain
// StorageClass with the `storageclass.kubernetes.io/is-default-class` annotation
// stripped, so applying it on a managed cluster adds the named class WITHOUT
// creating a second cluster-default (managed apl-core already owns the default).
// Comments are dropped in the round-trip; the resource semantics are preserved.
func managedBlockStorageClassYAML() (string, error) {
	var m map[string]any
	if err := yaml.Unmarshal(blockStorageClassYAML, &m); err != nil {
		return "", fmt.Errorf("unmarshal block-storage-class: %w", err)
	}
	if meta, ok := m["metadata"].(map[string]any); ok {
		if ann, ok := meta["annotations"].(map[string]any); ok {
			delete(ann, "storageclass.kubernetes.io/is-default-class")
			if len(ann) == 0 {
				delete(meta, "annotations")
			}
		}
	}
	out, err := yaml.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("marshal managed block-storage-class: %w", err)
	}
	return string(out), nil
}

// dryRunBootstrap prints the intended actions without touching the cluster — the
// operator-facing replacement for `terraform plan` on this layer.
func dryRunBootstrap(o bootstrapClusterOpts, kubeconfigPath string) error {
	fmt.Printf("→ (dry-run) bootstrap-cluster (managed App Platform / apl_enabled) env=%s kubeconfig=%s\n", o.env, kubeconfigPath)
	fmt.Printf("  0. kubectl apply --server-side block-storage-retain StorageClass (NON-default; managed keeps its own default)\n")
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

// resolveKubeconfig returns a filesystem path the KUBECONFIG env can point at,
// in priority order: an explicit --kubeconfig file (if non-empty); the EFFECTIVE
// kubeconfig — $KUBECONFIG if it points at a NON-EMPTY file, else kubectl's default
// ~/.kube/config (reusing ci diagnose-argocd's effectiveKubeconfig); else
// KUBECONFIG_RAW spilled to a 0600 tempfile.
//
// The non-empty check is load-bearing: fetch-kubeconfig writes the real kubeconfig
// to ~/.kube/config and most steps rely on that default, while $KUBECONFIG is
// often an EMPTY placeholder file. Using $KUBECONFIG blindly made kubectl read an
// empty config and every read returned empty (the e2e bootstrap failure). Only the
// tempfile path needs cleanup; the rest are no-ops.
func resolveKubeconfig(path string) (string, func(), error) {
	noop := func() {}
	// 1. Explicit --kubeconfig (non-empty file) → override the child env with it.
	if path != "" {
		if st, err := os.Stat(path); err == nil && st.Size() > 0 {
			return path, noop, nil
		}
	}
	// 2. KUBECONFIG_RAW → spill to a 0600 tempfile → override.
	if raw := os.Getenv("KUBECONFIG_RAW"); raw != "" {
		path, cleanup, err := writeTempKubeconfig("llz-bootstrap-kubeconfig-*", []byte(raw))
		if err != nil {
			return "", noop, err
		}
		return path, cleanup, nil
	}
	// 3. Otherwise INHERIT the ambient environment — let kubectl/helm resolve
	//    $KUBECONFIG / ~/.kube/config THEMSELVES, exactly like wait-cluster-ready +
	//    diagnose-argocd, which read the cluster fine. An empty return path signals
	//    "do not touch the child env". Fail loudly only when nothing is resolvable.
	if effectiveKubeconfig() == "" {
		return "", noop, fmt.Errorf("no usable kubeconfig: pass --kubeconfig, set a non-empty $KUBECONFIG or ~/.kube/config, or set KUBECONFIG_RAW")
	}
	return "", noop, nil
}

// runCombined runs cmd with stdout+stderr captured into one buffer and returns
// (combined output, exit-0). The run MUST happen before the buffer is read:
// `return buf.String(), cmd.Run() == nil` evaluates its operands left-to-right,
// snapshotting the buffer EMPTY before the command ever executes. That exact
// bug made every kubectl call return "" on the e2e bootstrap (the
// "empty kubeconfig" red herring) — see TestRunCombined_OutputAfterRun.
func runCombined(cmd *exec.Cmd) (string, bool) {
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	ok := cmd.Run() == nil
	return buf.String(), ok
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
}

const (
	aplGitSecretName = "apl-git-config"
	aplGitSecretNS   = "apl-secrets"
)

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

	// 1. Read apl-core's current values-repo coordinates (the Gitea default on managed).
	cur, err := readAplGitConfig(d)
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
func readAplGitConfig(d bootstrapDeps) (aplGitConfig, error) {
	get := func(key string) (string, error) {
		out, ok := d.kubectl("-n", aplGitSecretNS, "get", "secret", aplGitSecretName,
			"-o", "jsonpath={.data."+key+"}")
		if !ok {
			return "", fmt.Errorf("read %s/%s: is apl-core installed? %s", aplGitSecretNS, aplGitSecretName, strings.TrimSpace(out))
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
	if c.repoURL == "" {
		return c, fmt.Errorf("%s/%s has no repoUrl — apl-core not yet BYO-Git-configured", aplGitSecretNS, aplGitSecretName)
	}
	if c.branch == "" {
		c.branch = "main"
	}
	return c, nil
}

// migrateAplValuesToGitHub clones apl-core's CURRENT values tree, layers an AplApp
// enable file per app under env/apps/, and pushes the whole tree to the github
// apl-<env> branch (force — this branch is LLZ-owned, superseded every bootstrap).
func migrateAplValuesToGitHub(d bootstrapDeps, cur aplGitConfig, githubURL, branch string, apps []string, tok string) error {
	tmp, err := os.MkdirTemp("", "llz-apl-values-*")
	if err != nil {
		return fmt.Errorf("mktemp for apl-values migration: %w", err)
	}
	defer os.RemoveAll(tmp)

	srcAuth := basicAuthGitURL(cur.repoURL, cur.username, cur.password)
	secrets := []string{tok, cur.password}
	if out, ok := d.git("clone", "--depth", "1", "--branch", cur.branch, srcAuth, tmp); !ok {
		return fmt.Errorf("clone apl-core values tree from its current remote (branch %s): %s", cur.branch, redactSecrets(out, secrets))
	}

	// Layer the app-enable files onto the cloned tree. env/apps/<name>.yaml, kind
	// AplApp, spec.enabled:true — the operator keys the app off the FILENAME and reads
	// data.spec, so both the path and the spec wrapper are load-bearing.
	appsDir := filepath.Join(tmp, "env", "apps")
	if err := os.MkdirAll(appsDir, 0o755); err != nil {
		return fmt.Errorf("mkdir env/apps: %w", err)
	}
	for _, app := range apps {
		if err := os.WriteFile(filepath.Join(appsDir, app+".yaml"), []byte(aplAppEnableManifest(app)), 0o644); err != nil {
			return fmt.Errorf("write env/apps/%s.yaml: %w", app, err)
		}
	}

	if out, ok := d.git("-C", tmp, "add", "-A"); !ok {
		return fmt.Errorf("git add apl-values: %s", redactSecrets(out, secrets))
	}
	if out, ok := d.git("-C", tmp,
		"-c", "user.name=llz-bootstrap", "-c", "user.email=llz-bootstrap@noreply.local",
		"commit", "--allow-empty", "-m", "chore(llz): enable managed apps ["+strings.Join(apps, ",")+"] + point apl-core at github"); !ok {
		return fmt.Errorf("git commit apl-values: %s", redactSecrets(out, secrets))
	}
	dstAuth := basicAuthGitURL(githubURL, "x-access-token", tok)
	if out, ok := d.git("-C", tmp, "push", "--force", dstAuth, "HEAD:refs/heads/"+branch); !ok {
		return fmt.Errorf("push apl-values to %s (branch %s) — does APL_VALUES_REPO_TOKEN have Contents:write? %s",
			githubURL, branch, redactSecrets(out, secrets))
	}
	return nil
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
// x-secret, stripped from `required` before validation).
func aplAppEnableManifest(app string) string {
	return "kind: AplApp\nmetadata:\n  name: " + app + "\nspec:\n  enabled: true\n"
}

// basicAuthGitURL injects a user:secret credential into an https git URL so clone/push
// reach a private remote. Non-https URLs (or an empty secret) are returned unchanged.
// The result carries a secret and must never be logged.
func basicAuthGitURL(rawURL, user, secret string) string {
	const p = "https://"
	if secret == "" || !strings.HasPrefix(rawURL, p) {
		return rawURL
	}
	if user == "" {
		user = "x-access-token"
	}
	return p + user + ":" + secret + "@" + strings.TrimPrefix(rawURL, p)
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
