package identityconfig

// ci_keycloak_configure.go — `llz ci keycloak-configure`, the Keycloak-realm half
// of the team-scoped-credentials turnkey path. `llz ci bao-configure` provisions
// the OpenBao side (the keycloak auth mount + per-team policy/role); this ensures
// the matching realm objects so `llz openbao login` works with no manual console
// steps: a device-flow OIDC client, a `groups` claim mapper on it, and one realm
// group per spec.teams entry.
//
// It reaches Keycloak over an ephemeral kubectl port-forward to the keycloak pod
// (the pods/portforward subresource is allowed on LKE-E even where the apiserver
// service-proxy is webhook-denied), authenticates to the master realm with the
// in-cluster admin creds, and drives the admin REST API. Every interaction is
// best-effort: any failure WARNS and falls back to the manual runbook step
// (docs/runbooks/openbao-team-login.md) rather than failing — so an unexpected
// Keycloak API shape can never wedge the bootstrap it runs inside. See ADR 0004.

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/assertobs"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/keycloak"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/kube"
)

const (
	keycloakNS       = "keycloak"
	keycloakPod      = "keycloak-keycloakx-0" // apl-core's Keycloak.X StatefulSet pod-0
	keycloakHTTPPort = "8080"                 // in-pod plaintext listener (TLS terminates at the edge)
	keycloakRealm    = "otomi"
	// DeviceClientID is the public OIDC client `llz openbao login` uses;
	// overridable there via --client-id / OPENBAO_OIDC_CLIENT_ID.
	DeviceClientID = "llz"
	// AdminSecret holds the MASTER-realm admin creds (keycloak.AdminToken
	// direct-grants against /realms/master with client admin-cli). On managed
	// apl-core that is `keycloak-initial-admin` — the secret the Keycloak.X
	// StatefulSet consumes as KC_BOOTSTRAP_ADMIN_USERNAME/PASSWORD. The old
	// `platform-admin-credentials` was a self-installed-era name that NOTHING
	// provisions on managed, so keycloak-configure read empty creds and
	// warnKeycloakSkip'd every run — leaving the device-flow client uncreated and
	// team-OIDC OpenBao login silently unavailable. (The otomi-realm portal login
	// `platform-admin-initial-credentials` is a DIFFERENT secret and cannot
	// master-realm direct-grant.)
	AdminSecret = "keycloak-initial-admin"
)

// Bootstrap ordering guard: how long keycloak-configure waits for apl-core to
// converge the `openid` client scope before wiring the device client. ~5 min
// (30 × 10s) — apl-core Keycloak is usually up well before then. Vars (not
// consts) so tests can shrink them.
// The poll bounds themselves now live in internal/keycloak (exported vars, so the
// mutation test can still shrink them). Only the sleep seam stays here.
var keycloakSleepFn = time.Sleep

// PortForwardFn opens a port-forward to the Keycloak pod's HTTP port and
// returns the local base URL + teardown. A package var so tests seam it.
var PortForwardFn = portForwardKeycloak

func portForwardKeycloak() (string, func(), error) {
	cmd := exec.Command("kubectl", "port-forward", "-n", keycloakNS, "pod/"+keycloakPod, ":"+keycloakHTTPPort)
	cmd.Stderr = os.Stderr // surface wrong-context / pod-absent / RBAC errors (see portForwardOpenbao)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", nil, err
	}
	if err := cmd.Start(); err != nil {
		return "", nil, fmt.Errorf("kubectl port-forward: %w", err)
	}
	stop := func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }
	localPort, err := assertobs.ReadForwardPortTimeout(stdout, assertobs.ForwardEstablishTimeout)
	if err != nil {
		stop()
		return "", nil, err
	}
	go func() { _, _ = io.Copy(io.Discard, stdout) }()
	return "http://127.0.0.1:" + localPort, stop, nil
}

// ensureDeviceClient makes the public device-flow client exist and returns its
// UUID (idempotent: returns the existing client's id when already present).

// ensureAudienceMapper adds an OIDC audience protocol mapper that stamps
// `aud: <audience>` into the client's id + access tokens, so OpenBao's keycloak
// role (bound_audiences=[llz]) accepts tokens this client mints and rejects
// tokens minted for any other realm client. Idempotent: a 409 (mapper already
// present) is success.

// getOrCreateClient returns the UUID of clientID, creating the public device-flow
// client (idempotent) when it doesn't exist yet.

// ensureClientDefaultScope attaches realm client scope `name` to the client as a
// DEFAULT scope if not already assigned (idempotent). Returns an actionable error
// when the realm scope doesn't exist yet (apl-core hasn't converged Keycloak) —
// the caller warns and the bootstrap re-run fixes it once the scope appears.

// findClientScopeID returns the realm client-scope id for name, or "" if the
// realm has no such scope yet.

// waitForClientScope blocks until the realm client scope `name` exists (apl-core
// provisions it asynchronously), polling keycloak.ScopeAttempts times with
// keycloak.ScopeInterval between tries. It guards the bootstrap ordering race: we
// must not wire the device client before its groups-carrying scope exists, or the
// client is created scope-less and `llz openbao login` 403s. Returns an
// actionable error if the scope never appears (the caller warns, best-effort).

// NOTE: we deliberately do NOT create realm groups or a `groups` claim mapper.
// apl-core owns both: it provisions a `team-<name>` group + realm role from the
// teamConfig `llz render` emits, and its default `openid` client scope already
// carries a `groups` realm-role claim. This command's only job is the one thing
// apl-core won't do — a PUBLIC device-flow client for `llz openbao login`.

func RunKeycloakConfigure(dryRun bool, region string) error {
	if region == "" {
		return fmt.Errorf("--region is required")
	}
	// spec.teams is the intent gate: no teams → no team login path → no client.
	if teams := SpecTeams(); len(teams) == 0 {
		fmt.Println("No spec.teams declared — nothing to configure in Keycloak.")
		return nil
	}
	if dryRun {
		fmt.Fprintf(os.Stderr, "→ (dry-run) would ensure the public device-flow client %q (openid scope) in realm %s.\n", DeviceClientID, keycloakRealm)
		return nil
	}

	// Best-effort from here: warn + succeed on any Keycloak-side failure so a
	// realm/API-shape surprise never wedges the bootstrap this runs in. The
	// manual fallback is docs/runbooks/openbao-team-login.md step 3.
	user := kube.SecretFieldOf(keycloakNS, AdminSecret, "username")
	pass := kube.SecretFieldOf(keycloakNS, AdminSecret, "password")
	if user == "" || pass == "" {
		warnKeycloakSkip(region, fmt.Errorf("admin creds not readable from %s/%s (keys username/password)", keycloakNS, AdminSecret))
		return nil
	}

	// Readiness gate: keycloak-configure runs early in bootstrap and can outrun
	// apl-core bringing Keycloak up, so wait for the server to be serving before
	// giving up — retry the port-forward + master-token exchange (a successful
	// admin token IS the "Keycloak is up" signal) until it works or the budget
	// expires. Bounded + best-effort: a persistently-down Keycloak still warns +
	// exits 0, and a re-run finishes the wiring.
	hc := &http.Client{Timeout: 20 * time.Second}
	base, token, cleanup, err := Connect(hc, user, pass, keycloakSleepFn)
	if err != nil {
		warnKeycloakSkip(region, err)
		return nil
	}
	defer cleanup()
	k := &keycloak.Client{HC: hc, Base: base, Token: token, Realm: keycloakRealm}

	// Ordering guard: wait for apl-core to converge the `openid` client scope
	// (which carries the groups claim) BEFORE wiring the client, so a bootstrap
	// that runs ahead of apl-core doesn't create a scope-less client that 403s at
	// login. Best-effort — if it never appears, warn + exit 0 (the re-run fixes it).
	if err := k.WaitForClientScope("openid", keycloakSleepFn); err != nil {
		warnKeycloakSkip(region, err)
		return nil
	}

	if _, err := k.EnsureDeviceClient(DeviceClientID); err != nil {
		warnKeycloakSkip(region, fmt.Errorf("ensure device client %s: %w", DeviceClientID, err))
		return nil
	}
	fmt.Printf("Keycloak client %q ready (public device flow, openid scope) — operators can `llz openbao login --team <name>`.\n", DeviceClientID)
	return nil
}

func Connect(hc *http.Client, user, pass string, sleep func(time.Duration)) (base, token string, cleanup func(), err error) {
	for i := 0; i < keycloak.ScopeAttempts; i++ {
		b, c, e := PortForwardFn()
		if e == nil {
			tok, te := keycloak.AdminToken(hc, b, user, pass)
			if te == nil {
				return b, tok, c, nil
			}
			c() // this port-forward is done
			if errors.Is(te, keycloak.ErrAuthDenied) {
				return "", "", func() {}, fmt.Errorf("keycloak admin creds rejected (check %s/%s) — not a readiness problem: %w", keycloakNS, AdminSecret, te)
			}
			err = te // transient (5xx / not-ready) — retry
		} else {
			err = e
		}
		if i < keycloak.ScopeAttempts-1 {
			sleep(keycloak.ScopeInterval)
		}
	}
	return "", "", func() {}, fmt.Errorf("keycloak %s/%s did not become ready after ~%s (%w) — apl-core Keycloak has not converged; re-run `llz ci keycloak-configure` once it is up", keycloakNS, keycloakPod, time.Duration(keycloak.ScopeAttempts)*keycloak.ScopeInterval, err)
}

func warnKeycloakSkip(region string, err error) {
	fmt.Fprintf(os.Stderr, "::warning::keycloak-configure on %s could not finish (%v) — team OIDC login stays unavailable until the realm is wired by hand (docs/runbooks/openbao-team-login.md step 3). This does not block the bootstrap.\n", region, err)
}
