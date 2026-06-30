package main

// ci_openbao_configure.go — `llz ci bao-configure`, the native port of
// configure-openbao.sh: root-token preflight, KV v2 mount, Kubernetes +
// GitHub-OIDC auth, policies, roles, and the audit-device verify. Idempotent
// like the bash (enables tolerate "already enabled", writes upsert), so
// re-configure runs are safe. Part of the openbao CI family (ci_openbao.go).

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// platform-ci: read-only KV v2 — used by the ESO ClusterSecretStore. Paths
// are enumerated explicitly; wildcard read is intentionally avoided.
const policyPlatformCI = `path "secret/data/cert-automation/github-token" { capabilities = ["read"] }
path "secret/data/certmanager/dns01"            { capabilities = ["read"] }
path "secret/data/grafana/admin"                { capabilities = ["read"] }
path "secret/data/harbor/admin"                 { capabilities = ["read"] }
path "secret/data/harbor/pull-robot"            { capabilities = ["read"] }
path "secret/data/harbor/registry-s3"           { capabilities = ["read"] }
path "secret/data/harbor/robot"                 { capabilities = ["read"] }
path "secret/data/infra/github-dispatch-token"  { capabilities = ["read"] }
path "secret/data/linode/api-token"             { capabilities = ["read"] }
path "secret/data/loki/object-store"            { capabilities = ["read"] }
path "secret/data/otel/ingress"                 { capabilities = ["read"] }
path "secret/data/velero/object-store"          { capabilities = ["read"] }

path "secret/metadata/cert-automation/github-token" { capabilities = ["read", "list"] }
path "secret/metadata/certmanager/dns01"            { capabilities = ["read", "list"] }
path "secret/metadata/grafana/admin"                { capabilities = ["read", "list"] }
path "secret/metadata/harbor/admin"                 { capabilities = ["read", "list"] }
path "secret/metadata/harbor/docker-config"         { capabilities = ["read", "list"] }
path "secret/metadata/harbor/pull-robot"            { capabilities = ["read", "list"] }
path "secret/metadata/harbor/registry-s3"           { capabilities = ["read", "list"] }
path "secret/metadata/harbor/robot"                 { capabilities = ["read", "list"] }
path "secret/metadata/infra/github-dispatch-token"  { capabilities = ["read", "list"] }
path "secret/metadata/linode/api-token"             { capabilities = ["read", "list"] }
path "secret/metadata/loki/object-store"            { capabilities = ["read", "list"] }
path "secret/metadata/otel/ingress"                 { capabilities = ["read", "list"] }
path "secret/metadata/velero/object-store"          { capabilities = ["read", "list"] }
`

// secret-propagator: narrow write access to secret/linode/api-token, used by the
// rotation pipeline (secret-rotation.yml → propagate-linode-pat) to refresh the
// PAT after a mint. Consumed via the GitHub-OIDC jwt role `secret-propagator`
// (auth method, not the retired AppRole). Add paths here when extending it.
const policySecretPropagator = `path "secret/data/linode/api-token" { capabilities = ["create", "update", "read"] }
path "secret/metadata/linode/api-token" { capabilities = ["read"] }
`

// eso-pusher: narrow create/update access to the in-cluster-sourced secrets that
// ESO PushSecrets write into OpenBao — the self-generated grafana admin password
// and otel ingress bearer (apl-values/_shared/manifest/generated-secrets/), plus
// the Harbor admin password mirrored from Harbor's Helm Secret
// (apl-values/components/harbor/harbor-admin-push.yaml). Replaces the imperative
// `llz ci bao-seed` of these paths (root-token + kubectl exec) with a
// least-privilege, in-cluster write. On the data paths `read` covers the
// IfNotExists existence check. The metadata paths need create/update, not just
// read: ESO stamps a `managed-by: external-secrets` marker into the secret's
// custom_metadata on first push (a PUT to secret/metadata/<path>), so a
// read-only metadata grant makes the very first PushSecret fail with a 403 on
// the metadata write — which stalls the platform-bootstrap sync hooks and
// wedges convergence on a fresh cluster. The read-only `platform-ci` policy
// still serves every consumer. Mapped to the `eso-pusher` Kubernetes-auth role
// below (same ESO controller SA as `eso`).
const policyESOPusher = `path "secret/data/grafana/admin" { capabilities = ["create", "update", "read"] }
path "secret/data/otel/ingress"  { capabilities = ["create", "update", "read"] }
path "secret/data/harbor/admin"  { capabilities = ["create", "update", "read"] }
path "secret/metadata/grafana/admin" { capabilities = ["create", "update", "read"] }
path "secret/metadata/otel/ingress"  { capabilities = ["create", "update", "read"] }
path "secret/metadata/harbor/admin"  { capabilities = ["create", "update", "read"] }
`

// linode-rotator: write access to the in-cluster-only Linode credentials the
// in-cluster rotator owns (the linodeCredRotator CronJob, `llz ci
// rotate-linode-creds`). Scoped to exactly the rotated paths — the object-storage
// keys (Loki, Harbor registry) and the DNS-scoped token — never the provisioning
// PAT or any read-only consumer path. Mapped to the `linode-rotator`
// Kubernetes-auth role below. See docs/designs/linode-credential-rotator.md.
const policyLinodeRotator = `path "secret/data/loki/object-store"  { capabilities = ["create", "update", "read"] }
path "secret/data/harbor/registry-s3" { capabilities = ["create", "update", "read"] }
path "secret/data/certmanager/dns01"  { capabilities = ["create", "update", "read"] }
path "secret/metadata/loki/object-store"  { capabilities = ["read"] }
path "secret/metadata/harbor/registry-s3" { capabilities = ["read"] }
path "secret/metadata/certmanager/dns01"  { capabilities = ["read"] }
`

// baoConfigStep is one in-pod bao invocation of the configure sequence.
// Non-fatal steps are the `|| true` enables of the bash — re-runs hit
// "path is already in use" and must not abort the re-configure.
type baoConfigStep struct {
	desc  string
	args  []string
	stdin string
	fatal bool
}

// baoConfigureSteps is the ordered configure sequence: mounts/auth enables,
// then policies, then roles. Pure so the table is unit-testable. ghRepo is the
// instance's "<owner>/<name>" (from GITHUB_REPOSITORY); when set, the
// GitHub-OIDC (JWT) auth method + a repo-bound role are appended so CI can
// authenticate with a short-lived OIDC token instead of a long-lived AppRole
// secret_id stashed in GitHub Actions secrets.
func baoConfigureSteps(ghRepo string) []baoConfigStep {
	steps := []baoConfigStep{
		{desc: "enable KV v2 at secret/", args: []string{"secrets", "enable", "-version=2", "-path=secret", "kv"}},
		{desc: "enable kubernetes auth", args: []string{"auth", "enable", "kubernetes"}},
		// Kubernetes auth uses in-pod service account token/CA auto-discovery.
		{desc: "configure kubernetes auth", fatal: true,
			args: []string{"write", "auth/kubernetes/config", "kubernetes_host=https://kubernetes.default.svc:443"}},
		// platform-ci policy: read-only KV, mapped to the ESO k8s-auth role below and
		// the platform-ci GitHub-OIDC jwt role. The platform-ci AppRole + its rotator
		// were retired: ESO authenticates via Kubernetes auth, GitHub CI via OIDC, so
		// no AppRole, no secret_id, no rotation CronWorkflow, no in-cluster PAT.
		{desc: "write policy platform-ci", fatal: true, stdin: policyPlatformCI,
			args: []string{"policy", "write", "platform-ci", "-"}},
		{desc: "write policy secret-propagator", fatal: true, stdin: policySecretPropagator,
			args: []string{"policy", "write", "secret-propagator", "-"}},
		// eso-pusher policy: scoped create/update for the ESO PushSecrets that seed
		// the self-generated grafana/admin + otel/ingress paths declaratively (see
		// policyESOPusher). Replaces the imperative bao-seed of those two paths.
		{desc: "write policy eso-pusher", fatal: true, stdin: policyESOPusher,
			args: []string{"policy", "write", "eso-pusher", "-"}},
		// linode-rotator policy: scoped write for the in-cluster Linode credential
		// rotator (OBJ keys + DNS token). Mapped to the linode-rotator k8s-auth role.
		{desc: "write policy linode-rotator", fatal: true, stdin: policyLinodeRotator,
			args: []string{"policy", "write", "linode-rotator", "-"}},
		// Kubernetes auth role for the External Secrets Operator — lets the ESO
		// ClusterSecretStore authenticate with its in-cluster ServiceAccount token
		// (read-only platform-ci policy) instead of an AppRole secret_id seeded from
		// a GitHub secret and rotated in-cluster via `gh secret set`. ESO's
		// controller SA is the chart's release name (llz-external-secrets).
		{desc: "write kubernetes auth role eso", fatal: true,
			args: []string{"write", "auth/kubernetes/role/eso",
				"bound_service_account_names=llz-external-secrets",
				"bound_service_account_namespaces=llz-external-secrets",
				"policies=platform-ci", "ttl=15m"}},
		// Second Kubernetes-auth role for the SAME ESO controller SA, mapped to the
		// write-scoped eso-pusher policy. The `openbao-push` ClusterSecretStore
		// selects this role (role: eso-pusher) so PushSecrets can write the two
		// generated paths while the read `openbao` store stays read-only via `eso`.
		{desc: "write kubernetes auth role eso-pusher", fatal: true,
			args: []string{"write", "auth/kubernetes/role/eso-pusher",
				"bound_service_account_names=llz-external-secrets",
				"bound_service_account_namespaces=llz-external-secrets",
				"policies=eso-pusher", "ttl=15m"}},
		// Kubernetes auth role for the in-cluster Linode credential rotator — binds
		// the linode-cred-rotator ServiceAccount to the write-scoped linode-rotator
		// policy so the CronJob can write the rotated creds straight to OpenBao.
		{desc: "write kubernetes auth role linode-rotator", fatal: true,
			args: []string{"write", "auth/kubernetes/role/linode-rotator",
				"bound_service_account_names=linode-cred-rotator",
				"bound_service_account_namespaces=llz-linode-cred-rotator",
				"policies=linode-rotator", "ttl=15m"}},
	}

	// GitHub Actions OIDC (JWT) auth — repo-bound roles that let a workflow log in
	// with a short-lived, per-run OIDC token instead of a long-lived AppRole
	// secret_id stashed in GitHub Actions secrets (and the in-cluster PAT that
	// rotates it via `gh secret set`). The `secret-propagator` role is the live
	// GitHub-CI auth path (llz ci propagate-pat); `platform-ci` is read-only,
	// reserved for any future GitHub workflow that reads OpenBao directly (ESO
	// reads in-cluster via AppRole, not GitHub OIDC). Appended only when the
	// instance repo is known; a repo-less configure (local/dry-run without
	// GITHUB_REPOSITORY) omits them rather than create an unbindable role.
	if ghRepo != "" {
		owner := ghRepo
		if i := strings.IndexByte(ghRepo, '/'); i > 0 {
			owner = ghRepo[:i]
		}
		// SECURITY — bound_claims pins each role to THIS instance repo and
		// bound_audiences to the owner's GitHub-OIDC default audience. Without
		// BOTH, any GitHub repo's OIDC token could mint a token here.
		//
		// The role body is written as a JSON object over stdin (`bao write <path> -`)
		// rather than key=value args: bound_claims is a MAP field, and the CLI
		// rejects a key=value string for it ("expected map[string]interface{}, got
		// string"). JSON also types bound_audiences/token_policies as lists.
		jwtRole := func(name, policy string) baoConfigStep {
			body := fmt.Sprintf(
				`{"role_type":"jwt","user_claim":"sub","bound_audiences":["https://github.com/%s"],"bound_claims":{"repository":"%s"},"token_policies":["%s"],"token_ttl":"15m","token_max_ttl":"30m"}`,
				owner, ghRepo, policy)
			return baoConfigStep{desc: "write jwt role " + name, fatal: true, stdin: body,
				args: []string{"write", "auth/jwt/role/" + name, "-"}}
		}
		steps = append(steps,
			// Non-fatal enable (tolerates already-enabled on re-runs), matching
			// the other auth enables above.
			baoConfigStep{desc: "enable jwt (GitHub OIDC) auth",
				args: []string{"auth", "enable", "jwt"}},
			baoConfigStep{desc: "configure jwt with the GitHub Actions OIDC issuer", fatal: true,
				args: []string{"write", "auth/jwt/config",
					"oidc_discovery_url=https://token.actions.githubusercontent.com",
					"bound_issuer=https://token.actions.githubusercontent.com"}},
			jwtRole("platform-ci", "platform-ci"),
			jwtRole("secret-propagator", "secret-propagator"),
		)
	}
	return steps
}

// auditFileDeviceActive reports whether `bao audit list` shows the file/
// device. The device is enabled DECLARATIVELY by the chart values (the
// `audit "file" { … }` block in server.ha.raft.config) — OpenBao 2.5.0
// rejects API-based enablement ("cannot enable audit device via API; use
// declarative, config-based audit device management instead", observed in
// practice) — so configure only VERIFIES it is active.
func auditFileDeviceActive(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "file/") {
			return true
		}
	}
	return false
}

func ciBaoConfigureCmd() *cobra.Command {
	var region string
	c := &cobra.Command{
		Use:   "bao-configure",
		Short: "configure OpenBao: KV v2, Kubernetes + GitHub-OIDC auth, policies, roles, audit verify",
		Long: "Native port of configure-openbao.sh. Preflights $OPENBAO_ROOT_TOKEN (sha256\n" +
			"audit line + `token lookup` + root-policy check — without it the failure\n" +
			"mode is an unexplained cascade of 403s from every privileged call), then\n" +
			"applies the mounts/auth/policy/role sequence and verifies the declarative\n" +
			"file/ audit device is active (warns + sets BOOTSTRAP_ERRORS=true when not).\n" +
			"Idempotent: enables tolerate already-enabled, writes upsert.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIBaoConfigure(gopts, region) },
	}
	c.Flags().StringVar(&region, "region", "", "region name used in operator-facing error messages (required)")
	return c
}

func runCIBaoConfigure(g globalOpts, region string) error {
	if region == "" {
		return fmt.Errorf("--region is required")
	}
	token := os.Getenv("OPENBAO_ROOT_TOKEN")
	if token == "" {
		return fmt.Errorf("OPENBAO_ROOT_TOKEN is not set")
	}
	// Instance repo (GitHub Actions sets GITHUB_REPOSITORY to "<owner>/<name>").
	// Drives the GitHub-OIDC (JWT) role's repo binding; empty (e.g. a local
	// configure) omits the JWT steps.
	ghRepo := os.Getenv("GITHUB_REPOSITORY")
	if ghRepo == "" {
		fmt.Fprintln(os.Stderr, "::warning::GITHUB_REPOSITORY unset — skipping GitHub-OIDC (jwt) auth setup; CI propagation (llz ci propagate-pat) stays unavailable until re-run with GITHUB_REPOSITORY set.")
	}
	if g.dryRun {
		fmt.Fprintln(os.Stderr, "→ (dry-run) would preflight the root token and apply the configure sequence:")
		for _, s := range baoConfigureSteps(ghRepo) {
			fmt.Fprintf(os.Stderr, "    bao %s\n", strings.Join(s.args, " "))
		}
		return nil
	}
	pod := openbaoPodNames[0]

	// Token preflight. The sha256 is safe (irreversible) and cross-checks
	// against the sha256 `llz openbao regen-root` printed when it wrote the
	// env-secret — a mismatch means the value was mutated in transit (GHA
	// secret encoding, gh CLI truncation, …). Common invalid-token cause: a
	// stale OPENBAO_ROOT_TOKEN env secret left over from a prior bootstrap
	// (root is revoked at the end of every run).
	fmt.Printf("Token sha256 from env-secret: %s (len=%d)\n", sha256Hex(token), len(token))

	// `token lookup` (no args = self) succeeds for any valid token and needs
	// no special caps; `-self` isn't supported on every OpenBao version.
	// baoExec keeps stdout/stderr separate and pins -c openbao, so kubectl's
	// "Defaulted container" warning cannot poison the JSON (the bash needed
	// mktemp redirection for this).
	lookupOut, lookupErr, err := baoExecFn(pod, token, "", "token", "lookup", "-format=json")
	if err != nil {
		fmt.Fprintf(os.Stderr, "::error::OPENBAO_ROOT_TOKEN on %s is invalid (token lookup failed). Likely revoked by a prior bootstrap run or OpenBao was re-initialized. Regenerate root via 'bao operator generate-root' (quorum required) and re-seed the infra-%s environment secret.\n", region, region)
		for _, l := range strings.Split(strings.TrimSpace(firstNonEmpty(lookupErr, lookupOut)), "\n") {
			fmt.Fprintln(os.Stderr, "  "+l)
		}
		return fmt.Errorf("root-token preflight failed on %s", region)
	}
	if !policiesIncludeRoot(lookupOut) {
		fmt.Fprintf(os.Stderr, "::error::OPENBAO_ROOT_TOKEN on %s is a valid token but not root. Configure steps require root. Re-seed the infra-%s environment secret with an actual root token.\n", region, region)
		return fmt.Errorf("root-token preflight failed on %s: token is not root", region)
	}
	fmt.Printf("OPENBAO_ROOT_TOKEN preflight on %s OK — proceeding.\n", region)

	for _, step := range baoConfigureSteps(ghRepo) {
		out, errOut, err := baoExecFn(pod, token, step.stdin, step.args...)
		if err != nil {
			if step.fatal {
				return fmt.Errorf("%s: %s", step.desc, strings.TrimSpace(firstNonEmpty(errOut, out)))
			}
			// The bash's `|| true`: an enable against an existing mount/auth
			// method errors with "path is already in use" — fine on re-runs.
			fmt.Printf("%s: skipped (%s)\n", step.desc, strings.TrimSpace(firstNonEmpty(errOut, out)))
			continue
		}
		fmt.Printf("%s: done\n", step.desc)
	}

	auditOut, _, _ := baoExecFn(pod, token, "", "audit", "list")
	if auditFileDeviceActive(auditOut) {
		fmt.Println("audit device file/ active (declared in chart values).")
	} else {
		fmt.Fprintln(os.Stderr, "::warning::audit device file/ NOT active. Check pod logs for HCL parse errors and the llz-openbao-platform chart's audit block.")
		if err := appendGHAFile("GITHUB_ENV", "BOOTSTRAP_ERRORS=true"); err != nil {
			return err
		}
	}

	fmt.Println("OpenBao configuration complete.")
	return nil
}
