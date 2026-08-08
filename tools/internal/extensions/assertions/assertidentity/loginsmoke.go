package assertidentity

// ci_keycloak_smoke.go — `llz ci team-login-smoke`, an END-TO-END validation of
// the team-scoped OpenBao write path, browser-free. It exercises the exact chain
// that is otherwise only E2E-gated: apl-core provisions the `team-<name>` group +
// realm role → a member's OIDC token carries `groups: [team-<name>]` → OpenBao's
// `keycloak` role binds it → the `<name>-writer` policy scopes the write. It also
// covers the platform-admin path — a token carrying only `platform-admin` (not the
// team group) must mint the same writer token, since the role binds that role too.
//
// It does NOT drive the device-flow browser (that UX is generic OAuth 2.0 device
// grant, already httptest-covered). Instead it mints the same id_token via a
// throwaway PUBLIC direct-grant client — apl's realm allows direct access grants —
// so a scripted CI run gets a real, apl-shaped groups claim with no browser.
//
// Everything Keycloak-side runs against the PUBLIC realm URL (keycloak.<domain>)
// so the id_token's `iss` matches OpenBao's configured oidc_discovery_url; only
// the token exchange + the write assertions ride the OpenBao port-forward. All
// created objects (client, user) are torn down; the in-subtree smoke secret is
// left (the team token has no delete capability — harmless on a throwaway e2e KV).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/keycloak"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/openbao"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/platform"
)

// kc wraps the shared Keycloak client so this package can hang its own smoke
// helpers off it. Go will not let us add methods to keycloak.Client from here —
// which is the same constraint that forced the client into its own package, seen
// from the other side. An embedded struct is the idiomatic answer and costs one
// conversion at the call site.
type kc struct{ *keycloak.Client }

// smokeTargets resolves the public Keycloak base URL + the team name/subtree to
// validate, from the spec.
func smokeTargets(region, teamFlag string) (base, team, subtree string, err error) {
	lz, e := clusterspec.LoadInstance(".")
	if e != nil {
		return "", "", "", fmt.Errorf("load spec: %w", e)
	}
	teams := lz.Spec.Teams
	if len(teams) == 0 {
		return "", "", "", fmt.Errorf("no spec.teams declared — nothing to validate")
	}
	pick := teams[0]
	if teamFlag != "" {
		found := false
		for _, t := range teams {
			if t.Name == teamFlag {
				pick, found = t, true
				break
			}
		}
		if !found {
			return "", "", "", fmt.Errorf("team %q not in spec.teams", teamFlag)
		}
	}
	env, ok := lz.Env(region)
	if !ok {
		return "", "", "", fmt.Errorf("region %q not found in the spec", region)
	}
	domain := env.Cluster.Bootstrap.DomainSuffix
	if domain == "" && env.Cluster.Bootstrap.ManagedAppPlatform {
		// Managed App Platform: Linode owns the domain (no spec domainSuffix); the
		// smoke test runs against the live cluster, so discover it from apl-core.
		domain = caps.ManagedDomain()
	}
	if domain == "" {
		return "", "", "", fmt.Errorf("region %q has no cluster.bootstrap.domainSuffix (and no managed domain could be discovered) — can't form the public Keycloak URL", region)
	}
	return "https://keycloak." + domain, pick.Name, pick.OpenbaoSubtree, nil
}

func runTeamLoginSmoke(region, teamFlag string) error {
	if region == "" {
		return fmt.Errorf("--region is required")
	}
	// No teams declared → nothing to validate. A clean no-op (like keycloak-configure
	// / bao-configure) so the e2e team-write gate passes for teamless instances.
	if len(caps.SpecTeams()) == 0 {
		fmt.Println("No spec.teams declared — nothing to validate (team-login smoke skipped).")
		return nil
	}
	base, team, subtree, err := smokeTargets(region, teamFlag)
	if err != nil {
		return err
	}
	user := caps.SecretField(keycloak.NS, keycloak.AdminSecret, "username")
	pass := caps.SecretField(keycloak.NS, keycloak.AdminSecret, "password")
	if user == "" || pass == "" {
		// k8sSecretField returns "" for every failure mode alike, so say which one this
		// is — otherwise the e2e team-write lane dead-ends on an unactionable line.
		return fmt.Errorf("admin creds not readable from %s/%s (want keys username/password): %s",
			keycloak.NS, keycloak.AdminSecret, caps.DescribeSecret(keycloak.NS, keycloak.AdminSecret))
	}
	fmt.Fprintf(os.Stderr, "→ team-login smoke: team %q, subtree %q, realm %s at %s\n", team, subtree, keycloak.Realm, base)

	hc := &http.Client{Timeout: 30 * time.Second}
	adminTok, err := keycloak.AdminToken(hc, base, user, pass)
	if err != nil {
		return fmt.Errorf("keycloak admin token: %w", err)
	}
	k := &kc{&keycloak.Client{HC: hc, Base: base, Token: adminTok, Realm: keycloak.Realm}}

	group := "team-" + team
	gid, err := k.FindGroupID(group)
	if err != nil {
		return fmt.Errorf("look up group %s: %w", group, err)
	}

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	username := "llz-smoke-" + suffix
	password := "Smoke-" + suffix + "-Aa1!" // satisfy any realm password policy
	clientID := "llz-smoke-" + suffix

	clientUUID, err := k.EnsureDirectGrantClient(clientID)
	if err != nil {
		return fmt.Errorf("create smoke client: %w", err)
	}
	defer func() {
		if err := k.DeleteClient(clientUUID); err != nil {
			fmt.Fprintf(os.Stderr, "::error::failed to delete smoke client %s: %v — it is a public ROPC client with aud:llz (a standing login path into the OpenBao mount); delete it manually.\n", clientID, err)
			return
		}
		fmt.Fprintf(os.Stderr, "  torn down smoke client %s\n", clientID)
	}()

	uid, err := k.CreateSmokeUser(username, password)
	if err != nil {
		return fmt.Errorf("create test user: %w", err)
	}
	defer func() {
		// Neutralize before deleting: a disabled orphan can't authenticate even if
		// the delete fails. Surface any teardown failure LOUDLY — a lingering enabled
		// member of the team-<name> group is a standing OpenBao-write credential.
		disabled := k.DisableUser(uid) == nil
		if err := k.DeleteUser(uid); err != nil {
			state := "DISABLED"
			if !disabled {
				state = "still-ENABLED"
			}
			fmt.Fprintf(os.Stderr, "::error::failed to delete smoke user %s (id %s): %v — a %s member of Keycloak group %s is orphaned in the realm; delete it manually\n", username, uid, err, state, group)
			return
		}
		fmt.Fprintf(os.Stderr, "  torn down test user %s\n", username)
	}()
	// Grant the team's realm role — the source of the groups:[team-<name>] claim the
	// OpenBao role binds on. apl-core normally grants it via the team-<name> GROUP,
	// but that group is created LAZILY on the first onboarded member (the App
	// Platform Console), so a freshly-provisioned team has the ROLE but no group yet.
	// Use the group when it exists (exercises the real group→role→claim path);
	// otherwise grant the realm role directly — the realm-role→groups mapper keys off
	// the ROLE, not the group, so both yield the identical claim (verified live). This
	// lets the smoke validate a team before anyone is onboarded.
	if gid != "" {
		if err := k.AddUserToGroup(uid, gid); err != nil {
			return fmt.Errorf("add user to %s: %w", group, err)
		}
	} else {
		if err := k.AddRealmRoleToUser(uid, group); err != nil {
			return fmt.Errorf("grant realm role %s: %w", group, err)
		}
		fmt.Printf("  team-%s group not provisioned yet (created lazily on first onboarded member) — granted realm role %s directly\n", team, group)
	}

	// Mint the id_token (browser-free stand-in for the device flow) and prove it
	// carries the apl group→role→claim wiring the OpenBao role binds on.
	idToken, err := k.PasswordGrant(clientID, username, password)
	if err != nil {
		return fmt.Errorf("password grant: %w", err)
	}
	groups, err := decodeJWTGroups(idToken)
	if err != nil {
		return fmt.Errorf("decode id_token: %w", err)
	}
	if !containsString(groups, group) {
		return fmt.Errorf("id_token groups %v does not contain %q — apl group→role→claim wiring is broken (OpenBao would 403)", groups, group)
	}
	fmt.Printf("✓ id_token carries groups=%v (contains %s)\n", groups, group)

	// Exchange at OpenBao + assert the scope.
	addr, cleanup, err := caps.PortForwardOpenbao()
	if err != nil {
		return fmt.Errorf("port-forward to %s/%s: %w", "llz-openbao", "platform-openbao-0", err)
	}
	defer cleanup()
	ctx := context.Background()
	token, err := openbao.OIDCLogin(ctx, openbao.HTTPClientLoopback(30*time.Second), addr, "keycloak", team, idToken)
	if err != nil {
		return fmt.Errorf("openbao oidc login (mount keycloak, role %s): %w", team, err)
	}
	obc := openbao.NewWithClient(addr, token, "", openbao.HTTPClientLoopback(30*time.Second))

	inPath := subtree + "/_llz_smoke_" + suffix
	if err := obc.Write(ctx, inPath, map[string]string{"ok": "1"}); err != nil {
		return fmt.Errorf("EXPECTED scoped write to %s to SUCCEED, got: %w", inPath, err)
	}
	fmt.Printf("✓ scoped write to %s succeeded\n", inPath)

	outPath := "secret/linode/_llz_smoke_" + suffix
	if err := obc.Write(ctx, outPath, map[string]string{"nope": "1"}); err == nil {
		return fmt.Errorf("SECURITY: out-of-subtree write to %s SUCCEEDED but must be denied — the %s-writer policy is not scoped", outPath, team)
	} else if !isDenied(err) {
		return fmt.Errorf("out-of-subtree write to %s failed, but not with a 403/permission-denied: %w", outPath, err)
	}
	fmt.Printf("✓ out-of-subtree write to %s correctly denied\n", outPath)

	// ── Reader path: the ESO ClusterSecretStore consumes team secrets via the `eso`
	// Kubernetes-auth role, which bao-configure binds to the read-only <team>-reader
	// policy (issue #335). This is the READ half of the team-credentials feature —
	// without it a team can write secrets nothing in-cluster can consume. Prove the
	// binding end-to-end: mint the external-secrets controller SA token (the identity
	// the eso role binds), log in as role `eso`, and assert it can READ the key the
	// team just wrote, then is DENIED a path no policy grants.
	saJWT, err := mintServiceAccountToken(platform.ESONamespace, esoServiceAccount)
	if err != nil {
		return fmt.Errorf("mint %s/%s SA token for the eso-reader check: %w", platform.ESONamespace, esoServiceAccount, err)
	}
	esoTok, err := openbao.KubernetesLogin(ctx, openbao.HTTPClientLoopback(30*time.Second), addr, "kubernetes", "eso", saJWT)
	if err != nil {
		return fmt.Errorf("openbao kubernetes login (role eso) failed — is the eso k8s-auth role configured + reachable? %w", err)
	}
	esoC := openbao.NewWithClient(addr, esoTok, "", openbao.HTTPClientLoopback(30*time.Second))
	// %v, not %w, on err: the !ok / wrong-value branches reach here with a NIL err
	// (Get reports an absent secret as ok=false, err=nil), and %w would render that
	// as "%!w(<nil>)" in the one place an operator reads this — a color.Red e2e lane.
	if v, ok, err := esoC.Get(ctx, inPath, "ok"); err != nil || !ok || v != "1" {
		return fmt.Errorf("EXPECTED the eso role to READ %s (the team-written key) via the %s-reader policy, got value=%q ok=%v err=%v — bao-configure did not grant ESO read on the team subtree", inPath, team, v, ok, err)
	}
	fmt.Printf("✓ eso role read %s (team subtree is ESO-readable)\n", inPath)

	// A path under neither the team subtree nor platform-ci → must 403. (Not a
	// platform-ci path: the eso role legitimately carries platform-ci too.) The
	// leading '_' makes this unclaimable by construction: openbaoSubtreeRe pins team
	// subtrees to lowercase-kebab segments, so no spec.teams entry can ever cover it.
	//
	// A NIL error here is a finding, not a pass: OpenBao checks the ACL before the
	// backend, so an ungranted path 403s — it only 404s (ok=false, err=nil) once the
	// token is ALLOWED to look and finds nothing. Either way the path was reachable.
	denyPath := "secret/_llz_smoke_denied_" + suffix + "/x"
	if _, _, err := esoC.Get(ctx, denyPath, "k"); err == nil {
		return fmt.Errorf("SECURITY: the eso role was PERMITTED to read %s (no 403 — the read resolved, empty or not) but no policy grants it — the %s-reader scope is too broad", denyPath, team)
	} else if !isDenied(err) {
		return fmt.Errorf("read of %s by the eso role failed, but not with a 403/permission-denied: %w", denyPath, err)
	}
	fmt.Printf("✓ eso role denied %s (reader scoped to the team subtree + platform-ci)\n", denyPath)

	// ── Admin path: apl-core's built-in platform admin carries the all-teams realm
	// role `platform-admin` (keycloak.PlatformAdminRole), NOT the team's own group. The
	// OpenBao role now binds groups on {team-<name>, platform-admin}, so an admin
	// must ALSO be able to mint this team's writer token and write its subtree
	// WITHOUT being enrolled in team-<name>. Prove it end-to-end. Skipped only if the
	// built-in role is absent (an unconverged realm), mirroring the team-role
	// fallback above.
	adminRole := keycloak.PlatformAdminRole
	if ok, err := k.RealmRoleExists(adminRole); err != nil {
		return fmt.Errorf("look up realm role %s: %w", adminRole, err)
	} else if !ok {
		fmt.Printf("  %s realm role absent (unconverged realm) — admin-path check skipped\n", adminRole)
	} else {
		aSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
		aUser := "llz-smoke-admin-" + aSuffix
		aPass := "Smoke-" + aSuffix + "-Aa1!"
		aUID, err := k.CreateSmokeUser(aUser, aPass)
		if err != nil {
			return fmt.Errorf("create admin test user: %w", err)
		}
		defer func() {
			disabled := k.DisableUser(aUID) == nil
			if err := k.DeleteUser(aUID); err != nil {
				state := "DISABLED"
				if !disabled {
					state = "still-ENABLED"
				}
				fmt.Fprintf(os.Stderr, "::error::failed to delete admin smoke user %s (id %s): %v — a %s holder of realm role %s is orphaned; delete it manually\n", aUser, aUID, err, state, adminRole)
				return
			}
			fmt.Fprintf(os.Stderr, "  torn down admin test user %s\n", aUser)
		}()
		if err := k.AddRealmRoleToUser(aUID, adminRole); err != nil {
			return fmt.Errorf("grant realm role %s: %w", adminRole, err)
		}
		aIDToken, err := k.PasswordGrant(clientID, aUser, aPass)
		if err != nil {
			return fmt.Errorf("admin password grant: %w", err)
		}
		aGroups, err := decodeJWTGroups(aIDToken)
		if err != nil {
			return fmt.Errorf("decode admin id_token: %w", err)
		}
		if !containsString(aGroups, adminRole) {
			return fmt.Errorf("admin id_token groups %v does not contain %q — apl does not emit the platform-admin role into the groups claim, so the platform-admin bound-claim can never match", aGroups, adminRole)
		}
		if containsString(aGroups, group) {
			return fmt.Errorf("admin test user unexpectedly carries %q — it must validate the admin path via %q alone", group, adminRole)
		}
		fmt.Printf("✓ admin id_token carries groups=%v (has %s, not %s)\n", aGroups, adminRole, group)

		aOBTok, err := openbao.OIDCLogin(ctx, openbao.HTTPClientLoopback(30*time.Second), addr, "keycloak", team, aIDToken)
		if err != nil {
			return fmt.Errorf("admin openbao oidc login (role %s) must SUCCEED via %s but did not — the team role does not accept the platform-admin group: %w", team, adminRole, err)
		}
		aOBC := openbao.NewWithClient(addr, aOBTok, "", openbao.HTTPClientLoopback(30*time.Second))
		aPath := subtree + "/_llz_smoke_admin_" + aSuffix
		if err := aOBC.Write(ctx, aPath, map[string]string{"ok": "1"}); err != nil {
			return fmt.Errorf("EXPECTED platform-admin write to %s to SUCCEED via %s, got: %w", aPath, adminRole, err)
		}
		fmt.Printf("✓ platform-admin (%s) wrote %s without team-%s membership\n", adminRole, aPath, team)
	}

	fmt.Printf("SMOKE PASS: team %q — device-identity login → scoped write OK, out-of-subtree denied, platform-admin write OK.\n", team)
	return nil
}

// realmRoleExists reports whether an EXACT realm role name is present in the realm.

func isDenied(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "403") || strings.Contains(s, "permission denied")
}

// esoServiceAccount is the External Secrets Operator controller ServiceAccount
// (apl-core 6.x ships ESO in platform.ESONamespace with a same-named SA). It is the
// identity bao-configure's `eso` Kubernetes-auth role binds — see
// ci_openbao_configure.go (bound_service_account_names=external-secrets).
const esoServiceAccount = "external-secrets"

// mintServiceAccountToken issues a short-lived token for ns/sa via the
// TokenRequest API (kubectl create token), letting the smoke authenticate to
// OpenBao AS that ServiceAccount — the same identity ESO presents for the `eso`
// Kubernetes-auth role. Requires serviceaccounts/token create on ns/sa (the
// smoke already reads cluster secrets + port-forwards, so it runs privileged).
// Routed through the package-wide execOutput seam (exec.go), like every other
// output-capturing shell-out — this is not one of the interactive/stdin call
// sites that deliberately keep calling os/exec directly.
func mintServiceAccountToken(ns, sa string) (string, error) {
	out, err := caps.W().CreateToken(ns, sa, "10m")
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("kubectl create token %s/%s: %s", ns, sa, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("kubectl create token %s/%s: %w", ns, sa, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// decodeJWTGroups extracts the `groups` claim from a JWT's (unverified) payload.
// Verification is OpenBao's job on the exchange; here we only assert the claim
// shape apl-core is expected to emit.
func decodeJWTGroups(jwt string) ([]string, error) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("not a JWT (want 3 dot-separated parts)")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}
	var claims struct {
		Groups []string `json:"groups"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("parse claims: %w", err)
	}
	return claims.Groups, nil
}

// ── keycloak.Client smoke helpers (admin REST) ──────────────────────────────────────

// findGroupID returns the realm group id for an EXACT name, or "" if absent.

// ensureDirectGrantClient makes a PUBLIC client with direct access grants + the
// openid scope (so its password-grant tokens carry the groups claim). Returns the
// client uuid. Idempotent enough for the smoke (a fresh suffixed id each run).

// createSmokeUser makes an enabled realm user with an inline non-temporary
// password, returning its id.

// addRealmRoleToUser grants the named realm role to the user — the direct-grant
// equivalent of team-<name> group membership for the groups claim, used when the
// team's group is not provisioned yet (a fresh team before its first member).
