package main

// ci_openbao_configure.go — `llz ci bao-configure`, the native port of
// configure-openbao.sh: root-token preflight, KV v2 mount, AppRole +
// Kubernetes auth, policies, roles, and the audit-device verify. Idempotent
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
const policyPlatformCI = `path "secret/data/approle/rotation-secrets"     { capabilities = ["read"] }
path "secret/data/cert-automation/github-token" { capabilities = ["read"] }
path "secret/data/certmanager/dns01"            { capabilities = ["read"] }
path "secret/data/grafana/admin"                { capabilities = ["read"] }
path "secret/data/harbor/admin"                 { capabilities = ["read"] }
path "secret/data/harbor/docker-config"         { capabilities = ["read"] }
path "secret/data/harbor/pull-robot"            { capabilities = ["read"] }
path "secret/data/harbor/registry-s3"           { capabilities = ["read"] }
path "secret/data/harbor/robot"                 { capabilities = ["read"] }
path "secret/data/infra/github-dispatch-token"  { capabilities = ["read"] }
path "secret/data/loki/object-store"            { capabilities = ["read"] }
path "secret/data/otel/ingress"                 { capabilities = ["read"] }

path "secret/metadata/approle/rotation-secrets"     { capabilities = ["read", "list"] }
path "secret/metadata/cert-automation/github-token" { capabilities = ["read", "list"] }
path "secret/metadata/certmanager/dns01"            { capabilities = ["read", "list"] }
path "secret/metadata/grafana/admin"                { capabilities = ["read", "list"] }
path "secret/metadata/harbor/admin"                 { capabilities = ["read", "list"] }
path "secret/metadata/harbor/docker-config"         { capabilities = ["read", "list"] }
path "secret/metadata/harbor/pull-robot"            { capabilities = ["read", "list"] }
path "secret/metadata/harbor/registry-s3"           { capabilities = ["read", "list"] }
path "secret/metadata/harbor/robot"                 { capabilities = ["read", "list"] }
path "secret/metadata/infra/github-dispatch-token"  { capabilities = ["read", "list"] }
path "secret/metadata/loki/object-store"            { capabilities = ["read", "list"] }
path "secret/metadata/otel/ingress"                 { capabilities = ["read", "list"] }
`

// approle-rotator: manage platform-ci AND secret-propagator secret_ids — used
// by the approle-rotation CronWorkflow. Add a role here when extending the
// CronWorkflow to rotate it.
const policyAppRoleRotator = `path "auth/approle/role/platform-ci/secret-id"                  { capabilities = ["create", "update", "list"] }
path "auth/approle/role/platform-ci/secret-id-accessor/destroy" { capabilities = ["update"] }
path "auth/approle/role/platform-ci/role-id"                    { capabilities = ["read"] }
path "auth/approle/role/secret-propagator/secret-id"                  { capabilities = ["create", "update", "list"] }
path "auth/approle/role/secret-propagator/secret-id-accessor/destroy" { capabilities = ["update"] }
path "auth/approle/role/secret-propagator/role-id"                    { capabilities = ["read"] }
`

// secret-propagator: narrow write access to the kv paths the rotation
// pipeline (secret-rotation.yml → propagate-linode-pat) needs to refresh
// after a PAT/OBJ-key mint. Replaces the previous design that authed as root,
// which broke once bootstrap-openbao.yml revoked the root token. Add new
// paths here when extending the propagation pipeline.
const policySecretPropagator = `path "secret/data/linode/api-token" { capabilities = ["create", "update", "read"] }
path "secret/metadata/linode/api-token" { capabilities = ["read"] }
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
		{desc: "enable approle auth", args: []string{"auth", "enable", "approle"}},
		{desc: "enable kubernetes auth", args: []string{"auth", "enable", "kubernetes"}},
		// Kubernetes auth uses in-pod service account token/CA auto-discovery.
		{desc: "configure kubernetes auth", fatal: true,
			args: []string{"write", "auth/kubernetes/config", "kubernetes_host=https://kubernetes.default.svc:443"}},
		{desc: "write policy platform-ci", fatal: true, stdin: policyPlatformCI,
			args: []string{"policy", "write", "platform-ci", "-"}},
		{desc: "write policy approle-rotator", fatal: true, stdin: policyAppRoleRotator,
			args: []string{"policy", "write", "approle-rotator", "-"}},
		{desc: "write policy secret-propagator", fatal: true, stdin: policySecretPropagator,
			args: []string{"policy", "write", "secret-propagator", "-"}},
		{desc: "write approle role platform-ci", fatal: true,
			args: []string{"write", "auth/approle/role/platform-ci",
				"token_policies=platform-ci", "token_ttl=15m", "token_max_ttl=30m", "secret_id_ttl=2208h"}},
		// Pin role_id to "platform-ci" — must match the ClusterSecretStore roleId field.
		{desc: "pin platform-ci role-id", fatal: true,
			args: []string{"write", "auth/approle/role/platform-ci/role-id", "role_id=platform-ci"}},
		// secret-propagator AppRole. Pinned role_id; secret_id_ttl matches
		// platform-ci (≈92 days) so the existing rotation cadence covers it.
		{desc: "write approle role secret-propagator", fatal: true,
			args: []string{"write", "auth/approle/role/secret-propagator",
				"token_policies=secret-propagator", "token_ttl=15m", "token_max_ttl=30m", "secret_id_ttl=2208h"}},
		{desc: "pin secret-propagator role-id", fatal: true,
			args: []string{"write", "auth/approle/role/secret-propagator/role-id", "role_id=secret-propagator"}},
		// Kubernetes auth role for the approle-rotation CronWorkflow SA.
		{desc: "write kubernetes auth role approle-rotator", fatal: true,
			args: []string{"write", "auth/kubernetes/role/approle-rotator",
				"bound_service_account_names=approle-rotator", "bound_service_account_namespaces=" + openbaoNS,
				"policies=approle-rotator", "ttl=15m"}},
	}

	// GitHub Actions OIDC (JWT) auth — phase 1: stand the method + a repo-bound
	// role up ALONGSIDE AppRole (CI is switched to it in a later change). Lets a
	// workflow log in with a short-lived, per-run OIDC token, retiring the
	// long-lived AppRole secret_id kept in GitHub Actions secrets and the
	// in-cluster PAT that rotates it. Appended only when the instance repo is
	// known; a repo-less configure (local/dry-run without GITHUB_REPOSITORY)
	// omits it rather than create an unbindable role.
	if ghRepo != "" {
		owner := ghRepo
		if i := strings.IndexByte(ghRepo, '/'); i > 0 {
			owner = ghRepo[:i]
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
			// SECURITY — the two bindings below are load-bearing: bound_claims
			// pins this role to THIS instance repo, and bound_audiences to the
			// GitHub-OIDC default audience for the owner. Without BOTH, any
			// GitHub repo's OIDC token could mint a platform-ci token.
			// user_claim=sub identifies the caller by its unique workflow subject.
			baoConfigStep{desc: "write jwt role platform-ci", fatal: true,
				args: []string{"write", "auth/jwt/role/platform-ci",
					"role_type=jwt", "user_claim=sub",
					"bound_audiences=https://github.com/" + owner,
					`bound_claims={"repository":"` + ghRepo + `"}`,
					"token_policies=platform-ci", "token_ttl=15m", "token_max_ttl=30m"}},
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
		Short: "configure OpenBao: KV v2, AppRole + Kubernetes auth, policies, roles, audit verify",
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
		fmt.Fprintln(os.Stderr, "::warning::GITHUB_REPOSITORY unset — skipping GitHub-OIDC (jwt) auth setup; CI will fall back to AppRole.")
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
