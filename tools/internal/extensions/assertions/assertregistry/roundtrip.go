package assertregistry

// ci_assert_harbor_roundtrip.go implements `llz ci assert-harbor-roundtrip` — the
// gate that a minted Harbor robot can actually authenticate to the registry for
// both PULL and PUSH.
//
// THE REGRESSION IT EXISTS FOR. Managed instances rendered HARBOR_HOST as
// "harbor." — the domain suffix was empty, so the host was truncated to a
// trailing dot. Non-empty, so it defeated every `== ""` guard, including the
// systeminfo fallback that was supposed to discover the real host. The robot was
// minted correctly, published correctly, and stored correctly; every credential
// in the chain was valid. What broke was the HOST they were presented to, and the
// result was a 401 on push AND on pull. Nothing caught it, because nothing ever
// used the credential — the provisioner asserted it had CREATED a robot, not that
// the robot could log in anywhere.
//
// `harborauth.UsableRegistryHost` (ci_harbor_provisioner.go) is the static half of the fix
// and it is good, but it can only reject hosts that look malformed to it. This is
// the empirical half: present the credential to the registry and see.
//
// WHAT IT DOES. The OCI distribution v2 auth handshake, which is exactly what a
// `docker push` does before it moves a byte:
//
//   1. GET /v2/ unauthenticated → expect 401 with a Www-Authenticate: Bearer
//      challenge naming the token realm. (A 200 here means the registry is
//      unauthenticated, which is its own finding.)
//   2. Request a token from the realm with the robot's basic auth, for
//      scope "repository:<repo>:pull,push".
//   3. ASSERT THE TOKEN CARRIES THE REQUESTED ACCESS. This is the sharp bit:
//      Harbor's token service returns 200 with a VALID JWT and an EMPTY access
//      list when the credential lacks the scope. "I got a token" is not proof of
//      authorization, and a gate that stopped at the status code would pass for a
//      robot that can do nothing.
//   4. Use it: GET /v2/<repo>/tags/list (pull) and open + immediately cancel a
//      blob upload session, POST then DELETE (push). The upload session is the
//      cheapest operation that requires real push authorization, and cancelling
//      it leaves nothing behind.
//
// It uploads no layers and creates no tags — the push half is proven by the
// registry granting an upload session, not by pushing an artifact into a project
// that e2e would then have to clean up.
//
// FAIL-CLOSED: an unreachable registry, a missing credential Secret, a token with
// no access, or any unexpected status fails. Read-only in effect.

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/harborauth"
)

const (
	// harborauth.RobotSecretNS/Name is the Secret ESO materializes from
	// secret/harbor/robot (llz-cert-automation chart, harborDockerConfig).
	// ProbeRepo is the repository the scope is requested against. It need
	// not exist: a token scoped to a repository is granted (or refused) on the
	// robot's policy, and tags/list on a missing repo is a clean NAME_UNKNOWN.
	// ProbeRepo is EXPORTED because the cobra flag default lives in internal/cli.
	ProbeRepo = "library/llz-roundtrip-probe"
)

// harborauth.RobotCreds is what the round trip needs.
func readSecretWithSettle(ns, name string, settle, interval time.Duration) ([]byte, error) {
	deadline := time.Now().Add(settle)
	for attempt := 1; ; attempt++ {
		raw, err := harborauth.ReadRobotSecret(ns, name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "::error::could not read Secret %s/%s (%v)\n", ns, name, err)
			return nil, fmt.Errorf("could not read Secret %s/%s: %w", ns, name, err)
		}
		if len(bytes.TrimSpace(raw)) > 0 {
			return raw, nil
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "::error::the robot credential Secret %s/%s never appeared within %s\n", ns, name, settle)
			return nil, fmt.Errorf("robot credential Secret %s/%s never appeared within %s — ESO has not materialized it "+
				"from secret/harbor/robot; check the harbor-robot-provisioner CronJob has ticked and the ExternalSecret is Ready",
				ns, name, settle)
		}
		fmt.Printf("attempt %d: Secret %s/%s not present yet — retrying in %s\n", attempt, ns, name, interval)
		time.Sleep(interval)
	}
}

// probeHarborRoundTrip runs the full handshake once.
func probeHarborRoundTrip(creds harborauth.RobotCreds, repo string) error {
	base := "https://" + creds.RegistryHost

	// 1. Unauthenticated /v2/ → Bearer challenge.
	req, err := http.NewRequest(http.MethodGet, base+"/v2/", nil)
	if err != nil {
		return err
	}
	resp, err := harborauth.HTTP(req)
	if err != nil {
		return fmt.Errorf("GET %s/v2/: %w — the registry host is unreachable, which for a host that "+
			"passed the format guard usually means DNS or the ingress, not the credential", base, err)
	}
	challengeHeader := resp.Header.Get("Www-Authenticate")
	status := resp.StatusCode
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if status == http.StatusOK {
		return fmt.Errorf("GET %s/v2/ returned 200 with no auth challenge — this registry is serving the "+
			"distribution API unauthenticated", base)
	}
	if status != http.StatusUnauthorized {
		return fmt.Errorf("GET %s/v2/ returned HTTP %d, expected a 401 auth challenge", base, status)
	}
	ch, err := harborauth.ParseBearerChallenge(challengeHeader)
	if err != nil {
		return fmt.Errorf("%s/v2/: %w", base, err)
	}

	// 2. Exchange basic auth for a scoped token.
	tokURL, err := url.Parse(ch.Realm)
	if err != nil {
		return fmt.Errorf("token realm %q is not a URL: %w", ch.Realm, err)
	}
	q := tokURL.Query()
	q.Set("scope", "repository:"+repo+":pull,push")
	if ch.Service != "" {
		q.Set("service", ch.Service)
	}
	tokURL.RawQuery = q.Encode()

	treq, err := http.NewRequest(http.MethodGet, tokURL.String(), nil)
	if err != nil {
		return err
	}
	treq.SetBasicAuth(creds.Username, creds.Password)
	tresp, err := harborauth.HTTP(treq)
	if err != nil {
		return fmt.Errorf("token request to %s: %w", ch.Realm, err)
	}
	tbody, _ := io.ReadAll(tresp.Body)
	tresp.Body.Close()
	if tresp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("the robot credential was REJECTED by the token service (HTTP 401) — "+
			"robot %q cannot authenticate to %s", creds.Username, creds.RegistryHost)
	}
	if tresp.StatusCode < 200 || tresp.StatusCode >= 300 {
		return fmt.Errorf("token request returned HTTP %d: %s", tresp.StatusCode, harborauth.TruncateForError(tbody))
	}
	token, access, err := harborauth.ParseTokenResponse(tbody)
	if err != nil {
		return err
	}

	// 3. The token must actually GRANT what was asked for. Harbor answers 200
	//    with a valid JWT and an empty access list when the robot lacks the
	//    scope, so the status code proves nothing.
	granted := harborauth.GrantedActions(access, repo)
	if missing := harborauth.MissingActions(granted, "pull", "push"); len(missing) > 0 {
		return fmt.Errorf("the token service issued a token for robot %q that does NOT grant %s on %s "+
			"(granted: %v). A 200 here is not authorization — Harbor returns a valid token with an empty "+
			"access list when the credential lacks the scope, so the robot's project permissions are the thing to check",
			creds.Username, strings.Join(missing, "+"), repo, keysOf(granted))
	}

	// 4. Use it. PULL: tags/list. A repository that does not exist answers
	//    404/NAME_UNKNOWN, which still proves the token was ACCEPTED — the failure
	//    we are hunting is 401, not 404.
	if err := harborAuthedProbe(http.MethodGet, base+"/v2/"+repo+"/tags/list", token,
		map[int]bool{http.StatusOK: true, http.StatusNotFound: true}, "pull"); err != nil {
		return err
	}

	// PUSH: open a blob upload session, then cancel it. This is the cheapest
	// operation that requires genuine push authorization and it leaves nothing.
	loc, err := harborauth.UploadSession(base, repo, token)
	if err != nil {
		return err
	}
	if loc != "" {
		cancelURL := loc
		if strings.HasPrefix(loc, "/") {
			cancelURL = base + loc
		}
		// Best-effort cleanup: the session expires on its own, and failing the gate
		// because a cancel did not land would report a push problem that is not one.
		creq, err := http.NewRequest(http.MethodDelete, cancelURL, nil)
		if err == nil {
			creq.Header.Set("Authorization", "Bearer "+token)
			if cresp, err := harborauth.HTTP(creq); err == nil {
				_, _ = io.Copy(io.Discard, cresp.Body)
				cresp.Body.Close()
			}
		}
	}
	return nil
}

// harborUploadSession opens a blob upload and returns its Location.

// harborAuthedProbe issues an authenticated request and requires one of okStatus.
func harborAuthedProbe(method, u, token string, okStatus map[int]bool, what string) error {
	req, err := http.NewRequest(method, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := harborauth.HTTP(req)
	if err != nil {
		return fmt.Errorf("%s probe (%s): %w", what, u, err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%s DENIED: %s returned HTTP %d with a token that granted it — %s",
			strings.ToUpper(what), u, resp.StatusCode, harborauth.TruncateForError(body))
	}
	if !okStatus[resp.StatusCode] {
		return fmt.Errorf("%s probe %s returned HTTP %d: %s", what, u, resp.StatusCode, harborauth.TruncateForError(body))
	}
	return nil
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Run performs the OCI distribution v2 auth handshake with the robot credential
// ESO materialized from secret/harbor/robot.
// Run performs the OCI distribution v2 auth handshake with the robot credential
// ESO materialized from secret/harbor/robot.
//
// NO Deps PARAMETER, AND THAT IS THIS EXTRACTION'S FINDING. It is the first of
// seventeen to need no injected capabilities at all. Everything it touches is
// either stdlib (net/http, for a handshake against a public registry endpoint) or
// already behind internal/harborauth's OWN package vars — NamespaceExists and
// ReadRobotSecret are swappable there, which is how the tests below drive it.
//
// A Deps struct was written for this package and then deleted. It carried
// ReadSecret (duplicating harborauth's var) and a Now/Sleep clock pair (for a
// settle loop the existing tests already exercise with millisecond budgets).
// Both were seams for capabilities that were already seamed. That is the third
// time in three extractions: internal/converge's LokiConfigText, then
// internal/assertreconciler's leaseHolderRenew, now this. **Ask what is ALREADY
// injectable before adding a Deps field — a redundant seam is not free, it splits
// one swap point into two and lets a test stub half of it.**
func Run(secretNS, secretName, registry, repo string, settle, interval time.Duration) error {
	fmt.Println("## Harbor robot round-trip assertion (pull + push authorization)")

	// Is the component here at all? A managed App Platform cluster renders a
	// minimal app set that does not include llz-cert-automation, and on
	// lke638103 the namespace simply did not exist ("namespaces
	// \"llz-cert-automation\" not found"). That is a deployment shape, not a
	// broken credential.
	switch present, err := harborauth.NamespaceExists(secretNS); {
	case err != nil:
		fmt.Fprintf(os.Stderr, "::error::could not tell whether namespace %s exists (%v)\n", secretNS, err)
		return fmt.Errorf("could not determine whether namespace %s exists: %w", secretNS, err)
	case !present:
		fmt.Printf("SKIP: namespace %s does not exist — the llz-cert-automation component is not deployed on this "+
			"cluster (managed App Platform renders a minimal app set), so there is no robot credential to round-trip.\n", secretNS)
		return nil
	}

	// The Secret READ is inside the settle loop, not before it. It is written by
	// ESO from secret/harbor/robot, which the harbor-robot-provisioner CronJob
	// seeds only AFTER Harbor's registry is serving — internal/health/allowlists.go
	// documents it as deferred on a fresh bootstrap and explicitly says it must not
	// pin the convergence gate. Reading once, first, made this gate fail on exactly
	// the window that file says to expect.
	raw, err := readSecretWithSettle(secretNS, secretName, settle, interval)
	if err != nil {
		return err
	}
	creds, err := harborauth.DecodeRobotSecret(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "::error::%v\n", err)
		return err
	}
	if registry != "" {
		creds.RegistryHost = registry
	}
	fmt.Printf("robot %q → registry %s, scope repository:%s:pull,push\n", creds.Username, creds.RegistryHost, repo)

	var lastErr error
	deadline := time.Now().Add(settle)
	for attempt := 1; ; attempt++ {
		lastErr = probeHarborRoundTrip(creds, repo)
		if lastErr == nil {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		fmt.Printf("attempt %d: %v — retrying in %s\n", attempt, lastErr, interval)
		time.Sleep(interval)
	}

	if lastErr != nil {
		fmt.Printf("FAIL: %v\n", lastErr)
		fmt.Fprintln(os.Stderr, "::error::the Harbor robot cannot complete a pull+push round trip")
		return fmt.Errorf("harbor robot round trip failed: %w", lastErr)
	}
	fmt.Printf("OK: robot %q authenticated to %s and holds pull+push on %s\n", creds.Username, creds.RegistryHost, repo)
	return nil
}
