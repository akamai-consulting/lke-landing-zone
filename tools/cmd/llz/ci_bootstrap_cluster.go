package main

// ci_bootstrap_cluster.go implements `llz ci bootstrap-cluster` — the native
// port of the entire cluster-bootstrap Terraform workspace (instance-template
// terraform-iac-bootstrap/cluster-bootstrap + terraform-modules/
// llz-cluster-bootstrap). Terraform now owns day-0 infra ONLY (vpc, cluster,
// object-storage); the in-cluster bootstrap — the apl-core Helm install, the
// StorageClass + namespace + Argo-bridge server-side applies, the apl-pipeline
// readiness gate, and the two race-ahead Kyverno policies — runs here instead,
// driven from CI with a kubeconfig. ArgoCD/apl-core own everything day-2.
//
// WHY IT LEFT TERRAFORM — this layer was `helm_release` + a tree of
// `kubectl_manifest` server-side applies + `null_resource` local-execs that
// already shelled out to `llz ci wait-apl-pipeline` / `llz ci
// apply-kyverno-policy`. It fought Terraform's plan/apply/state model hardest
// (provider bootstrap from a mid-apply kubeconfig, `state rm` destroy surgery,
// lifecycle ignore_changes handing ownership to in-cluster controllers), while
// its imperative building blocks were already Go. Porting it removes the state
// backend, the destroy-time `state rm` dance, and any provisioner that could
// fire against a live cluster.
//
// CONVERGENCE CONTRACT — see docs/architecture/convergence-contract.md. This
// command returning 0 means "every bootstrap resource was placed AND the
// apl-operator pipeline reached the hand-off state" (enforced by the loud
// `waitAplPipeline` gate below). The deep-convergence verdict remains `llz ci
// converge` at the tail of llz-bootstrap-openbao.yml, unchanged.
//
// ORDERING + RACE SEMANTICS are ported verbatim from the module. The one
// non-obvious requirement: the two Kyverno policies must race AHEAD of the
// argocd/cert-manager readiness gate (in TF they depended only on
// helm_release.apl, never null_resource.apl_pipeline_ready) — the extra ~minute
// the gate waits is exactly the window in which apl-operator's helmfile creates
// the oauth2-proxy redis PVC unmutated. So step 9 runs the policies CONCURRENTLY
// with the gate. See bootstrapCluster.
//
// The kubectl/helm/clock/password seams are injected (bootstrapDeps) so the
// whole flow is unit-tested without a cluster; the two reused funcs
// (waitAplPipeline, applyKyvernoPolicy) are called in-process, not re-shelled.

import (
	"bytes"
	"crypto/rand"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"
)

//go:embed manifests/block-storage-class.yaml
var blockStorageClassYAML []byte

//go:embed manifests/kyverno-pvc-encrypted-storage-class.yaml
var kyvernoPVCEncryptedYAML []byte

//go:embed manifests/kyverno-sc-default-demote.yaml
var kyvernoSCDefaultDemoteYAML []byte

// defaultAplChartVersion is the apl-core chart version an instance deploys when
// neither --apl-chart-version nor spec.cluster.bootstrap.aplChartVersion (an
// OPTIONAL field) is set. It mirrors the pinned default the retired
// cluster-bootstrap terraform.tfvars.example carried (apl_chart_version =
// "6.0.0"), so removing that workspace did not silently change what deploys.
// Bump this in lockstep when upgrading the platform's baseline apl-core.
const defaultAplChartVersion = "6.0.0"

// bootstrapValuePlaceholders is the SECRETS-ONLY set of ${...} tokens the
// committed apl-values/<env>/values.yaml still carries after `llz render`
// resolves everything else from the spec. It is the single source of truth for
// both this command's injectRuntimeValues (below) AND `llz ci
// validate-apl-values`'s offline var-contract check (ci_apl_schema.go) — the
// former FILLS them, the latter asserts a rendered file references no OTHER
// ${...} (the apl_values_repo_url class of stale placeholder). Ported from the
// former cluster-bootstrap/main.tf templatefile() var map.
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
	aplChartVersion  string
	appsRepoRevision string
	instanceRepo     string
	upstreamOrg      string
	templateRef      string
	valuesDir        string
	dryRun           bool
}

// bootstrapClusterOpts is the resolved config bootstrapCluster consumes (flags +
// spec defaults + env secrets), mirroring the former TF_VAR_* set.
type bootstrapClusterOpts struct {
	env              string
	aplChartVersion  string
	appsRepoRevision string
	instanceRepo     string
	upstreamOrg      string
	templateRef      string
	valuesPath       string // apl-values/<env>/values.yaml
	envRevisionPath  string // apl-values/<env>/manifest/env-revision-configmap.yaml

	aplValuesRepoToken string // APL_VALUES_REPO_TOKEN
	linodeDNSToken     string // LINODE_DNS_TOKEN
	ghcrUsername       string // GHCR_USERNAME (optional)
	ghcrToken          string // GHCR_TOKEN (optional)
}

// bootstrapDeps are the seams the flow drives. kubectl runs one read/wait
// invocation (KUBECONFIG wired) returning combined output + exit-0; apply pipes
// a manifest to `kubectl apply --server-side`; helm runs one helm invocation;
// now/sleep make the deadline loops testable; genPassword is the loki
// first-install password source (crypto/rand in prod, deterministic in tests).
type bootstrapDeps struct {
	kubectl     func(args ...string) (string, bool)
	apply       func(stdinYAML, fieldManager string, force bool) (string, bool)
	helm        func(args ...string) (string, bool)
	git         func(args ...string) (string, bool)
	now         func() time.Time
	sleep       func(time.Duration)
	genPassword func() string
}

func ciBootstrapClusterCmd() *cobra.Command {
	var f bootstrapFlags
	c := &cobra.Command{
		Use:   "bootstrap-cluster",
		Short: "install apl-core + the Argo bridge on a fresh cluster (native port of the cluster-bootstrap TF workspace)",
		Long: "Drives the in-cluster bootstrap that used to be the cluster-bootstrap\n" +
			"Terraform workspace: reads the live coredns ClusterIP, injects the runtime\n" +
			"secrets into the committed apl-values, server-side-applies the block-storage\n" +
			"StorageClass + apl-operator/argocd namespaces, `helm upgrade --install`s\n" +
			"apl-core, then — CONCURRENTLY — waits for the apl-operator pipeline\n" +
			"(argocd/kyverno/cert-manager) to serve while racing the two Kyverno policies\n" +
			"ahead of it, and finally applies the platform-bootstrap AppProject +\n" +
			"Applications. Idempotent (server-side apply + `helm upgrade --install`), so CI\n" +
			"re-runs it every apply; the apl-core chart version is spec-driven. Reads the\n" +
			"kubeconfig from --kubeconfig (or KUBECONFIG_RAW), and the secrets from\n" +
			"APL_VALUES_REPO_TOKEN / LINODE_DNS_TOKEN / GHCR_USERNAME / GHCR_TOKEN.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runBootstrapCluster(f) },
	}
	fl := c.Flags()
	fl.StringVar(&f.kubeconfig, "kubeconfig", "", "path to the cluster kubeconfig (from the fetch-kubeconfig action); falls back to $KUBECONFIG_RAW")
	fl.StringVar(&f.env, "env", "", "apl-values environment subdir, e.g. primary (required)")
	fl.StringVar(&f.aplChartVersion, "apl-chart-version", "", "apl-core chart version (default: spec.cluster.bootstrap.aplChartVersion)")
	fl.StringVar(&f.appsRepoRevision, "apps-repo-revision", "", "bootstrap Application targetRevision (default: spec, then main)")
	fl.StringVar(&f.instanceRepo, "instance-repo", "", "owner/name of the instance repo (bootstrap App source) (required)")
	fl.StringVar(&f.upstreamOrg, "upstream-org", "akamai-consulting", "template repo org (llz-secret-store App source + AppProject sourceRepos)")
	fl.StringVar(&f.templateRef, "template-ref", "main", "template repo ref for the llz-secret-store Application")
	fl.StringVar(&f.valuesDir, "values-dir", "", "apl-values root holding <env>/values.yaml (default: auto-detected instance layout)")
	fl.BoolVar(&f.dryRun, "dry-run", false, "print the intended actions without touching the cluster")
	return c
}

func runBootstrapCluster(f bootstrapFlags) error {
	if f.env == "" {
		return fmt.Errorf("--env is required (the apl-values subdir, e.g. primary)")
	}
	if f.instanceRepo == "" {
		return fmt.Errorf("--instance-repo is required (owner/name of the instance repo the bootstrap Application syncs from)")
	}

	// Resolve the apl-values root: explicit flag, else the auto-detected layout
	// (instance-template/apl-values in the template repo, apl-values in a rendered
	// instance) — the same resolution `llz render` uses.
	valuesDir := f.valuesDir
	if valuesDir == "" {
		_, aplDir, _ := instanceLayout()
		valuesDir = aplDir
	}

	o := bootstrapClusterOpts{
		env:                f.env,
		aplChartVersion:    f.aplChartVersion,
		appsRepoRevision:   f.appsRepoRevision,
		instanceRepo:       f.instanceRepo,
		upstreamOrg:        firstNonEmpty(f.upstreamOrg, "akamai-consulting"),
		templateRef:        firstNonEmpty(f.templateRef, "main"),
		valuesPath:         filepath.Join(valuesDir, f.env, "values.yaml"),
		envRevisionPath:    filepath.Join(valuesDir, f.env, "manifest", "env-revision-configmap.yaml"),
		aplValuesRepoToken: os.Getenv("APL_VALUES_REPO_TOKEN"),
		linodeDNSToken:     os.Getenv("LINODE_DNS_TOKEN"),
		ghcrUsername:       os.Getenv("GHCR_USERNAME"),
		ghcrToken:          os.Getenv("GHCR_TOKEN"),
	}
	if o.aplValuesRepoToken == "" {
		return fmt.Errorf("APL_VALUES_REPO_TOKEN must be set (rendered into apl-core otomi.git.password)")
	}
	if o.linodeDNSToken == "" {
		// CI passes a non-blocking placeholder when the secret isn't provisioned;
		// an empty value would leave a literal ${linode_dns_token} in the values
		// and fail apl-core's schema. Require non-empty (placeholder is fine).
		return fmt.Errorf("LINODE_DNS_TOKEN must be set (rendered into dns.provider.linode.apiToken; a placeholder is acceptable)")
	}

	// Spec-derived defaults: chart version + apps-repo-revision come from the
	// LandingZone spec when the flag is unset (the spec is the single source of
	// truth `llz render` also reads). apps-repo-revision falls back to "main".
	if o.aplChartVersion == "" || o.appsRepoRevision == "" {
		if lz, present, err := loadSpec(); present && err == nil {
			if e, ok := lz.Env(o.env); ok {
				if o.aplChartVersion == "" {
					o.aplChartVersion = e.Cluster.Bootstrap.AplChartVersion
				}
				if o.appsRepoRevision == "" {
					o.appsRepoRevision = e.Cluster.Bootstrap.AppsRepoRevision
				}
			}
		} else if err != nil {
			return fmt.Errorf("load spec for chart-version/apps-repo-revision defaults: %w", err)
		}
	}
	o.appsRepoRevision = firstNonEmpty(o.appsRepoRevision, "main")
	// spec.cluster.bootstrap.aplChartVersion is OPTIONAL (the example marks it so);
	// fall back to the baked default, mirroring the pinned default the retired
	// cluster-bootstrap terraform.tfvars.example carried. Without this, any instance
	// that omits the optional field (including the release-e2e instance) would fail
	// here instead of deploying the same version it used to.
	o.aplChartVersion = firstNonEmpty(o.aplChartVersion, defaultAplChartVersion)

	// Resolve the kubeconfig into a file the seams point KUBECONFIG at: an
	// existing --kubeconfig path (what the fetch-kubeconfig action writes) is used
	// as-is; otherwise KUBECONFIG_RAW is spilled to a 0600 tempfile (the same
	// contract wait-apl-pipeline / apply-kyverno-policy honor).
	kubeconfigPath, cleanup, err := resolveKubeconfig(f.kubeconfig)
	if err != nil {
		return err
	}
	defer cleanup()

	if f.dryRun {
		return dryRunBootstrap(o, kubeconfigPath)
	}

	d := bootstrapDeps{
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
		helm: func(args ...string) (string, bool) {
			cmd := exec.Command("helm", args...)
			if kubeconfigPath != "" {
				cmd.Env = envWithKubeconfig(kubeconfigPath)
			}
			return runCombined(cmd)
		},
		git: func(args ...string) (string, bool) {
			// git talks to the remote values repo, NOT the cluster — no kubeconfig.
			return runCombined(exec.Command("git", args...))
		},
		now:         time.Now,
		sleep:       time.Sleep,
		genPassword: genLokiPassword,
	}
	return bootstrapCluster(o, d)
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
		tmp, err := os.CreateTemp("", "llz-bootstrap-kubeconfig-*")
		if err != nil {
			return "", noop, fmt.Errorf("create kubeconfig tempfile: %w", err)
		}
		if _, err := tmp.WriteString(raw); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return "", noop, fmt.Errorf("write kubeconfig: %w", err)
		}
		tmp.Close()
		return tmp.Name(), func() { os.Remove(tmp.Name()) }, nil
	}
	// 3. Otherwise INHERIT the ambient environment — let kubectl/helm resolve
	//    $KUBECONFIG / ~/.kube/config THEMSELVES, exactly like wait-cluster-ready +
	//    diagnose-argocd, which read the cluster fine. Re-resolving the path here and
	//    overriding the child's KUBECONFIG instead made kubectl read an empty config
	//    on the e2e (a $RUNNER_TEMP-vs-runner.temp / stat-vs-kubectl mismatch). An
	//    empty return path signals "do not touch the child env". Fail loudly only
	//    when nothing is resolvable at all.
	if effectiveKubeconfig() == "" {
		return "", noop, fmt.Errorf("no usable kubeconfig: pass --kubeconfig, set a non-empty $KUBECONFIG or ~/.kube/config, or set KUBECONFIG_RAW")
	}
	return "", noop, nil
}

// runCombined runs cmd with stdout+stderr captured into one buffer and returns
// (combined output, exit-0). The run MUST happen before the buffer is read:
// `return buf.String(), cmd.Run() == nil` evaluates its operands left-to-right,
// snapshotting the buffer EMPTY before the command ever executes. That exact
// bug made every kubectl/helm call return "" on the e2e bootstrap (the
// "empty kubeconfig" red herring) — see TestRunCombined_OutputAfterRun.
func runCombined(cmd *exec.Cmd) (string, bool) {
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	ok := cmd.Run() == nil
	return buf.String(), ok
}

// envWithKubeconfig returns the process env with KUBECONFIG set to exactly `path`
// — dropping any inherited KUBECONFIG first so kubectl/helm can't read a duplicate
// (often empty) entry instead. Duplicate KUBECONFIG env keys are resolved
// inconsistently, which is how an empty placeholder $KUBECONFIG shadowed the real
// resolved path in the e2e.
func envWithKubeconfig(path string) []string {
	src := os.Environ()
	env := make([]string, 0, len(src)+1)
	for _, e := range src {
		if strings.HasPrefix(e, "KUBECONFIG=") {
			continue
		}
		env = append(env, e)
	}
	return append(env, "KUBECONFIG="+path)
}

// bootstrapCluster runs the ordered flow (the numbered steps mirror the former
// module's resource graph). Every kubectl apply is server-side (idempotent by
// construction, so CI re-runs are safe); helm is `upgrade --install`.
func bootstrapCluster(o bootstrapClusterOpts, d bootstrapDeps) error {
	// ── 1. coredns ClusterIP (was data.kubernetes_service.coredns) ──
	// nginx in the loki-gateway needs the DNS Service *IP* as its `resolver`; the
	// chart otherwise templates a hostname nginx can't use and crashloops. Read it
	// live and render it in. BEST-EFFORT / NON-FATAL: if it can't be resolved we
	// warn and proceed with "" rather than blocking the whole bootstrap on this one
	// read (the loki gateway is a downstream, separately-fixable concern; the next
	// step's SSA apply is the real "does kubectl work" signal). Mirrors the old TF
	// `try(data.kubernetes_service.coredns…, "")`.
	coreDNSIP := readCoreDNSClusterIP(d)

	// ── 2. Render values (was templatefile + random_password.loki_admin) ──
	// Fill the four secrets-only placeholders in the committed values.yaml. The
	// loki admin password is FIRST-INSTALL-ONLY: on an upgrade we read the value
	// helm already stored so it does not churn across CI re-runs (apl-core v6
	// self-generates the real in-cluster credential; our value only satisfies the
	// chart's schema at install). Hard-fails if any OTHER ${...} survives.
	rawValues, err := os.ReadFile(o.valuesPath)
	if err != nil {
		return fmt.Errorf("read apl-values %s: %w", o.valuesPath, err)
	}
	lokiPassword := existingLokiPassword(d)
	if lokiPassword == "" {
		lokiPassword = d.genPassword()
	}
	rendered, err := injectRuntimeValues(string(rawValues), map[string]string{
		"apl_values_repo_password": o.aplValuesRepoToken,
		"linode_dns_token":         o.linodeDNSToken,
		"coredns_cluster_ip":       coreDNSIP,
		"loki_admin_password":      lokiPassword,
	})
	if err != nil {
		return fmt.Errorf("render %s: %w", o.valuesPath, err)
	}

	// ── 3. env-revision precondition (was the local_file lifecycle precondition) ──
	// The manifest tree's env-revision-configmap.data.revision drives every in-repo
	// child Application's targetRevision; it MUST match the bootstrap App's own
	// revision or child Apps sync a different branch. Catch it before any mutation.
	if err := assertEnvRevision(o.envRevisionPath, o.appsRepoRevision); err != nil {
		return err
	}

	// ── 4. apl-operator namespace (SSA, pre-tagged for Helm adoption) ──
	// The chart ships templates/00-namespace.yaml; pre-stamping the three Helm
	// ownership markers lets the install adopt the existing namespace instead of
	// erroring "cannot be imported into the current release", and makes a
	// failed-then-retried apply idempotent.
	if err := applyManifest(d, aplOperatorNamespaceManifest(), "cluster-bootstrap-tf", true); err != nil {
		return err
	}

	// ── 5. block-storage-retain StorageClass (SSA) ──
	// Must exist in the apiserver before apl-operator's helmfile starts the first
	// PVC-creating chart; landing it here (not via an Argo sync-wave) closes that
	// race. Field-manager name kept stable for SSA ownership continuity.
	if out, ok := d.apply(string(blockStorageClassYAML), "cluster-bootstrap-tf", false); !ok {
		fmt.Fprint(os.Stderr, out)
		return fmt.Errorf("apply block-storage StorageClass failed")
	}

	// ── 6a. Ensure the apl-<env> values branch exists — BEFORE the helm install ──
	// Ordering is load-bearing. apl-operator's INSTALLER phase starts the moment the
	// chart deploys, and it is the only phase that bootstraps the full env values
	// into otomi.git.branch. On the e2e cluster the installer completed while the
	// branch was absent (it bootstrapped locally, pushed nothing, marked
	// installation completed) and every later reconcile re-cloned the branch, found
	// no values, and crashed its derived-values template ("map has no entry for key
	// customRootCA") — a wedge only un-doable by resetting apl-installation-status.
	// Seeding the branch FIRST means the installer finds it and pushes its full env
	// tree as part of installation, on fresh and reused clusters alike.
	repoURL, branch := aplValuesGitCoords(rendered)
	seedSHA, err := ensureAplValuesBranch(d, o, repoURL, branch)
	if err != nil {
		return err
	}

	// ── 6. apl-core (helm upgrade --install) ──
	_, aplStatus, aplFound := deployedAplRelease(d)
	aplWasDeployed := aplFound && aplStatus == "deployed"
	installSkipped, err := helmInstallApl(d, o, rendered)
	if err != nil {
		return err
	}

	// ── 6b. Re-arm the operator's installer when needed ── only the INSTALLER
	// phase bootstraps the full env values into the branch AND ingests changed
	// helm values (VALUES_INPUT); once apl-installation-status is "completed"
	// every cycle is reconcile-only. Two triggers:
	//   * the branch was (re-)seeded — reconcile can never repopulate an empty
	//     branch (gitops-* apps starve on "app path does not exist"), and
	//   * a real VALUES upgrade landed on an already-deployed release — the
	//     upgrade rolls the operator pod but does NOT reset its installation
	//     status, and whether reconcile mode ever re-ingests changed VALUES_INPUT
	//     is undocumented apl-core behavior; the installer re-run makes the
	//     propagation deterministic through the same validated bootstrap-and-push.
	// The reset itself probes the status and no-ops on a fresh install
	// (absent/pending — the installer runs on its own). Validated live on 632033.
	valuesUpgradedExisting := !installSkipped && aplWasDeployed
	if seedSHA != "" || valuesUpgradedExisting {
		if err := resetAplInstaller(d); err != nil {
			return err
		}
	}

	// ── 7. argocd namespace (SSA) ──
	// helm's wait only covers the apl-operator Deployment; the argocd namespace
	// won't exist for 10-15m as the helmfile runs. Create it now so the Argo
	// bridge (step 10) can land; apl-core adopts it later (SSA merges ownership).
	if err := applyManifest(d, argocdNamespaceManifest(), "cluster-bootstrap-tf", true); err != nil {
		return err
	}

	// ── 8. optional GHCR secrets (SSA; gated on a token, like the count guard) ──
	// Only for a private fork keeping its first-party OCI charts private, or the
	// optional internal firewall-controller image. Empty token = public path, skip.
	if o.ghcrToken != "" {
		if err := applyManifest(d, ghcrOCIRepoSecretManifest(o), "cluster-bootstrap-tf", true); err != nil {
			return err
		}
		if err := applyManifest(d, ghcrPullSecretManifest(o), "cluster-bootstrap-tf", true); err != nil {
			return err
		}
	}

	// ── 9. Race the Kyverno policies AHEAD of the readiness gate ──
	// CRITICAL FIDELITY REQUIREMENT (see file header): in TF the kyverno_*
	// null_resources depended only on helm_release.apl, NOT apl_pipeline_ready —
	// the gate's extra argocd/cert-manager wait is the ~1-min window in which
	// apl-operator's helmfile creates the oauth2-proxy redis PVC unmutated. So the
	// policies (which poll Kyverno readiness themselves and apply the instant it
	// can admit) run CONCURRENTLY with the loud pipeline gate, not after it. The
	// gate is the hard error; the policies soft-fail (::warning::) exactly as they
	// did as local-execs.
	if err := gateAndPolicies(o, d); err != nil {
		return err
	}

	// ── 10. Argo bridge (after the gate) ──
	// apl-operator does NOT materialize server.additionalApplications, so declare
	// the bridge directly: the source-pinned AppProject, the platform-bootstrap
	// Application (instance repo apl-values/<env>/manifest), and the carved
	// llz-secret-store Application (template repo platform-apl/manifest-secret-store).
	//
	// force=true (--force-conflicts). The bridge is ours to declare, but two things
	// make a plain SSA conflict on re-apply/migration:
	//   1. The OLD cluster-bootstrap TF applied these Applications with the
	//      gavinbunney kubectl provider's DEFAULT field manager — literally
	//      "kubectl" — not our "cluster-bootstrap-tf". A cluster that TF once
	//      bootstrapped therefore has .spec.source.* owned by "kubectl"; our
	//      differently-named manager can't update it without forcing.
	//   2. llz-secret-store's targetRevision is the template-ref (a commit SHA that
	//      changes every push), so that field's VALUE differs on essentially every
	//      apply — the exact case SSA raises a conflict for. (platform-bootstrap's
	//      targetRevision is apps-repo-revision, usually a stable branch, so it
	//      rarely trips — but force it too for symmetry.)
	// We are the sole intended owner of the bridge spec (apl-core creates none of
	// it; Argo owns status + the child resources, not these Application specs), so
	// taking ownership is correct, not a stomp. The namespaces/GHCR secrets above
	// already force for the same reason.
	if err := applyManifest(d, platformBootstrapAppProjectManifest(o), "cluster-bootstrap-tf", true); err != nil {
		return err
	}
	if err := applyManifest(d, platformBootstrapApplicationManifest(o), "cluster-bootstrap-tf", true); err != nil {
		return err
	}
	if err := applyManifest(d, secretStoreApplicationManifest(o), "cluster-bootstrap-tf", true); err != nil {
		return err
	}

	// ── 11. When we seeded the branch, block until apl-operator POPULATES it ──
	// The hand-off contract: bootstrap-cluster returning 0 means the gitops-* apps
	// can actually resolve their env/ paths. The installer's first push takes ~6m
	// after the (re-)arm; on a FRESH cluster the pipeline gate above absorbs that,
	// but on a REUSED cluster the gate is near-instant and the downstream converge
	// treats the gitops apps' ComparisonError as a HARD failure with only a 60s
	// re-check — run 29543955173 converge-failed at 00:16:19, 47s before the
	// installer's push landed at 00:17:06. Waiting here (ref moves past our seed
	// commit) closes that race deterministically on both cluster shapes.
	if seedSHA != "" {
		if err := waitAplValuesBranchPopulated(d, o, repoURL, branch, seedSHA); err != nil {
			return err
		}
	}

	fmt.Print(bootstrapNextSteps(o.env))
	return nil
}

// gateAndPolicies runs the apl-pipeline readiness gate and the two Kyverno
// policies concurrently (see step 9). The gate error is fatal; a policy's hard
// apply error is fatal; policy soft-fails are ::warning:: + nil. Kubectl is
// read-only from both sides against the same kubeconfig, so concurrent use is
// safe.
func gateAndPolicies(o bootstrapClusterOpts, d bootstrapDeps) error {
	gate := aplGateDeps{kubectl: d.kubectl, now: d.now, sleep: d.sleep}
	kdeps := kyvernoDeps{kubectl: d.kubectl, now: d.now, sleep: d.sleep}

	policies, cleanup, err := kyvernoPolicySpecs()
	if err != nil {
		return err
	}
	defer cleanup()

	var wg sync.WaitGroup
	var gateErr error
	polErrs := make([]error, len(policies))

	wg.Add(1)
	go func() {
		defer wg.Done()
		gateErr = waitAplPipeline(aplPipelineStages(), gate)
	}()
	for i := range policies {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			polErrs[i] = applyKyvernoPolicy(policies[i], kdeps)
		}(i)
	}
	wg.Wait()

	if gateErr != nil {
		return gateErr
	}
	for _, e := range polErrs {
		if e != nil {
			return e
		}
	}
	return nil
}

// kyvernoPolicySpecs writes the two embedded Kyverno manifests to tempfiles (the
// reused applyKyvernoPolicy takes a manifest PATH) and returns the opts that
// carry the exact race-ahead warnings the null_resource `environment` blocks
// set. cleanup removes the tempfiles.
func kyvernoPolicySpecs() (specs []kyvernoPolicyOpts, cleanup func(), err error) {
	var tmpFiles []string
	cleanup = func() {
		for _, p := range tmpFiles {
			os.Remove(p)
		}
	}
	write := func(name string, body []byte) (string, error) {
		tmp, err := os.CreateTemp("", "llz-"+name+"-*.yaml")
		if err != nil {
			return "", err
		}
		if _, err := tmp.Write(body); err != nil {
			tmp.Close()
			return "", err
		}
		tmp.Close()
		tmpFiles = append(tmpFiles, tmp.Name())
		return tmp.Name(), nil
	}

	pvcPath, err := write("kyverno-pvc-encrypted-storage-class", kyvernoPVCEncryptedYAML)
	if err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("stage PVC-encryption policy: %w", err)
	}
	scPath, err := write("kyverno-sc-default-demote", kyvernoSCDefaultDemoteYAML)
	if err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("stage sc-default-demote policy: %w", err)
	}

	specs = []kyvernoPolicyOpts{
		{
			policyManifest:     pvcPath,
			fieldManager:       "cluster-bootstrap-tf",
			waitForKyverno:     true,
			waitTimeout:        900 * time.Second,
			timeoutWarning:     "Kyverno admission controller not Ready within 15m — skipping PVC policy apply. The oauth2-proxy redis PVC may land on linode-block-storage; re-run bootstrap once Kyverno is up.",
			webhookRaceWarning: "Kyverno admission webhook not yet reachable — policy apply skipped. Re-run bootstrap once kyverno-svc has Ready endpoints.",
		},
		{
			policyManifest:     scPath,
			fieldManager:       "cluster-bootstrap-tf",
			waitForKyverno:     true,
			waitTimeout:        900 * time.Second,
			timeoutWarning:     "Kyverno admission controller not Ready within 15m — skipping sc-default-demote policy apply. linode-block-storage-retain may stay a second default StorageClass; re-run bootstrap once Kyverno is up.",
			webhookRaceWarning: "Kyverno admission webhook not yet reachable — sc-default-demote policy apply skipped. Re-run bootstrap once kyverno-svc has Ready endpoints.",
		},
	}
	return specs, cleanup, nil
}

// ── helpers ───────────────────────────────────────────────────────────────

// coreDNSReadBudget / coreDNSReadInterval bound the DNS ClusterIP poll below.
// Package vars so tests zero them.
var (
	coreDNSReadBudget   = 5 * time.Minute
	coreDNSReadInterval = 5 * time.Second
)

// dnsClusterIPFromServicesJSON parses `kubectl get services -o json` and returns
// the ClusterIP of the Service that serves DNS (has a port 53) — name/label-
// agnostic, and it EXCLUDES the sibling workload-coredns-metrics Service (port
// 9153). Empty if none match or the JSON is unparseable. Parsing `-o json` in Go
// avoids `-o jsonpath` entirely, which was an unreliable variable across kubectl
// versions in this loop.
func dnsClusterIPFromServicesJSON(jsonOut string) string {
	var list struct {
		Items []struct {
			Spec struct {
				ClusterIP string `json:"clusterIP"`
				Ports     []struct {
					Port int `json:"port"`
				} `json:"ports"`
			} `json:"spec"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(jsonOut), &list) != nil {
		return ""
	}
	for _, s := range list.Items {
		for _, p := range s.Spec.Ports {
			if p.Port == 53 && s.Spec.ClusterIP != "" && s.Spec.ClusterIP != "None" {
				return s.Spec.ClusterIP
			}
		}
	}
	return ""
}

// readCoreDNSClusterIP resolves the cluster DNS Service's ClusterIP (the loki
// gateway nginx `resolver`) by listing kube-system Services as JSON and finding
// the one that serves DNS (a port 53).
//
// BEST-EFFORT / NON-FATAL: it polls (a freshly-ready cluster's Flux-managed CoreDNS
// can lag), and on the budget expiring it WARNS and returns "" instead of failing —
// so this one read never blocks the whole bootstrap (the loki gateway is a
// downstream, separately-fixable concern; the very next SSA apply is the real
// "does kubectl work" signal). Prints a one-time cluster-access diagnostic on the
// first miss so kubectl health is visible independently of DNS resolution.
func readCoreDNSClusterIP(d bootstrapDeps) string {
	deadline := d.now().Add(coreDNSReadBudget)
	first := true
	for {
		if out, ok := d.kubectl("-n", "kube-system", "get", "services", "-o", "json"); ok {
			if ip := dnsClusterIPFromServicesJSON(out); ip != "" {
				if !first {
					fmt.Printf("cluster DNS ClusterIP resolved: %s\n", ip)
				}
				return ip
			}
		}
		if first {
			diagnoseClusterAccess(d)
			fmt.Println("Waiting for the cluster DNS Service to have a ClusterIP...")
			first = false
		}
		if !d.now().Before(deadline) {
			warn(fmt.Sprintf("cluster DNS Service ClusterIP not resolved within %s — proceeding with an EMPTY loki resolver (see the cluster-access diagnostics above). This does NOT block the bootstrap; the loki gateway may need a follow-up.", coreDNSReadBudget))
			return ""
		}
		d.sleep(coreDNSReadInterval)
	}
}

// diagnoseClusterAccess prints what this command's kubectl seam can actually see —
// identity, API server, and node/namespace/service visibility — so an empty DNS
// read is distinguishable from a genuinely not-yet-ready cluster. Best-effort, all
// to stderr.
func diagnoseClusterAccess(d bootstrapDeps) {
	fmt.Fprintln(os.Stderr, "── cluster-access diagnostics (what bootstrap-cluster's kubectl sees) ──")

	// Go-side kubeconfig introspection FIRST: an empty $KUBECONFIG file (or a
	// missing ~/.kube/config) is the upstream cause when every kubectl call returns
	// empty — this shows it without needing kubectl to work at all.
	statLine := func(label, p string) {
		if p == "" {
			fmt.Fprintf(os.Stderr, "  %s: (unset)\n", label)
			return
		}
		if st, err := os.Stat(p); err == nil {
			fmt.Fprintf(os.Stderr, "  %s: %s (size=%d)\n", label, p, st.Size())
		} else {
			fmt.Fprintf(os.Stderr, "  %s: %s (%v)\n", label, p, err)
		}
	}
	statLine("$KUBECONFIG", os.Getenv("KUBECONFIG"))
	if home, err := os.UserHomeDir(); err == nil {
		statLine("~/.kube/config", filepath.Join(home, ".kube", "config"))
	}

	probes := []struct {
		label string
		args  []string
	}{
		{"config current-context", []string{"config", "current-context"}},
		{"cluster-info", []string{"cluster-info"}},
		{"auth whoami", []string{"auth", "whoami"}},
		{"nodes", []string{"get", "nodes", "-o", "name"}},
		{"namespaces", []string{"get", "namespaces", "-o", "name"}},
		{"all services (-A)", []string{"get", "services", "-A", "--no-headers"}},
	}
	for _, p := range probes {
		out, ok := d.kubectl(p.args...)
		fmt.Fprintf(os.Stderr, "  [%s] ok=%v\n", p.label, ok)
		if s := strings.TrimRight(out, "\n"); s != "" {
			fmt.Fprintln(os.Stderr, s)
		}
	}
	fmt.Fprintln(os.Stderr, "──────────────────────────────────────────────────────────────────────")
}

// injectRuntimeValues fills the secrets-only ${...} placeholders and hard-fails
// if any UNESCAPED ${...} not in subst survives (the templatefile unknown-var
// contract — catches a stale placeholder `llz render` should have resolved).
// Escaped $${x} is left untouched, then de-escaped to a literal ${x} last (the
// values file uses $${x} in comments to name the placeholders).
func injectRuntimeValues(raw string, subst map[string]string) (string, error) {
	// Leftover-check FIRST on the raw file: any unescaped ${var} whose name is not
	// a known placeholder is the failure the offline guard also catches.
	for _, m := range unescapedPlaceholderRe.FindAllStringSubmatch(raw, -1) {
		if _, ok := subst[m[2]]; !ok {
			return "", fmt.Errorf("apl-values references ${%s}, which is not a known runtime placeholder (%s) — a stale template `llz render` should have resolved",
				m[2], strings.Join(bootstrapValuePlaceholders, ", "))
		}
	}
	// Substitute each known placeholder. Replace the unescaped ${var} only, via a
	// callback that preserves the leading non-$ boundary char the regex captured.
	out := unescapedPlaceholderRe.ReplaceAllStringFunc(raw, func(match string) string {
		g := unescapedPlaceholderRe.FindStringSubmatch(match)
		val, ok := subst[g[2]]
		if !ok {
			return match // unreachable (guarded above), keep as-is
		}
		return g[1] + val
	})
	// De-escape $${x} → ${x} (a literal the file intends, e.g. in comments).
	out = strings.ReplaceAll(out, "$${", "${")
	return out, nil
}

// envRevisionRe pulls `revision: <value>` from the env-revision-configmap,
// tolerating whitespace + optional quotes (ported from the module's local).
var envRevisionRe = regexp.MustCompile(`revision:\s*['"]?([^\s'"#]+)['"]?`)

// assertEnvRevision fails (with the module's error text) when the committed
// env-revision-configmap.data.revision does not match the bootstrap App's
// revision — the mismatch that syncs child Apps off a different branch.
func assertEnvRevision(path, wantRevision string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read env-revision-configmap %s: %w", path, err)
	}
	m := envRevisionRe.FindStringSubmatch(string(raw))
	if m == nil {
		return fmt.Errorf("could not parse data.revision from %s — reformatted beyond the tolerated shape", path)
	}
	got := strings.TrimSpace(m[1])
	if got != wantRevision {
		return fmt.Errorf(`env-revision-configmap.yaml mismatch: %s data.revision=%q but apps-repo-revision=%q.
These MUST match — the configmap drives every in-repo child Application's targetRevision while the flag drives the bootstrap App's own revision; a mismatch syncs child Apps off a different (typically stale 'main') branch.
Fix: set --apps-repo-revision to %q, or edit the configmap's data.revision to %q on the same branch the bootstrap targets.`,
			path, got, wantRevision, got, wantRevision)
	}
	return nil
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

// helmInstallApl runs `helm upgrade --install apl` with the rendered values —
// but SKIPS the upgrade when apl is already `deployed` at the target chart
// version, so a re-apply on a REUSED cluster does not needlessly roll the
// apl-operator Deployment.
//
// That roll is not cosmetic. apl-operator's helmfile takes 10-15m, and under the
// per-env branch-isolation model (apl-values/.../otomi.git.branch = apl-<env>) its
// FIRST post-roll commit is what CREATES the apl-<env> branch the gitops-* Apps
// read. Re-asserting the release right before the convergence gate rolls
// apl-operator, resets that 10-15m clock, and the gate can time out with the
// gitops-* Apps stuck "unable to resolve apl-<env> to a commit SHA" — exactly the
// reused-cluster e2e failure this guards against. On a fresh cluster there is no
// release yet, so the install still runs.
//
// Bumping spec.cluster.bootstrap.aplChartVersion still upgrades (deployed version
// != target — the deliberate spec-driven upgrade path), and a release in any
// non-`deployed` state (absent, pending-*, failed) still runs so a half-applied
// prior run self-heals.
func helmInstallApl(d bootstrapDeps, o bootstrapClusterOpts, renderedValues string) (skipped bool, err error) {
	if ver, status, ok := deployedAplRelease(d); ok && status == "deployed" && ver == o.aplChartVersion {
		// Version matches — but only skip when the VALUES are also unchanged.
		// The apl release's values are the operator's whole input (VALUES_INPUT):
		// otomi.git credentials, the loki/harbor app values, everything. Skipping
		// on version alone would strand any values-only change (e.g. the loki
		// memberlist publishNotReadyAddresses fix, a rotated values-repo token)
		// on every reused cluster — the operator would keep reconciling stale
		// values forever. A changed-values upgrade DOES roll apl-operator, but
		// that is the intent: the change must reach it, and the pipeline gate +
		// the values-branch populated-wait absorb the roll.
		if cur, ok := d.helm("get", "values", "apl", "-n", "apl-operator", "-o", "yaml"); ok {
			if yamlSemanticallyEqual(cur, renderedValues) {
				fmt.Printf("apl-core %s already deployed with identical values — skipping helm upgrade so a reused-cluster re-apply does not roll apl-operator.\n", ver)
				return true, nil
			}
			fmt.Printf("apl-core %s deployed but the rendered values changed — running helm upgrade so the change reaches apl-operator.\n", ver)
		} else {
			// Can't read the current values (transient helm/apiserver blip). Bias
			// toward NOT rolling apl-operator — the skip is the safe side on a
			// reused cluster, and a real values change will land on the next apply.
			warn("could not read the deployed apl values to compare — skipping the upgrade (bias: don't roll apl-operator); re-run the apply if a values change must land")
			return true, nil
		}
	}

	tmp, err := os.CreateTemp("", "llz-apl-values-*.yaml")
	if err != nil {
		return false, fmt.Errorf("create apl-values tempfile: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		tmp.Close()
		return false, fmt.Errorf("chmod apl-values tempfile: %w", err)
	}
	if _, err := tmp.WriteString(renderedValues); err != nil {
		tmp.Close()
		return false, fmt.Errorf("write apl-values: %w", err)
	}
	tmp.Close()

	out, ok := d.helm("upgrade", "--install", "apl", "apl",
		"--repo", aplChartRepo,
		"--version", o.aplChartVersion,
		"--namespace", "apl-operator",
		"--values", tmp.Name(),
		"--wait", "--wait-for-jobs", "--timeout", "600s")
	if !ok {
		fmt.Fprint(os.Stderr, out)
		return false, fmt.Errorf("helm upgrade --install apl (%s) failed", o.aplChartVersion)
	}
	fmt.Printf("apl-core %s installed (apl-operator namespace).\n", o.aplChartVersion)
	return false, nil
}

// aplValuesGitCoords pulls otomi.git.repoUrl + otomi.git.branch out of the rendered
// apl-values. `llz render` resolves both at render time (they are NOT runtime
// placeholders), so the committed file already carries the concrete coordinates
// apl-operator itself reads — no need to re-derive the apl-<env> convention here.
func aplValuesGitCoords(renderedValues string) (repoURL, branch string) {
	var v struct {
		Otomi struct {
			Git struct {
				RepoURL string `json:"repoUrl"`
				Branch  string `json:"branch"`
			} `json:"git"`
		} `json:"otomi"`
	}
	if err := yaml.Unmarshal([]byte(renderedValues), &v); err != nil {
		return "", ""
	}
	return v.Otomi.Git.RepoURL, v.Otomi.Git.Branch
}

// authedGitURL injects an x-access-token basic-auth credential into an https git
// URL so ls-remote reaches a PRIVATE instance repo. Non-https URLs (or an empty
// token) are returned unchanged. The result carries a secret and must never be
// logged — callers log the credential-free repoURL instead.
func authedGitURL(rawURL, token string) string {
	const p = "https://"
	if token == "" || !strings.HasPrefix(rawURL, p) {
		return rawURL
	}
	return p + "x-access-token:" + token + "@" + strings.TrimPrefix(rawURL, p)
}

// ensureAplValuesBranch creates the apl-<env> values branch on the instance repo
// when it does not exist, seeded off the repo's default branch. apl-core v6's
// apl-operator reads otomi.git.branch = apl-<env> by PULLING it and deadlocks on a
// missing ref (it never self-creates — verified on the e2e cluster), so a fresh
// instance must have the branch primed for the operator to take over. Idempotent:
// an existing branch is left untouched (reused clusters, and any future operator
// that does self-create). A push failure surfaces LOUD and EARLY here — naming the
// values-repo token — instead of as an opaque converge timeout downstream.
// A no-coords case (values missing otomi.git.*) is a clean skip.
func ensureAplValuesBranch(d bootstrapDeps, o bootstrapClusterOpts, repoURL, branch string) (seedSHA string, err error) {
	if repoURL == "" || branch == "" {
		fmt.Println("otomi.git.repoUrl/branch absent from the rendered values — skipping the apl-values branch bootstrap.")
		return "", nil
	}
	authURL := authedGitURL(repoURL, o.aplValuesRepoToken)
	tok := o.aplValuesRepoToken
	ref := "refs/heads/" + branch

	// Already present (reused cluster / prior run)? Nothing to do.
	if out, ok := d.git("ls-remote", authURL, ref); ok && strings.TrimSpace(out) != "" {
		fmt.Printf("values branch %q already exists on %s — apl-operator can pull it.\n", branch, repoURL)
		return "", nil
	}

	// Seed an EMPTY orphan branch (a single history-less empty commit). Content
	// matters: an earlier iteration seeded a COPY OF MAIN, and apl-operator then
	// applied the instance-repo tree as its otomi env repo — its derived-values
	// template crashed ("map has no entry for key customRootCA") and it still never
	// pushed env/manifests/**. Empty is the contract the operator's gitea heritage
	// expects: it clones an empty repo, bootstraps the env structure, commits, and
	// pushes — exactly what it does with a fresh gitea repo.
	fmt.Printf("values branch %q absent on %s — seeding it EMPTY (orphan commit) for apl-operator to bootstrap...\n", branch, repoURL)
	tmp, err := os.MkdirTemp("", "llz-apl-branch-*")
	if err != nil {
		return "", fmt.Errorf("mktemp for values-branch seed: %w", err)
	}
	defer os.RemoveAll(tmp)

	if out, ok := d.git("init", "--initial-branch", branch, tmp); !ok {
		return "", fmt.Errorf("init values-branch seed repo: %s", redactSecret(out, tok))
	}
	if out, ok := d.git("-C", tmp,
		"-c", "user.name=llz-bootstrap", "-c", "user.email=llz-bootstrap@noreply.local",
		"commit", "--allow-empty", "-m", "chore: seed "+branch+" — empty branch for apl-operator to bootstrap (llz ci bootstrap-cluster)"); !ok {
		return "", fmt.Errorf("create empty seed commit for %q: %s", branch, redactSecret(out, tok))
	}
	if out, ok := d.git("-C", tmp, "push", authURL, "HEAD:refs/heads/"+branch); !ok {
		return "", fmt.Errorf("push values branch %q to %s — does the values-repo token (otomi.git.password / APL_VALUES_REPO_TOKEN) have Contents:write? git: %s",
			branch, repoURL, redactSecret(out, tok))
	}
	sha, ok := d.git("-C", tmp, "rev-parse", "HEAD")
	if !ok || strings.TrimSpace(sha) == "" {
		// The push succeeded; a failed rev-parse only degrades the populated-wait
		// (it will treat any resolvable ref as populated). Use a sentinel.
		warn("could not read the seed commit sha — the populated-wait will accept the first resolvable ref")
		return "unknown-seed-sha", nil
	}
	fmt.Printf("seeded EMPTY values branch %q on %s — apl-operator will bootstrap its env tree onto it.\n", branch, repoURL)
	return strings.TrimSpace(sha), nil
}

// resetAplInstaller re-arms apl-operator's INSTALLER phase: patch
// apl-installation-status back to pending and restart the operator Deployment.
// Only the installer bootstraps the full env values into the otomi.git branch;
// once status is "completed" every later cycle is reconcile-only and can never
// repopulate a re-seeded (empty) branch. This is the exact recovery validated
// manually on e2e cluster 632033 (status patch + rollout restart → the installer
// re-ran, bootstrapped values, and pushed them to the branch within ~6 minutes).
// The rollout wait is bounded; the downstream apl-pipeline gate + converge
// budgets absorb the installer's run itself.
// aplBranchPopulateBudget/Interval bound the wait for apl-operator's first real
// push onto a freshly-seeded values branch (installer completes ~6m after its
// (re-)arm; 15m covers a cold start). Package vars so tests shrink them.
var (
	aplBranchPopulateBudget   = 15 * time.Minute
	aplBranchPopulateInterval = 15 * time.Second
)

// waitAplValuesBranchPopulated polls `git ls-remote` until the values branch ref
// moves PAST the empty seed commit — i.e. apl-operator has pushed its bootstrapped
// env tree and the gitops-* Applications can resolve env/manifests/**. Only called
// when this run seeded the branch (an intact pre-existing branch is already
// populated). Fails loud on the budget: a stuck installer surfaces HERE with the
// operator named, not as a downstream converge timeout.
func waitAplValuesBranchPopulated(d bootstrapDeps, o bootstrapClusterOpts, repoURL, branch, seedSHA string) error {
	authURL := authedGitURL(repoURL, o.aplValuesRepoToken)
	ref := "refs/heads/" + branch
	fmt.Printf("Waiting for apl-operator to populate the values branch %q (ref must move past the seed commit; up to %s)...\n",
		branch, aplBranchPopulateBudget)
	deadline := d.now().Add(aplBranchPopulateBudget)
	for {
		if out, ok := d.git("ls-remote", authURL, ref); ok {
			sha := strings.TrimSpace(out)
			if i := strings.IndexAny(sha, " \t"); i > 0 {
				sha = sha[:i]
			}
			if sha != "" && sha != seedSHA {
				fmt.Printf("values branch %q populated (%.8s) — the gitops-* Applications can resolve their env/ paths.\n", branch, sha)
				return nil
			}
		}
		if !d.now().Before(deadline) {
			return fmt.Errorf("apl-operator did not push its env tree to the values branch %q within %s — the gitops-* Applications cannot resolve env/manifests/** and converge would fail. Check the apl-operator logs (kubectl -n apl-operator logs deploy/apl-operator) and its values-repo push credentials",
				branch, aplBranchPopulateBudget)
		}
		d.sleep(aplBranchPopulateInterval)
	}
}

func resetAplInstaller(d bootstrapDeps) error {
	// Only a COMPLETED installation is reconcile-only and needs the re-arm; an
	// absent/pending status means the installer will (re)run on its own.
	status, ok := d.kubectl("-n", "apl-operator", "get", "configmap", "apl-installation-status",
		"-o", "jsonpath={.data.status}")
	if !ok || strings.TrimSpace(status) != "completed" {
		fmt.Printf("apl-installation-status is %q — the installer will run on its own; no re-arm needed.\n", strings.TrimSpace(status))
		return nil
	}
	fmt.Println("values branch was re-seeded but apl-operator's installation is 'completed' (reconcile-only) — re-arming the installer (status→pending + restart) so it re-bootstraps the env values onto the branch.")
	if out, ok := d.kubectl("-n", "apl-operator", "patch", "configmap", "apl-installation-status",
		"--type", "merge", "-p", `{"data":{"status":"pending"}}`); !ok {
		fmt.Fprint(os.Stderr, out)
		return fmt.Errorf("could not re-arm apl-operator's installer (patch apl-installation-status) — the seeded values branch would never be populated")
	}
	if out, ok := d.kubectl("-n", "apl-operator", "rollout", "restart", "deployment/apl-operator"); !ok {
		fmt.Fprint(os.Stderr, out)
		return fmt.Errorf("restart apl-operator after re-arming its installer failed")
	}
	if out, ok := d.kubectl("-n", "apl-operator", "rollout", "status", "deployment/apl-operator", "--timeout=180s"); !ok {
		fmt.Fprint(os.Stderr, out)
		return fmt.Errorf("apl-operator did not come back after the installer re-arm restart")
	}
	fmt.Println("apl-operator restarted with installer re-armed — it will bootstrap + push the env values on this cycle.")
	return nil
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

// yamlSemanticallyEqual reports whether two YAML documents carry the same data,
// ignoring key order / formatting / comments. Used to compare `helm get values`
// against the freshly-rendered values; parse failures compare as NOT equal so a
// malformed side falls through to the (safe, explicit) upgrade path.
func yamlSemanticallyEqual(a, b string) bool {
	var av, bv any
	if err := yaml.Unmarshal([]byte(a), &av); err != nil {
		return false
	}
	if err := yaml.Unmarshal([]byte(b), &bv); err != nil {
		return false
	}
	// sigs.k8s.io/yaml round-trips through JSON, so numbers/keys normalize
	// identically on both sides — DeepEqual is sound here.
	return reflect.DeepEqual(av, bv)
}

// deployedAplRelease reports the apl release's chart version + Helm status via
// `helm list -o json`. ok=false when the release is absent or the output is
// unreadable/unparseable — in which case the caller installs. The chart field is
// "<name>-<version>" (e.g. "apl-6.0.0"); the version is the segment after the last
// "-" so it is robust to a hyphenated chart name.
func deployedAplRelease(d bootstrapDeps) (version, status string, ok bool) {
	out, run := d.helm("list", "--namespace", "apl-operator", "--filter", "^apl$", "-o", "json")
	if !run {
		return "", "", false
	}
	var releases []struct {
		Name   string `json:"name"`
		Chart  string `json:"chart"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &releases); err != nil {
		return "", "", false
	}
	for _, r := range releases {
		if r.Name != "apl" {
			continue
		}
		v := r.Chart
		if i := strings.LastIndex(r.Chart, "-"); i >= 0 {
			v = r.Chart[i+1:]
		}
		return v, r.Status, true
	}
	return "", "", false
}

// lokiAdminPasswordRe matches the sole `adminPassword:` (loki's) in the values
// helm stored — used to reuse it on upgrade so it never churns across re-runs.
var lokiAdminPasswordRe = regexp.MustCompile(`(?m)^\s*adminPassword:\s*(\S+)\s*$`)

// existingLokiPassword returns the loki admin password helm already stored for
// the apl release (the values we passed on first install), or "" if the release
// is absent / unreadable. On first install this is "", so the caller generates
// one; on upgrade it returns the stable first-install value.
func existingLokiPassword(d bootstrapDeps) string {
	out, ok := d.helm("get", "values", "apl", "-n", "apl-operator")
	if !ok {
		return ""
	}
	if m := lokiAdminPasswordRe.FindStringSubmatch(out); m != nil {
		v := strings.TrimSpace(m[1])
		// `helm get values` prints "null" for an absent value and could echo the
		// un-filled placeholder on a broken prior run — treat both as "no value".
		if v != "" && v != "null" && !strings.Contains(v, "${") {
			return v
		}
	}
	return ""
}

// lokiPasswordAlphabet matches apl-core's `randAlphaNum 20` generator charset
// (nginx-safe: no specials).
const lokiPasswordAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// genLokiPassword returns a 20-char alphanumeric password (crypto/rand), matching
// the former random_password.loki_admin (length 20, special=false).
func genLokiPassword() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is fatal-ish; fall back to a fixed-length marker that
		// still satisfies the schema length so the install can surface the real
		// error rather than a values-schema violation.
		return strings.Repeat("x", 20)
	}
	out := make([]byte, 20)
	for i, v := range b {
		out[i] = lokiPasswordAlphabet[int(v)%len(lokiPasswordAlphabet)]
	}
	return string(out)
}

// dryRunBootstrap prints the intended actions without touching the cluster — the
// operator-facing replacement for `terraform plan` on this layer.
func dryRunBootstrap(o bootstrapClusterOpts, kubeconfigPath string) error {
	fmt.Printf("→ (dry-run) bootstrap-cluster env=%s kubeconfig=%s\n", o.env, kubeconfigPath)
	fmt.Printf("  1. read coredns ClusterIP (kube-system/coredns)\n")
	fmt.Printf("  2. render %s (fill %d runtime placeholders)\n", o.valuesPath, len(bootstrapValuePlaceholders))
	fmt.Printf("  3. assert env-revision == %s (%s)\n", o.appsRepoRevision, o.envRevisionPath)
	fmt.Printf("  4. kubectl apply --server-side apl-operator Namespace\n")
	fmt.Printf("  5. kubectl apply --server-side block-storage-retain StorageClass\n")
	fmt.Printf("  6a. ensure the otomi.git values branch exists (seed EMPTY orphan branch when absent)\n")
	fmt.Printf("  6. helm upgrade --install apl apl/apl --version %s -n apl-operator --wait (skipped when deployed at this version with identical values)\n", o.aplChartVersion)
	fmt.Printf("  6b. if the branch was seeded and apl-installation-status is 'completed': re-arm the installer (patch status + restart apl-operator)\n")
	fmt.Printf("  7. kubectl apply --server-side argocd Namespace\n")
	if o.ghcrToken != "" {
		fmt.Printf("  8. kubectl apply --server-side GHCR OCI repo Secret + pull Secret\n")
	} else {
		fmt.Printf("  8. (skip GHCR secrets — no token, public-charts path)\n")
	}
	fmt.Printf("  9. wait-apl-pipeline (argocd/kyverno/cert-manager) CONCURRENTLY with the 2 Kyverno policies\n")
	fmt.Printf(" 10. kubectl apply --server-side platform-bootstrap AppProject + Application + llz-secret-store Application\n")
	fmt.Printf(" 11. if the branch was seeded: wait for apl-operator to populate it (ref must move past the seed commit)\n")
	return nil
}
