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

	// ── 6. apl-core (helm upgrade --install) ──
	if err := helmInstallApl(d, o, rendered); err != nil {
		return err
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
	if err := applyManifest(d, platformBootstrapAppProjectManifest(o), "cluster-bootstrap-tf", false); err != nil {
		return err
	}
	if err := applyManifest(d, platformBootstrapApplicationManifest(o), "cluster-bootstrap-tf", false); err != nil {
		return err
	}
	if err := applyManifest(d, secretStoreApplicationManifest(o), "cluster-bootstrap-tf", false); err != nil {
		return err
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

// helmInstallApl runs `helm upgrade --install apl` with the rendered values.
// `--install` + the same version each run makes CI re-apply idempotent; bumping
// spec.cluster.bootstrap.aplChartVersion + re-dispatching is the deliberate
// upgrade path (the former lifecycle ignore_changes=[version] is replaced by
// this explicit spec-driven model).
func helmInstallApl(d bootstrapDeps, o bootstrapClusterOpts, renderedValues string) error {
	tmp, err := os.CreateTemp("", "llz-apl-values-*.yaml")
	if err != nil {
		return fmt.Errorf("create apl-values tempfile: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod apl-values tempfile: %w", err)
	}
	if _, err := tmp.WriteString(renderedValues); err != nil {
		tmp.Close()
		return fmt.Errorf("write apl-values: %w", err)
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
		return fmt.Errorf("helm upgrade --install apl (%s) failed", o.aplChartVersion)
	}
	fmt.Printf("apl-core %s installed (apl-operator namespace).\n", o.aplChartVersion)
	return nil
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
	fmt.Printf("  6. helm upgrade --install apl apl/apl --version %s -n apl-operator --wait\n", o.aplChartVersion)
	fmt.Printf("  7. kubectl apply --server-side argocd Namespace\n")
	if o.ghcrToken != "" {
		fmt.Printf("  8. kubectl apply --server-side GHCR OCI repo Secret + pull Secret\n")
	} else {
		fmt.Printf("  8. (skip GHCR secrets — no token, public-charts path)\n")
	}
	fmt.Printf("  9. wait-apl-pipeline (argocd/kyverno/cert-manager) CONCURRENTLY with the 2 Kyverno policies\n")
	fmt.Printf(" 10. kubectl apply --server-side platform-bootstrap AppProject + Application + llz-secret-store Application\n")
	return nil
}
