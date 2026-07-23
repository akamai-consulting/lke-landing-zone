package main

// ci_openbao_configure.go — `llz ci bao-configure`, the native port of
// configure-openbao.sh: root-token preflight, KV v2 mount, Kubernetes +
// GitHub-OIDC auth, policies, roles, and the audit-device verify. Idempotent
// like the bash (enables tolerate "already enabled", writes upsert), so
// re-configure runs are safe. Part of the openbao CI family (ci_openbao.go).

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/forge"
	"github.com/spf13/cobra"
)

// platform-ci: read-only KV v2 — used by the ESO ClusterSecretStore. Paths
// are enumerated explicitly; wildcard read is intentionally avoided.
const policyPlatformCI = `path "secret/data/alerts/webhooks"              { capabilities = ["read"] }
path "secret/data/cert-automation/github-token" { capabilities = ["read"] }
path "secret/data/grafana/admin"                { capabilities = ["read"] }
path "secret/data/harbor/admin"                 { capabilities = ["read"] }
path "secret/data/harbor/pull-robot"            { capabilities = ["read"] }
path "secret/data/harbor/registry-s3"           { capabilities = ["read"] }
path "secret/data/harbor/robot"                 { capabilities = ["read"] }
path "secret/data/infra/github-dispatch-token"  { capabilities = ["read"] }
path "secret/data/linode/api-token"             { capabilities = ["read"] }
path "secret/data/linode/broad-pat"             { capabilities = ["read"] }
path "secret/data/linode/cloud-firewall"        { capabilities = ["read"] }
path "secret/data/loki/object-store"            { capabilities = ["read"] }
path "secret/data/obj/platform"                 { capabilities = ["read"] }
path "secret/data/otel/ingress"                 { capabilities = ["read"] }

path "secret/metadata/alerts/webhooks"              { capabilities = ["read", "list"] }
path "secret/metadata/cert-automation/github-token" { capabilities = ["read", "list"] }
path "secret/metadata/grafana/admin"                { capabilities = ["read", "list"] }
path "secret/metadata/harbor/admin"                 { capabilities = ["read", "list"] }
path "secret/metadata/harbor/docker-config"         { capabilities = ["read", "list"] }
path "secret/metadata/harbor/pull-robot"            { capabilities = ["read", "list"] }
path "secret/metadata/harbor/registry-s3"           { capabilities = ["read", "list"] }
path "secret/metadata/harbor/robot"                 { capabilities = ["read", "list"] }
path "secret/metadata/infra/github-dispatch-token"  { capabilities = ["read", "list"] }
path "secret/metadata/linode/api-token"             { capabilities = ["read", "list"] }
path "secret/metadata/linode/broad-pat"             { capabilities = ["read", "list"] }
path "secret/metadata/linode/cloud-firewall"        { capabilities = ["read", "list"] }
path "secret/metadata/loki/object-store"            { capabilities = ["read", "list"] }
path "secret/metadata/obj/platform"                 { capabilities = ["read", "list"] }
path "secret/metadata/otel/ingress"                 { capabilities = ["read", "list"] }
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
// and otel ingress bearer (platform-apl/manifest/generated-secrets/), plus
// the Harbor admin password mirrored from Harbor's Helm Secret
// (platform-apl/components/harbor/harbor-admin-push.yaml). Replaces the imperative
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
// keys (Loki, Harbor registry) — never the provisioning
// PAT or any read-only consumer path. Mapped to the `linode-rotator`
// Kubernetes-auth role below. See docs/designs/linode-credential-rotator.md.
const policyLinodeRotator = `path "secret/data/loki/object-store"  { capabilities = ["create", "update", "read"] }
path "secret/data/harbor/registry-s3" { capabilities = ["create", "update", "read"] }
path "secret/data/obj/platform"       { capabilities = ["create", "update", "read"] }
path "secret/metadata/loki/object-store"  { capabilities = ["read"] }
path "secret/metadata/harbor/registry-s3" { capabilities = ["read"] }
path "secret/metadata/obj/platform"       { capabilities = ["read"] }
`

// harbor-provisioner: read/write on exactly the two robot-credential paths the
// in-cluster harbor-robot-provisioner CronJob owns (`llz ci harbor-provisioner`,
// platform-apl/components/harbor/). Read covers the steady-state "already seeded?"
// check; create/update covers the seed after a robot create. Mapped to the
// `harbor-provisioner` Kubernetes-auth role below. Never the harbor admin path
// (ESO's harbor-admin-push owns that) and never any consumer path.
const policyHarborProvisioner = `path "secret/data/harbor/robot"      { capabilities = ["create", "update", "read"] }
path "secret/data/harbor/pull-robot" { capabilities = ["create", "update", "read"] }
path "secret/metadata/harbor/robot"      { capabilities = ["read"] }
path "secret/metadata/harbor/pull-robot" { capabilities = ["read"] }
`

// reconciler-read: metadata-ONLY read on the two in-cluster-rotated object-storage
// key paths, for the in-cluster llz reconciler's credential-age gauges
// (--reconcile-openbao-gauges). It reads updated_time to compute rotation age; it
// never needs the secret data, so this grants no secret/data access — strictly
// less than linode-rotator. Mapped to the `reconciler` k8s-auth role below.
const policyReconcilerRead = `path "secret/metadata/loki/object-store"  { capabilities = ["read"] }
path "secret/metadata/harbor/registry-s3" { capabilities = ["read"] }
`

// broad-pat-rotator: read/write on EXACTLY the broad-PAT path the in-cluster
// broad-PAT rotator owns (the broadPatRotator CronJob, `llz ci rotate-broad-pat`).
// It reads rotated_at (due-check) + re-writes {token, rotated_at} after each mint.
// Never any consumer path, never the narrow PAT (secret/linode/api-token). The GitHub
// token + the current broad PAT reach the CronJob via ESO (the read `platform-ci`
// policy), NOT this role. Mapped to the `broad-pat-rotator` Kubernetes-auth role.
const policyBroadPATRotator = `path "secret/data/linode/broad-pat"     { capabilities = ["create", "update", "read"] }
path "secret/metadata/linode/broad-pat" { capabilities = ["read"] }
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
func baoConfigureSteps(ghRepo, keycloakIssuer string, teams []clusterspec.Team) []baoConfigStep {
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
		// rotator (OBJ keys). Mapped to the linode-rotator k8s-auth role.
		{desc: "write policy linode-rotator", fatal: true, stdin: policyLinodeRotator,
			args: []string{"policy", "write", "linode-rotator", "-"}},
		// harbor-provisioner policy: scoped read/write on the two robot-credential
		// paths for the in-cluster harbor-robot-provisioner CronJob. Mapped to the
		// harbor-provisioner k8s-auth role.
		{desc: "write policy harbor-provisioner", fatal: true, stdin: policyHarborProvisioner,
			args: []string{"policy", "write", "harbor-provisioner", "-"}},
		// reconciler-read policy: metadata-only read for the in-cluster reconciler's
		// credential-age gauges. Mapped to the reconciler k8s-auth role.
		{desc: "write policy reconciler-read", fatal: true, stdin: policyReconcilerRead,
			args: []string{"policy", "write", "reconciler-read", "-"}},
		// broad-pat-rotator policy: scoped read/write on secret/linode/broad-pat for the
		// in-cluster broad-PAT rotator CronJob. Mapped to the broad-pat-rotator role.
		{desc: "write policy broad-pat-rotator", fatal: true, stdin: policyBroadPATRotator,
			args: []string{"policy", "write", "broad-pat-rotator", "-"}},
		// Kubernetes auth role for the External Secrets Operator — lets the ESO
		// ClusterSecretStore authenticate with its in-cluster ServiceAccount token
		// (read-only platform-ci policy) instead of an AppRole secret_id seeded from
		// a GitHub secret and rotated in-cluster via `gh secret set`.
		//
		// apl-core 6.x: ESO is now a CORE, always-on app shipped by apl-core in the
		// `external-secrets` namespace; the landing zone no longer runs its own ESO
		// (the former llz-external-secrets controller). The controller SA is the
		// chart's release name `external-secrets` in namespace `external-secrets`,
		// so the role binds that identity. ESO mints the SA token via TokenRequest
		// (the apl-core ESO ClusterRole carries serviceaccounts/token create).
		{desc: "write kubernetes auth role eso", fatal: true,
			args: []string{"write", "auth/kubernetes/role/eso",
				"bound_service_account_names=external-secrets",
				"bound_service_account_namespaces=external-secrets",
				"policies=platform-ci", "ttl=15m"}},
		// Second Kubernetes-auth role for the SAME ESO controller SA, mapped to the
		// write-scoped eso-pusher policy. The `openbao-push` ClusterSecretStore
		// selects this role (role: eso-pusher) so PushSecrets can write the two
		// generated paths while the read `openbao` store stays read-only via `eso`.
		{desc: "write kubernetes auth role eso-pusher", fatal: true,
			args: []string{"write", "auth/kubernetes/role/eso-pusher",
				"bound_service_account_names=external-secrets",
				"bound_service_account_namespaces=external-secrets",
				"policies=eso-pusher", "ttl=15m"}},
		// NOTE: the standalone linode-rotator Kubernetes-auth role (bound to the
		// linode-cred-rotator CronJob ServiceAccount) was removed when that CronJob
		// was retired — the in-cluster reconciler performs Linode cred rotation under
		// the `reconciler` role (via OPENBAO_KUBERNETES_ROLE=reconciler), which
		// carries the linode-rotator POLICY (see the reconciler role below). The
		// policy stays; only the now-unbound linode-rotator role is gone.
		//
		// Kubernetes auth role for the in-cluster Harbor robot provisioner — binds
		// the harbor-robot-provisioner ServiceAccount (harbor namespace, where the
		// CronJob mounts harbor-admin-password) to the harbor-provisioner policy so
		// it can seed secret/harbor/{robot,pull-robot} without a root token. Harbor
		// provisioning stays a CronJob: the in-cluster reconciler cannot reach the
		// mesh-protected harbor-core Service from the llz-reconciler namespace.
		{desc: "write kubernetes auth role harbor-provisioner", fatal: true,
			args: []string{"write", "auth/kubernetes/role/harbor-provisioner",
				"bound_service_account_names=harbor-robot-provisioner",
				"bound_service_account_namespaces=harbor",
				"policies=harbor-provisioner", "ttl=15m"}},
		// Kubernetes auth role for the in-cluster reconciler — binds the
		// llz-reconciler ServiceAccount to reconciler-read (metadata-only gauge read,
		// --reconcile-openbao-gauges) + linode-rotator (read_write on the two
		// object-storage key paths, --reconcile-linode-creds; it took over the
		// linodeCredRotator CronJob's work). NOT harbor-provisioner — harbor stays a
		// CronJob. Harmless when a flag is disabled (the SA/namespace never match).
		{desc: "write kubernetes auth role reconciler", fatal: true,
			args: []string{"write", "auth/kubernetes/role/reconciler",
				"bound_service_account_names=llz-reconciler",
				"bound_service_account_namespaces=llz-reconciler",
				"policies=reconciler-read,linode-rotator", "ttl=15m"}},
		// Kubernetes auth role for the in-cluster broad-PAT rotator — binds the
		// broad-pat-rotator ServiceAccount (llz-pat-rotator namespace) to the
		// broad-pat-rotator write policy so it can re-write secret/linode/broad-pat.
		// Isolated from the reconciler on purpose (this workload holds the
		// account:read_write token). Harmless when the component is disabled (the
		// SA/namespace never exist, so nothing can assume the role).
		{desc: "write kubernetes auth role broad-pat-rotator", fatal: true,
			args: []string{"write", "auth/kubernetes/role/broad-pat-rotator",
				"bound_service_account_names=broad-pat-rotator",
				"bound_service_account_namespaces=llz-pat-rotator",
				"policies=broad-pat-rotator", "ttl=15m"}},
		// NOTE: an OpenBao kubernetes-auth role for a day-2 Argo Workflows SA will be
		// added HERE when the first rotation-style day-2 Argo job lands — bound to
		// THAT job's own SA + namespace with a scoped write policy (the same shape as
		// `reconciler` above), alongside its OpenBao :8200 egress NetworkPolicy. The
		// cluster-health workflow itself is kube-only (its SA needs no OpenBao), so no
		// role is wired for it. `llz ci openbao-login` is the auth primitive those
		// jobs will use. See docs/designs/day2-incluster-health.md.
	}

	// GitHub Actions OIDC (JWT) auth — repo-bound roles that let a workflow log in
	// with a short-lived, per-run OIDC token instead of a long-lived AppRole
	// secret_id stashed in GitHub Actions secrets (and the in-cluster PAT that
	// rotates it via `gh secret set`). The `secret-propagator` role is the live
	// GitHub-CI auth path (llz ci rotate-incluster-pat); `platform-ci` is read-only,
	// reserved for any future GitHub workflow that reads OpenBao directly (ESO
	// reads in-cluster via Kubernetes auth, not GitHub OIDC). Appended only when the
	// instance repo is known; a repo-less configure (local/dry-run without
	// GITHUB_REPOSITORY) omits them rather than create an unbindable role.
	if ghRepo != "" {
		// The forge (GitHub by default; GHES/GitLab via LLZ_FORGE) supplies the
		// OIDC issuer, audience, and repo-identity claim. On GitHub this is
		// byte-identical to the previously hardcoded config (locked by
		// forge.OpenBaoJWTRoleBody's test); on GHES the issuer becomes the
		// appliance's /_services/token, and on GitLab the bound claim is
		// project_path, not repository.
		f, err := forgeFromEnv()
		if err != nil {
			fmt.Fprintf(os.Stderr, "::warning::forge resolution failed (%v) — skipping OIDC (jwt) auth setup\n", err)
			return steps
		}
		discoveryURL, boundIssuer := forge.OpenBaoJWTAuthConfig(f)
		// SECURITY — bound_claims pins each role to THIS instance repo and
		// bound_audiences to the owner's OIDC default audience. Without BOTH, any
		// repo's OIDC token could mint a token here.
		//
		// The role body is written as a JSON object over stdin (`bao write <path> -`)
		// rather than key=value args: bound_claims is a MAP field, and the CLI
		// rejects a key=value string for it ("expected map[string]interface{}, got
		// string"). JSON also types bound_audiences/token_policies as lists.
		jwtRole := func(name, policy string) baoConfigStep {
			body, _ := forge.OpenBaoJWTRoleBody(f, ghRepo, policy) // json.Marshal of a fixed struct
			return baoConfigStep{desc: "write jwt role " + name, fatal: true, stdin: body,
				args: []string{"write", "auth/jwt/role/" + name, "-"}}
		}
		steps = append(steps,
			// Non-fatal enable (tolerates already-enabled on re-runs), matching
			// the other auth enables above.
			baoConfigStep{desc: "enable jwt (OIDC) auth",
				args: []string{"auth", "enable", "jwt"}},
			baoConfigStep{desc: "configure jwt with the CI OIDC issuer", fatal: true,
				args: []string{"write", "auth/jwt/config",
					"oidc_discovery_url=" + discoveryURL,
					"bound_issuer=" + boundIssuer}},
			jwtRole("platform-ci", "platform-ci"),
			jwtRole("secret-propagator", "secret-propagator"),
		)
	}

	// Keycloak-group OIDC — a SECOND jwt auth mount (path `keycloak`, distinct
	// from the GitHub-OIDC `jwt` mount above) that lets human operators mint a
	// short-lived, team-scoped WRITE token instead of reconstituting root. Each
	// spec.teams entry becomes a `<name>-writer` policy + a role bound on the
	// apl-core realm role `team-<name>`. Appended only when a realm issuer is known
	// (derived from the env's domainSuffix) AND teams are declared; otherwise
	// omitted, so a domain-less or team-less instance is unchanged.
	steps = append(steps, keycloakTeamSteps(keycloakIssuer, teams)...)
	return steps
}

// keycloakRoleBody is the JSON body for a `keycloak` jwt-auth role. bound_claims
// is a map (the CLI rejects key=value for map fields), so — like the GitHub-OIDC
// role — the body is written over stdin as JSON. groups is the Keycloak realm
// group claim; user_claim=sub attributes each write to the operator.
type keycloakRoleBody struct {
	RoleType  string `json:"role_type"`
	UserClaim string `json:"user_claim"`
	// BoundAudiences pins the token `aud` to the llz OIDC client, so ONLY tokens
	// deliberately minted for OpenBao login (the device-flow `llz` client + the
	// smoke direct-grant client, both stamped with this aud by an audience mapper)
	// are accepted — not an arbitrary id_token from another realm client (Grafana,
	// Harbor, the console) that merely carries the `groups` claim.
	BoundAudiences []string          `json:"bound_audiences"`
	BoundClaims    map[string]string `json:"bound_claims"`
	TokenPolicies  []string          `json:"token_policies"`
	TokenTTL       string            `json:"token_ttl"`
	TokenMaxTTL    string            `json:"token_max_ttl"`
}

// keycloakInternalJWKS is Keycloak's realm JWKS on its INTERNAL http service —
// the URL OpenBao (in-cluster) can actually reach to validate team-login tokens
// (the public keycloak.<domain> URL hairpins off the cluster's own LB). The
// service name is apl-core v6's Keycloak.X chart (`keycloak-keycloakx-http`) in
// the `keycloak` namespace; the realm is `otomi`. The egress allow is in
// kubernetes-charts/llz-openbao-platform (platform.networkPolicy.keycloakNamespace).
const keycloakInternalJWKS = "http://keycloak-keycloakx-http.keycloak.svc.cluster.local:8080/realms/otomi/protocol/openid-connect/certs"

// keycloakTeamSteps builds the `keycloak` auth mount + per-team policy/role
// steps. Pure (spec → step table) so it is unit-tested without a cluster. Returns
// nil when there is no issuer or no team, so the configure sequence is unchanged
// on instances that declare neither.
func keycloakTeamSteps(issuer string, teams []clusterspec.Team) []baoConfigStep {
	if issuer == "" || len(teams) == 0 {
		return nil
	}
	steps := []baoConfigStep{
		// Non-fatal enable (tolerates already-enabled on re-runs), like the other
		// auth enables. `-path=keycloak` keeps it separate from the CI `jwt` mount.
		{desc: "enable jwt (OIDC) auth at keycloak/",
			args: []string{"auth", "enable", "-path=keycloak", "jwt"}},
		// Validate id_tokens using Keycloak's INTERNAL JWKS (reachable in-cluster)
		// while binding the PUBLIC issuer (the token's `iss`). We deliberately do
		// NOT use oidc_discovery_url=<public issuer>: in-cluster OpenBao can't reach
		// the public keycloak.<domain> URL (it resolves to the cluster's own LB IP →
		// hairpin, unsupported on LKE-E), so key fetch would time out. jwks_url +
		// bound_issuer also sidesteps the discovery issuer-match check (the internal
		// URL's host differs from the public `iss`). The netpol allowing this egress
		// is in kubernetes-charts/llz-openbao-platform (keycloakNamespace).
		//
		// skip_jwks_validation=true: OpenBao 2.5.0 EAGERLY fetches jwks_url at
		// config-write time (fails the write if unreachable). At bootstrap-openbao
		// time Keycloak has not converged yet (the bootstrap gate waits on Argo/
		// Kyverno/cert-manager, not Keycloak), so an eager fetch would fail and wedge
		// this fatal step → the whole bao-configure job. We validate keys LAZILY at
		// first login instead, by which point Keycloak (and the internal JWKS) is up.
		// The signature is still verified on every login; this only defers the fetch.
		{desc: "configure keycloak auth (internal jwks_url + public bound_issuer)", fatal: true,
			args: []string{"write", "auth/keycloak/config",
				"jwks_url=" + keycloakInternalJWKS, "bound_issuer=" + issuer,
				"skip_jwks_validation=true"}},
	}
	for _, t := range teams {
		policy := t.Name + "-writer"
		// secret/<sub> → secret/data/<sub> (writes) + secret/metadata/<sub> (list).
		data := strings.Replace(t.OpenbaoSubtree, "secret/", "secret/data/", 1)
		meta := strings.Replace(t.OpenbaoSubtree, "secret/", "secret/metadata/", 1)
		hcl := fmt.Sprintf(
			"path %q { capabilities = [\"create\", \"update\", \"read\"] }\n"+
				"path %q { capabilities = [\"read\", \"list\"] }\n",
			data+"/*", meta+"/*")
		body, _ := json.Marshal(keycloakRoleBody{
			RoleType: "jwt",
			// Only accept tokens minted for the llz OIDC client (device flow + the
			// e2e smoke client, both audience-mapped to this id in keycloak-configure).
			BoundAudiences: []string{keycloakDeviceClientID},
			UserClaim:      "sub",
			// Bind on the apl-core realm role `team-<name>` — the value apl-core's
			// default groups claim (a realm-role mapper on the `openid` client
			// scope) carries for a member of the native `team-<name>` group. We do
			// NOT create this group/role: `llz render` declares the team in
			// teamConfig and apl-core provisions it. See clusterspec.Team.AplRole.
			BoundClaims:   map[string]string{"groups": t.AplRole()},
			TokenPolicies: []string{policy},
			TokenTTL:      "15m",
			TokenMaxTTL:   "30m",
		})
		steps = append(steps,
			baoConfigStep{desc: "write policy " + policy, fatal: true, stdin: hcl,
				args: []string{"policy", "write", policy, "-"}},
			baoConfigStep{desc: "write keycloak role " + t.Name, fatal: true, stdin: string(body),
				args: []string{"write", "auth/keycloak/role/" + t.Name, "-"}},
		)
	}
	return steps
}

// keycloakIssuerFor resolves the OpenBao OIDC discovery URL (the Keycloak
// `otomi` realm issuer). On a self-installed cluster it derives from the env's
// domainSuffix. On a Linode Managed App Platform cluster (managedAppPlatform:
// true) Linode owns the lke<id>.akamai-apl.net domain and the spec has no
// domainSuffix, so we discover apl-core's own issuer in-cluster from the
// otomi/otomi-api ConfigMap (its SSO_ISSUER key). Returns "" when nothing can be
// resolved — the Keycloak team steps are then skipped (with a warning at the
// call site).
func keycloakIssuerFor(region string) string {
	lz, err := clusterspec.LoadInstance(".")
	if err != nil {
		return ""
	}
	e, ok := lz.Env(region)
	if !ok {
		return ""
	}
	// Managed App Platform: Linode owns the domain and the spec should carry no
	// domainSuffix. Bind ONLY to the in-cluster-discovered issuer — NEVER fall back
	// to a spec domainSuffix, which on managed could be a stale/wrong value (the
	// gsap incident) and would silently bind OpenBao to the wrong Keycloak. An empty
	// result safely skips the team steps (with a warning at the call site).
	if e.Cluster.Bootstrap.ManagedAppPlatform {
		return discoverKeycloakIssuerFromCluster()
	}
	if e.Cluster.Bootstrap.DomainSuffix != "" {
		return "https://keycloak." + e.Cluster.Bootstrap.DomainSuffix + "/realms/otomi"
	}
	return ""
}

// discoverKeycloakIssuerFromCluster reads apl-core's own SSO_ISSUER from the
// otomi/otomi-api ConfigMap — the source of truth on a Managed App Platform
// cluster (e.g. https://keycloak.lke634445.akamai-apl.net/realms/otomi). Returns
// "" when the ConfigMap/key is absent or the cluster is unreachable (kubectl
// runs with the bootstrap kubeconfig, so this is available at configure time).
func discoverKeycloakIssuerFromCluster() string {
	out, err := kubectlOut("-n", "otomi", "get", "cm", "otomi-api",
		"-o", "jsonpath={.data.SSO_ISSUER}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// discoverManagedDomain returns the Managed App Platform domain suffix
// (lke<clusterID>.akamai-apl.net) discovered from apl-core's own in-cluster
// config, or "" when unavailable. It reuses the Keycloak realm issuer that
// discoverKeycloakIssuerFromCluster reads from the otomi/otomi-api ConfigMap and
// strips it down to the bare domain. This is the single runtime source of truth
// on a managed cluster, where Linode owns the domain and the spec has no
// domainSuffix. (The platform HTTPRoute hostnames — console.<domain>,
// harbor.<domain> — are an equivalent source; the issuer is reused here to keep
// one discovery read.)
func discoverManagedDomain() string {
	return managedDomainFromIssuer(discoverKeycloakIssuerFromCluster())
}

// managedDomainFromIssuer extracts the bare domain suffix from a Keycloak realm
// issuer URL of the form https://keycloak.<domain>/realms/otomi. Pure and
// unit-tested; returns "" for anything that doesn't match that shape.
func managedDomainFromIssuer(issuer string) string {
	s, ok := strings.CutPrefix(issuer, "https://keycloak.")
	if !ok {
		return ""
	}
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	return s
}

// specTeams returns spec.teams, or nil when the spec can't be loaded.
func specTeams() []clusterspec.Team {
	lz, err := clusterspec.LoadInstance(".")
	if err != nil {
		return nil
	}
	return lz.Spec.Teams
}

// regionIsManaged reports whether this region is a Managed App Platform cluster.
func regionIsManaged(region string) bool {
	lz, err := clusterspec.LoadInstance(".")
	if err != nil {
		return false
	}
	e, ok := lz.Env(region)
	return ok && e.Cluster.Bootstrap.ManagedAppPlatform
}

// managedTeamsPreflight guards the Keycloak team-OIDC setup on a MANAGED cluster.
// On managed, LLZ cannot create teams (Linode owns apl-core's values) — the
// operator creates them in the App Platform Console, which provisions the
// team-<name> namespace AND the Keycloak group the OpenBao role binds on. So a
// team the OpenBao role would bind to may not exist yet. Verify each declared
// team's namespace (the reliable, admin-cred-free proxy for "the team exists")
// and drop (with a loud, actionable warning) any that isn't there — so we never
// create an OpenBao role bound to a nonexistent group. Self-install is unchanged
// (llz render declares teamConfig and apl-core provisions the team).
func managedTeamsPreflight(region string, teams []clusterspec.Team) ([]clusterspec.Team, error) {
	return filterManagedTeams(regionIsManaged(region), teams, namespaceStatus)
}

// namespaceStatus reports whether a namespace exists, and whether that answer is
// DEFINITE. `kubectl get namespace <ns> --ignore-not-found -o name` exits 0 with
// EMPTY output for a genuine NotFound and 0 with `namespace/<ns>` when it exists;
// any other (non-nil) error is a transient/systemic failure (API unreachable, RBAC,
// throttle) whose answer is NOT definite — so the caller must not read it as "missing".
func namespaceStatus(ns string) (exists, definite bool) {
	out, err := kubectlOut("get", "namespace", ns, "--ignore-not-found", "-o", "name")
	if err != nil {
		return false, false
	}
	return strings.TrimSpace(out) != "", true
}

// filterManagedTeams is the pure core of managedTeamsPreflight (unit-tested):
// self-install passes teams through; managed keeps only teams whose team-<name>
// namespace DEFINITELY exists, warning about the definitely-missing ones. A team
// whose existence can't be determined (transient kubectl failure) aborts with an
// error rather than being silently dropped — dropping it would under-provision team
// credentials while the command still exits 0.
func filterManagedTeams(managed bool, teams []clusterspec.Team, nsStatus func(string) (exists, definite bool)) ([]clusterspec.Team, error) {
	if !managed {
		return teams, nil
	}
	var ready []clusterspec.Team
	for _, t := range teams {
		exists, definite := nsStatus("team-" + t.Name)
		if !definite {
			return nil, fmt.Errorf("managed App Platform: could not determine whether team %q exists (kubectl get namespace team-%s failed) — refusing to silently drop teams on a transient failure; check cluster access and re-run `llz ci bao-configure`", t.Name, t.Name)
		}
		if !exists {
			fmt.Fprintf(os.Stderr, "::warning::managed App Platform: team %q has no team-%s namespace — create the team in the App Platform Console (Platform → Teams) first so its Keycloak group team-%s is provisioned, then re-run `llz ci bao-configure`. Skipping its OpenBao Keycloak role for now.\n", t.Name, t.Name, t.Name)
			continue
		}
		ready = append(ready, t)
	}
	return ready, nil
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
		fmt.Fprintln(os.Stderr, "::warning::GITHUB_REPOSITORY unset — skipping GitHub-OIDC (jwt) auth setup; the CI in-cluster-PAT rotation (llz ci rotate-incluster-pat) stays unavailable until re-run with GITHUB_REPOSITORY set.")
	}
	// Keycloak-group OIDC for human team writes (spec.teams). The issuer is
	// derived from this region's domainSuffix; skip (with a warning) when teams
	// are declared but no issuer can be formed, so a misconfigured domain doesn't
	// silently drop the team roles.
	teams := specTeams()
	// LoadInstance (which specTeams uses) does NOT run Validate — only render does.
	// Gate here so a spec that reached this cluster without a render pass can't make
	// us build OpenBao policies from an unvalidated/unsafe subtree.
	if errs := clusterspec.ValidateTeams(teams); len(errs) > 0 {
		fmt.Fprintf(os.Stderr, "::warning::spec.teams failed validation (%v) — skipping the keycloak team auth setup; fix the spec and re-run `llz ci bao-configure`.\n", errs)
		teams = nil
	}
	// Managed App Platform: only bind OpenBao to teams whose Keycloak group actually
	// exists (created by the operator in the Console). Drops not-yet-created teams
	// with an actionable warning instead of binding to a nonexistent group. Skipped
	// on --dry-run: it needs live cluster access and would show a cluster-FILTERED
	// plan; dry-run prints the full intended plan (managed filtering happens at apply).
	if !g.dryRun {
		filtered, err := managedTeamsPreflight(region, teams)
		if err != nil {
			return err
		}
		teams = filtered
	}
	keycloakIssuer := keycloakIssuerFor(region)
	if len(teams) > 0 && keycloakIssuer == "" {
		fmt.Fprintf(os.Stderr, "::warning::spec.teams declares %d team(s) but no Keycloak issuer could be derived for region %q (spec unreadable or cluster.bootstrap.domainSuffix unset) — skipping the keycloak team auth setup.\n", len(teams), region)
	}
	if g.dryRun {
		fmt.Fprintln(os.Stderr, "→ (dry-run) would preflight the root token and apply the configure sequence:")
		for _, s := range baoConfigureSteps(ghRepo, keycloakIssuer, teams) {
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

	for _, step := range baoConfigureSteps(ghRepo, keycloakIssuer, teams) {
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
